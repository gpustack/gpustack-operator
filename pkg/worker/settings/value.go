package settings

import (
	"gpustack.ai/gpustack/pkg/setting"
)

var settings = setting.Settings{}

// Indexer returns the index function of the settings,
// which can be used to index the settings in other packages.
func Indexer() setting.IndexFunc {
	return settings.Index
}

// the built-in settings for worker.
var (
	// Global.

	// ContainerRegistry is the registry to pull images, which is used in all built-in applications' deployment.
	ContainerRegistry = settings.NewEditable(
		"container-registry",
		"Indicates the registry to pull images.",
		setting.InitializeFromEnv(),
		setting.AllowBlank(),
		setting.AllowContainerRegistry(),
	)

	// ContainerNamespace is the namespace to pull images, which is used in all built-in applications' deployment.
	ContainerNamespace = settings.NewEditable(
		"container-namespace",
		"Indicates the namespace to pull images.",
		setting.InitializeFromEnv(),
		setting.AllowBlank(),
	)

	// ImagePullSecrets is the image pull secret for pulling images, which is used in all built-in applications' deployment.
	ImagePullSecrets = settings.NewEditable(
		"image-pull-secrets",
		"Indicates the image pull secret for pulling images.",
		setting.InitializeFromEnv(),
		setting.AllowBlank(),
	)

	// ImagePullPolicy is the image pull policy for pulling images, which is used in all built-in applications' deployment.
	ImagePullPolicy = settings.NewEditable(
		"image-pull-policy",
		"Indicates the image pull policy for pulling images.",
		setting.InitializeFromEnv("IfNotPresent"),
		setting.AllowBlank(),
	)

	// Instance.

	// InstanceGeneralResourcesOvercommit indicates to overcommit instance general resources or not,
	// which is used when deploying Instances.
	InstanceGeneralResourcesOvercommit = settings.NewEditable(
		"instance-general-resources-overcommit",
		"Indicates to overcommit instance general resources or not. "+
			"With this enabled, normal instance types will request 800m(CPU)/128Mi(RAM) per unit and one-eight local storage, "+
			"acceleratable instances type will request 100m(CPU)/128Mi(RAM) per unit and one-eight local storage. "+
			"For example, an acceleratable instance type defines 1C/4Gi unit resources and 128Gi local storage,"+
			"when requesting 2 accelerators and 64Gi local storage of this type, "+
			"then resulting resource request will be 200m(CPU)/256Mi(RAM) and 8Gi local storage.",
		setting.InitializeFromEnv("true"),
		setting.AllowBool(),
	)

	// InstanceSSHServerImage is the image of the SSH server,
	// which is used when deploying Instances.
	InstanceSSHServerImage = settings.NewEditable(
		"instance-ssh-server-image",
		"Indicates the image of the SSH server, when deploying Instances.",
		setting.InitializeFromEnv("gpustack/ssh-server:v1.3.0"),
		setting.AllowContainerImageReference(),
	)

	// KVCacheBackendImage is the image every role of a KVCacheBackend runs when the object does
	// not name one itself.
	//
	// The default is this project's own build, pack/mirrored-mooncake. It is the only image that can
	// run leader.highAvailability, because no published upstream image carries a leadership backend
	// at all, and it is the build every cluster case in this repository exercises.
	//
	// WHAT THE DEFAULT DOES NOT FIT, because one value cannot be right for every backend at once:
	// it is a CPU build carrying TCP and EFA over DRAM, so a backend whose members sit on a vendor
	// fabric -- the Ascend transport, or a member group placed on other accelerator hardware -- needs
	// a build assembled against that runtime, and gets a transport it cannot use from this one. The
	// leader and the members do not want the same thing either: the leader needs no accelerator
	// runtime at all, while a member's transports and the runtime it links are compiled into its
	// wheel. Those backends name their image on the object, which always wins over this setting.
	//
	// The failure mode moved with the default, and that is the cost of having one. Blank made a
	// missing image an ADMISSION REFUSAL naming both places to fix; a default makes a WRONG image a
	// loader error at runtime, which is quieter and further from the person who can fix it.
	// Clearing this setting restores the refusal -- it still allows blank for exactly that reason --
	// so a cluster that would rather have every backend name its own image can have that.
	KVCacheBackendImage = settings.NewEditable(
		"kv-cache-backend-image",
		"Indicates the image to run a KV cache backend, when the backend does not name one.",
		setting.InitializeFromEnv("gpustack/mirrored-mooncake:0.3.13.post1-cpu"),
		setting.AllowBlank(),
		setting.AllowContainerImageReference(),
	)

	// Model deployment.

	// The three images below front a ModelDeployment's roles. Unlike KVCacheBackendImage they DO
	// ship defaults, because one value is right for every cluster at once: none of them runs a
	// model, so none of them links an accelerator runtime, and the operator renders configuration
	// they must be able to read. They are settings rather than constants so that an air-gapped
	// cluster can point them at its own registry, and so that a broken upstream release can be
	// pinned back without rebuilding the operator.

	// ModelDeploymentRouterImage is the image a managed router runs when the object does not name
	// one itself.
	//
	// ONE IMAGE CARRIES ALL THREE ROUTERS, and WHICH BINARY RUNS IS DECIDED BY THE RENDERED COMMAND
	// rather than by this value. `spec.router.name` picks the binary; this setting only says where
	// the binaries come from. A reader who expects one image per router would otherwise look for
	// two settings that do not exist, and an air-gapped cluster would mirror three tags where one
	// is enough.
	//
	// The default is pinned to a release of this project's own build. It is not an upstream tag: the
	// three routers are compiled from three separate sources, one of them with a patch this
	// repository carries, so there is no upstream image that holds them together. A moving tag would
	// let two clusters installed months apart run different routers against the one configuration
	// this operator renders.
	ModelDeploymentRouterImage = settings.NewEditable(
		"model-deployment-router-image",
		"Indicates the image a managed router runs, when the ModelDeployment does not name one. "+
			"It carries every router this operator supports; the rendered command chooses which "+
			"binary runs.",
		setting.InitializeFromEnv("gpustack/llm-router:v0.1.0"),
		setting.AllowContainerImageReference(),
	)

	// ModelDeploymentRouterProxyImage is the proxy that fronts a managed router's endpoint picker.
	//
	// It has no field on the API to override it: the proxy's configuration is rendered by this
	// operator against one proxy's configuration schema, so a cluster swapping the binary would
	// have to swap that configuration too. This setting exists for registry redirection and for
	// pinning, not for running a different proxy.
	ModelDeploymentRouterProxyImage = settings.NewEditable(
		"model-deployment-router-proxy-image",
		"Indicates the proxy image fronting a managed router's endpoint picker.",
		setting.InitializeFromEnv("gpustack/mirrored-envoy:distroless-v1.33.2"),
		setting.AllowContainerImageReference(),
	)

	// ModelDeploymentRoutingSidecarImage is the sidecar a decoder runs to accept a remote prefill
	// handoff. It carries the same caveat as the proxy above: this operator renders its arguments,
	// so the setting is for redirection and pinning rather than for a different implementation.
	ModelDeploymentRoutingSidecarImage = settings.NewEditable(
		"model-deployment-routing-sidecar-image",
		"Indicates the routing sidecar image a decoder runs to accept a remote prefill handoff.",
		setting.InitializeFromEnv("gpustack/mirrored-llm-d-router-disagg-sidecar:v0.10.0"),
		setting.AllowContainerImageReference(),
	)

	// ModelDeploymentTCPTWReuse renders net.ipv4.tcp_tw_reuse=1 into the Pod security context of
	// the prefill half of every SGLang prefill/decode pair.
	//
	// That half opens a new TCP connection for every transfer, to its decode half and to each store
	// member, and closes it first, so the TIME-WAIT sockets pile up in its network namespace until
	// they hold every ephemeral port; from then on every transfer fails until the Pod is recreated.
	// The sysctl lets the kernel reuse a TIME-WAIT port for a new outgoing connection. It changes
	// nothing on the accepting side, which is why the decode half and the store members do not get
	// it, and a vLLM pair keeps its transfer connections open across requests, so it does not get it
	// either.
	//
	// IT IS OFF BY DEFAULT BECAUSE THE KUBELET REFUSES IT UNTIL TOLD OTHERWISE. The sysctl is
	// namespaced but not on the kubelet's safe list, so a node whose kubelet does not allow it
	// through --allowed-unsafe-sysctls rejects the Pod with SysctlForbidden, and the replacement is
	// rejected the same way. The kubelet change is the cluster administrator's, not this operator's.
	//
	// Flipping it moves the spec hash of the Pods it targets, so their replicas are recreated at the
	// deployment's next reconcile.
	ModelDeploymentTCPTWReuse = settings.NewEditable(
		"model-deployment-tcp-tw-reuse",
		"Indicates to render net.ipv4.tcp_tw_reuse=1 on the prefill half of every SGLang "+
			"prefill/decode pair, which otherwise runs out of local ports under sustained load. "+
			"Every node that runs such a Pod must allow the sysctl through the kubelet's "+
			"--allowed-unsafe-sysctls first; without it the Pod fails with SysctlForbidden, "+
			"reported as an event in the deployment's namespace, and every replacement fails the same way.",
		setting.InitializeFromEnv("false"),
		setting.AllowBool(),
	)

	// InstanceAccessStaticAddress is the access static address for all Instances,
	// which is used to access Instances.
	InstanceAccessStaticAddress = settings.NewEditable(
		"instance-access-static-address",
		"Indicates the access static address for all Instances. "+
			"If not set, the access address will be generated by host IPs.",
		setting.InitializeFromEnv(),
		setting.AllowBlank(),
	)

	// InstanceAccessWildcardDNS is the wildcard DNS for all Instances,
	// which is used to generate the domain name for each Instance, like <instance-host-ip>.<wildcard-dns>.
	InstanceAccessWildcardDNS = settings.NewEditable(
		"instance-access-wildcard-dns",
		"Indicates the wildcard DNS for all Instances, like traefik.me. "+
			"Only effective if `instance-access-static-address` is not set.",
		setting.InitializeFromEnv(),
		setting.AllowBlank(),
	)

	// InstancePrivilegedAllowed indicates to allow Instances to request privileged mode,
	// which escapes the container boundary and exposes the node's devices and kernel surface.
	// Enforced when an Instance takes privileged mode, whether at creation or by a later
	// change: turning it off never blocks an Instance that already runs privileged from
	// being updated, edited while stopped, or restarted.
	InstancePrivilegedAllowed = settings.NewEditable(
		"instance-privileged-allowed",
		"Indicates to allow Instances to request privileged mode. "+
			"Enforced when an Instance takes privileged mode, at creation or later, "+
			"so disabling it never blocks an already-privileged Instance from being updated or restarted.",
		setting.InitializeFromEnv("false"),
		setting.AllowBool(),
	)

	// InstanceHostPathVolumeAllowed indicates to allow Instances to mount hostPath volumes,
	// which reaches the node's filesystem. It is separate from InstancePrivilegedAllowed
	// because it grants strictly less: the filesystem, but not the node's devices or kernel.
	// Enforced when an Instance takes a hostPath mount, like InstancePrivilegedAllowed.
	InstanceHostPathVolumeAllowed = settings.NewEditable(
		"instance-host-path-volume-allowed",
		"Indicates to allow Instances to mount hostPath volumes. "+
			"Enforced when an Instance takes a hostPath mount, at creation or later, "+
			"so disabling it never blocks an Instance that already has one from being updated or restarted.",
		setting.InitializeFromEnv("false"),
		setting.AllowBool(),
	)

	// InstanceType.

	// NodeManagementManual indicates to skip auto-managing nodes.
	// When false (default) the operator auto-injects `gpustack.ai/managed=true` to
	// onboard discovered nodes; when true an administrator must label nodes manually.
	NodeManagementManual = settings.NewEditable(
		"node-management-manual",
		"Indicates to skip auto-managing nodes. "+
			"When false (default), the operator auto-onboards discovered nodes by injecting the managed label; "+
			"when true, an administrator must opt nodes in manually.",
		setting.InitializeFromEnv("false"),
		setting.AllowBool(),
	)

	// InstanceTypeMixedOnNode indicates whether one node may surface both an
	// accelerator (GPU) and a general (CPU-only) InstanceType.
	// When true (default, current behavior) a node is summarized into every type it
	// can serve; when false a node with accelerators is summarized only as a GPU
	// InstanceType and a CPU-only node only as a general one.
	InstanceTypeMixedOnNode = settings.NewEditable(
		"instance-type-mixed-on-node",
		"Indicates whether one node may surface both a GPU and a CPU-only InstanceType. "+
			"When true (default), a node is summarized into every type it can serve; "+
			"when false, a node with accelerators yields only a GPU InstanceType and a CPU-only node only a general one.",
		setting.InitializeFromEnv("true"),
		setting.AllowBool(),
	)

	// InstanceTypeDerivedFromNode indicates whether the operator auto-derives the
	// InstanceType (and its backing ClusterQueue) from node hardware.
	// When true (default, current behavior) the operator derives both; when false it
	// only aligns the ResourceFlavor and the administrator defines the ClusterQueue
	// via the InstanceType API.
	InstanceTypeDerivedFromNode = settings.NewEditable(
		"instance-type-derived-from-node",
		"Indicates whether the operator auto-derives the InstanceType and its backing ClusterQueue from node hardware. "+
			"When true (default), the operator derives both; "+
			"when false, it only aligns the ResourceFlavor and the administrator defines the ClusterQueue via the InstanceType API.",
		setting.InitializeFromEnv("true"),
		setting.AllowBool(),
	)

	// InstanceTypeDrainWhenNoFlavors indicates whether a ClusterQueue whose pool has lost
	// all its ResourceFlavors is drained (HoldAndDrain) before its resource groups are
	// emptied. When true (default) the queue is drained first so admitted workloads are
	// evicted; when false the operator waits for the reservations to clear on their own,
	// then empties without draining. Either way the resource groups are emptied only once
	// all reservations are zero, so Kueue's counters never go negative.
	InstanceTypeDrainWhenNoFlavors = settings.NewEditable(
		"instance-type-drain-when-no-flavors",
		"Indicates whether a ClusterQueue whose pool lost all ResourceFlavors is drained "+
			"(HoldAndDrain) before its resource groups are emptied. When true (default), it is "+
			"drained first; when false, the operator waits for reservations to clear, then empties.",
		setting.InitializeFromEnv("true"),
		setting.AllowBool(),
	)

	// InstanceTypeAwareCPUManufacturer governs whether the derived ClusterQueue/InstanceType/
	// InstanceTypeFlavor aggregation layer treats the CPU manufacturer as a discriminator. When
	// false (default) non-accelerated flavors collapse into one generic pool per os/arch and
	// accelerated flavors pool per accelerator (CPU ignored); when true every pool splits by the
	// CPU key. It never changes a ResourceFlavor's name or labels — flavors are always the finest
	// grain — and additionally gates the cpuDetail note on an accelerated flavor.
	InstanceTypeAwareCPUManufacturer = settings.NewEditable(
		"instance-type-aware-cpu-manufacturer",
		"Indicates whether the derived ClusterQueue/InstanceType/InstanceTypeFlavor split by CPU "+
			"manufacturer. When false (default), non-accelerated flavors collapse into one generic pool "+
			"per os/arch and accelerated flavors pool per accelerator; when true, every pool splits by "+
			"the CPU manufacturer.",
		setting.InitializeFromEnv("false"),
		setting.AllowBool(),
	)
)
