package kuberess

import (
	"context"
	"fmt"
	"path/filepath"

	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/kubeapp/helm"
	"gpustack.ai/gpustack/pkg/system"
)

const (
	CSIProvisionerS3 = "s3.csi.gpustack.ai"
)

func installCSIDriverS3(ctx context.Context, helmCli *helm.Client, globalValuesContext map[string]any, disable sets.Set[string]) error {
	// NB: please update the following files if changed.
	// - pack/gpustack-operator/image/Dockerfile.
	// - If update csi-node-driver-registrar and csi-provisioner,
	//   please also update the versions in pkg/worker/kuberess/apps_csi_driver_nfs.go.

	name := "csi-driver-s3"
	version := "0.43.7"
	if disable.Has(name) {
		return nil
	}

	release := "gpustack-csi-driver-s3"
	path := filepath.Join(system.SubConfDir("charts"), fmt.Sprintf("%s-%s.tgz", name, version))
	download := fmt.Sprintf("https://thxcode.github.io/k8s-csi-s3/charts/csi-s3-%[1]s.tgz", version)

	valuesContext := globalValuesContext
	valuesContext["Release"] = release
	valuesContext["Namespace"] = helmCli.DefaultNamespace()
	valuesContext["DriverName"] = CSIProvisionerS3

	values := getCSIDriverS3ChartTemplateValues(name, valuesContext)

	chart := &helm.Chart{
		Name:        name,
		Version:     version,
		Release:     release,
		Path:        path,
		DownloadURL: download,
		Values:      values,
	}
	_, err := helmCli.Install(ctx, chart)
	if err != nil {
		return err
	}

	return nil
}

const csiDriverS3ChartTemplate = `
{{- $registry := default "docker.io" $.ContainerRegistry }}
{{- $namespace := default "gpustack" $.ContainerNamespace }}
{{- $prefix := "mirrored" }}
images:

  # Original: docker.io/thxcode/csi-s3/csi-s3-driver:0.43.7
  #
  s3: {{ $registry }}/{{ $namespace}}/{{ $prefix }}-csi-s3-driver:v0.43.7

  # Original: registry.k8s.io/sig-storage/csi-provisioner:v6.1.0
  #
  csiProvisioner: {{ $registry }}/{{ $namespace}}/{{ $prefix }}-csi-provisioner:v6.1.0

  # Original: registry.k8s.io/sig-storage/livenessprobe:v2.17.0
  #
  livenessProbe: {{ $registry }}/{{ $namespace}}/{{ $prefix }}-csi-livenessprobe:v2.17.0

  # Original: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.15.0
  #
  nodeDriverRegistrar: {{ $registry }}/{{ $namespace}}/{{ $prefix }}-csi-node-driver-registrar:v2.15.0

imagePullPolicy: "{{ default "IfNotPresent" $.ImagePullPolicy }}"

controller:
  name: csi-s3-controller
  priorityClassName: system-cluster-critical
  tolerations:
    - key: "node-role.kubernetes.io/master"
      operator: "Exists"
      effect: "NoSchedule"
    - key: "node-role.kubernetes.io/controlplane"
      operator: "Exists"
      effect: "NoSchedule"
    - key: "node-role.kubernetes.io/control-plane"
      operator: "Exists"
      effect: "NoSchedule"
    - key: "CriticalAddonsOnly"
      operator: "Exists"
      effect: "NoSchedule"

serviceAccount:
  create: true
  controller: csi-s3-controller-sa
  node: csi-s3-node-sa

rbac:
  create: true
  name: csi-s3

driver:
  name: {{ $.DriverName }}

nodeDriverRegistrar:
  livenessProbe:
    enabled: false

node:
  name: csi-s3-node
  priorityClassName: system-cluster-critical
  livenessProbe:
    healthPort: 29663
  tolerations:
    - operator: "Exists"

{{- if $.ImagePullSecrets }}
imagePullSecrets:
{{- range $.ImagePullSecrets }}
  - name: {{ . }}
{{- end }}
{{- end }}

secret:
  create: false

storageClass:
  create: false

`

func getCSIDriverS3ChartTemplateValues(name string, data map[string]any) helm.TemplateValues {
	return helm.TemplateValues{
		Application: name,
		Template:    csiDriverS3ChartTemplate,
		Context:     data,
	}
}
