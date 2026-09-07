package mooncake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache"
)

const (
	// memberResourceNoteRole distinguishes a member group's objects from the leader's.
	memberResourceNoteRole = "member"

	// memberEntrypoint is the image's own console script, measured rather than assumed. It builds
	// the store, mounts a segment and then serves an HTTP API; its main() installs shutdown signal
	// handlers and its stop() closes the store, so it unmounts on SIGTERM by itself.
	memberEntrypoint = "mc_store_rest_server"

	// memberRESTPortBase is where the FIRST group's entrypoint serves its HTTP API, and it is the
	// entrypoint's own default. Later groups offset from it; see memberRESTPort for why.
	memberRESTPortBase int32 = 8080

	// RDMADevicePath is the device tree an RDMA member needs from its host.
	//
	// Exported because admission has to refuse a disk tier that would land on top of it: the two
	// mounts are rendered into one container, and a collision there is resolved by the kubelet
	// rather than reported by this operator.
	RDMADevicePath = "/dev/infiniband"

	// MemberPodSpecHashAnnotation carries the fingerprint of a member's pod template, minus its node
	// selector. It rides on the template, so every Pod the DaemonSet creates inherits it and the
	// reconciler can tell a Pod built from the current spec from one built before a change.
	MemberPodSpecHashAnnotation = "kvcache." + systemname.LabelPrefix + "pod-spec-hash"

	// memberShutdownSeconds is the time the entrypoint needs for its own shutdown: closing its
	// store and letting the leader see the client go, rather than being cut off mid-shutdown and
	// leaving the leader to time the client out after its client_ttl instead.
	//
	// It is NOT a drain window for the memory segment. Nothing reachable from a Pod's shutdown can
	// drain one: the member's own API takes a graceful unmount with a grace period, but it wants
	// the segment ids and no route returns a client its own.
	//
	// A group with a local disk tier adds its scale-in grace ON TOP of this, rather than sharing
	// it, which is what keeps the kubelet from killing the container in the middle of a wait the
	// spec asked for. See memberTerminationGracePeriodSeconds.
	memberShutdownSeconds int64 = 60

	// rdmaDeviceVolumeName names that mount.
	rdmaDeviceVolumeName = "rdma-devices"

	// memberLocalDiskVolumeName names the host directory holding a group's disk tier.
	memberLocalDiskVolumeName = "local-disk"

	// memberUnmountLocalDiskPath is the entrypoint's own route for deregistering this store's SSD
	// tier before the process goes away. The master stops naming this store as the owner of its
	// offloaded keys, so a reader gets a clean miss instead of a peer that is about to disappear,
	// and the call then holds for the grace so offload reads already in flight finish here.
	//
	// It is the one thing a shutdown CAN drain, and it needs no segment id, which is exactly why
	// the memory segment's counterpart is unreachable.
	memberUnmountLocalDiskPath = "/api/unmount_local_disk"

	// MemberMaxGracePeriodSeconds is the entrypoint's own ceiling on that call. Above it the
	// handler answers 400, so a larger value would render a hook that fails every time it runs.
	//
	// It is exported because admission is what refuses it, and the rule belongs beside the route
	// it protects rather than restated as a number in another package.
	MemberMaxGracePeriodSeconds = 3600

	// memberHookTimeoutMarginSeconds is how much longer than the grace the shutdown hook waits
	// before giving up on its own request.
	//
	// It is deliberately much smaller than memberShutdownSeconds. The Pod's termination budget
	// covers the hook AND the entrypoint's shutdown, in that order, so every second the hook spends
	// past the grace is a second taken from the close that follows it.
	memberHookTimeoutMarginSeconds int64 = 10
)

// The member's environment. Every key the client reads has a real named variable, so the whole
// configuration renders as environment and there is no ConfigMap, no volume and no init container.
const (
	// memberEnvMetadataServer carries an underscore inside META_DATA. It is NOT
	// MOONCAKE_TE_METADATA_SERVER, and normalising it to the spelling that reads correctly does not
	// error — it silently degrades the metadata plane. A unit test asserts this byte for byte.
	memberEnvMetadataServer = "MOONCAKE_TE_META_DATA_SERVER"
	// memberMetadataServerValue is the literal the metadata plane takes, unconditionally: this
	// scope ships a peer-to-peer plane, so there is no store to point at.
	memberMetadataServerValue = "P2PHANDSHAKE"

	memberEnvMaster            = "MOONCAKE_MASTER"
	memberEnvProtocol          = "MOONCAKE_PROTOCOL"
	memberEnvLocalHostname     = "MOONCAKE_LOCAL_HOSTNAME"
	memberEnvGlobalSegmentSize = "MOONCAKE_GLOBAL_SEGMENT_SIZE"
	memberEnvLocalBufferSize   = "MOONCAKE_LOCAL_BUFFER_SIZE"

	// The local disk tier's three keys. The first two are the client's own enable_ssd_offload and
	// ssd_offload_path; without both, the client registers no local disk segment and the leader's
	// offload queue has nowhere to send it.
	memberEnvOffloadEnabled = "MOONCAKE_OFFLOAD_ENABLED"
	memberEnvOffloadPath    = "MOONCAKE_OFFLOAD_FILE_STORAGE_PATH"
	// memberEnvOffloadSizeLimit caps what the tier stores. Left unrendered, the client's own
	// ceiling applies, so a ceiling that moves upstream shows up as a change to investigate rather
	// than one this renderer silently restated.
	memberEnvOffloadSizeLimit = "MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES"
)

// memberProtocols maps this API's spelling of a transport onto the artifact's.
//
// Auto maps to tcp rather than resolving upward against the node, and the two reasons are in the
// API type's own comment: one DaemonSet covers every node a group selects and so cannot carry a
// per-node transport, and promoting to RDMA would grant hostNetwork and two capabilities nobody
// asked for. The artifact has no "auto" either — it looks its protocol string up in a transport map,
// so rendering the literal would reach a lookup that finds nothing.
// MemberProtocolAuto is the transport the schema defaults to, named because the renderer falls back
// to it for an object that never reached an API server.
const MemberProtocolAuto = "Auto"

var memberProtocols = map[string]string{
	MemberProtocolAuto: "tcp",
	"TCP":              "tcp",
	"RDMA":             "rdma",
	"HIP":              "hip",
	"Ascend":           "ascend",
}

// MemberObjectName is the name of the objects rendered for one member group.
//
// The group's INDEX is in the name because a group has no name of its own, and because the position
// is what the spec's list already identifies it by. A second group is therefore a second DaemonSet
// rather than a rename of this one.
func MemberObjectName(kvcb *workercore.KVCacheBackend, group int) string {
	return fmt.Sprintf("%s-member-%d", kvcb.Name, group)
}

// MemberSelectorLabels is what the DaemonSet selects its Pods by, and what the reconciler lists
// this backend's member Pods with.
//
// A DaemonSet's spec.selector is IMMUTABLE, exactly as a Deployment's is. It carries identity
// only — which backend, which role, which group — and never the group's nodeSelector, which widening
// a group is expected to change and which would otherwise strand the object.
func MemberSelectorLabels(kvcb *workercore.KVCacheBackend, group int) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "kv-cache-backend",
		"app.kubernetes.io/instance":  kvcb.Name,
		"app.kubernetes.io/component": fmt.Sprintf("member-%d", group),
	}
}

// BackendLabels selects everything rendered for one backend, whatever its role or group.
//
// It is MemberSelectorLabels without the component, which is the part that names a group. Teardown
// needs that: a group removed from the spec before the object was deleted has a DaemonSet no
// per-group name derives any more, and walking the spec would leave it running.
func BackendLabels(kvcb *workercore.KVCacheBackend) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "kv-cache-backend",
		"app.kubernetes.io/instance": kvcb.Name,
	}
}

func memberPodLabels(kvcb *workercore.KVCacheBackend, group int) map[string]string {
	labels := MemberSelectorLabels(kvcb, group)
	labels["app.kubernetes.io/part-of"] = "gpustack-operator-worker"
	return labels
}

// MemberProtocol is the transport a group resolves to, in the artifact's own spelling. An
// unrecognized value renders nothing rather than a guess: the schema enumerates this field, so an
// empty result means the object never went through admission.
func MemberProtocol(kvcb *workercore.KVCacheBackend) string {
	protocol := kvcb.Spec.Transport.Protocol
	if protocol == "" {
		// The schema defaults this, so an object that came through an API server always names one.
		// An object that did not — one a test builds, or one written before the containing object
		// carried its own default — would otherwise render an EMPTY protocol, which is not a value
		// the artifact knows: it looks its protocol up in a transport map, so an empty string
		// reaches a lookup that finds nothing. Falling back to the same value the schema would have
		// chosen keeps the two answers identical instead of nearly identical.
		protocol = MemberProtocolAuto
	}
	return memberProtocols[protocol]
}

// RenderMemberDaemonSet renders one member group into a DaemonSet.
//
// A DaemonSet rather than a Deployment because a member contributes A NODE's medium: it claims that
// node's memory or disk, and on the RDMA path that node's devices, so its identity IS the node. A
// Deployment with anti-affinity only approximates that.
//
// The image is a parameter for the same reason the leader's is, and the group's own image wins over
// it — a group selects its own nodes, so two groups can sit on different accelerator hardware and
// need the client wheel built for it. The transport is backend-wide and not part of that split.
func RenderMemberDaemonSet(
	kvcb *workercore.KVCacheBackend, group int, image string,
) *apps.DaemonSet {
	member := kvcb.Spec.Connection.Managed.Members[group]
	if member.Image != "" {
		image = member.Image
	}

	ds := &apps.DaemonSet{
		ObjectMeta: meta.ObjectMeta{
			Name:      MemberObjectName(kvcb, group),
			Namespace: kuberess.SystemNamespaceName,
			Labels:    memberPodLabels(kvcb, group),
		},
		Spec: apps.DaemonSetSpec{
			Selector: &meta.LabelSelector{MatchLabels: MemberSelectorLabels(kvcb, group)},
			// OnDelete, and this operator decides when to restart. A DaemonSet's node selector
			// lives in the pod template, so under RollingUpdate widening a group would invalidate
			// every running Pod's revision hash and roll the whole group — unmounting and
			// re-mounting every segment to add one node. Deployment and StatefulSet have the same
			// property for the same reason; none of the three can move where a workload runs
			// without restarting what already runs.
			//
			// OnDelete alone would go too far the other way: a changed image would be written and
			// never applied. So the reconciler deletes exactly the Pods whose fingerprint moved,
			// which restarts on WHAT changed rather than on THAT the template changed.
			UpdateStrategy: apps.DaemonSetUpdateStrategy{
				Type: apps.OnDeleteDaemonSetStrategyType,
			},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{Labels: memberPodLabels(kvcb, group)},
				Spec: core.PodSpec{
					// The group's selector is what places the members; the DaemonSet runs one on
					// each node it matches, and widening it adds members without touching the rest.
					NodeSelector:                  member.NodeSelector,
					TerminationGracePeriodSeconds: ptr.To(memberTerminationGracePeriodSeconds(kvcb, member)),
					// Rendered explicitly even though it is the server's own default, because the
					// RDMA path changes it. A field this renderer sets on one path and leaves to
					// the server on the other cannot be converged in both directions: switching a
					// backend from RDMA back to TCP would leave ClusterFirstWithHostNet behind.
					DNSPolicy: core.DNSClusterFirst,
					// The member talks to its leader and to nothing else; the API server's default
					// would mount a service-account token into a third-party image that has no use
					// for one, and on the RDMA path that image also holds two capabilities.
					AutomountServiceAccountToken: ptr.To(false),
					ImagePullSecrets:             kvcb.Spec.ImagePullSecrets,
					Containers: []core.Container{
						memberContainerSpec(kvcb, member, group, image),
					},
				},
			},
		},
	}

	applyMemberFabric(ds, MemberProtocol(kvcb))
	applyMemberLocalDisk(ds, kvcb, member, group)

	// Stamped last, over a template that is otherwise complete. The fingerprint therefore covers
	// the fabric fields applied just above, and — because it is computed from a copy with the node
	// selector and this very annotation removed — it does not cover itself.
	ds.Spec.Template.Annotations = map[string]string{
		MemberPodSpecHashAnnotation: MemberPodSpecHash(ds.Spec.Template),
	}

	systemmeta.NoteResource(ds, kvcache.ResourceType, map[string]string{
		kvcache.ResourceNoteBackend: kvcb.Name,
		"role":                      memberResourceNoteRole,
		"group":                     strconv.Itoa(group),
	})
	kubemeta.ControlOnWithoutBlock(ds, kvcb,
		workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))

	return ds
}

// memberContainerSpec is the container one member runs.
func memberContainerSpec(
	kvcb *workercore.KVCacheBackend,
	member workercore.KVCacheBackendMember,
	group int,
	image string,
) core.Container {
	return core.Container{
		Name:            "member",
		Image:           image,
		ImagePullPolicy: kvcache.EffectivePullPolicy(kvcb, image),
		// An image lacking the transport's vendor runtime — ascend needs CANN — dies in the dynamic
		// linker on stderr, which the default policy does not read.
		TerminationMessagePolicy: core.TerminationMessageFallbackToLogsOnError,
		Command:                  []string{memberEntrypoint},
		Args:                     renderMemberArgs(member, group),
		Env:                      renderMemberEnv(kvcb, member),
		// NO data-plane containerPort, on any path. The transfer engine binds its ports at
		// random — one observed run took 15002 and 15995, a second client 16566 and 16655, none of
		// them configured — so a fixed list here would be a false statement about the process. The
		// reachability requirement is a port RANGE, and the documentation states it as one.
		//
		// The entrypoint mounts its segment BEFORE it serves this port, so a connection proves the
		// mount and not merely the process. Without this the kubelet reports a member Ready as soon
		// as its container runs, and the leader has not listed it yet — which the reconciler reads
		// as a shortfall. TCP because the API serves only /api/* data verbs: there is no route to
		// GET without a key or a side effect.
		ReadinessProbe: &core.Probe{
			ProbeHandler: core.ProbeHandler{
				TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(memberRESTPort(group))},
			},
			PeriodSeconds:    5,
			FailureThreshold: 3,
		},
		Resources: core.ResourceRequirements{
			Requests: memberRequests(member),
		},
	}
}

// renderMemberEnv builds the member's whole configuration.
//
// What is NOT set is deliberate. MOONCAKE_DEVICE is left unset so the client's device filter comes
// out empty, which is what it reads as "every device" — see keys.go for why the documented
// "auto-discovery" string is a trap. MOONCAKE_TENANT_ID is left at its own default, because the
// quota spec owns tenancy and a value here would pre-empt it.
func renderMemberEnv(
	kvcb *workercore.KVCacheBackend, member workercore.KVCacheBackendMember,
) []core.EnvVar {
	env := []core.EnvVar{
		{Name: memberEnvMetadataServer, Value: memberMetadataServerValue},
		{
			Name:  memberEnvMaster,
			Value: fmt.Sprintf("%s:%d", LeaderServiceHost(kvcb), LeaderRPCPort),
		},
		{Name: memberEnvProtocol, Value: MemberProtocol(kvcb)},
		{
			// This is the ADDRESS the leader hands to clients, not just a label: it becomes the host
			// half of the segment's te_endpoint. The transfer engine binds its data port inside the
			// pod's network namespace, so a node name here advertises a port nothing listens on
			// there — measured on a two-node cluster, a client pod got ECONNREFUSED against both
			// the node name and the node IP, and connected on the pod IP.
			//
			// It also becomes the segment's NAME, verbatim: the client keeps this string and neither
			// it nor the leader ever rewrites it. The port that is fresh on every start — one
			// restart moved a segment endpoint from <host>:13720 to <host>:14071 — belongs to
			// te_endpoint, which the client derives on its own under the peer-to-peer metadata
			// plane this scope ships. The name outlives a restart; the endpoint does not.
			//
			// LIMITED: the name is only as unique as this value is. On the RDMA path the pod holds
			// the host's network namespace, so every member group on one node reports the same
			// name — the collision the members-mounted condition reports rather than guesses at.
			Name: memberEnvLocalHostname,
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{FieldPath: "status.podIP"},
			},
		},
	}

	if size := member.CapacityPerMember.Value(); size > 0 {
		env = append(env, core.EnvVar{
			Name:  memberEnvGlobalSegmentSize,
			Value: strconv.FormatInt(size, 10),
		})
	}
	if size := member.LocalBufferSize.Value(); size > 0 {
		env = append(env, core.EnvVar{
			Name:  memberEnvLocalBufferSize,
			Value: strconv.FormatInt(size, 10),
		})
	}

	if disk := member.LocalDisk; disk != nil {
		// Both keys or neither. The client registers a local disk segment only when offloading is
		// enabled AND it has somewhere to put the bytes, so a path without the switch, or a switch
		// without a path, is a member that comes up holding no tier while the object says it has
		// one.
		env = append(env,
			core.EnvVar{Name: memberEnvOffloadEnabled, Value: "true"},
			core.EnvVar{Name: memberEnvOffloadPath, Value: disk.Path})

		if size := disk.Capacity.Value(); size > 0 {
			env = append(env, core.EnvVar{
				Name:  memberEnvOffloadSizeLimit,
				Value: strconv.FormatInt(size, 10),
			})
		}
	}

	return env
}

// renderMemberArgs builds the member's argv: the REST port when it has to move, then extraArgs as
// the entrypoint's own per-key override.
//
// The override is "-D key=value" here and "-key=value" on the leader, because the two binaries
// accept different things. The entrypoint applies these AFTER the environment and they win over it,
// which is what makes the hatch useful for a key the renderer derives from a node-specific fact.
//
// A key the client's config object does not carry is IGNORED SILENTLY — the entrypoint guards the
// assignment with hasattr and warns only when the "=" is missing. So a typo here costs a
// configuration that never applied and never said so.
//
// --port is a real flag on this entrypoint's parser rather than a config key, so it cannot be
// reached through extraArgs — a "-D port=..." would set a config key of that name and the server
// would keep listening where it was.
func renderMemberArgs(member workercore.KVCacheBackendMember, group int) []string {
	var args []string

	// Rendered only where it has to MOVE the port, and the first group is not that place: the base
	// is the entrypoint's own default, so passing it there would add an argument to every member
	// running today, move the fingerprint, and delete and recreate all of them to tell each one to
	// keep doing what it was already doing.
	if port := memberRESTPort(group); port != memberRESTPortBase {
		args = append(args, "--port", strconv.Itoa(int(port)))
	}

	// Sorted, so two renders of one spec are byte-identical and the DaemonSet does not churn.
	for _, key := range slices.Sorted(maps.Keys(member.ExtraArgs)) {
		args = append(args, "-D", fmt.Sprintf("%s=%s", key, member.ExtraArgs[key]))
	}
	return args
}

// memberRESTPort is the port a group's entrypoint serves its HTTP API on, and it is the ONE place
// that decides it: the argv, the readiness probe and the preStop hook all call this, so they cannot
// drift into naming different ports for the same container.
//
// The port is derived from the group's position because on the RDMA path the member holds the host's
// network namespace, and the API binds 0.0.0.0. Two groups whose node selectors overlap therefore
// place two host-network Pods on one node, and a single fixed port would let only the first bind:
// the second would run, never pass its readiness probe, and report nothing about why. Admission
// cannot refuse that pair instead, because whether two selectors ever meet depends on node labels it
// does not have — the collision is real only at placement time, so the fix has to be that there is
// no collision to have.
//
// Deriving from the position rather than allocating is what makes it stable: the position already
// identifies a group everywhere else (its DaemonSet name and its selector labels), so a group's port
// is as durable as its identity, and no state has to be kept to remember which port went where.
//
// Why the collision was SILENT is worth keeping, because it is what makes this worth deriving rather
// than documenting: nothing here declares a containerPort or a hostPort. A hostPort would have been
// caught by the scheduler, which treats it as a node-level resource and leaves the second Pod
// Pending with the conflict named. hostNetwork plus a port the manifest never mentions is invisible
// to that check — the Pod is placed, the process fails to bind, and the only evidence is a container
// log. So the port cannot be allowed to collide in the first place.
func memberRESTPort(group int) int32 {
	return memberRESTPortBase + int32(group)
}

// memberRequests declares the member's claim so capacity planning can see it. A member that does not
// fit stays Pending and the backend reads Degraded, which is the honest outcome — the alternative is
// a member that overcommits the node it landed on.
//
// The claim is MEMORY and only memory, whatever else the group carries. A disk tier adds a host
// directory, which is outside the kubelet's ephemeral-storage accounting entirely — that covers the
// container filesystem, emptyDir volumes and logs, never a hostPath. Requesting against it would
// reserve a figure nothing polices and would then keep the member off the very node that has the
// disk. Watching that filesystem is the operator's, and the documentation says so.
func memberRequests(member workercore.KVCacheBackendMember) core.ResourceList {
	claim := member.CapacityPerMember.DeepCopy()
	claim.Add(member.LocalBufferSize)

	return core.ResourceList{core.ResourceMemory: claim}
}

// applyMemberFabric grants what a fabric needs, and only to the path that needs it.
//
// The RDMA path takes hostNetwork, the device tree and two capabilities — and NEVER privileged,
// which would hand the member its whole node for the sake of two operations. Every other path,
// including the Auto that resolved to TCP, is left exactly as rendered: no security context at all
// rather than an empty one, since an empty struct is an invitation to add a capability to it.
func applyMemberFabric(ds *apps.DaemonSet, protocol string) {
	if protocol != "rdma" {
		return
	}

	podSpec := &ds.Spec.Template.Spec

	podSpec.HostNetwork = true
	// A hostNetwork Pod that keeps ClusterFirst resolves against the host's resolver and cannot
	// find the leader's Service name.
	podSpec.DNSPolicy = core.DNSClusterFirstWithHostNet

	podSpec.Volumes = append(podSpec.Volumes, core.Volume{
		Name: rdmaDeviceVolumeName,
		VolumeSource: core.VolumeSource{
			HostPath: &core.HostPathVolumeSource{Path: RDMADevicePath},
		},
	})

	container := &podSpec.Containers[0]
	container.VolumeMounts = append(container.VolumeMounts, core.VolumeMount{
		Name:      rdmaDeviceVolumeName,
		MountPath: RDMADevicePath,
	})
	container.SecurityContext = &core.SecurityContext{
		Capabilities: &core.Capabilities{
			// IPC_LOCK to pin the memory the fabric registers, SYS_RESOURCE to raise the locked
			// memory limit that pinning runs into. Two operations, two capabilities.
			Add: []core.Capability{"IPC_LOCK", "SYS_RESOURCE"},
		},
	}
}

// memberTerminationGracePeriodSeconds is how long the kubelet waits after SIGTERM before it kills
// the member.
//
// It is DERIVED from the scale-in grace rather than set beside it, and that is the whole mechanism
// behind "the window always holds the grace": two independent fields could be set so that the
// kubelet kills the container in the middle of a wait the spec asked for, and no amount of
// validation makes that relationship true — it only makes it checkable. Derived, it cannot be
// violated.
//
// A group with no disk tier has nothing to wait for, so it keeps the entrypoint's own shutdown
// budget and its Pod template does not move.
func memberTerminationGracePeriodSeconds(
	kvcb *workercore.KVCacheBackend, member workercore.KVCacheBackendMember,
) int64 {
	if member.LocalDisk == nil {
		return memberShutdownSeconds
	}
	return int64(memberScaleInGraceSeconds(kvcb)) + memberShutdownSeconds
}

// memberScaleInGraceSeconds is the grace a departing member holds its disk tier open for.
func memberScaleInGraceSeconds(kvcb *workercore.KVCacheBackend) int32 {
	managed := kvcb.Spec.Connection.Managed
	if managed == nil || managed.ScaleIn == nil {
		return 0
	}
	return managed.ScaleIn.GracePeriodSeconds
}

// applyMemberLocalDisk grants the group's disk tier: the host directory it lives on, and the hook
// that deregisters it before the member goes away.
//
// A group without one is left exactly as rendered, which is what keeps an existing backend's Pod
// template — and therefore its fingerprint, and therefore its running members — untouched by this
// feature.
func applyMemberLocalDisk(
	ds *apps.DaemonSet,
	kvcb *workercore.KVCacheBackend,
	member workercore.KVCacheBackendMember,
	group int,
) {
	if member.LocalDisk == nil {
		return
	}

	podSpec := &ds.Spec.Template.Spec

	// Directory and NOT DirectoryOrCreate, which is the more convenient one and does not work.
	// Measured: a directory the kubelet creates is owned by root with mode 0755, while the store
	// image runs as uid 65532 — so the member comes up, finds it cannot write, and retries its
	// store setup until it gives up. Nothing this renderer can add fixes that from inside the Pod:
	// fsGroup does not apply to hostPath (measured too, the directory stays root-owned), and the
	// alternatives all mean granting a container root on the node to chown a host directory.
	//
	// So the directory is the node's to provide, which is also where it belongs — who owns a path
	// on a host is not something an operator should rewrite on an administrator's behalf. Directory
	// makes a missing one a FailedMount the Pod stops at, rather than a mount that silently cannot
	// be written to. The documentation carries how to create it, for the uid the chosen image runs
	// as.
	//
	// A hostPath and NOT an emptyDir, on two counts. An emptyDir dies with the Pod, so every
	// restart would discard the tier whose entire value is surviving one; and an emptyDir the
	// kubelet accounts for makes it evict the member when the tier fills, which is the opposite of
	// what a cache tier should do when it reaches its own ceiling.
	podSpec.Volumes = append(podSpec.Volumes, core.Volume{
		Name: memberLocalDiskVolumeName,
		VolumeSource: core.VolumeSource{
			HostPath: &core.HostPathVolumeSource{
				Path: member.LocalDisk.Path,
				Type: ptr.To(core.HostPathDirectory),
			},
		},
	})

	container := &podSpec.Containers[0]
	// Mounted at the path the client was told to write to, so one field says both where on the
	// host the bytes live and where in the container they are addressed.
	container.VolumeMounts = append(container.VolumeMounts, core.VolumeMount{
		Name:      memberLocalDiskVolumeName,
		MountPath: member.LocalDisk.Path,
	})
	container.Lifecycle = &core.Lifecycle{
		PreStop: memberUnmountLocalDiskHook(memberScaleInGraceSeconds(kvcb), memberRESTPort(group)),
	}
}

// memberUnmountLocalDiskHook builds the shutdown hook that deregisters this member's disk tier.
//
// It is an EXEC and not an httpGet, and that is a constraint rather than a preference: a
// lifecycle httpGet sends no request body and cannot choose a method, while this route is a POST
// whose handler decodes the body FIRST and answers 400 when there is none. An httpGet hook would
// therefore fail on every single shutdown — and a failing preStop is recorded as an event and
// otherwise ignored, so the hook would look configured while draining nothing.
//
// The interpreter is the image's own: this container's command is a Python console script, so an
// image that cannot run python3 cannot run the member either. Only the standard library is used,
// for the same reason — a curl the image may not carry would fail the same silent way.
func memberUnmountLocalDiskHook(graceSeconds, restPort int32) *core.LifecycleHandler {
	// The timeout is the grace plus a SMALL margin, and the smallness is the point.
	//
	// It has to exceed the grace, because the handler holds for exactly that long before answering
	// and a shorter timeout would abandon the wait it asked for. But it must stay well under the
	// Pod's whole termination budget, because that budget is NOT preStop's alone: the kubelet
	// starts the countdown before running the hook and sends SIGTERM only once the hook returns, so
	// a hook that ran to grace + memberShutdownSeconds would finish at the instant the container is
	// force-killed, and the entrypoint would be SIGKILLed mid-close — the exact outcome
	// memberShutdownSeconds exists to prevent.
	script := fmt.Sprintf(
		`import json,urllib.request;`+
			`urllib.request.urlopen(urllib.request.Request(`+
			`"http://127.0.0.1:%d%s",`+
			`data=json.dumps({"grace_period_seconds":%d}).encode(),`+
			`headers={"Content-Type":"application/json"},method="POST"),timeout=%d)`,
		restPort, memberUnmountLocalDiskPath, graceSeconds,
		int64(graceSeconds)+memberHookTimeoutMarginSeconds)

	return &core.LifecycleHandler{
		Exec: &core.ExecAction{Command: []string{"python3", "-c", script}},
	}
}

// MemberPodSpecHash fingerprints everything in a member's pod template EXCEPT its node selector.
//
// The exclusion is the entire point. A widening changes the node selector and nothing else, and a
// widening must not restart the members already running; every other change to the template must.
// So the node selector is stripped before hashing, and the DaemonSet is left on OnDelete so that
// nothing but this fingerprint decides a restart.
//
// The annotation the hash is stored in is stripped too, so the fingerprint never covers itself — a
// value that did would change on every render and roll the group forever.
func MemberPodSpecHash(template core.PodTemplateSpec) string {
	subject := *template.DeepCopy()
	subject.Spec.NodeSelector = nil
	delete(subject.Annotations, MemberPodSpecHashAnnotation)
	if len(subject.Annotations) == 0 {
		subject.Annotations = nil
	}

	// JSON rather than fmt: it orders map keys, so two renders of one spec hash identically. A
	// marshal failure is not reachable for a PodTemplateSpec, and treating it as an empty digest
	// would silently disable every restart, so it is surfaced as a value nothing can match.
	encoded, err := json.Marshal(subject)
	if err != nil {
		return "unhashable-" + err.Error()
	}

	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
