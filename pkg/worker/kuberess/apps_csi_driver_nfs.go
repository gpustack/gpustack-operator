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
	CSIProvisionerNFS = "nfs.csi.gpustack.ai"
)

func installCSIDriverNFS(ctx context.Context, helmCli *helm.Client, globalValuesContext map[string]any, disable sets.Set[string]) error {
	// NB: please update the following files if changed.
	// - pack/gpustack/image/Dockerfile.
	// - If update csi-node-driver-registrar and csi-provisioner,
	//   please also update the versions in pkg/worker/kuberess/apps_csi_driver_s3.go.

	name := "csi-driver-nfs"
	version := "4.13.2"
	if disable.Has(name) {
		return nil
	}

	release := "gpustack-csi-driver-nfs"
	path := filepath.Join(system.SubConfDir("charts"), fmt.Sprintf("%s-%s.tgz", name, version))
	download := fmt.Sprintf("https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/refs/heads/master/charts/v%[1]s/csi-driver-nfs-%[1]s.tgz", version) // nolint: lll

	valuesContext := globalValuesContext
	valuesContext["Release"] = release
	valuesContext["Namespace"] = helmCli.DefaultNamespace()
	valuesContext["DriverName"] = CSIProvisionerNFS

	values := getCSIDriverNFSChartTemplateValues(name, valuesContext)

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

const csiDriverNFSChartTemplate = `
{{- $registry := default "docker.io" $.ContainerRegistry }}
{{- $namespace := default "gpustack" $.ContainerNamespace }}
{{- $prefix := "mirrored" }}
image:
  baseRepo: {{ $registry }}

  # Original: registry.k8s.io/sig-storage/nfsplugin:v4.13.0
  #
  nfs:
    repository: /{{ $namespace }}/{{ $prefix }}-csi-nfs-driver
    tag: v4.13.0
    pullPolicy: "{{ default "IfNotPresent" $.ImagePullPolicy }}"
  
  # Original: registry.k8s.io/sig-storage/csi-provisioner:v6.1.0
  #
  csiProvisioner:
    repository: /{{ $namespace }}/{{ $prefix }}-csi-provisioner
    tag: v6.1.0
    pullPolicy: "{{ default "IfNotPresent" $.ImagePullPolicy }}"

  # Original: registry.k8s.io/sig-storage/csi-resizer:v2.0.0
  #
  csiResizer:
    repository: /{{ $namespace }}/{{ $prefix }}-csi-resizer
    tag: v2.0.0
    pullPolicy: "{{ default "IfNotPresent" $.ImagePullPolicy }}"

  # Original: registry.k8s.io/sig-storage/csi-snapshotter:v8.4.0
  #
  csiSnapshotter:
    repository: /{{ $namespace }}/{{ $prefix }}-csi-snapshotter
    tag: v8.4.0
    pullPolicy: "{{ default "IfNotPresent" $.ImagePullPolicy }}"

  # Original: registry.k8s.io/sig-storage/livenessprobe:v2.17.0
  #
  livenessProbe:
    repository: /{{ $namespace }}/{{ $prefix }}-csi-livenessprobe
    tag: v2.17.0
    pullPolicy: "{{ default "IfNotPresent" $.ImagePullPolicy }}"

  # Original: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.15.0
  #
  nodeDriverRegistrar:
    repository: /{{ $namespace }}/{{ $prefix }}-csi-node-driver-registrar
    tag: v2.15.0
    pullPolicy: "{{ default "IfNotPresent" $.ImagePullPolicy }}"

controller:
  name: csi-nfs-controller
  priorityClassName: system-cluster-critical
  livenessProbe:
    healthPort: 29652
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
  controller: csi-nfs-controller-sa
  node: csi-nfs-node-sa

rbac:
  create: true
  name: csi-nfs

driver:
  name: {{ $.DriverName }}
  mountPermissions: 0

feature:
  enableFSGroupPolicy: true
  enableInlineVolume: false
  propagateHostMountOptions: false

nodeDriverRegistrar:
  livenessProbe:
    enabled: false

node:
  name: csi-nfs-node
  priorityClassName: system-cluster-critical
  livenessProbe:
    healthPort: 29653
  tolerations:
    - operator: "Exists"

externalSnapshotter:
  enabled: false

volumeSnapshotClass:
  create: false

{{- if $.ImagePullSecrets }}
imagePullSecrets:
{{- range $.ImagePullSecrets }}
  - name: {{ . }}
{{- end }}
{{- end }}

storageClass:
  create: false

`

func getCSIDriverNFSChartTemplateValues(name string, data map[string]any) helm.TemplateValues {
	return helm.TemplateValues{
		Application: name,
		Template:    csiDriverNFSChartTemplate,
		Context:     data,
	}
}
