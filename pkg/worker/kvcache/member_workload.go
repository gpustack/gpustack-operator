package kvcache

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
)

const (
	// memberResourceNoteRole distinguishes a member group's objects from the leader's.
	memberResourceNoteRole = "member"

	// memberEntrypoint is the image's own console script, measured rather than assumed. It builds
	// the store, mounts a segment and then serves an HTTP API; its main() installs shutdown signal
	// handlers and its stop() closes the store, so it unmounts on SIGTERM by itself.
	memberEntrypoint = "mc_store_rest_server"

	// memberRESTPort is where the entrypoint serves its HTTP API. It is the entrypoint's own
	// default and nothing here can move it: the renderer passes no --port, and an ExtraArgs entry
	// renders as a -D config key, which is a different setting.
	memberRESTPort int32 = 8080

	// rdmaDevicePath is the device tree an RDMA member needs from its host.
	rdmaDevicePath = "/dev/infiniband"

	// MemberPodSpecHashAnnotation carries the fingerprint of a member's pod template, minus its node
	// selector. It rides on the template, so every Pod the DaemonSet creates inherits it and the
	// reconciler can tell a Pod built from the current spec from one built before a change.
	MemberPodSpecHashAnnotation = "kvcache." + systemname.LabelPrefix + "pod-spec-hash"

	// memberTerminationGracePeriodSeconds is how long the kubelet waits after SIGTERM before it
	// kills the member.
	//
	// It is NOT a drain window — nothing reachable from a Pod's shutdown can drain a segment
	// (F10). It is the time the entrypoint needs to close its store and let the leader see the
	// client go, rather than being cut off mid-shutdown and leaving the leader to time the client
	// out after its client_ttl instead.
	memberTerminationGracePeriodSeconds int64 = 60
	// rdmaDeviceVolumeName names that mount.
	rdmaDeviceVolumeName = "rdma-devices"
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
					TerminationGracePeriodSeconds: ptr.To(memberTerminationGracePeriodSeconds),
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
						memberContainerSpec(kvcb, member, image),
					},
				},
			},
		},
	}

	applyMemberFabric(ds, MemberProtocol(kvcb))

	// Stamped last, over a template that is otherwise complete. The fingerprint therefore covers
	// the fabric fields applied just above, and — because it is computed from a copy with the node
	// selector and this very annotation removed — it does not cover itself.
	ds.Spec.Template.Annotations = map[string]string{
		MemberPodSpecHashAnnotation: MemberPodSpecHash(ds.Spec.Template),
	}

	systemmeta.NoteResource(ds, ResourceType, map[string]string{
		ResourceNoteBackend: kvcb.Name,
		"role":              memberResourceNoteRole,
		"group":             strconv.Itoa(group),
	})
	kubemeta.ControlOnWithoutBlock(ds, kvcb,
		workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))

	return ds
}

// memberContainerSpec is the container one member runs.
func memberContainerSpec(
	kvcb *workercore.KVCacheBackend,
	member workercore.KVCacheBackendMember,
	image string,
) core.Container {
	return core.Container{
		Name:            "member",
		Image:           image,
		ImagePullPolicy: EffectivePullPolicy(kvcb, image),
		// An image lacking the transport's vendor runtime — ascend needs CANN — dies in the dynamic
		// linker on stderr, which the default policy does not read.
		TerminationMessagePolicy: core.TerminationMessageFallbackToLogsOnError,
		Command:                  []string{memberEntrypoint},
		Args:                     renderMemberOverrides(member),
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
				TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(memberRESTPort)},
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
			// It costs no stability to use the pod IP. The leader appends a port of its own choosing
			// to build the segment name, and that port is fresh on every start — one restart moved a
			// segment from <host>:13720 to <host>:14071 — so the name was never durable across one.
			// On the RDMA path the pod holds the host's network namespace and this resolves to the
			// node's own address anyway.
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

	return env
}

// renderMemberOverrides renders extraArgs as the entrypoint's own per-key override.
//
// It is "-D key=value" here and "-key=value" on the leader, because the two binaries accept
// different things. The entrypoint applies these AFTER the environment and they win over it, which
// is what makes the hatch useful for a key the renderer derives from a node-specific fact.
//
// ⚠️ A key the client's config object does not carry is IGNORED SILENTLY — the entrypoint guards the
// assignment with hasattr and warns only when the "=" is missing. So a typo here costs a
// configuration that never applied and never said so.
func renderMemberOverrides(member workercore.KVCacheBackendMember) []string {
	if len(member.ExtraArgs) == 0 {
		return nil
	}

	// Sorted, so two renders of one spec are byte-identical and the DaemonSet does not churn.
	args := make([]string, 0, len(member.ExtraArgs)*2)
	for _, key := range slices.Sorted(maps.Keys(member.ExtraArgs)) {
		args = append(args, "-D", fmt.Sprintf("%s=%s", key, member.ExtraArgs[key]))
	}
	return args
}

// memberRequests declares the member's claim so capacity planning can see it. A member that does not
// fit stays Pending and the backend reads Degraded, which is the honest outcome — the alternative is
// a member that overcommits the node it landed on.
func memberRequests(member workercore.KVCacheBackendMember) core.ResourceList {
	claim := member.CapacityPerMember.DeepCopy()
	claim.Add(member.LocalBufferSize)

	name := core.ResourceMemory
	if member.Medium == "LocalDisk" {
		name = core.ResourceEphemeralStorage
	}
	return core.ResourceList{name: claim}
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
			HostPath: &core.HostPathVolumeSource{Path: rdmaDevicePath},
		},
	})

	container := &podSpec.Containers[0]
	container.VolumeMounts = append(container.VolumeMounts, core.VolumeMount{
		Name:      rdmaDeviceVolumeName,
		MountPath: rdmaDevicePath,
	})
	container.SecurityContext = &core.SecurityContext{
		Capabilities: &core.Capabilities{
			// IPC_LOCK to pin the memory the fabric registers, SYS_RESOURCE to raise the locked
			// memory limit that pinning runs into. Two operations, two capabilities.
			Add: []core.Capability{"IPC_LOCK", "SYS_RESOURCE"},
		},
	}
}

// memberFileBackedMedia are the media whose capacity the leader reports through its FILE families
// rather than its memory ones.
//
// The split is the leader's own, not a taxonomy invented here: it keeps two independent pairs of
// gauges — master_total_capacity_bytes / master_allocated_bytes for segments it holds in memory, and
// master_total_file_capacity_bytes / master_allocated_file_size_bytes for segments backed by a file
// — and a group read from the wrong pair reports another group's figures or a zero.
//
// ⚠️ Only DRAM is exercised end to end. The rest are classified by the flags the leader documents
// for them: LocalDisk and DFS are two of its three file backends, NoF is block storage reached the
// same way, and CXL is a DAX device the leader treats as memory. A run on any of the four is what
// would turn these from classified into measured.
var memberFileBackedMedia = map[string]bool{
	"LocalDisk": true,
	"NoF":       true,
	"DFS":       true,
}

// MemberMediumIsFileBacked reports which pair of the leader's capacity gauges a group is read from.
func MemberMediumIsFileBacked(medium string) bool {
	return memberFileBackedMedia[medium]
}

// MemberMediumDRAM is the one medium this scope renders end to end.
const MemberMediumDRAM = "DRAM"

// MemberMediumIsReconciled reports whether anything here actually CONFIGURES a medium, which is a
// narrower question than whether the schema accepts it.
//
// The two are deliberately different. The enum carries all five media the store supports so that
// a tiered backend does not have to change the shape later, but a member group only becomes a
// running segment of the medium it names if something renders that medium's configuration — the
// leader's file or DAX flags, and a mount on the member. Nothing renders them yet, so every medium
// but this one is refused at admission. Were it accepted instead, the member would come up holding
// its segment in DRAM while status read the file gauges, and the two would disagree in silence.
func MemberMediumIsReconciled(medium string) bool {
	return medium == MemberMediumDRAM
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
