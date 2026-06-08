package kuberess

import (
	"context"
	"fmt"
	"text/template"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/devicemanager"
	"gpustack.ai/gpustack/pkg/devicemanager/kuberess"
	"gpustack.ai/gpustack/pkg/kubeapp/helm"
	"gpustack.ai/gpustack/pkg/kubeappyaml"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/utils/version"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

func installGPUStackDeviceManager(ctx context.Context, helmCli *helm.Client, globalValuesContext map[string]any, disable sets.Set[string]) error {
	name := "device-manager"
	if disable.Has(name) {
		return nil
	}

	ctrCfg := extractContainerConfig(ctx, helmCli.KubeClientSet())

	valuesContext := globalValuesContext
	valuesContext["Namespace"] = helmCli.DefaultNamespace()
	valuesContext["Image"] = ctrCfg.Image
	valuesContext["ImagePullPolicy"] = ctrCfg.ImagePullPolicy
	if len(ctrCfg.Env) > 0 {
		valuesContext["Env"] = ctrCfg.Env
	}
	valuesContext["Version"] = version.Version
	valuesContext["SecurePort"] = devicemanager.NewOptions().ServerOptions.BindPort

	funcMap := extendDeviceManagerApplyYamlTemplateFuncMap()
	funcMap["lookup"] = func(apiversion, kind, namespace, name string) (map[string]any, error) {
		return kubediscovery.Lookup(ctx, helmCli.KubeClientSet().Discovery(), apiversion, kind, namespace, name)
	}
	content, err := renderDeviceManagerApplyYamlTemplate(valuesContext, funcMap)
	if err != nil {
		return err
	}

	return kubeappyaml.ApplyWithRestClientGetter(ctx, content, helmCli.KubeRestClientGetter())
}

const deviceManagerApplyYamlTemplate kubeappyaml.Template = `
apiVersion: v1
kind: ServiceAccount
metadata:
  namespace: {{ $.Namespace }}
  name: gpustack-operator-device-manager
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator-device-manager"
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: gpustack-operator-device-manager
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator-device-manager"
subjects:
  - kind: ServiceAccount
    namespace: {{ $.Namespace }}
    name: gpustack-operator-device-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
---
apiVersion: v1
kind: Service
metadata:
  namespace: {{ $.Namespace }}
  name: gpustack-operator-device-manager
  annotations:
    "prometheus.io/scrape": "true"
    "prometheus.io/port": "443"
    "prometheus.io/path": "/metrics"
    "prometheus.io/scheme": "https"
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator-device-manager"
spec:
  selector:
    "app.kubernetes.io/part-of": "gpustack-operator"
    "app.kubernetes.io/component": "device-manager"
    "app.kubernetes.io/name": "gpustack-operator-device-manager"
  sessionAffinity: ClientIP
  ports:
    - name: https
      port: 443
      targetPort: https
{{- $image := "" }}
{{- if $.Image -}}
  {{- $image = $.Image -}}
{{- else -}}
  {{- $registry := default "docker.io" $.ContainerRegistry -}}
  {{- $namespace := default "gpustack" $.ContainerNamespace -}}
  {{- $image = printf "%s/%s/gpustack-operator:%s" $registry $namespace $.Version -}}
{{- end }}
{{- range $.Manufacturers }}
{{- $manu := . }}
{{- $manuPciID := getPciID $manu }}
{{- $daemonSet := lookup "apps/v1" "DaemonSet" $.Namespace (printf "gpustack-operator-device-manager-%s" $manu) }}
{{- if or (not $daemonSet) (ne $image ($daemonSet | dig "spec" "template" "spec" "containers" (list (dict)) | first | dig "image" "")) }}
{{- if has $manu (list "nvidia" "mthreads") }}
{{- if not (lookup "node.k8s.io/v1" "RuntimeClass" "" $manu) }}
---
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: {{ $manu }}
handler: {{ $manu }}
{{- end }}
{{- end }}
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  namespace: "{{ $.Namespace }}"
  name: gpustack-operator-device-manager-{{ $manu }}
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator"
    "app.kubernetes.io/component": "device-manager"
    "app.kubernetes.io/name": "gpustack-operator-device-manager"
spec:
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
      maxSurge: 0
  revisionHistoryLimit: 5
  selector:
    matchLabels:
      "app.kubernetes.io/part-of": "gpustack-operator"
      "app.kubernetes.io/component": "device-manager"
      "app.kubernetes.io/name": "gpustack-operator-device-manager"
  template:
    metadata:
      labels:
        "app.kubernetes.io/part-of": "gpustack-operator"
        "app.kubernetes.io/component": "device-manager"
        "app.kubernetes.io/name": "gpustack-operator-device-manager"
    spec:
{{- if $.ImagePullSecrets }}
      imagePullSecrets:
{{- range $.ImagePullSecrets }}
        - name: {{ . }}
{{- end }}
{{- end }}
{{- if has $manu (list "nvidia" "mthreads") }}
      runtimeClassName: {{ $manu }}
{{- end }}
      nodeSelector:
        # Rely on NFD.
        #
        feature.node.kubernetes.io/pci-{{ $manuPciID }}.present: "true"
      serviceAccountName: gpustack-operator-device-manager
      priorityClassName: system-cluster-critical
      tolerations:
        - operator: "Exists"
      containers:
        - name: main
          image: "{{ $image }}"
          imagePullPolicy: "{{ default "IfNotPresent" $.ImagePullPolicy }}"
          args:
            - gpustack-operator
            - device-manager
            - serve
            - -v=2
            - --secure-port={{ $.SecurePort }}
            - --manufacturer={{ $manu }}
          resources:
            limits:
              cpu: '4'
              memory: '8Gi'
            requests:
              cpu: '100m'
              memory: '128Mi'
          securityContext:
            allowPrivilegeEscalation: true
            capabilities: {}
            privileged: true
            readOnlyRootFilesystem: false
            runAsNonRoot: false
          env:
            # Pass pod IP and name to worker, which can be used for worker identification and debugging.
            #
            - name: KUBERNETES_POD_IP
              valueFrom:
                fieldRef:
                  fieldPath: status.podIP
            - name: KUBERNETES_POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: KUBERNETES_POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: KUBERNETES_NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: KUBERNTES_SERVICE_NAME
              value: "gpustack-operator-device-manager"
{{- if $.Env }}
{{- toYaml $.Env | nindent 12 }}
{{- end }}
          ports:
            - name: https
              containerPort: {{ $.SecurePort }}
          startupProbe:
            failureThreshold: 10
            periodSeconds: 5
            httpGet:
              port: https
              path: /readyz
              scheme: HTTPS
          readinessProbe:
            failureThreshold: 3
            timeoutSeconds: 5
            periodSeconds: 5
            httpGet:
              port: https
              path: /readyz
              scheme: HTTPS
          livenessProbe:
            failureThreshold: 10
            timeoutSeconds: 5
            periodSeconds: 10
            httpGet:
              port: https
              path: /livez
              scheme: HTTPS
          volumeMounts:
            - name: dev-dir
              mountPath: /dev
            - name: sys-dir
              mountPath: /sys
            - name: gpustack-data-dir
              mountPath: /var/lib/gpustack
            - name: cdi-dir
              mountPath: /var/run/cdi
            - name: kubelet-device-plugins-dir
              mountPath: /var/lib/kubelet/device-plugins
{{- if eq $manu "amd" }}
            - name: gpustack-amd-driver
              mountPath: /opt/rocm
              readOnly: true
{{- end }}
{{- if eq $manu "ascend" }}
            - name: gpustack-ascend-driver
              mountPath: /usr/local/dcmi
              readOnly: true
            - name: gpustack-ascend-toolkit
              mountPath: /usr/local/Ascend
              readOnly: true
{{- end }}
{{- if eq $manu "cambricon" }}
            - name: gpustack-cambricon-driver
              mountPath: /usr/local/neuware
              readOnly: true
{{- end }}
{{- if eq $manu "hygon" }}
            - name: gpustack-hygon-driver
              mountPath: /opt/hyhal
              readOnly: true
            - name: gpustack-hygon-toolkit
              mountPath: /opt/dtk
              readOnly: true
{{- end }}
{{- if eq $manu "iluvatar" }}
            - name: gpustack-iluvatar-toolkit
              mountPath: /usr/local/corex
              readOnly: true
{{- end }}
{{- if eq $manu "metax" }}
            - name: gpustack-metax-driver
              mountPath: /opt/mxdriver
              readOnly: true
            - name: gpustack-metax-toolkit
              mountPath: /opt/maca
              readOnly: true
{{- end }}
{{- if eq $manu "thead" }}
            - name: gpustack-thead-toolkit
              mountPath: /usr/local/PPU_SDK
              readOnly: true
{{- end }}
      volumes:
        - name: dev-dir
          hostPath:
            path: /dev
            type: Directory
        - name: sys-dir
          hostPath:
            path: /sys
            type: Directory
        - name: gpustack-data-dir
          hostPath:
            path: /var/lib/gpustack
            type: DirectoryOrCreate
        - name: cdi-dir
          hostPath:
            path: /var/run/cdi
            type: DirectoryOrCreate
        - name: kubelet-device-plugins-dir
          hostPath:
            path: /var/lib/kubelet/device-plugins
            type: DirectoryOrCreate
{{- if eq $manu "amd" }}
        - name: gpustack-amd-driver
          hostPath:
            path: /opt/rocm
            type: DirectoryOrCreate
{{- end }}
{{- if eq $manu "ascend" }}
        - name: gpustack-ascend-driver
          hostPath:
            path: /usr/local/dcmi
            type: DirectoryOrCreate
        - name: gpustack-ascend-toolkit
          hostPath:
            path: /usr/local/Ascend
            type: DirectoryOrCreate
{{- end }}
{{- if eq $manu "cambricon" }}
        - name: gpustack-cambricon-driver
          hostPath:
            path: /usr/local/neuware
            type: DirectoryOrCreate
{{- end }}
{{- if eq $manu "hygon" }}
        - name: gpustack-hygon-driver
          hostPath:
            path: /opt/hyhal
            type: DirectoryOrCreate
        - name: gpustack-hygon-toolkit
          hostPath:
            path: /opt/dtk
            type: DirectoryOrCreate
{{- end }}
{{- if eq $manu "iluvatar" }}
        - name: gpustack-iluvatar-toolkit
          hostPath:
            path: /usr/local/corex
            type: DirectoryOrCreate
{{- end }}
{{- if eq $manu "metax" }}
        - name: gpustack-metax-driver
          hostPath:
            path: /opt/mxdriver
            type: DirectoryOrCreate
        - name: gpustack-metax-toolkit
          hostPath:
            path: /opt/maca
            type: DirectoryOrCreate
{{- end }}
{{- if eq $manu "thead" }}
        - name: gpustack-thead-toolkit
          hostPath:
            path: /usr/local/PPU_SDK
            type: DirectoryOrCreate
{{- end }}
{{- end }}
{{- end }}
`

func renderDeviceManagerApplyYamlTemplate(data map[string]any, extendFuncMap template.FuncMap) (string, error) {
	return deviceManagerApplyYamlTemplate.Render(data, extendFuncMap)
}

func extendDeviceManagerApplyYamlTemplateFuncMap() template.FuncMap {
	return map[string]any{
		"getPciID": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return devicefeature.GetPciID(s)
		},
		"lookup": func(apiversion, kind, namespace, name string) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
}

type _ContainerConfig struct {
	Image           string
	ImagePullPolicy string
	Env             []core.EnvVar
}

func extractContainerConfig(ctx context.Context, cli kubernetes.Interface) (ctrCfg _ContainerConfig) {
	if v := osx.Getenv("KUBERNETES_POD_NAME"); v != "" {
		pod, err := cli.CoreV1().
			Pods(kuberess.SystemNamespaceName).
			Get(ctx, v,
				meta.GetOptions{
					ResourceVersion: "0",
				})
		if err == nil {
			var ctr *core.Container
			for i := range pod.Spec.Containers {
				if pod.Spec.Containers[i].Name == "main" {
					ctr = &pod.Spec.Containers[i]
					break
				}
			}
			if ctr == nil {
				ctr = &pod.Spec.Containers[0]
			}
			ctrCfg.Image = ctr.Image
			ctrCfg.ImagePullPolicy = string(ctr.ImagePullPolicy)
			ctrCfg.Env = make([]core.EnvVar, 0, len(ctr.Env))
			for i := range ctr.Env {
				if !stringx.HasPrefix(ctr.Env[i].Name, "GPUSTACK_") {
					continue
				}
				ctrCfg.Env = append(ctrCfg.Env, ctr.Env[i])
			}
			return ctrCfg
		}
	}

	ctrCfg.ImagePullPolicy = settings.ImagePullPolicy.ShouldValueFromRemote(ctx)
	return ctrCfg
}
