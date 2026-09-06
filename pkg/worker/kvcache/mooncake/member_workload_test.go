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
	"k8s.io/apimachinery/pkg/api/resource"

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

// withMemberDiskTier declares a complete local disk tier on the canonical group.
//
// Both halves, because a tier declared on one side only is refused at admission — a fixture that
// set one would describe an object that cannot exist, and the renderer would be asked a question
// the API never puts to it.
func withMemberDiskTier(kvcb *workercore.KVCacheBackend) {
	kvcb.Spec.Connection.Managed.Members[0].LocalDisk = &workercore.KVCacheBackendMemberLocalDisk{
		Path:     "/var/lib/kvcache",
		Capacity: resource.MustParse("4Ti"),
	}
	kvcb.Spec.Connection.Managed.Leader.Offload = &workercore.KVCacheBackendLeaderOffload{Enabled: true}
}

// withMemberScaleInGrace sets the grace a departing member holds its tier open for.
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

// withSecondGroupDiskTier puts the tier on the SECOND group, so the preStop hook is rendered for a
// group whose port moved. Only one group may carry a tier, so this is the tier rather than a second.
func withSecondGroupDiskTier(kvcb *workercore.KVCacheBackend) {
	kvcb.Spec.Connection.Managed.Members[1].LocalDisk = &workercore.KVCacheBackendMemberLocalDisk{
		Path:     "/var/lib/kvcache",
		Capacity: resource.MustParse("4Ti"),
	}
	kvcb.Spec.Connection.Managed.Leader.Offload = &workercore.KVCacheBackendLeaderOffload{Enabled: true}
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
	}, envWithoutDownwardAPI(t, kvcb, "mooncake:v0.3.13"),
		"the whole environment, so a key added later has to be added here too")

	_, hasMetadataNormalised := env["MOONCAKE_TE_METADATA_SERVER"]
	assert.False(t, hasMetadataNormalised,
		"MOONCAKE_TE_METADATA_SERVER is the wrong spelling and fails silently; it must never appear")
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

// TestMemberWorkload_NoDiskTierRendersWhatItAlwaysDid is the guard that this feature does not roll
// every backend already running.
//
// It compares against a template RECORDED from the renderer as it stood before the disk tier
// existed, not against a fresh render — a fresh one moves with the code and would be green by
// construction, which is exactly the failure this guard exists to catch.
//
// What is at stake is not cosmetic. The pod-spec hash is in the recording, and the reconciler
// deletes every member Pod whose hash has moved. A byte of drift here means every member of every
// existing backend is deleted and recreated on upgrade, and each one comes back with an empty
// segment: the cache is gone, and nothing about the change said it would be.
//
// This guard was falsified before it was trusted: changing memberShutdownSeconds from 60 to 61 —
// one byte, on a field that has nothing to do with the disk tier — turns it red and moves the hash
// from e4e1f6a5… to 950bb64a…. A guard this load-bearing that has never been seen to fail is a
// guard nobody has checked.
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

// TestMemberWorkload_DiskTierIsAllOrNothing asserts the tier's five rendered items as a WHOLE.
//
// One case per item would pass on a renderer that emitted four of the five, and four-fifths of a
// tier is the shape this whole design exists to refuse: a member that reports disk capacity to the
// leader while having nowhere to write, or somewhere to write that the leader never sends anything
// to. The absence case matters for the same reason in reverse — it is what would catch a future
// path granting one of the five to a group that asked for none.
func TestMemberWorkload_DiskTierIsAllOrNothing(t *testing.T) {
	t.Run("a group with a disk tier gets all five", func(t *testing.T) {
		kvcb := testMemberBackend(withMemberDiskTier)
		ds := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
		container := ds.Spec.Template.Spec.Containers[0]
		env := memberEnv(t, kvcb, "mooncake:v0.3.13")

		assert.Equal(t, "true", env["MOONCAKE_OFFLOAD_ENABLED"])
		assert.Equal(t, "/var/lib/kvcache", env["MOONCAKE_OFFLOAD_FILE_STORAGE_PATH"])
		assert.Equal(t, "4398046511104", env["MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES"],
			"4Ti in bytes, since the client reads a byte count and not a quantity")

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

	t.Run("a group without one gets none of the five", func(t *testing.T) {
		kvcb := testMemberBackend()
		ds := RenderMemberDaemonSet(kvcb, 0, "mooncake:v0.3.13")
		env := memberEnv(t, kvcb, "mooncake:v0.3.13")

		for _, key := range []string{
			"MOONCAKE_OFFLOAD_ENABLED",
			"MOONCAKE_OFFLOAD_FILE_STORAGE_PATH",
			"MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES",
		} {
			_, present := env[key]
			assert.False(t, present, "%s must not be set for a group with no disk tier", key)
		}
		assert.Empty(t, ds.Spec.Template.Spec.Volumes)
		assert.Empty(t, ds.Spec.Template.Spec.Containers[0].VolumeMounts)
		assert.Nil(t, ds.Spec.Template.Spec.Containers[0].Lifecycle)
	})

	t.Run("an unset capacity leaves the store's own ceiling alone", func(t *testing.T) {
		kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
			withMemberDiskTier(k)
			k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("0")
		})
		env := memberEnv(t, kvcb, "mooncake:v0.3.13")

		assert.Equal(t, "true", env["MOONCAKE_OFFLOAD_ENABLED"], "the tier is still declared")
		_, present := env["MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES"]
		assert.False(t, present,
			"an unrendered limit is the store's own, so a ceiling that moves upstream is a change "+
				"to investigate rather than one this renderer silently restated")
	})
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
		{requested: "HIP", rendered: "hip", privileged: false},
		{requested: "Ascend", rendered: "ascend", privileged: false},
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
	for _, protocol := range []string{"Auto", "TCP", "RDMA"} {
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

// TestMemberWorkload_ExtraArgs pins the escape hatch's rendering. It is `-D key=value` on this
// side — the entrypoint's own per-key override — and not the leader's `-key=value`, because the two
// binaries accept different things.
func TestMemberWorkload_ExtraArgs(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Members[0].ExtraArgs = map[string]string{
			"enable_ssd_offload": "true",
			"client_ttl":         "30",
		}
	})

	assert.Equal(t, []string{
		"-D", "client_ttl=30",
		"-D", "enable_ssd_offload=true",
	}, memberContainer(t, kvcb, "mooncake:v0.3.13").Args,
		"sorted by key, so two renders of one spec are byte-identical")
}

// TestMemberWorkload_IsDeterministic pins that one group renders identically every time. The
// reconciler converges this object on every pass, so a wandering render would rewrite the DaemonSet
// forever and roll every member with it.
func TestMemberWorkload_IsDeterministic(t *testing.T) {
	kvcb := testMemberBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Members[0].ExtraArgs = map[string]string{
			"a": "1", "b": "2", "c": "3", "d": "4", "e": "5",
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
		group.ExtraArgs = map[string]string{"client_ttl": "30"}
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
				k.Spec.Connection.Managed.Members[0].ExtraArgs = map[string]string{"client_ttl": "30"}
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
				k.Spec.Connection.Managed.Members[0].LocalDisk.Path = "/var/lib/elsewhere"
			},
		},
		{
			field: "the tier's capacity, which admission deliberately leaves editable",
			mutate: func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("8Ti")
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
