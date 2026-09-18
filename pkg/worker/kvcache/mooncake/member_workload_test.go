package mooncake

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// testMemberBackend is the canonical one-group backend. Each case mutates the one thing it is about.
func testMemberBackend(mutate ...func(*workercore.KVCacheBackend)) *workercore.KVCacheBackend {
	kvcb := testBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Transport.Protocol = "Auto"
		k.Spec.Connection.Managed.Members = []workercore.KVCacheBackendMember{{
			NodeSelector:      map[string]string{"kvcache-dram": "true"},
			Medium:            "DRAM",
			CapacityPerMember: resource.MustParse("500Gi"),
			LocalBufferSize:   resource.MustParse("4Gi"),
		}}
	})
	for _, m := range mutate {
		m(kvcb)
	}
	return kvcb
}

func memberGroup(kvcb *workercore.KVCacheBackend) workercore.KVCacheBackendMember {
	return kvcb.Spec.Connection.Managed.Members[0]
}

// withMemberDiskTier declares a local disk tier on the canonical group.
//
// The member side is the whole declaration: the group's localDisks is what turns the tier on, and
// the leader's flags are derived from it, so there is no second half for a fixture to set.
func withMemberDiskTier(kvcb *workercore.KVCacheBackend) {
	kvcb.Spec.Connection.Managed.Members[0].LocalDisks = []workercore.KVCacheBackendMemberLocalDisk{{
		Path:     "/var/lib/kvcache",
		Capacity: resource.MustParse("4Ti"),
	}}
}

// withMemberScaleInGrace sets the wait a departing member's process holds for after deregistering
// its tier.
func withMemberScaleInGrace(seconds int32) func(*workercore.KVCacheBackend) {
	return func(kvcb *workercore.KVCacheBackend) {
		kvcb.Spec.Connection.Managed.ScaleIn = &workercore.KVCacheBackendScaleIn{GracePeriodSeconds: seconds}
	}
}

// withSecondMemberGroup adds a group whose selector deliberately does NOT say it is disjoint from
// the first: whether two selectors ever meet on a node is a runtime fact, which is the reason the
// port has to be unique rather than the pair refused.
func withSecondMemberGroup(kvcb *workercore.KVCacheBackend) {
	kvcb.Spec.Connection.Managed.Members = append(kvcb.Spec.Connection.Managed.Members,
		workercore.KVCacheBackendMember{
			NodeSelector:      map[string]string{"kvcache-dram-cold": "true"},
			Medium:            "DRAM",
			CapacityPerMember: resource.MustParse("500Gi"),
			LocalBufferSize:   resource.MustParse("4Gi"),
		})
}

// withSecondMemberGroupVRAM adds a VRAM group on the SAME selector as the first group: one node
// contributing both media is the shape the medium choice exists for. The mutators carry what a
// case adds — a device resource, a transport — so the fixture itself stays the plain form.
func withSecondMemberGroupVRAM(mutate ...func(*workercore.KVCacheBackendMember)) func(*workercore.KVCacheBackend) {
	return func(kvcb *workercore.KVCacheBackend) {
		group := workercore.KVCacheBackendMember{
			NodeSelector:      map[string]string{"kvcache-dram": "true"},
			Medium:            "VRAM",
			CapacityPerMember: resource.MustParse("80Gi"),
			LocalBufferSize:   resource.MustParse("4Gi"),
		}
		for _, m := range mutate {
			m(&group)
		}
		kvcb.Spec.Connection.Managed.Members = append(kvcb.Spec.Connection.Managed.Members, group)
	}
}

// withSecondGroupDiskTier puts the tier on the SECOND group, so the preStop hook is rendered for a
// group whose port moved. Only one group may carry a tier, so this is the tier rather than a second.
func withSecondGroupDiskTier(kvcb *workercore.KVCacheBackend) {
	kvcb.Spec.Connection.Managed.Members[1].LocalDisks = []workercore.KVCacheBackendMemberLocalDisk{{
		Path:     "/var/lib/kvcache",
		Capacity: resource.MustParse("4Ti"),
	}}
}

func memberContainer(t *testing.T, kvcb *workercore.KVCacheBackend, image string) core.Container {
	t.Helper()
	ds := RenderMemberDaemonSet(kvcb, 0, image)
	require.Len(t, ds.Spec.Template.Spec.Containers, 1, "a member runs exactly one container")
	return ds.Spec.Template.Spec.Containers[0]
}

func memberEnv(t *testing.T, kvcb *workercore.KVCacheBackend, image string) map[string]string {
	t.Helper()
	env := make(map[string]string)
	for _, e := range memberContainer(t, kvcb, image).Env {
		env[e.Name] = e.Value
	}
	return env
}

// envWithoutDownwardAPIForGroup is envWithoutDownwardAPI for a backend carrying more than one member
// group, where which group is being read is the point of the assertion.
func envWithoutDownwardAPIForGroup(
	t *testing.T, kvcb *workercore.KVCacheBackend, group int, image string,
) map[string]string {
	t.Helper()
	ds := RenderMemberDaemonSet(kvcb, group, image)
	require.Len(t, ds.Spec.Template.Spec.Containers, 1, "a member runs exactly one container")
	env := make(map[string]string)
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		if e.ValueFrom == nil {
			env[e.Name] = e.Value
		}
	}
	return env
}

// TestMemberWorkload_Shape pins where the workload lands and what selects its nodes.
func TestMemberWorkload_Shape(t *testing.T) {
	kvcb := testMemberBackend()
	ds := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")

	assert.Equal(t, "mooncake-dram-member-0", ds.Name,
		"the group's index is in the name: a second group is a second DaemonSet, not a rename of this one")
	assert.Equal(t, kuberess.SystemNamespaceName, ds.Namespace)
	assert.Equal(t, memberGroup(kvcb).NodeSelector, ds.Spec.Template.Spec.NodeSelector,
		"the group's selector is what places the members; the DaemonSet does the rest")

	container := memberContainer(t, kvcb, "mooncake:v0.3.13")
	assert.Equal(t, []string{"mc_store_rest_server"}, container.Command,
		"the entrypoint is the image's own console script, measured rather than assumed")
}

// TestWorkload_ADeadContainerCanSayWhy covers both renderers, because it is one contract rather than
// two: whatever dies, its status has to carry the process's own words.
//
// The default policy reads only /dev/termination-log, which neither of these artifacts writes — so
// without this the status of a member whose image lacks CANN reports the reason "Error" and an empty
// message, and the documented loader failure reaches nobody. The status reader that surfaces it is
// only as good as this field.
func TestWorkload_ADeadContainerCanSayWhy(t *testing.T) {
	cases := []struct {
		name      string
		container core.Container
	}{
		{"leader", leaderContainer(t, testBackend(), "mooncake:v0.3.13")},
		{"member", memberContainer(t, testMemberBackend(), "mooncake:v0.3.13")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, core.TerminationMessageFallbackToLogsOnError, c.container.TerminationMessagePolicy,
				"the fallback is what puts the tail of stderr into the termination message")
		})
	}
}

// TestWorkload_MountsNoServiceAccountToken covers both renderers, because it is one contract: these
// are third-party store binaries that never call the API server, and the default is to mount a
// service-account bearer token into them anyway. On the RDMA path that same container also holds two
// capabilities and the host's network namespace.
//
// Asserted as an explicitly rendered false rather than as "not true": left unset, the API server
// fills it in, and a field the renderer omits is one the aligner has nothing to converge toward.
func TestWorkload_MountsNoServiceAccountToken(t *testing.T) {
	cases := []struct {
		name string
		spec core.PodSpec
	}{
		{"leader", RenderLeaderDeployment(testBackend(), "mooncake:v0.3.13").Spec.Template.Spec},
		{"member", RenderMemberDaemonSet(testMemberBackend(), 0, "mooncake:v0.3.13").Spec.Template.Spec},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.NotNil(t, c.spec.AutomountServiceAccountToken,
				"rendered, not left to the server: an omitted field cannot be converged")
			assert.False(t, *c.spec.AutomountServiceAccountToken)
		})
	}
}

// TestMemberWorkload_Environment asserts the whole environment element by element.
//
// MOONCAKE_TE_META_DATA_SERVER carries an underscore inside META_DATA. Normalising it to the
// spelling that reads correctly does not error — it silently degrades the metadata plane — so the
// name is asserted byte for byte rather than probed for a substring.
func TestMemberWorkload_Environment(t *testing.T) {
	kvcb := testMemberBackend()
	env := memberEnv(t, kvcb, "mooncake:v0.3.13")

	assert.Equal(t, map[string]string{
		"MOONCAKE_TE_META_DATA_SERVER": "P2PHANDSHAKE",
		"MOONCAKE_MASTER":              "mooncake-dram-leader.gpustack-system.svc:50051",
		"MOONCAKE_PROTOCOL":            "tcp",
		"MOONCAKE_GLOBAL_SEGMENT_SIZE": fmt.Sprintf("%d", 500*1024*1024*1024),
		"MOONCAKE_LOCAL_BUFFER_SIZE":   fmt.Sprintf("%d", 4*1024*1024*1024),
		"MC_TE_METRIC":                 "1",
		"AMD_VISIBLE_DEVICES":          "void",
		"CAMBRICON_VISIBLE_DEVICES":    "void",
		"IX_VISIBLE_DEVICES":           "void",
		"MTHREADS_VISIBLE_DEVICES":     "void",
		"NVIDIA_VISIBLE_DEVICES":       "void",
	}, envWithoutDownwardAPI(t, kvcb, "mooncake:v0.3.13"),
		"the whole environment, so a key added later has to be added here too")

	_, hasMetadataNormalised := env["MOONCAKE_TE_METADATA_SERVER"]
	assert.False(t, hasMetadataNormalised,
		"MOONCAKE_TE_METADATA_SERVER is the wrong spelling and fails silently; it must never appear")
}

// TestMemberWorkload_VisibleDevicesFollowTheMedium is the other half of the assertion above: the
// host-memory group hides the accelerators, and the device-memory group must not, or it would hide
// the devices its own segment is made of.
//
// Asserted as a pair in one test because neither half means anything alone. "DRAM hides them" is
// satisfied by a renderer that hides them from everybody, which would leave no VRAM group able to
// start at all.
func TestMemberWorkload_VisibleDevicesFollowTheMedium(t *testing.T) {
	kvcb := testMemberBackend(withSecondMemberGroupVRAM())

	dram := envWithoutDownwardAPIForGroup(t, kvcb, 0, "mooncake:v0.3.13")
	vram := envWithoutDownwardAPIForGroup(t, kvcb, 1, "mooncake:v0.3.13")

	for _, name := range []string{
		"AMD_VISIBLE_DEVICES",
		"CAMBRICON_VISIBLE_DEVICES",
		"IX_VISIBLE_DEVICES",
		"MTHREADS_VISIBLE_DEVICES",
		"NVIDIA_VISIBLE_DEVICES",
	} {
		assert.Equal(t, "void", dram[name],
			"a host-memory group must be denied every vendor's devices, or its image decides the medium")

		_, present := vram[name]
		assert.False(t, present,
			"a device-memory group must keep its devices: %s", name)
	}
}

// TestMemberWorkload_TransferMetricsAreOnForEveryMedium pins the switch that does NOT follow the
// medium, asserted against the same two groups as the test above so the contrast is in one place.
//
// The medium decides which devices a group may see; it says nothing about whether the group's data
// plane is measured. Rendering this one conditionally would leave whichever medium lost the
// condition reporting no throughput and no task latency at all, which reads exactly like a member
// that is transferring nothing.
//
// That every group renders it is also what makes this switch cost a roll of every member rather
// than of one medium's members, which the recording guard states as the wider of its two blast
// radii.
func TestMemberWorkload_TransferMetricsAreOnForEveryMedium(t *testing.T) {
	kvcb := testMemberBackend(withSecondMemberGroupVRAM())

	dram := envWithoutDownwardAPIForGroup(t, kvcb, 0, "mooncake:v0.3.13")
	vram := envWithoutDownwardAPIForGroup(t, kvcb, 1, "mooncake:v0.3.13")

	assert.Equal(t, "1", dram["MC_TE_METRIC"],
		"the artifact defaults this OFF, so an absent key is an unmeasured host-memory data plane")
	assert.Equal(t, "1", vram["MC_TE_METRIC"],
		"the artifact defaults this OFF, so an absent key is an unmeasured device-memory data plane")

	assert.Contains(t, MemberDerivedEnvs, "MC_TE_METRIC",
		"rendered unconditionally and therefore reserved: a group could otherwise define it a "+
			"second time through extraEnv, and a container carrying one name twice leaves the "+
			"winner to the runtime")
}

// envWithoutDownwardAPI returns the literal-valued environment, leaving out the entries sourced from
// the downward API — those are asserted separately, by their field path rather than by a value.
func envWithoutDownwardAPI(t *testing.T, kvcb *workercore.KVCacheBackend, image string) map[string]string {
	t.Helper()
	env := make(map[string]string)
	for _, e := range memberContainer(t, kvcb, image).Env {
		if e.ValueFrom == nil {
			env[e.Name] = e.Value
		}
	}
	return env
}

// TestMemberWorkload_LocalHostnameIsTheReachableAddress pins the field this resolves from, because
// the value is an ADDRESS the leader hands to clients — it becomes the host half of the segment's
// te_endpoint — and not merely a label the member is known by.
//
// The node name was measured to be unreachable: the transfer engine binds its data port inside the
// pod's network namespace, so a client pod got ECONNREFUSED against both the node name and the node
// IP, and connected only on the pod IP.
func TestMemberWorkload_LocalHostnameIsTheReachableAddress(t *testing.T) {
	container := memberContainer(t, testMemberBackend(), "mooncake:v0.3.13")

	var found bool
	for _, e := range container.Env {
		if e.Name != "MOONCAKE_LOCAL_HOSTNAME" {
			continue
		}
		found = true
		require.NotNil(t, e.ValueFrom, "the address comes from the downward API, not a literal")
		require.NotNil(t, e.ValueFrom.FieldRef)
		assert.Equal(t, "status.podIP", e.ValueFrom.FieldRef.FieldPath,
			"the pod IP is where the engine's data port can actually be reached; on the RDMA path "+
				"the pod holds the host's network namespace and this is the node's address anyway")
	}
	assert.True(t, found, "MOONCAKE_LOCAL_HOSTNAME must be set")
}

// TestMemberWorkload_TheMemorySegmentIsDeclaredAtStartupAndNotMountedAfterIt pins the fact on THIS
// side that the refusal to render a memory-unmount hook rests on.
//
// The member's unmount routes reach only segments the client filed in its allocated or mounted
// records, and a segment asked for through MOONCAKE_GLOBAL_SEGMENT_SIZE is in neither: setup() mounts
// it at startup. Measured against mooncake 0.3.13, both routes answer 500 for such a segment at every
// grace period, for the id the leader itself published — see memberShutdownSeconds.
//
// THE ONE CHANGE THAT WOULD MAKE IT UNMOUNTABLE IS THE ONE THIS ASSERTS AGAINST: drop the key and
// mount through POST /api/mount from a postStart hook, which files the segment in the records those
// routes read. It is not taken here, because it leaves a member Ready while holding no segment, and a
// postStart that fails leaves one running with none while nothing reports it. Pinned so that taking
// the trade later is a decision somebody makes rather than a side effect of tidying the environment.
func TestMemberWorkload_TheMemorySegmentIsDeclaredAtStartupAndNotMountedAfterIt(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*workercore.KVCacheBackend)
	}{
		{name: "a group with no disk tier"},
		// The group that already has a preStop, so "there is a hook here" never stands in for
		// "there is a postStart here".
		{name: "a group WITH a disk tier", mutate: withMemberDiskTier},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutations := []func(*workercore.KVCacheBackend){}
			if c.mutate != nil {
				mutations = append(mutations, c.mutate)
			}
			kvcb := testMemberBackend(mutations...)

			size, declared := envWithoutDownwardAPI(t, kvcb, "mooncake:v0.3.13")[memberEnvGlobalSegmentSize]
			require.True(t, declared,
				"the segment is asked for at startup; without this key the member mounts none at all")
			assert.NotEmpty(t, size, "a size is what makes setup() mount a segment")

			// Read out unconditionally rather than under a nil check on Lifecycle, so the assertion
			// fires on the group that has no Lifecycle at all instead of being skipped there.
			var postStart *core.LifecycleHandler
			if container := memberContainer(t, kvcb, "mooncake:v0.3.13"); container.Lifecycle != nil {
				postStart = container.Lifecycle.PostStart
			}
			assert.Nil(t, postStart,
				"a postStart mount is what would move this segment into the records the unmount "+
					"routes read, and it is the trade the memory-unmount decision declines")
		})
	}
}

// TestMemberWorkload_Requests pins that a member claims its memory segment and nothing else. The
// request is what makes that claim visible to capacity planning, and a member that cannot fit stays
// Pending rather than overcommitting the node it landed on.
func TestMemberWorkload_Requests(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*workercore.KVCacheBackend)
		resource core.ResourceName
		absent   core.ResourceName
	}{
		{
			name:     "a group with no disk tier",
			resource: core.ResourceMemory,
			absent:   core.ResourceEphemeralStorage,
		},
		{
			// The tier is a hostPath, which is outside the kubelet's ephemeral-storage accounting
			// entirely — that covers the container filesystem, emptyDir volumes and logs, never a
			// hostPath. A request against it would reserve a figure nothing polices and would then
			// keep the member off the very node that has the disk.
			name:     "a group WITH a 4Ti disk tier still claims only its memory",
			mutate:   withMemberDiskTier,
			resource: core.ResourceMemory,
			absent:   core.ResourceEphemeralStorage,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutations := []func(*workercore.KVCacheBackend){}
			if c.mutate != nil {
				mutations = append(mutations, c.mutate)
			}
			kvcb := testMemberBackend(mutations...)

			requests := memberContainer(t, kvcb, "mooncake:v0.3.13").Resources.Requests

			want := resource.MustParse("504Gi")
			got := requests[c.resource]
			assert.True(t, want.Equal(got),
				"want %s of %s (capacityPerMember + localBufferSize), got %s", &want, c.resource, &got)

			_, present := requests[c.absent]
			assert.False(t, present, "%s must not be requested", c.absent)
			assert.Len(t, requests, 1, "a member claims exactly one resource: its memory segment")
		})
	}
}

// TestMemberWorkload_TwoMediaOnTheSameNodes renders the shape the medium choice exists for: one
// backend whose DRAM group and VRAM group select the SAME nodes. Each group is its own DaemonSet,
// each accounts its segment against the memory its medium is made of, and the fabric privileges
// follow each group's OWN protocol rather than the backend's.
func TestMemberWorkload_TwoMediaOnTheSameNodes(t *testing.T) {
	kvcb := testMemberBackend(withSecondMemberGroupVRAM(func(group *workercore.KVCacheBackendMember) {
		group.Transport = &workercore.KVCacheBackendMemberTransport{Protocol: "RDMA"}
	}))

	dram := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
	vram := RenderMemberDaemonSet(kvcb, 1, "mooncake:v0.3.13")

	assert.NotEqual(t, dram.Name, vram.Name, "two groups are two DaemonSets, never one")
	assert.Equal(t, dram.Spec.Template.Spec.NodeSelector, vram.Spec.Template.Spec.NodeSelector,
		"both select the same nodes: one node contributes both media")

	dramContainer := dram.Spec.Template.Spec.Containers[0]
	dramMemory := dramContainer.Resources.Requests[core.ResourceMemory]
	assert.True(t, resource.MustParse("504Gi").Equal(dramMemory),
		"a DRAM segment is host memory: capacityPerMember + localBufferSize, got %s", &dramMemory)
	assert.Empty(t, dramContainer.Resources.Limits, "a DRAM group charges no device")
	assert.False(t, dram.Spec.Template.Spec.HostNetwork)
	assert.Nil(t, dramContainer.SecurityContext,
		"the DRAM group declares no transport, so it inherits the backend's TCP and claims no fabric")

	vramPodSpec := vram.Spec.Template.Spec
	vramContainer := vramPodSpec.Containers[0]
	vramMemory := vramContainer.Resources.Requests[core.ResourceMemory]
	assert.True(t, resource.MustParse("4Gi").Equal(vramMemory),
		"a VRAM segment is device memory: host memory carries localBufferSize only, got %s", &vramMemory)
	assert.Empty(t, vramContainer.Resources.Limits,
		"device memory is claimed by allocating it: charging an accelerator here would take a whole "+
			"one from inference to account for a fraction of one device's memory")
	assert.True(t, vramPodSpec.HostNetwork)
	require.NotNil(t, vramContainer.SecurityContext)
	require.NotNil(t, vramContainer.SecurityContext.Capabilities)
	assert.ElementsMatch(t, []core.Capability{"IPC_LOCK", "SYS_RESOURCE"},
		vramContainer.SecurityContext.Capabilities.Add,
		"the group's own RDMA gets the fabric's two capabilities while the backend stays on TCP")
	assert.Nil(t, vramContainer.SecurityContext.Privileged,
		"a named device resource is what keeps the member off the privileged fallback")

	env := map[string]string{}
	for _, e := range vramContainer.Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "rdma", env["MOONCAKE_PROTOCOL"],
		"the member is told its own group's protocol, not the backend's")
}

// TestMemberWorkload_GroupTransportOverridesTheBackend pins the inheritance in both directions:
// a group that declares a transport renders its own, and a group that declares none renders the
// backend's — the second being what keeps a one-group backend byte-identical to what it rendered
// before the field existed.
func TestMemberWorkload_GroupTransportOverridesTheBackend(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Transport.Protocol = "RDMA"
	}, withSecondMemberGroupVRAM(func(group *workercore.KVCacheBackendMember) {
		group.Transport = &workercore.KVCacheBackendMemberTransport{Protocol: "TCP"}
	}))

	inherited := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13").Spec.Template.Spec
	assert.True(t, inherited.HostNetwork,
		"the group that declares nothing renders the backend's fabric")

	overridden := RenderMemberDaemonSet(kvcb, 1, "mooncake:v0.3.13").Spec.Template.Spec
	assert.False(t, overridden.HostNetwork, "the group's own protocol replaces the backend's")
	assert.Equal(t, core.DNSClusterFirst, overridden.DNSPolicy)
	assert.Empty(t, overridden.Volumes, "TCP mounts no device tree")
	assert.Nil(t, overridden.Containers[0].SecurityContext,
		"no security context at all on the path that needs none")
	assert.Empty(t, overridden.Containers[0].Resources.Limits,
		"a VRAM group charges no extended resource whatever its transport is")

	env := map[string]string{}
	for _, e := range overridden.Containers[0].Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "tcp", env["MOONCAKE_PROTOCOL"], "the member is told its own group's protocol")
}

// TestMemberWorkload_VRAMGrantsNothingItWasNotDeclared pins that the medium on its own grants no
// privilege, mounts nothing, and charges nothing.
//
// Two earlier designs are kept out by this test. One read an absent device-resource name as a
// request for a privileged host-network Pod: the privilege it granted reached the node's device
// nodes and not the vendor's user-space driver, which is not under /dev, so it produced a member
// that started, looked healthy, and could not allocate a segment on two of the three vendors. The
// other charged one extended resource per VRAM member, which takes a whole accelerator away from
// inference to account for a fraction of one device's memory — a member's segment is one cudaMalloc
// on one device, so it cannot use the rest of what it took.
//
// The fabric rendering still applies, because it is keyed on the protocol and never on the medium:
// on an RDMA backend this group gets the device tree and the two capabilities like any other.
func TestMemberWorkload_VRAMGrantsNothingItWasNotDeclared(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Transport.Protocol = "RDMA"
	}, withSecondMemberGroupVRAM())

	podSpec := RenderMemberDaemonSet(kvcb, 1, "mooncake:v0.3.13").Spec.Template.Spec
	container := podSpec.Containers[0]

	require.NotNil(t, container.SecurityContext)
	assert.Nil(t, container.SecurityContext.Privileged,
		"the medium is not a request for privilege")
	require.NotNil(t, container.SecurityContext.Capabilities)
	assert.Equal(t, []core.Capability{"IPC_LOCK", "SYS_RESOURCE"}, container.SecurityContext.Capabilities.Add,
		"the fabric path is keyed on the protocol, so the medium does not exempt this group from it")

	assert.True(t, podSpec.HostNetwork, "rdma takes the host network whatever the medium is")
	require.Len(t, podSpec.Volumes, 1, "the device tree, from the fabric path")
	assert.Equal(t, RDMADevicePath, podSpec.Volumes[0].HostPath.Path)

	memory := container.Resources.Requests[core.ResourceMemory]
	assert.True(t, resource.MustParse("4Gi").Equal(memory),
		"host memory still carries localBufferSize only, got %s", &memory)
	assert.Empty(t, container.Resources.Limits,
		"nothing is charged: device memory is claimed by allocating it")
}

// TestMemberWorkload_DeclaredSecurityContextMergesOntoTheFabricOne is the test for the one merge
// rule that is not obvious, and the one whose failure is silent.
//
// A group declaring a security context on a host fabric keeps IPC_LOCK and SYS_RESOURCE, because
// without them the transfer engine cannot pin the memory it registers — and it fails at
// registration, long after the container started and looked healthy. A whole-struct substitution
// would have dropped both while turning this test's own privileged assertion green, which is why
// the capability assertion is here rather than in a test of its own.
func TestMemberWorkload_DeclaredSecurityContextMergesOntoTheFabricOne(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Transport.Protocol = "RDMA"
		members := k.Spec.Connection.Managed.Members
		members[0].SecurityContext = &core.SecurityContext{
			Privileged:   ptr.To(true),
			RunAsUser:    ptr.To(int64(0)),
			Capabilities: &core.Capabilities{Add: []core.Capability{"SYS_ADMIN"}},
		}
	})

	container := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13").Spec.Template.Spec.Containers[0]

	require.NotNil(t, container.SecurityContext)
	require.NotNil(t, container.SecurityContext.Privileged)
	assert.True(t, *container.SecurityContext.Privileged, "a field set here wins")
	require.NotNil(t, container.SecurityContext.RunAsUser)
	assert.Equal(t, int64(0), *container.SecurityContext.RunAsUser)

	require.NotNil(t, container.SecurityContext.Capabilities)
	assert.Equal(t,
		[]core.Capability{"IPC_LOCK", "SYS_ADMIN", "SYS_RESOURCE"},
		container.SecurityContext.Capabilities.Add,
		"the union, sorted: dropping the fabric's two would fail only at memory registration")
}

// TestMemberWorkload_DeclaredSecurityContextOnATCPGroupStandsAlone is the other half: with no
// fabric context to merge onto, what is declared is what is rendered, and nothing is added.
func TestMemberWorkload_DeclaredSecurityContextOnATCPGroupStandsAlone(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Members[0].SecurityContext = &core.SecurityContext{
			Privileged: ptr.To(true),
		}
	})

	container := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13").Spec.Template.Spec.Containers[0]

	require.NotNil(t, container.SecurityContext)
	require.NotNil(t, container.SecurityContext.Privileged)
	assert.True(t, *container.SecurityContext.Privileged)
	assert.Nil(t, container.SecurityContext.Capabilities,
		"tcp renders no capabilities, so there is nothing to union with")
}

// TestMemberWorkload_DeclaredHostPathsMountInOrder pins the vendor-driver path: the mounts appear in
// the order declared, and each volume is named from its POSITION so it can collide with neither
// another entry nor the two volumes this renderer owns.
func TestMemberWorkload_DeclaredHostPathsMountInOrder(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Members[0].HostPaths = []workercore.KVCacheBackendMemberHostPath{
			{
				Path:      "/usr/local/Ascend/driver",
				MountPath: "/usr/local/Ascend/driver",
				Type:      ptr.To(core.HostPathDirectory),
				ReadOnly:  true,
			},
			{Path: "/usr/local/dcmi", MountPath: "/usr/local/dcmi"},
		}
	})

	podSpec := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13").Spec.Template.Spec
	container := podSpec.Containers[0]

	require.Len(t, podSpec.Volumes, 2)
	assert.Equal(t, "host-path-0", podSpec.Volumes[0].Name)
	assert.Equal(t, "/usr/local/Ascend/driver", podSpec.Volumes[0].HostPath.Path)
	require.NotNil(t, podSpec.Volumes[0].HostPath.Type)
	assert.Equal(t, core.HostPathDirectory, *podSpec.Volumes[0].HostPath.Type)
	assert.Equal(t, "host-path-1", podSpec.Volumes[1].Name)
	assert.Nil(t, podSpec.Volumes[1].HostPath.Type,
		"an entry that names no type gets the empty one, which the kubelet does not check")

	require.Len(t, container.VolumeMounts, 2)
	assert.Equal(t, "/usr/local/Ascend/driver", container.VolumeMounts[0].MountPath)
	assert.True(t, container.VolumeMounts[0].ReadOnly)
	assert.Equal(t, "/usr/local/dcmi", container.VolumeMounts[1].MountPath)
	assert.False(t, container.VolumeMounts[1].ReadOnly)
}

// TestMemberWorkload_DeclaredRuntimeClassReachesThePodSpec pins the third of the three declared
// grants. It is on the pod spec rather than the container, unlike the other two.
func TestMemberWorkload_DeclaredRuntimeClassReachesThePodSpec(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Members[0].RuntimeClassName = "ascend"
	})

	podSpec := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13").Spec.Template.Spec

	require.NotNil(t, podSpec.RuntimeClassName)
	assert.Equal(t, "ascend", *podSpec.RuntimeClassName)
}

// TestMemberWorkload_NoDiskTierRendersWhatItAlwaysDid is the guard that this feature does not roll
// every backend already running.
//
// It compares against a RECORDED template rather than a fresh render — a fresh one moves with the
// code and would be green by construction, which is exactly the failure this guard exists to catch.
//
// What is at stake is not cosmetic. The pod-spec hash is in the recording, and the reconciler
// deletes every member Pod whose hash has moved. A byte of drift here means every member of every
// existing backend is deleted and recreated on upgrade, and each one comes back with an empty
// segment: the cache is gone, and nothing about the change said it would be.
//
// THE RECORDING HAS BEEN DELIBERATELY MOVED TWICE, and a reader comparing this against an older
// checkout should know which change did it rather than treating it as drift. It was first recorded
// from the renderer as it stood before the local disk tier existed, at hash e4e1f6a5…. It then held
// 8bae493a…, which is that same template plus the five vendor visibility variables a host-memory
// group renders so that no accelerator is injected into it. It now holds 47b8aec8…, which adds the
// transfer engine's metrics switch.
//
// EACH MOVE COST A ROLL, AND THE TWO DID NOT COST THE SAME ONE. The visibility variables render
// only on a host-memory group, so that move rolled those and left device-memory groups rendering
// exactly as before. The metrics switch renders on EVERY group, so the second move rolls every
// member of every backend regardless of medium — a strictly wider blast radius than the first, and
// the reason this paragraph separates them rather than counting moves.
//
// Both were accepted with that cost understood. The first refused to leave a group asking for host
// memory free to consume device memory instead, unaccounted for by the scheduler and fatal to
// whichever workload had properly requested that card. The second buys the only measurement of the
// member's data plane there is: the leader's Prometheus surface counts keys and bytes and says
// nothing about throughput or task latency, and the engine end of the same transfer already
// reports both.
//
// This guard has been seen to fail three times, which is why it is trusted. Changing
// memberShutdownSeconds from 60 to 61 — one byte, on a field unrelated to any of this — turned it
// red against the first recording. The visibility variables turned it red against that same
// recording, and the metrics switch against the second. Each time that is how the cost became
// visible at all rather than being discovered on somebody's cluster. A guard this load-bearing that
// has never been seen to fail is a guard nobody has checked.
func TestMemberWorkload_NoDiskTierRendersWhatItAlwaysDid(t *testing.T) {
	recorded, err := os.ReadFile("testdata/member_pod_template_no_disk_tier.json")
	require.NoError(t, err, "the recording is the contract; without it this test proves nothing")

	rendered, err := json.MarshalIndent(
		RenderMemberDaemonSet(testMemberBackend(), 0, "mooncake:v0.3.13").Spec.Template, "", "  ")
	require.NoError(t, err)

	assert.JSONEq(t, string(recorded), string(rendered),
		"a member group with no disk tier must render exactly what it rendered before this feature")

	// Asserted separately and by value, because JSONEq would report a moved hash as one difference
	// among many and this is the field that decides whether running members are deleted.
	var want struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(recorded, &want))
	assert.Equal(t,
		want.Metadata.Annotations[MemberPodSpecHashAnnotation],
		RenderMemberDaemonSet(testMemberBackend(), 0, "mooncake:v0.3.13").
			Spec.Template.Annotations[MemberPodSpecHashAnnotation],
		"the fingerprint must not move, or every existing member is deleted and comes back empty")
}

// TestMemberWorkload_DiskTierIsAllOrNothing asserts the tier's rendered items as a WHOLE.
//
// One case per item would pass on a renderer that emitted all but one, and a tier missing one item
// is the shape this whole design exists to refuse: a member that reports disk capacity to the
// leader while having nowhere to write, somewhere to write that the leader never sends anything
// to, or — the case this cost a cluster investigation to find — a complete-looking tier whose bucket
// is never closed, so not one byte is written. The absence case matters for the same reason in
// reverse: it is what would catch a future path granting one of them to a group that asked for none.
func TestMemberWorkload_DiskTierIsAllOrNothing(t *testing.T) {
	t.Run("a group with a disk tier gets all of them", func(t *testing.T) {
		kvcb := testMemberBackend(withMemberDiskTier)
		ds := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
		container := ds.Spec.Template.Spec.Containers[0]
		env := memberEnv(t, kvcb, "mooncake:v0.3.13")

		assert.Equal(t, "true", env["MOONCAKE_OFFLOAD_ENABLED"])
		assert.Equal(t, "/var/lib/kvcache", env["MOONCAKE_OFFLOAD_FILE_STORAGE_PATH"])
		assert.Equal(t, "4398046511104", env["MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES"],
			"4Ti in bytes, since the client reads a byte count and not a quantity")
		assert.Equal(t, "4398046511104", env["MOONCAKE_OFFLOAD_BUCKET_MAX_TOTAL_SIZE"],
			"the same ceiling again, under the name the watermarks are a fraction of: its own "+
				"default is 0, which the client's eviction path reads as no quota at all")
		assert.Equal(t, strconv.FormatInt(MemberBucketSizeLimit, 10),
			env["MOONCAKE_OFFLOAD_BUCKET_SIZE_LIMIT_BYTES"],
			"the bucket is the unit the tier is written in, and the client's own threshold is what "+
				"leaves a modest backend's tier empty")
		assert.Equal(t, strconv.FormatInt(MemberBucketKeysLimit, 10),
			env["MOONCAKE_OFFLOAD_BUCKET_KEYS_LIMIT"],
			"the other threshold a bucket closes on; either alone leaves one kind of workload stuck")

		require.Len(t, ds.Spec.Template.Spec.Volumes, 1)
		volume := ds.Spec.Template.Spec.Volumes[0]
		require.NotNil(t, volume.HostPath,
			"a hostPath and not an emptyDir: an emptyDir dies with the Pod, so every restart would "+
				"discard the tier whose entire value is surviving one")
		assert.Equal(t, "/var/lib/kvcache", volume.HostPath.Path)
		require.NotNil(t, volume.HostPath.Type)
		assert.Equal(t, core.HostPathDirectory, *volume.HostPath.Type,
			"Directory and never DirectoryOrCreate: measured, a directory the kubelet creates is "+
				"root-owned 0755 while the image runs as uid 65532, so the member comes up unable "+
				"to write and retries its store setup until it gives up. A missing directory must "+
				"be a FailedMount the Pod stops at, not a mount it cannot use")

		require.Len(t, container.VolumeMounts, 1)
		assert.Equal(t, "/var/lib/kvcache", container.VolumeMounts[0].MountPath,
			"mounted where the client was told to write, so one field says both things")
	})

	t.Run("a group without one gets none of them", func(t *testing.T) {
		kvcb := testMemberBackend()
		ds := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
		env := memberEnv(t, kvcb, "mooncake:v0.3.13")

		for _, key := range []string{
			"MOONCAKE_OFFLOAD_ENABLED",
			"MOONCAKE_OFFLOAD_FILE_STORAGE_PATH",
			"MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES",
			"MOONCAKE_OFFLOAD_BUCKET_MAX_TOTAL_SIZE",
			"MOONCAKE_OFFLOAD_BUCKET_SIZE_LIMIT_BYTES",
			"MOONCAKE_OFFLOAD_BUCKET_KEYS_LIMIT",
		} {
			_, present := env[key]
			assert.False(t, present, "%s must not be set for a group with no disk tier", key)
		}
		assert.Empty(t, ds.Spec.Template.Spec.Volumes)
		assert.Empty(t, ds.Spec.Template.Spec.Containers[0].VolumeMounts)
		assert.Nil(t, ds.Spec.Template.Spec.Containers[0].Lifecycle)
	})

	t.Run("an unset capacity leaves the store's own ceilings alone", func(t *testing.T) {
		kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
			withMemberDiskTier(k)
			k.Spec.Connection.Managed.Members[0].LocalDisks[0].Capacity = resource.MustParse("0")
		})
		env := memberEnv(t, kvcb, "mooncake:v0.3.13")

		assert.Equal(t, "true", env["MOONCAKE_OFFLOAD_ENABLED"], "the tier is still declared")
		for _, key := range []string{
			"MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES",
			"MOONCAKE_OFFLOAD_BUCKET_MAX_TOTAL_SIZE",
		} {
			_, present := env[key]
			assert.False(t, present, "%s: an unrendered limit is the store's own, so a ceiling that "+
				"moves upstream is a change to investigate rather than one this renderer silently "+
				"restated", key)
		}
		assert.Equal(t, strconv.FormatInt(MemberBucketSizeLimit, 10),
			env["MOONCAKE_OFFLOAD_BUCKET_SIZE_LIMIT_BYTES"],
			"the bucket pair is NOT conditional on a capacity: it is what makes the tier take a "+
				"byte, so it goes with the tier rather than with the ceiling")
	})

	t.Run("an unset key limit leaves the store's own alone", func(t *testing.T) {
		kvcb := testMemberBackend(withMemberDiskTier)
		env := memberEnv(t, kvcb, "mooncake:v0.3.13")

		_, present := env["MOONCAKE_OFFLOAD_TOTAL_KEYS_LIMIT"]
		assert.False(t, present, "the same rule as the byte ceiling, on the count")
	})

	t.Run("a set key limit is rendered", func(t *testing.T) {
		kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
			withMemberDiskTier(k)
			k.Spec.Connection.Managed.Members[0].LocalDisks[0].KeyLimit = 500000
		})
		env := memberEnv(t, kvcb, "mooncake:v0.3.13")

		assert.Equal(t, "500000", env["MOONCAKE_OFFLOAD_TOTAL_KEYS_LIMIT"])
	})
}

// TestMemberBucketThresholds_StayUnderTheStoresOwn is the one assertion about the VALUE of the pair,
// and it asserts a relation rather than a number so that tuning it does not redden the test that
// exists to stop it being tuned back.
//
// The store's own 256Mi and 500 objects are not a neutral default here: they are the reason a
// declared tier held nothing on every cluster this was tried on, since a backend that never
// accumulates either never closes a bucket and therefore never writes one. A pair at or above them
// is that failure restored, whatever else looks right.
func TestMemberBucketThresholds_StayUnderTheStoresOwn(t *testing.T) {
	const (
		storeBucketSizeLimit int64 = 256 * 1024 * 1024
		storeBucketKeysLimit int64 = 500
	)

	assert.Less(t, MemberBucketSizeLimit, storeBucketSizeLimit,
		"a bucket at or above the store's own size threshold leaves a modest backend's tier empty, "+
			"which is the whole reason this pair is rendered")
	assert.Less(t, MemberBucketKeysLimit, storeBucketKeysLimit,
		"and the same on the object count, which is the threshold a small-object workload reaches "+
			"first")
	assert.Positive(t, MemberBucketSizeLimit,
		"the store refuses to start with a non-positive bucket size")
	assert.Positive(t, MemberBucketKeysLimit,
		"the store refuses to start with a non-positive bucket key count")
}

// TestMemberWorkload_DiskTierEviction pins what each shape of the eviction block renders.
//
// The absent and enabled-without-settings cases carry the rule the rest of this package follows: a
// setting the spec does not address is NOT rendered, so a default that moves upstream shows up as a
// change to investigate. The disabled case is the exception worth spelling out — "off" has to be
// said explicitly, at both layers, because there is no absence that means it.
func TestMemberWorkload_DiskTierEviction(t *testing.T) {
	withEviction := func(
		mutate func(*workercore.KVCacheBackendMemberLocalDiskEviction),
	) func(*workercore.KVCacheBackend) {
		return func(k *workercore.KVCacheBackend) {
			withMemberDiskTier(k)
			eviction := &workercore.KVCacheBackendMemberLocalDiskEviction{}
			mutate(eviction)
			k.Spec.Connection.Managed.Members[0].LocalDisks[0].Eviction = eviction
		}
	}

	cases := []struct {
		name   string
		mutate func(*workercore.KVCacheBackend)
		want   map[string]string
		absent []string
	}{
		{
			"no block at all",
			withMemberDiskTier,
			nil,
			[]string{
				"MOONCAKE_OFFLOAD_BUCKET_EVICTION_POLICY",
				"MOONCAKE_OFFLOAD_ENABLE_DISK_WATERMARK_EVICTION",
				"MOONCAKE_OFFLOAD_DISK_EVICTION_HIGH_WATERMARK_RATIO",
				"MOONCAKE_OFFLOAD_DISK_EVICTION_LOW_WATERMARK_RATIO",
			},
		},
		{
			"enabled with nothing else, which is the client's own behavior",
			withEviction(func(e *workercore.KVCacheBackendMemberLocalDiskEviction) {
				e.Enabled = ptr.To(true)
			}),
			nil,
			[]string{
				"MOONCAKE_OFFLOAD_BUCKET_EVICTION_POLICY",
				"MOONCAKE_OFFLOAD_ENABLE_DISK_WATERMARK_EVICTION",
			},
		},
		{
			"a policy",
			withEviction(func(e *workercore.KVCacheBackendMemberLocalDiskEviction) { e.Policy = "LRU" }),
			map[string]string{"MOONCAKE_OFFLOAD_BUCKET_EVICTION_POLICY": "lru"},
			[]string{"MOONCAKE_OFFLOAD_ENABLE_DISK_WATERMARK_EVICTION"},
		},
		{
			"the other policy",
			withEviction(func(e *workercore.KVCacheBackendMemberLocalDiskEviction) { e.Policy = "FIFO" }),
			map[string]string{"MOONCAKE_OFFLOAD_BUCKET_EVICTION_POLICY": "fifo"},
			nil,
		},
		{
			// Both layers, because the client reads them at different levels and a policy of none
			// only reaches one of them.
			"disabled",
			withEviction(func(e *workercore.KVCacheBackendMemberLocalDiskEviction) {
				e.Enabled = ptr.To(false)
			}),
			map[string]string{
				"MOONCAKE_OFFLOAD_BUCKET_EVICTION_POLICY":         "none",
				"MOONCAKE_OFFLOAD_ENABLE_DISK_WATERMARK_EVICTION": "false",
			},
			nil,
		},
		{
			// A fraction with two decimals, not a percentage and not the shortest notation: the
			// client parses the whole string and silently falls back to its own default for
			// anything it cannot consume entirely.
			"a watermark band",
			withEviction(func(e *workercore.KVCacheBackendMemberLocalDiskEviction) {
				e.Watermark = &workercore.KVCacheBackendMemberLocalDiskEvictionWatermark{High: 90, Low: 80}
			}),
			map[string]string{
				"MOONCAKE_OFFLOAD_DISK_EVICTION_HIGH_WATERMARK_RATIO": "0.90",
				"MOONCAKE_OFFLOAD_DISK_EVICTION_LOW_WATERMARK_RATIO":  "0.80",
			},
			nil,
		},
		{
			"a band at the top of the range",
			withEviction(func(e *workercore.KVCacheBackendMemberLocalDiskEviction) {
				e.Watermark = &workercore.KVCacheBackendMemberLocalDiskEvictionWatermark{High: 100, Low: 5}
			}),
			map[string]string{
				"MOONCAKE_OFFLOAD_DISK_EVICTION_HIGH_WATERMARK_RATIO": "1.00",
				"MOONCAKE_OFFLOAD_DISK_EVICTION_LOW_WATERMARK_RATIO":  "0.05",
			},
			nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := memberEnv(t, testMemberBackend(c.mutate), "mooncake:v0.3.13")

			for name, value := range c.want {
				assert.Equal(t, value, env[name], "%s", name)
			}
			for _, name := range c.absent {
				_, present := env[name]
				assert.False(t, present, "%s must not be rendered here", name)
			}
		})
	}
}

// TestMemberWorkload_ExtraEnv pins the hatch's rendering: every entry reaches the container, in
// the order written, after everything derived.
func TestMemberWorkload_ExtraEnv(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Members[0].ExtraEnv = []workercore.InstanceEnvVar{
			{Name: "MOONCAKE_OFFLOAD_USE_URING", Value: "true"},
			{Name: "MOONCAKE_OFFLOAD_HEARTBEAT_INTERVAL_SECONDS", Value: "5"},
		}
	})
	container := memberContainer(t, kvcb, "mooncake:v0.3.13")

	env := memberEnv(t, kvcb, "mooncake:v0.3.13")
	assert.Equal(t, "true", env["MOONCAKE_OFFLOAD_USE_URING"])
	assert.Equal(t, "5", env["MOONCAKE_OFFLOAD_HEARTBEAT_INTERVAL_SECONDS"])

	names := make([]string, 0, len(container.Env))
	for _, e := range container.Env {
		names = append(names, e.Name)
	}
	require.Len(t, names, len(env), "no name may appear twice: Kubernetes takes a duplicate and "+
		"leaves the winner to the runtime, which is why admission refuses a derived name here")
	assert.Equal(t,
		[]string{"MOONCAKE_OFFLOAD_USE_URING", "MOONCAKE_OFFLOAD_HEARTBEAT_INTERVAL_SECONDS"},
		names[len(names)-2:],
		"last and in the order written, so two renders of one spec are byte-identical")
}

// TestMemberDerivedEnvs_CoversEveryNameTheRendererEmits holds the reserved list equal to what is
// actually rendered, in both directions.
//
// The list is what admission refuses in extraEnv, and it is a SECOND copy of a fact the renderer
// already has — so the failure it is exposed to is drift, in either direction and both silent. A
// name the renderer emits but the list forgets is a container carrying that name twice, with the
// winner left to the runtime; a name the list holds but nothing emits is a hatch entry refused for a
// collision that cannot happen.
//
// The union is taken over several fixtures on purpose. No single backend renders all of them: the
// eviction policy is emitted either as a policy or as the "off" spelling, the watermark switch only
// when eviction is off, and the library path only on one transport.
func TestMemberDerivedEnvs_CoversEveryNameTheRendererEmits(t *testing.T) {
	fixtures := []struct {
		name   string
		mutate []func(*workercore.KVCacheBackend)
	}{
		{"a plain TCP group", nil},
		{"a tier with every ceiling and an eviction band", []func(*workercore.KVCacheBackend){
			func(k *workercore.KVCacheBackend) {
				withMemberDiskTier(k)
				disk := &k.Spec.Connection.Managed.Members[0].LocalDisks[0]
				disk.KeyLimit = 500000
				disk.Eviction = &workercore.KVCacheBackendMemberLocalDiskEviction{
					Enabled: ptr.To(true),
					Policy:  "LRU",
					Watermark: &workercore.KVCacheBackendMemberLocalDiskEvictionWatermark{
						High: 90, Low: 80,
					},
				}
			},
		}},
		{"a tier with eviction switched off", []func(*workercore.KVCacheBackend){
			func(k *workercore.KVCacheBackend) {
				withMemberDiskTier(k)
				k.Spec.Connection.Managed.Members[0].LocalDisks[0].Eviction = &workercore.KVCacheBackendMemberLocalDiskEviction{Enabled: ptr.To(false)}
			},
		}},
		{"the transport that mounts the host's libfabric", []func(*workercore.KVCacheBackend){
			func(k *workercore.KVCacheBackend) { k.Spec.Transport.Protocol = "EFA" },
		}},
	}

	rendered := make(map[string]string)
	for _, f := range fixtures {
		kvcb := testMemberBackend(f.mutate...)
		for _, e := range memberContainer(t, kvcb, "mooncake:v0.3.13").Env {
			rendered[e.Name] = f.name
		}
	}

	for name, fixture := range rendered {
		assert.Contains(t, MemberDerivedEnvs, name,
			"%s is rendered by %q and is not reserved, so a group could define it a second time "+
				"through extraEnv and nothing would report the collision", name, fixture)
	}
	for _, name := range MemberDerivedEnvs {
		_, ok := rendered[name]
		assert.True(t, ok,
			"%s is reserved and no fixture here renders it: either the renderer stopped emitting it, "+
				"in which case the reservation refuses a hatch entry for a collision that cannot "+
				"happen, or this test is missing the path that does", name)
	}
}

// TestMemberWorkload_ShutdownDrainsTheDiskTier pins the hook and the window that has to hold it.
func TestMemberWorkload_ShutdownDrainsTheDiskTier(t *testing.T) {
	t.Run("the hook posts the configured grace to the member's own endpoint", func(t *testing.T) {
		kvcb := testMemberBackend(withMemberDiskTier, withMemberScaleInGrace(30))
		container := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13").Spec.Template.Spec.Containers[0]

		require.NotNil(t, container.Lifecycle)
		require.NotNil(t, container.Lifecycle.PreStop)
		require.NotNil(t, container.Lifecycle.PreStop.Exec,
			"an exec and not an httpGet: a lifecycle httpGet sends no body and cannot choose a "+
				"method, while this route is a POST whose handler answers 400 without one — the "+
				"hook would fail on every shutdown while looking configured")
		assert.Nil(t, container.Lifecycle.PreStop.HTTPGet)

		command := container.Lifecycle.PreStop.Exec.Command
		require.Len(t, command, 3)
		assert.Equal(t, "python3", command[0],
			"the image's own interpreter: this container's command is a Python console script")
		assert.Contains(t, command[2], `"grace_period_seconds":30`,
			"the configured grace reaches the request body")
		assert.Contains(t, command[2], "127.0.0.1:8080/api/unmount_local_disk")
		assert.Contains(t, command[2], "timeout=40",
			"grace + a small margin: above the grace because the handler holds for exactly that "+
				"long, and far below the termination budget because the hook and the entrypoint's "+
				"own shutdown share it")
	})

	// The relationship, asserted independently of either number. The kubelet starts the termination
	// countdown BEFORE the hook and sends SIGTERM only once it returns, so a hook allowed to run for
	// the whole budget finishes exactly when the container is force-killed — and the entrypoint's
	// shutdown, which the budget exists to protect, never happens.
	t.Run("the hook cannot consume the whole termination budget", func(t *testing.T) {
		for _, grace := range []int32{0, 30, 3600} {
			kvcb := testMemberBackend(withMemberDiskTier, withMemberScaleInGrace(grace))
			ds := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
			command := ds.Spec.Template.Spec.Containers[0].Lifecycle.PreStop.Exec.Command

			budget := *ds.Spec.Template.Spec.TerminationGracePeriodSeconds
			timeout := int64(grace) + memberHookTimeoutMarginSeconds

			assert.Contains(t, command[2], fmt.Sprintf("timeout=%d", timeout))
			assert.Less(t, timeout, budget,
				"a grace of %d leaves the hook %ds inside a %ds budget; at or above it the "+
					"entrypoint is SIGKILLed mid-close", grace, timeout, budget)
			assert.GreaterOrEqual(t, budget-timeout, memberShutdownSeconds-memberHookTimeoutMarginSeconds,
				"what is left after the hook is what the entrypoint closes its store in")
		}
	})

	t.Run("the termination window is derived so it always holds the grace", func(t *testing.T) {
		// Two graces, not one: a single case passes against a renderer that returns a constant,
		// and what is under test is the derivation rather than one arithmetic result.
		for _, c := range []struct {
			grace  int32
			window int64
		}{{grace: 10, window: 70}, {grace: 300, window: 360}, {grace: 0, window: 60}} {
			kvcb := testMemberBackend(withMemberDiskTier, withMemberScaleInGrace(c.grace))
			ds := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")

			require.NotNil(t, ds.Spec.Template.Spec.TerminationGracePeriodSeconds)
			assert.Equal(t, c.window, *ds.Spec.Template.Spec.TerminationGracePeriodSeconds,
				"a grace of %d needs a window above it, or the kubelet kills the container in the "+
					"middle of the wait the spec asked for", c.grace)
		}
	})

	t.Run("a group with no disk tier keeps the entrypoint's own budget", func(t *testing.T) {
		// Even with a grace configured: there is nothing to drain, so nothing is waited for. The
		// grace is inert rather than refused, so that declaring a policy does not depend on the
		// order two independent fields are edited in.
		ds := RenderMemberDaemonSet(
			testMemberBackend(withMemberScaleInGrace(300)), 0, "mooncake:v0.3.13")

		require.NotNil(t, ds.Spec.Template.Spec.TerminationGracePeriodSeconds)
		assert.Equal(t, int64(60), *ds.Spec.Template.Spec.TerminationGracePeriodSeconds)
		assert.Nil(t, ds.Spec.Template.Spec.Containers[0].Lifecycle)
	})
}

// TestMemberWorkload_Protocol covers every value the API accepts.
//
// Auto and TCP are asserted to render an IDENTICAL Pod spec, not merely the same MOONCAKE_PROTOCOL:
// the resolution is a rename and not a second code path, and a path that resolved Auto while also
// granting it something TCP does not get would pass a value-only assertion.
func TestMemberWorkload_Protocol(t *testing.T) {
	cases := []struct {
		requested  string
		rendered   string
		privileged bool
	}{
		{requested: "Auto", rendered: "tcp", privileged: false},
		{requested: "TCP", rendered: "tcp", privileged: false},
		{requested: "RDMA", rendered: "rdma", privileged: true},
		{requested: "EFA", rendered: "efa", privileged: true},
		{requested: "ROCM", rendered: "hip", privileged: false},
		{requested: "MUSA", rendered: "musa", privileged: false},
		{requested: "MACA", rendered: "maca", privileged: false},
		// This spelling has a consumer outside this package: inject's engineTransportConstraint
		// records that vLLM-Ascend's store backend accepts exactly this string, so renaming it here
		// would refuse every CANN pool that engine can use.
		{requested: "CANN", rendered: "ascend", privileged: false},
	}

	for _, c := range cases {
		t.Run(c.requested, func(t *testing.T) {
			kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
				k.Spec.Transport.Protocol = c.requested
			})

			assert.Equal(t, c.rendered,
				envWithoutDownwardAPI(t, kvcb, "mooncake:v0.3.13")["MOONCAKE_PROTOCOL"],
				"the artifact has no %q; it looks its protocol up in a transport map", "auto")

			podSpec := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13").Spec.Template.Spec
			assert.Equal(t, c.privileged, podSpec.HostNetwork,
				"only the fabric that needs the host's network namespace gets it")
		})
	}

	autoSpec := RenderMemberDaemonSet(testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Transport.Protocol = "Auto"
	}), 0, "mooncake:v0.3.13").Spec.Template.Spec
	tcpSpec := RenderMemberDaemonSet(testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Transport.Protocol = "TCP"
	}), 0, "mooncake:v0.3.13").Spec.Template.Spec
	assert.Equal(t, tcpSpec, autoSpec,
		"Auto resolves to TCP and gets nothing else; the resolution is a rename, not a branch")

	// An object whose transport was never set renders the SAME thing, and this is not belt and
	// braces. Structural-schema defaulting does not descend into an absent object, so before the
	// containing object carried its own default the common spec — one that never mentions a
	// transport — stored no protocol at all and rendered MOONCAKE_PROTOCOL as the empty string,
	// which is not a value the artifact's transport map has. The schema is fixed; this pins the
	// renderer so an object that never passed through an API server cannot resurrect it.
	unsetSpec := RenderMemberDaemonSet(testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Transport = workercore.KVCacheBackendTransport{}
	}), 0, "mooncake:v0.3.13").Spec.Template.Spec
	assert.Equal(t, tcpSpec, unsetSpec,
		"an unset transport renders exactly what Auto does, never an empty protocol")
}

// TestMemberProtocols pins the offers an engine is matched against: every group's effective
// transport in declaration order, with the backend-wide value as the whole list for a backend
// that declares no groups — the shape an external backend takes, and every backend written before
// groups could disagree.
func TestMemberProtocols(t *testing.T) {
	t.Run("each group's own transport, in declaration order", func(t *testing.T) {
		kvcb := testMemberBackend(withSecondMemberGroupVRAM(func(group *workercore.KVCacheBackendMember) {
			group.Transport = &workercore.KVCacheBackendMemberTransport{Protocol: "RDMA"}
		}))
		assert.Equal(t, []string{"tcp", "rdma"}, MemberProtocols(kvcb))
	})

	t.Run("a group declaring none inherits the backend's", func(t *testing.T) {
		kvcb := testMemberBackend(withSecondMemberGroup)
		assert.Equal(t, []string{"tcp", "tcp"}, MemberProtocols(kvcb))
	})

	t.Run("a backend with no groups offers the backend-wide value alone", func(t *testing.T) {
		kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed = nil
			k.Spec.Transport.Protocol = "RDMA"
		})
		assert.Equal(t, []string{"rdma"}, MemberProtocols(kvcb),
			"the slice is never empty, so a caller matching against it never asks whether the "+
				"pool offers anything")
	})
}

// TestMemberWorkload_RDMAContext pins the security context of the one path that needs one.
// It is modest on purpose: hostNetwork, the device mount and two capabilities — and never
// privileged, which would hand the member the whole node.
func TestMemberWorkload_RDMAContext(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Transport.Protocol = "RDMA"
	})
	ds := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
	podSpec := ds.Spec.Template.Spec
	container := podSpec.Containers[0]

	assert.True(t, podSpec.HostNetwork)
	assert.Equal(t, core.DNSClusterFirstWithHostNet, podSpec.DNSPolicy,
		"a hostNetwork Pod that keeps ClusterFirst cannot resolve the leader's Service name")

	require.Len(t, podSpec.Volumes, 1, "exactly one volume: the RDMA device tree")
	require.NotNil(t, podSpec.Volumes[0].HostPath)
	assert.Equal(t, "/dev/infiniband", podSpec.Volumes[0].HostPath.Path)
	require.NotNil(t, podSpec.Volumes[0].HostPath.Type,
		"an untyped hostPath is not a weaker check, it is no check: the kubelet's mounter returns "+
			"without looking at the path at all")
	assert.Equal(t, core.HostPathDirectory, *podSpec.Volumes[0].HostPath.Type,
		"a node with no device tree has to stop the member at the mount. Left to start, it "+
			"discovers no device, installs TCP and serves, while the object still says RDMA")
	require.Len(t, container.VolumeMounts, 1)
	assert.Equal(t, "/dev/infiniband", container.VolumeMounts[0].MountPath)

	require.NotNil(t, container.SecurityContext)
	require.NotNil(t, container.SecurityContext.Capabilities)
	assert.ElementsMatch(t,
		[]core.Capability{"IPC_LOCK", "SYS_RESOURCE"},
		container.SecurityContext.Capabilities.Add,
		"the Add list is exactly these two, so a third added later has to be justified here. It is "+
			"what this grants, not what the container ends up holding: Add is layered over the "+
			"runtime's default set, and nothing here drops that set")
	assert.Nil(t, container.SecurityContext.Privileged,
		"never privileged: the two capabilities are what the fabric needs, and nothing more")
}

// TestMemberWorkload_EFAContext pins what the EFA path takes over the host-fabric base it shares
// with RDMA: one device from the plugin that advertises EFA, and nothing else. No host install
// prefix is mounted and no loader path is rendered, because the libfabric an EFA member runs on is
// in the image.
func TestMemberWorkload_EFAContext(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Transport.Protocol = "EFA"
	})
	ds := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
	podSpec := ds.Spec.Template.Spec
	container := podSpec.Containers[0]

	assert.True(t, podSpec.HostNetwork)
	assert.Equal(t, core.DNSClusterFirstWithHostNet, podSpec.DNSPolicy)

	require.Len(t, podSpec.Volumes, 1, "the device tree, and only the device tree")
	require.NotNil(t, podSpec.Volumes[0].HostPath)
	assert.Equal(t, "/dev/infiniband", podSpec.Volumes[0].HostPath.Path)
	require.NotNil(t, podSpec.Volumes[0].HostPath.Type)
	assert.Equal(t, core.HostPathDirectory, *podSpec.Volumes[0].HostPath.Type,
		"the type is on the shared base, so EFA gets the loud missing-device-tree failure too")
	require.Len(t, container.VolumeMounts, 1)
	assert.Equal(t, "/dev/infiniband", container.VolumeMounts[0].MountPath)

	efa := container.Resources.Limits[efaDeviceResource]
	assert.Equal(t, int64(1), efa.Value(),
		"one EFA device, asked for through the plugin: the hostPath above carries the device node "+
			"into the mount namespace, and the device cgroup still refuses to open it without an "+
			"allocation. A member that cannot open it discovers no HCA and starts TCP instead")

	env := map[string]string{}
	for _, e := range container.Env {
		env[e.Name] = e.Value
	}
	assert.NotContains(t, env, "LD_LIBRARY_PATH",
		"the image's own loader cache already ranks the AWS libfabric ahead of the distro one, so "+
			"pointing the loader at a host prefix would only reintroduce a tree whose own "+
			"dependencies are not mounted")

	require.NotNil(t, container.SecurityContext)
	require.NotNil(t, container.SecurityContext.Capabilities)
	assert.ElementsMatch(t,
		[]core.Capability{"IPC_LOCK", "SYS_RESOURCE"},
		container.SecurityContext.Capabilities.Add,
		"the EFA provider pins registered memory exactly like the verbs one does")
	assert.Nil(t, container.SecurityContext.Privileged,
		"never privileged: the two capabilities are what the fabric needs, and nothing more")
}

// TestMemberWorkload_TCPClaimsNoHost is the counterpart, and it asserts the absences as a
// whole rather than one at a time — a path that granted one of the three silently is exactly what
// this is here to catch.
func TestMemberWorkload_TCPClaimsNoHost(t *testing.T) {
	for _, protocol := range []string{"Auto", "TCP"} {
		t.Run(protocol, func(t *testing.T) {
			kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
				k.Spec.Transport.Protocol = protocol
			})
			podSpec := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13").Spec.Template.Spec

			assert.False(t, podSpec.HostNetwork, "no host network")
			assert.Equal(t, core.DNSClusterFirst, podSpec.DNSPolicy,
				"rendered explicitly, so switching back from RDMA converges instead of "+
					"leaving ClusterFirstWithHostNet behind")
			assert.Empty(t, podSpec.Volumes, "no device mount")
			assert.Empty(t, podSpec.Containers[0].VolumeMounts)
			assert.Nil(t, podSpec.Containers[0].SecurityContext,
				"no security context at all, rather than an empty one that invites a capability")
		})
	}
}

// TestMemberWorkload_DeclaresNoDataPlanePort pins an absence that a reader would otherwise
// assume was an oversight. The transfer engine binds its data ports at random — one observed run
// took 15002 and 15995, a second client 16566 and 16655, none of them configured — so a fixed
// containerPort would be a false statement about which ports the process uses.
func TestMemberWorkload_DeclaresNoDataPlanePort(t *testing.T) {
	for _, protocol := range []string{"Auto", "TCP", "RDMA", "EFA"} {
		t.Run(protocol, func(t *testing.T) {
			kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
				k.Spec.Transport.Protocol = protocol
			})
			assert.Empty(t, memberContainer(t, kvcb, "mooncake:v0.3.13").Ports,
				"the reachability requirement is a port RANGE, and the docs state it as one")
		})
	}
}

// TestMemberWorkload_ReadinessProvesTheMount pins the one signal that tells a member which has
// mounted its segment from one whose process merely started. The entrypoint mounts before it serves
// this port, so a connection is proof; without the probe the kubelet reports Ready as soon as the
// container runs, and the reconciler — which holds every ready Pod to the leader's listing — reads
// that window as a shortfall and moves a healthy backend to Degraded for the length of a rollout.
func TestMemberWorkload_ReadinessProvesTheMount(t *testing.T) {
	probe := memberContainer(t, testMemberBackend(), "mooncake:v0.3.13").ReadinessProbe
	require.NotNil(t, probe,
		"without a probe, Ready means the process started and says nothing about the mount")

	require.NotNil(t, probe.TCPSocket,
		"TCP: the entrypoint serves only /api/* data verbs, so a probe has no route to GET without "+
			"a key or a side effect, and an unrouted path answers 404 — which never becomes ready")
	assert.Nil(t, probe.HTTPGet)
	// The literal, never the constant: comparing the constant to what the constant rendered is a
	// tautology that survives any change to it. 8080 is the entrypoint's own --port default, which
	// is what the FIRST group runs on; a later group moves off it, and that is pinned separately in
	// TestMemberWorkload_RESTPortIsPerGroup.
	assert.Equal(t, int32(8080), probe.TCPSocket.Port.IntVal)
}

// TestMemberMaxGracePeriodSeconds_MatchesTheSchemaBound ties two literals that cannot share a
// source: the ceiling is a `+k8s:validation:maximum` MARKER on the field — a comment, compiled into
// the CRD — while the webhook's own check reads this constant. Neither can reference the other.
//
// So the drift is made to fail instead. Reading the bound out of the GENERATED CRD rather than
// re-typing 3600 here is the whole point: this compares the two things that can diverge, and a test
// asserting the constant against a literal would agree with itself while the schema moved.
func TestMemberMaxGracePeriodSeconds_MatchesTheSchemaBound(t *testing.T) {
	crd, ok := workercore.GetCustomResourceDefinitions()["KVCacheBackend"]
	require.True(t, ok, "the KVCacheBackend CRD is what carries the marker's compiled form")
	require.Len(t, crd.Spec.Versions, 1)

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	node := schema.
		Properties["spec"].
		Properties["connection"].
		Properties["managed"].
		Properties["scaleIn"].
		Properties["gracePeriodSeconds"]

	require.NotNil(t, node.Maximum,
		"the field has to carry a maximum: the hook holds for this long inside a termination budget "+
			"the kubelet is already counting, and an unbounded value outlives it")
	assert.Equal(t, float64(MemberMaxGracePeriodSeconds), *node.Maximum,
		"the schema refuses above %v while the webhook refuses above %d — a request between the two "+
			"is accepted by one and rejected by the other, and which one an operator meets depends "+
			"on whether the webhook is reachable", *node.Maximum, MemberMaxGracePeriodSeconds)
}

// TestMemberWorkload_RESTPortIsPerGroup pins a collision that became reachable the moment several
// member groups were allowed.
//
// On the RDMA path every member holds the host's network namespace and the entrypoint binds
// 0.0.0.0, so two groups whose selectors meet on one node would both want one port: the first binds
// it, the second runs, never passes its readiness probe, and reports nothing about why. Admission
// cannot refuse that pair instead — whether two selectors ever meet depends on node labels it does
// not have — so the port is derived from the group's position and there is no collision to have.
//
// The three places the port is spelled are asserted TOGETHER, because the defect this guards
// against is them disagreeing: a probe pointed at a port the process does not serve is a member
// that never becomes ready, and a hook pointed at one is a tier that is never drained.
func TestMemberWorkload_RESTPortIsPerGroup(t *testing.T) {
	// The tier sits on the second group on purpose: it is the group whose port moves, so the hook —
	// the third spelling — is rendered exactly where a drift would show.
	kvcb := testMemberBackend(withSecondMemberGroup, withSecondGroupDiskTier, withMemberScaleInGrace(30))

	cases := []struct {
		group int
		port  int32
		// Whether argv carries --port at all. The first group's port IS the entrypoint's default,
		// and rendering the flag there would add an argument to every member running today and roll
		// all of them to say what they were already doing.
		flagged  bool
		wantHook bool
	}{
		{group: 0, port: 8080, flagged: false, wantHook: false},
		{group: 1, port: 8081, flagged: true, wantHook: true},
	}

	for _, c := range cases {
		t.Run(fmt.Sprintf("group %d serves %d", c.group, c.port), func(t *testing.T) {
			container := RenderMemberDaemonSet(kvcb, c.group, "mooncake:v0.3.13").
				Spec.Template.Spec.Containers[0]

			if c.flagged {
				assert.Equal(t, []string{"--port", strconv.Itoa(int(c.port))}, container.Args,
					"a group off the default must say so on the command line: --port is a real flag "+
						"on the parser, and a -D config key of that name would not move the listener")
			} else {
				assert.Empty(t, container.Args,
					"the first group runs on the entrypoint's own default, so its argv must stay "+
						"exactly what it was before several groups were allowed")
			}

			require.NotNil(t, container.ReadinessProbe.TCPSocket)
			assert.Equal(t, c.port, container.ReadinessProbe.TCPSocket.Port.IntVal,
				"the probe must reach the port THIS group's process serves")

			if !c.wantHook {
				return
			}
			require.NotNil(t, container.Lifecycle)
			assert.Contains(t, container.Lifecycle.PreStop.Exec.Command[2],
				fmt.Sprintf("127.0.0.1:%d/api/unmount_local_disk", c.port),
				"the hook talks to its own container, so it must use its own group's port")
		})
	}

	t.Run("two groups never want the same port", func(t *testing.T) {
		first := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
		second := RenderMemberDaemonSet(kvcb, 1, "mooncake:v0.3.13")

		assert.NotEqual(t,
			first.Spec.Template.Spec.Containers[0].ReadinessProbe.TCPSocket.Port.IntVal,
			second.Spec.Template.Spec.Containers[0].ReadinessProbe.TCPSocket.Port.IntVal,
			"this is the whole point: on hostNetwork one node can hold both, and a shared port "+
				"leaves the second member running and permanently unready")
	})

	// The port is derived from the position, and a group whose position moved is rebuilt anyway, so
	// this is not a free dimension today. It has to be in the fingerprint for the OTHER direction:
	// if how the port is derived ever changes, every running member is serving the old one, and a
	// fingerprint blind to it would leave them there while the spec says otherwise.
	t.Run("the port reaches the fingerprint", func(t *testing.T) {
		// Two groups identical in every field EXCEPT their node selectors — and the fingerprint
		// strips the node selector. So a difference between these two hashes can only be the port.
		twins := testMemberBackend(func(k *workercore.KVCacheBackend) {
			second := k.Spec.Connection.Managed.Members[0]
			second.NodeSelector = map[string]string{"kvcache-dram-cold": "true"}
			k.Spec.Connection.Managed.Members = append(k.Spec.Connection.Managed.Members, second)
		})

		assert.NotEqual(t,
			MemberPodSpecHash(RenderMemberDaemonSet(twins, 0, "mooncake:v0.3.13").Spec.Template),
			MemberPodSpecHash(RenderMemberDaemonSet(twins, 1, "mooncake:v0.3.13").Spec.Template),
			"these two differ only in a stripped field and their port, so equal hashes would mean "+
				"the port is invisible to the one thing that decides a member restarts")
	})
}

// TestMemberWorkload_CarriesNoIndirection pins the other absences. The member's whole
// configuration is environment variables, so there is nothing to mount and nothing to prepare.
//
// The backend under test declares NO disk tier, and that is what the assertions are about. A group
// that declares one carries a volume and an init container for the tier; this pins that neither
// arrives for a group that did not ask, which is what keeps an existing backend's members in place
// across an upgrade.
func TestMemberWorkload_CarriesNoIndirection(t *testing.T) {
	podSpec := RenderMemberDaemonSet(testMemberBackend(), 0, "mooncake:v0.3.13").Spec.Template.Spec

	assert.Empty(t, podSpec.InitContainers, "no init container")
	assert.Empty(t, podSpec.Volumes, "no volume, and so no ConfigMap to mount")
	for _, env := range podSpec.Containers[0].Env {
		if env.ValueFrom != nil {
			assert.Nil(t, env.ValueFrom.ConfigMapKeyRef, "%s must not come from a ConfigMap", env.Name)
			assert.Nil(t, env.ValueFrom.SecretKeyRef, "%s must not come from a Secret", env.Name)
		}
	}
}

// TestMemberWorkload_ImageFallsBackPerGroup pins the one image split the shape cannot express with
// a single field: a group selects its own nodes, so two groups can sit on different accelerator
// hardware and need the client wheel built for it.
func TestMemberWorkload_ImageFallsBackPerGroup(t *testing.T) {
	t.Run("a group naming its own image wins", func(t *testing.T) {
		kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Image = "ascend-flavoured:v1"
		})
		assert.Equal(t, "ascend-flavoured:v1",
			memberContainer(t, kvcb, "the-backend-wide:v0").Image)
	})

	t.Run("a group naming none takes the backend's", func(t *testing.T) {
		assert.Equal(t, "the-backend-wide:v0",
			memberContainer(t, testMemberBackend(), "the-backend-wide:v0").Image)
	})
}

// TestMemberWorkload_PullPolicyAndSecrets is the member half of the leader's case: the two fields
// are backend-wide, so a group that names its OWN image is pulled with the same policy and the same
// credentials. That is what makes a per-group override usable at all — the group carries an image
// and nothing else, so without this it could only ever name a public one.
func TestMemberWorkload_PullPolicyAndSecrets(t *testing.T) {
	t.Run("unset resolves to the default the tag implies", func(t *testing.T) {
		ds := RenderMemberDaemonSet(testMemberBackend(), 0, "mooncake:v1")

		assert.Equal(t, core.PullIfNotPresent, ds.Spec.Template.Spec.Containers[0].ImagePullPolicy)
		assert.Empty(t, ds.Spec.Template.Spec.ImagePullSecrets)
	})

	// A group's own image decides its own policy: the override is the image that actually runs, so
	// resolving from the backend-wide one would give a :latest group the default of a pinned tag.
	t.Run("a group's override decides its own default", func(t *testing.T) {
		kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Image = "mooncake:latest"
		})

		ds := RenderMemberDaemonSet(kvcb, 0, "mooncake:v1")

		assert.Equal(t, core.PullAlways, ds.Spec.Template.Spec.Containers[0].ImagePullPolicy)
	})

	t.Run("both reach the rendered pod, including a group with its own image", func(t *testing.T) {
		kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
			k.Spec.ImagePullPolicy = core.PullAlways
			k.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: "registry-creds"}}
			k.Spec.Connection.Managed.Members[0].Image = "private.example.com/mooncake:v1"
		})

		ds := RenderMemberDaemonSet(kvcb, 0, "ignored:v0")

		assert.Equal(t, "private.example.com/mooncake:v1", ds.Spec.Template.Spec.Containers[0].Image)
		assert.Equal(t, core.PullAlways, ds.Spec.Template.Spec.Containers[0].ImagePullPolicy)
		assert.Equal(t, []core.LocalObjectReference{{Name: "registry-creds"}},
			ds.Spec.Template.Spec.ImagePullSecrets)
	})

	// Both are inside the fingerprint, because both change what a running Pod is. A credential
	// added after the group came up is added precisely because the Pods could not pull; leaving
	// them alive would leave the fix applied and not taken.
	t.Run("both move the fingerprint", func(t *testing.T) {
		base := MemberPodSpecHash(RenderMemberDaemonSet(testMemberBackend(), 0, "mooncake:v1").Spec.Template)

		policy := MemberPodSpecHash(RenderMemberDaemonSet(
			testMemberBackend(func(k *workercore.KVCacheBackend) {
				k.Spec.ImagePullPolicy = core.PullAlways
			}), 0, "mooncake:v1").Spec.Template)
		secrets := MemberPodSpecHash(RenderMemberDaemonSet(
			testMemberBackend(func(k *workercore.KVCacheBackend) {
				k.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: "registry-creds"}}
			}), 0, "mooncake:v1").Spec.Template)

		assert.NotEqual(t, base, policy)
		assert.NotEqual(t, base, secrets)
	})
}

// TestMemberWorkload_ExtraArgs pins the escape hatch's rendering. An entry is written as its own
// flag token and renders as the entrypoint's "-D key=value" override with the dashes gone — and
// not the leader's verbatim "-key=value", because the two binaries accept different things.
func TestMemberWorkload_ExtraArgs(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Members[0].ExtraArgs = []string{
			"-enable_ssd_offload=true",
			"-client_ttl=30",
		}
	})

	assert.Equal(t, []string{
		"-D", "enable_ssd_offload=true",
		"-D", "client_ttl=30",
	}, memberContainer(t, kvcb, "mooncake:v0.3.13").Args,
		"in the order written, so two renders of one spec are byte-identical")
}

// TestMemberWorkload_IsDeterministic pins that one group renders identically every time. The
// reconciler converges this object on every pass, so a wandering render would rewrite the DaemonSet
// forever and roll every member with it.
func TestMemberWorkload_IsDeterministic(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Members[0].ExtraArgs = []string{
			"-a=1", "-b=2", "-c=3", "-d=4", "-e=5",
		}
	})

	first := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
	for range 20 {
		assert.Equal(t, first, RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13"))
	}
}

// TestMemberWorkload_SelectorSurvivesASpecChange is the same immutability constraint the
// leader's Deployment has: a DaemonSet's spec.selector cannot be changed after creation.
func TestMemberWorkload_SelectorSurvivesASpecChange(t *testing.T) {
	before := RenderMemberDaemonSet(testMemberBackend(), 0, "mooncake:v0.3.13")

	after := RenderMemberDaemonSet(testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Image = "mooncake:v0.4.0"
		k.Spec.Transport.Protocol = "RDMA"
		group := &k.Spec.Connection.Managed.Members[0]
		group.NodeSelector = map[string]string{"kvcache-dram": "true", "zone": "b"}
		group.CapacityPerMember = resource.MustParse("1Ti")
		group.ExtraArgs = []string{"-client_ttl=30"}
	}), 0, "mooncake:v0.4.0")

	require.NotNil(t, before.Spec.Selector)
	assert.Equal(t, before.Spec.Selector, after.Spec.Selector,
		"the selector is immutable, so it may not carry anything a spec update can move — "+
			"the node selector especially, which widening a group is expected to change")

	assert.NotEqual(t, before.Spec.Template.Spec.NodeSelector, after.Spec.Template.Spec.NodeSelector,
		"the mutation did reach the template — otherwise the assertion above proves nothing")
}

// TestMemberWorkload_UsesOnDelete pins the strategy the restart policy rests on. Under the default
// the DaemonSet would roll every member whenever the node selector moved — and widening a group,
// which is how members are added, moves exactly that.
func TestMemberWorkload_UsesOnDelete(t *testing.T) {
	ds := RenderMemberDaemonSet(testMemberBackend(), 0, "mooncake:v0.3.13")

	assert.Equal(t, apps.OnDeleteDaemonSetStrategyType, ds.Spec.UpdateStrategy.Type,
		"the operator decides when a member restarts, so the built-in strategy must not")

	require.NotNil(t, ds.Spec.Template.Spec.TerminationGracePeriodSeconds)
	assert.Positive(t, *ds.Spec.Template.Spec.TerminationGracePeriodSeconds,
		"the entrypoint needs time to close its store; being cut short leaves the leader to time "+
			"the client out instead")
}

// TestMemberWorkload_FingerprintIgnoresTheNodeSelector is the case the whole OnDelete arrangement
// exists for: widening a group must add a member without restarting the members already running.
func TestMemberWorkload_FingerprintIgnoresTheNodeSelector(t *testing.T) {
	narrow := RenderMemberDaemonSet(testMemberBackend(), 0, "mooncake:v0.3.13")
	wide := RenderMemberDaemonSet(testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Members[0].NodeSelector = map[string]string{
			"kvcache-dram": "true", "zone": "b",
		}
	}), 0, "mooncake:v0.3.13")

	assert.NotEqual(t, narrow.Spec.Template.Spec.NodeSelector, wide.Spec.Template.Spec.NodeSelector,
		"the widening must actually reach the template, or the assertion below proves nothing")
	assert.Equal(t,
		narrow.Spec.Template.Annotations[MemberPodSpecHashAnnotation],
		wide.Spec.Template.Annotations[MemberPodSpecHashAnnotation],
		"a widening moves no fingerprint, so no member is restarted to add a node")
}

// TestMemberWorkload_FingerprintCoversEveryOtherField asserts the fingerprint field by field rather
// than once.
//
// A fingerprint over too little is indistinguishable from a correct one until the field it misses is
// the one that changed — and the failure mode then is a configuration written and never applied,
// which nothing reports. So each field that must move it gets its own case.
func TestMemberWorkload_FingerprintCoversEveryOtherField(t *testing.T) {
	baseline := RenderMemberDaemonSet(testMemberBackend(), 0, "mooncake:v0.3.13").
		Spec.Template.Annotations[MemberPodSpecHashAnnotation]
	require.NotEmpty(t, baseline)

	cases := []struct {
		field  string
		mutate func(*workercore.KVCacheBackend)
		image  string
	}{
		{
			field: "image",
			image: "mooncake:v0.4.0",
		},
		{
			field: "the group's own image override",
			mutate: func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members[0].Image = "ascend-flavoured:v1"
			},
		},
		{
			field: "capacity, which is both a request and an environment variable",
			mutate: func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("1Ti")
			},
		},
		{
			field: "the local buffer size",
			mutate: func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members[0].LocalBufferSize = resource.MustParse("8Gi")
			},
		},
		// The medium is deliberately NOT in this table. It has one value, it is frozen at
		// admission, and — since the disk tier stopped being a medium — it reaches nothing the
		// renderer emits, so a case here would assert that changing an inert field moves the hash.
		{
			field:  "declaring a disk tier at all",
			mutate: withMemberDiskTier,
		},
		// The tier's own fields are not here, and the reason is that this table's baseline has no
		// tier: against it, "the capacity moved" and "a tier appeared" are the same observation,
		// so a case would pass whether or not the capacity reached the hash. They have their own
		// test, whose baseline already carries a tier.
		{
			field: "extraArgs",
			mutate: func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members[0].ExtraArgs = []string{"-client_ttl=30"}
			},
		},
		{
			field: "the transport, which brings the whole fabric context with it",
			mutate: func(k *workercore.KVCacheBackend) {
				k.Spec.Transport.Protocol = "RDMA"
			},
		},
	}

	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			image := c.image
			if image == "" {
				image = "mooncake:v0.3.13"
			}
			mutations := []func(*workercore.KVCacheBackend){}
			if c.mutate != nil {
				mutations = append(mutations, c.mutate)
			}

			got := RenderMemberDaemonSet(testMemberBackend(mutations...), 0, image).
				Spec.Template.Annotations[MemberPodSpecHashAnnotation]

			assert.NotEqual(t, baseline, got,
				"changing %s must move the fingerprint, or the change is written and never applied",
				c.field)
		})
	}
}

// TestMemberWorkload_FingerprintCoversTheDiskTier is the per-field guard for the tier's own fields,
// against a baseline that ALREADY has a tier.
//
// It is separate from the table above for a reason that would otherwise make it useless. That
// table's baseline has no tier, so against it "the capacity moved" and "a tier appeared" are the
// same observation — a case there would pass whether or not the capacity ever reached the hash, and
// the field it was meant to cover could be dropped from the render with nothing going red.
//
// What the hash decides is whether the reconciler deletes a group's members. A field that moves and
// does not reach the hash is a change written to the DaemonSet and never applied, since the update
// strategy is OnDelete and nothing else restarts a member.
func TestMemberWorkload_FingerprintCoversTheDiskTier(t *testing.T) {
	fingerprint := func(mutate ...func(*workercore.KVCacheBackend)) string {
		return RenderMemberDaemonSet(testMemberBackend(mutate...), 0, "mooncake:v0.3.13").
			Spec.Template.Annotations[MemberPodSpecHashAnnotation]
	}

	baseline := fingerprint(withMemberDiskTier)

	cases := []struct {
		field  string
		mutate func(*workercore.KVCacheBackend)
	}{
		{
			field: "the tier's path, which is both a mount and an environment variable",
			mutate: func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members[0].LocalDisks[0].Path = "/var/lib/elsewhere"
			},
		},
		{
			field: "the tier's capacity, which admission deliberately leaves editable",
			mutate: func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members[0].LocalDisks[0].Capacity = resource.MustParse("8Ti")
			},
		},
		{
			field: "the scale-in grace, which is in the hook and in the termination window",
			mutate: func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.ScaleIn = &workercore.KVCacheBackendScaleIn{GracePeriodSeconds: 45}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			assert.NotEqual(t, baseline, fingerprint(withMemberDiskTier, c.mutate),
				"changing %s must move the fingerprint, or the change is written and never applied",
				c.field)
		})
	}
}

// TestMemberWorkload_FingerprintDoesNotCoverItself pins that the hash is stable across renders. A
// fingerprint that included the annotation holding it would differ on every render and roll the
// whole group forever.
func TestMemberWorkload_FingerprintDoesNotCoverItself(t *testing.T) {
	kvcb := testMemberBackend()

	first := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
	for range 5 {
		again := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
		assert.Equal(t,
			first.Spec.Template.Annotations[MemberPodSpecHashAnnotation],
			again.Spec.Template.Annotations[MemberPodSpecHashAnnotation])
	}

	// Hashing the rendered template — annotation and all — must give back the value in it.
	assert.Equal(t,
		first.Spec.Template.Annotations[MemberPodSpecHashAnnotation],
		MemberPodSpecHash(first.Spec.Template),
		"re-hashing a stamped template reproduces its own stamp; otherwise the value drifts on "+
			"every pass")
}

// TestMemberWorkload_SurveyReportsAnUnreadableTierDistinctly pins the producer half of the survey's
// two-signal contract.
//
// The reader refuses a negative count, which is worth nothing unless the script actually emits one.
// The count travels through a pipe, and a pipeline reports the LAST command's status, so `ls | wc -l`
// says whether `wc` ran and never whether `ls` could read the directory. Both failures then look
// like `entries=0`, and "empty" is the answer that suppresses the warning -- written once and
// permanently. The control cannot cover this: it is counted on the root directory, so it is healthy
// exactly when the tier is not readable.
func TestMemberWorkload_SurveyReportsAnUnreadableTierDistinctly(t *testing.T) {
	ds := RenderMemberDaemonSet(testMemberBackend(withMemberDiskTier), 0, "mooncake:v0.3.13")

	var survey *core.Container
	for i := range ds.Spec.Template.Spec.InitContainers {
		if ds.Spec.Template.Spec.InitContainers[i].Name == MemberLocalDiskSurveyContainerName {
			survey = &ds.Spec.Template.Spec.InitContainers[i]
			break
		}
	}
	if !assert.NotNil(t, survey, "a declared tier renders a survey") {
		return
	}
	script := survey.Command[2]

	assert.Contains(t, script, `ls -A '/var/lib/kvcache' >/dev/null 2>&1 || t=-1`,
		"the tier's own exit status is the only signal that separates unreadable from empty")
	assert.Contains(t, script, `c=$(ls -A / 2>/dev/null | wc -l)`,
		"the control still has to prove a shell and an ls exist")
	assert.Contains(t, script, "exit 0",
		"the survey reports and never blocks the member on its own findings")
}

// TestMemberWorkload_SurveyQuotesThePathAgainstTheShell pins that the path cannot become a command.
//
// The renderer once interpolated it with %q, which produces DOUBLE quotes -- and `sh` expands $,
// backticks and $(...) inside those, so it looked like quoting while being none. Admission takes any
// absolute path without "..", with no restriction on the character set, so this quoting is the whole
// of what stands between a field of the object and command execution in the member's init container.
func TestMemberWorkload_SurveyQuotesThePathAgainstTheShell(t *testing.T) {
	for _, tc := range []struct {
		name, path, want string
	}{
		{
			name: "a command substitution is a literal, not a command",
			path: "/mnt/$(id -u)", want: `'/mnt/$(id -u)'`,
		},
		{
			name: "a backtick is a literal too",
			path: "/mnt/`id -u`", want: "'/mnt/`id -u`'",
		},
		{
			// A single quote cannot be escaped inside single quotes; the run is closed, an escaped
			// quote is contributed, and a new run is opened. Without this the path would end the
			// quoting and the rest would be parsed as shell.
			name: "a single quote closes and reopens the run rather than escaping the quoting",
			path: "/mnt/it's", want: `'/mnt/it'\''s'`,
		},
		{
			name: "a space stays one argument",
			path: "/mnt/a b", want: `'/mnt/a b'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := RenderMemberDaemonSet(testMemberBackend(func(kvcb *workercore.KVCacheBackend) {
				withMemberDiskTier(kvcb)
				kvcb.Spec.Connection.Managed.Members[0].LocalDisks[0].Path = tc.path
			}), 0, "mooncake:v0.3.13")

			var script string
			for i := range ds.Spec.Template.Spec.InitContainers {
				if ds.Spec.Template.Spec.InitContainers[i].Name == MemberLocalDiskSurveyContainerName {
					script = ds.Spec.Template.Spec.InitContainers[i].Command[2]
				}
			}
			assert.Contains(t, script, tc.want,
				"the path has to reach `sh` as one literal word")
			assert.NotContains(t, script, `"`+tc.path+`"`,
				"double quotes do not suppress expansion in sh, so they are not quoting here")
		})
	}
}

// TestMemberWorkload_FabricDeviceResource pins which extended resource a host-fabric member asks
// for, across every combination of protocol and declaration.
//
// The name is not a property of the fabric. It belongs to whichever device plugin the cluster's
// administrator installed, and the two common RDMA plugins both let that name be configured, so
// there is nothing this operator could hard-code that would be right on two clusters. EFA is the
// single exception -- its plugin is AWS's own and advertises one name -- which is why it has a
// fallback and RDMA does not.
//
// The case that matters most is the third row. An unset declaration on a fabric with no fallback
// renders no request at all, which is what every backend written before this field did: the device
// tree is mounted, the device cgroup refuses the open, the store finds no HCA and installs TCP, and
// the object still reads as RDMA. Pinning it here says that outcome is reachable on purpose rather
// than by omission.
func TestMemberWorkload_FabricDeviceResource(t *testing.T) {
	const declared = "rdma/hca_shared_devices_a"

	cases := []struct {
		name     string
		protocol string
		declare  string
		want     core.ResourceName
		absent   core.ResourceName
	}{
		{
			name:     "EFA with nothing declared falls back to the one name its plugin advertises",
			protocol: "EFA",
			want:     efaDeviceResource,
		},
		{
			name:     "a declaration overrides the EFA fallback rather than joining it",
			protocol: "EFA",
			declare:  declared,
			want:     declared,
			absent:   efaDeviceResource,
		},
		{
			name:     "RDMA with nothing declared asks for nothing, which is the old behavior",
			protocol: "RDMA",
			absent:   declared,
		},
		{
			name:     "RDMA asks for exactly the resource the administrator named",
			protocol: "RDMA",
			declare:  declared,
			want:     declared,
		},
		{
			name:     "a declaration on a path with no fabric renders nothing at all",
			protocol: "TCP",
			declare:  declared,
			absent:   declared,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
				k.Spec.Transport.Protocol = c.protocol
				k.Spec.Transport.DeviceResourceName = c.declare
			})

			limits := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13").
				Spec.Template.Spec.Containers[0].Resources.Limits

			if c.want != "" {
				got := limits[c.want]
				assert.Equal(t, int64(1), got.Value(),
					"the device cgroup refuses to open the node the hostPath carried in until a "+
						"plugin allocation adds the rule, and one device is what a member needs")
			}

			if c.absent != "" {
				assert.NotContains(t, limits, c.absent,
					"a resource asked for here is one the node must advertise, so an unwanted one "+
						"makes the member unschedulable rather than merely over-provisioned")
			}

			if c.want == "" {
				assert.Empty(t, limits,
					"no fabric resource was asked for, and nothing else on this path sets a limit")
			}
		})
	}
}

// TestMemberProtocols_EveryEnumValueResolves is the guard on the one drift that produces an empty
// transport rather than an error.
//
// memberProtocols translates the API's spelling into the artifact's, and a lookup that misses
// returns the empty string. Nothing downstream treats that as a failure on its own: an unconstrained
// engine accepts any offer, so a ninth enum value added without its map entry would reach the engine
// as an empty MOONCAKE_PROTOCOL rather than as a refusal. The two lists live in different packages
// and nothing but this test makes one follow the other.
//
// The enum is read out of the GENERATED CRD rather than restated here, because a copy of it would
// drift in exactly the case this exists to catch. Both schema sites are read: the backend's
// spec.transport.protocol and a member group's own, which carry the same values through separate
// markers and can therefore diverge.
func TestMemberProtocols_EveryEnumValueResolves(t *testing.T) {
	crd, ok := workercore.GetCustomResourceDefinitions()["KVCacheBackend"]
	require.True(t, ok, "the KVCacheBackend CRD is generated under this key")

	var schema *apiext.JSONSchemaProps
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Schema != nil && crd.Spec.Versions[i].Schema.OpenAPIV3Schema != nil {
			schema = crd.Spec.Versions[i].Schema.OpenAPIV3Schema
			break
		}
	}
	require.NotNil(t, schema, "the CRD carries a structural schema")

	enumAt := func(t *testing.T, path ...string) []string {
		t.Helper()

		node := schema
		for _, step := range path {
			if step == "" {
				// The array step: descend into the item schema rather than the property.
				require.NotNil(t, node.Items, "the schema still has an item schema here")
				node = node.Items.Schema
				continue
			}
			next, found := node.Properties[step]
			require.True(t, found, "the schema still has a %q under %v", step, path)
			node = &next
		}

		require.NotEmpty(t, node.Enum, "the field at %v still carries an enum", path)

		values := make([]string, 0, len(node.Enum))
		for _, raw := range node.Enum {
			var value string
			require.NoError(t, json.Unmarshal(raw.Raw, &value))
			values = append(values, value)
		}
		return values
	}

	backendEnum := enumAt(t, "spec", "transport", "protocol")
	groupEnum := enumAt(t, "spec", "connection", "managed", "members", "", "transport", "protocol")

	assert.Equal(t, backendEnum, groupEnum,
		"a group's protocol replaces the backend's, so one value accepted at one site and not the "+
			"other would be accepted and then unresolvable")

	for _, value := range backendEnum {
		resolved, found := memberProtocols[value]
		assert.True(t, found,
			"enum value %q has no entry in memberProtocols: it would resolve to the empty string, "+
				"which reaches the member as an empty MOONCAKE_PROTOCOL rather than as an error", value)
		assert.NotEmpty(t, resolved, "enum value %q maps to the empty string", value)
	}
}
