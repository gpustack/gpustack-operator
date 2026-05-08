package server

import (
	"context"
	"fmt"
	"text/template"

	sprig "github.com/go-task/slim-sprig/v3"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	server "gpustack.ai/gpustack/api/server/v1"
	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/server/kuberess"
	"gpustack.ai/gpustack/pkg/server/settings"
	"gpustack.ai/gpustack/pkg/utils/bytex"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/utils/version"
	"gpustack.ai/gpustack/pkg/worker"
)

// ClusterImportConfigHandler handles v1.ClusterImportConfig objects,
// which is a subresource of v1.Cluster objects.
type ClusterImportConfigHandler struct {
	extensionapi.ObjectInfo
	extensionapi.GetOperation

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func newClusterImportConfigHandler(opts extensionapi.SetupOptions) *ClusterImportConfigHandler {
	h := &ClusterImportConfigHandler{}

	// As storage.
	h.ObjectInfo = &server.ClusterConfig{}
	h.GetOperation = extensionapi.WithGet(h)

	// Set client.
	h.Client = opts.Manager.GetClient()

	return h
}

var (
	_ rest.Storage = (*ClusterImportConfigHandler)(nil)
	_ rest.Getter  = (*ClusterImportConfigHandler)(nil)
)

func (h *ClusterImportConfigHandler) New() runtime.Object {
	return &server.ClusterImportConfig{}
}

func (h *ClusterImportConfigHandler) Destroy() {}

func (h *ClusterImportConfigHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// Get cluster.
	cls := new(servercore.Cluster)
	err := h.Client.Get(ctx, key, cls, &opts)
	if err != nil {
		return nil, kerrors.NewInternalError(err)
	}

	// Fill cluster import config.
	clsImpCfg := &server.ClusterImportConfig{
		ObjectMeta: cls.ObjectMeta,
		Status: server.ClusterImportConfigStatus{
			Type: cls.Spec.Type,
		},
	}

	// Generate cluster import config content.
	ctrReg, err := settings.ContainerRegistry.Value(ctx)
	if err != nil {
		return nil, kerrors.NewInternalError(fmt.Errorf("get container registry setting: %w", err))
	}
	ctrNas, err := settings.ContainerNamespace.Value(ctx)
	if err != nil {
		return nil, kerrors.NewInternalError(fmt.Errorf("get container namespace setting: %w", err))
	}
	specImg, specImgPullPolicy := extractImageConfig(ctx, h.APIReader)
	srvUrl, err := settings.ServeUrl.Value(ctx)
	if err != nil {
		return nil, kerrors.NewInternalError(fmt.Errorf("get server url setting: %w", err))
	}
	data := map[string]any{
		"ContainerRegistry":  ctrReg,
		"ContainerNamespace": ctrNas,
		"Namespace":          kuberess.SystemNamespaceName,
		"Image":              specImg,
		"ImagePullPolicy":    specImgPullPolicy,
		"Version":            version.Version,
		"ClusterType":        cls.Spec.Type,
		"ServerURL":          srvUrl,
		"Token":              stringx.SumBySHA256(string(cls.UID), cls.Namespace, cls.Name),
		"Team":               cls.Namespace,
		"Cluster":            cls.Name,
		"SecurePort":         worker.NewOptions().BindPort,
	}
	clsImpCfg.Status.Config, err = renderClusterImportConfigTemplate(data)
	if err != nil {
		return nil, kerrors.NewInternalError(fmt.Errorf("render cluster import config template: %w", err))
	}

	return clsImpCfg, nil
}

const templateClusterImportConfig = `
apiVersion: v1
kind: Namespace
metadata:
  name: {{ $.Namespace }}
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator"
---
apiVersion: v1
kind: ServiceAccount
metadata:
  namespace: {{ $.Namespace }}
  name: gpustack-operator-worker
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator-worker"
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: gpustack-operator-worker
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator-worker"
subjects:
  - kind: ServiceAccount
    namespace: {{ $.Namespace }}
    name: gpustack-operator-worker
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
---
apiVersion: v1
kind: Service
metadata:
  namespace: {{ $.Namespace }}
  name: gpustack-operator-worker
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator-worker"
spec:
  selector:
    "app.kubernetes.io/part-of": "gpustack-operator"
    "app.kubernetes.io/component": "worker"
    "app.kubernetes.io/name": "gpustack-operator-worker"
  sessionAffinity: ClientIP
  ports:
    - name: https
      port: 443
      targetPort: https
---
apiVersion: apps/v1
kind: Deployment
metadata:
  namespace: {{ $.Namespace }}
  name: gpustack-operator-worker
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator"
    "app.kubernetes.io/component": "worker"
    "app.kubernetes.io/name": "gpustack-operator-worker"
spec:
  replicas: 1
  selector:
    matchLabels:
      "app.kubernetes.io/part-of": "gpustack-operator"
      "app.kubernetes.io/component": "worker"
      "app.kubernetes.io/name": "gpustack-operator-worker"
  template:
    metadata:
      labels:
        "app.kubernetes.io/part-of": "gpustack-operator"
        "app.kubernetes.io/component": "worker"
        "app.kubernetes.io/name": "gpustack-operator-worker"
    spec:
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                topologyKey: "kubernetes.io/hostname"
                labelSelector:
                  matchExpressions:
                    - key: "app.kubernetes.io/part-of"
                      operator: In
                      values:
                        - "gpustack-operator"
                    - key: "app.kubernetes.io/component"
                      operator: In
                      values:
                        - "worker"
                    - key: "app.kubernetes.io/name"
                      operator: In
                      values:
                        - "gpustack-operator-worker"
      restartPolicy: Always
      serviceAccountName: gpustack-operator-worker
      containers:
        - name: main
{{- $image := "" }}
{{- if $.Image -}}
  {{- $image = $.Image -}}
{{- else -}}
  {{- $registry := default "docker.io" $.ContainerRegistry -}}
  {{- $namespace := default "gpustack" $.ContainerNamespace -}}
  {{- $image = printf "%s/%s/gpustack:%s" $registry $namespace $.Version -}}
{{- end }}
          image: "{{ $image }}"
          imagePullPolicy: "{{ default "IfNotPresent" $.ImagePullPolicy }}"
          args:
            - gpustack
            - worker
            - -v=2
            - --secure-port={{ $.SecurePort }}
{{- if ne .ClusterType "ReverseProxy" }}
            - --disable-peer
{{- end }}
          resources:
            limits:
              cpu: '4'
              memory: '8Gi'
            requests:
              cpu: '500m'
              memory: '512Mi'
          env:
            # Pass envs to worker, which can be used for worker identification and debugging.
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
            - name: KUBERNETES_SERVICE_NAME
              value: "gpustack-operator-worker"
            # Pass container registry and namespace settings to worker, which can be used for pulling images in the future.
            #
            - name: GPUSTACK_CONTAINER_REGISTRY
              value: "{{ $.ContainerRegistry }}"
            - name: GPUSTACK_CONTAINER_NAMESPACE
              value: "{{ $.ContainerNamespace }}"
            # Pass server URL and token to worker, which can be used for worker registration and authentication when cluster type is ReverseProxy.
            #
{{- if eq .ClusterType "ReverseProxy" }}
            - name: GPUSTACK_SERVER_URL
              value: "{{ $.ServerURL }}"
            - name: GPUSTACK_TOKEN
              value: "{{ $.Token }}"
            - name: GPUSTACK_TEAM
              value: "{{ $.Team }}"
            - name: GPUSTACK_CLUSTER
              value: "{{ $.Cluster }}"
{{- end }}
          ports:
            - name: https
              containerPort: {{ $.SecurePort }}
          startupProbe:
            failureThreshold: 10
            periodSeconds: 5
            httpGet:
              scheme: HTTPS
              port: https
              path: /readyz
          readinessProbe:
            failureThreshold: 3
            timeoutSeconds: 5
            periodSeconds: 5
            httpGet:
              scheme: HTTPS
              port: https
              path: /readyz
          livenessProbe:
            failureThreshold: 10
            timeoutSeconds: 5
            periodSeconds: 10
            httpGet:
              scheme: HTTPS
              port: https
              path: /livez
          volumeMounts:
            - name: gpustack-data-dir
              mountPath: /var/lib/gpustack
      volumes:
        - name: gpustack-data-dir
          emptyDir: {}
`

func renderClusterImportConfigTemplate(data map[string]any) (string, error) {
	tmpl, err := template.New("cluster-import-template").
		Funcs(sprig.TxtFuncMap()).
		Parse(templateClusterImportConfig)
	if err != nil {
		return "", err
	}

	buf := bytex.GetBuffer()
	defer bytex.Put(buf)

	if err = tmpl.Execute(buf, data); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func extractImageConfig(ctx context.Context, apiReader ctrlcli.Reader) (img, imgPullPolicy string) {
	img = osx.Getenv("GPUSTACK_IMAGE_DEBUGGING")
	if img == "" {
		podName := osx.Getenv("KUBERNETES_POD_NAME")
		if podName != "" {
			pod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{
					Name:      podName,
					Namespace: kuberess.SystemNamespaceName,
				},
			}
			opts := ctrlcli.GetOptions{Raw: &meta.GetOptions{ResourceVersion: "0"}}
			err := apiReader.Get(ctx, ctrlcli.ObjectKeyFromObject(pod), pod, &opts)
			if err == nil {
				for i := range pod.Spec.Containers {
					ctr := &pod.Spec.Containers[i]
					if ctr.Name == "main" {
						img = ctr.Image
						imgPullPolicy = string(ctr.ImagePullPolicy)
						break
					}
				}
				if img == "" {
					img = pod.Spec.Containers[0].Image
					imgPullPolicy = string(pod.Spec.Containers[0].ImagePullPolicy)
				}
				return img, imgPullPolicy
			}
		}
	}

	imgPullPolicy = osx.Getenv("GPUSTACK_IMAGE_PULL_POLICY_DEBUGGING")
	if imgPullPolicy == "" {
		imgPullPolicy = "IfNotPresent"
	}
	return img, imgPullPolicy
}
