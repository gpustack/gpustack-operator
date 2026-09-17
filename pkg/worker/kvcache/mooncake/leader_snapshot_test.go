package mooncake

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// snapshotBackend is a three-replica backend keeping its snapshot on a named claim, which is the
// shape the whole feature is for.
func snapshotBackend(mutate ...func(*workercore.KVCacheBackendLeaderSnapshot)) *workercore.KVCacheBackend {
	snapshot := &workercore.KVCacheBackendLeaderSnapshot{
		PersistentVolumeClaimName: "mooncake-snapshots",
	}
	for _, m := range mutate {
		m(snapshot)
	}
	return haBackend(func(kvcb *workercore.KVCacheBackend) {
		kvcb.Spec.Connection.Managed.Leader.HighAvailability.Snapshot = snapshot
	})
}

// TestRenderLeaderFlags_Snapshot asserts the whole argv rather than probing it, for the reason the
// golden lists above give: a flag silently added is exactly what a substring assertion cannot see.
//
// The interval and the retention count are asserted ABSENT when their fields are unset. Rendering
// either at the artifact's own value would restate a default, and a default that moved upstream
// would then look like a value this API had chosen.
func TestRenderLeaderFlags_Snapshot(t *testing.T) {
	cases := []struct {
		name string
		kvcb *workercore.KVCacheBackend
		want []string
	}{
		{
			name: "the claim alone renders the three flags that cannot be omitted",
			kvcb: snapshotBackend(),
			want: []string{
				"-enable_snapshot=true",
				"-enable_snapshot_restore=true",
				"-snapshot_object_store_type=local",
			},
		},
		{
			name: "the two tuning fields render only when set",
			kvcb: snapshotBackend(func(s *workercore.KVCacheBackendLeaderSnapshot) {
				s.IntervalSeconds = ptr.To[int32](120)
				s.RetentionCount = ptr.To[int32](3)
			}),
			want: []string{
				"-enable_snapshot=true",
				"-enable_snapshot_restore=true",
				"-snapshot_object_store_type=local",
				"-snapshot_interval_seconds=120",
				"-snapshot_retention_count=3",
			},
		},
		{
			name: "one of the two set renders one of the two",
			kvcb: snapshotBackend(func(s *workercore.KVCacheBackendLeaderSnapshot) {
				s.RetentionCount = ptr.To[int32](5)
			}),
			want: []string{
				"-enable_snapshot=true",
				"-enable_snapshot_restore=true",
				"-snapshot_object_store_type=local",
				"-snapshot_retention_count=5",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			flags := RenderLeaderFlags(c.kvcb)

			var snapshotFlags []string
			for _, flag := range flags {
				if len(flag) > 10 && flag[:10] == "-snapshot_" {
					snapshotFlags = append(snapshotFlags, flag)
					continue
				}
				if flag == "-enable_snapshot=true" || flag == "-enable_snapshot_restore=true" {
					snapshotFlags = append(snapshotFlags, flag)
				}
			}
			assert.Equal(t, c.want, snapshotFlags)
		})
	}
}

// TestRenderLeaderFlags_NoSnapshotRendersNothing is the negative half, and it is the one that
// matters: the store treats these flags as a group whose absence is a working single-master
// deployment, so a backend nobody asked to snapshot must run the command line it ran before this
// field existed.
func TestRenderLeaderFlags_NoSnapshotRendersNothing(t *testing.T) {
	for _, flag := range RenderLeaderFlags(haBackend()) {
		assert.NotContains(t, flag, "snapshot",
			"a backend that declares no snapshot renders no part of the group")
	}
}

// TestRenderLeaderFlags_SnapshotIsNotGatedOnTheElection pins the ONE place this group deliberately
// parts company with the election flags beside it.
//
// A single leader restores its own last snapshot when it restarts, so the field earns its flags
// below two replicas as well. The election flags in the same block do not, because an image without
// the lease backend refuses to start on them -- and asserting both halves here is what keeps a
// later edit from collapsing the two gates into one.
func TestRenderLeaderFlags_SnapshotIsNotGatedOnTheElection(t *testing.T) {
	one := testBackend(func(kvcb *workercore.KVCacheBackend) {
		kvcb.Spec.Connection.Managed.Leader.HighAvailability = &workercore.KVCacheBackendLeaderHighAvailability{
			Snapshot: &workercore.KVCacheBackendLeaderSnapshot{
				PersistentVolumeClaimName: "mooncake-snapshots",
			},
		}
	})

	flags := RenderLeaderFlags(one)
	assert.Contains(t, flags, "-enable_snapshot=true",
		"one leader restoring its own last snapshot is worth having, so this does not wait for a second")
	for _, flag := range flags {
		assert.NotContains(t, flag, "enable_ha",
			"the election still has nothing to elect between, and this case is what holds the two gates apart")
	}
}

// TestRenderLeaderDeployment_Snapshot asserts the three things that have to arrive together, and
// says why each alone is a failure.
//
// The variable is the load-bearing one and the least visible: the store's local object store reads
// its root from the environment with NO default and no flag that could carry it, so a mounted claim
// without the variable is a master that refuses to start, and a variable without the claim is a
// directory inside one pod that the standby cannot see.
func TestRenderLeaderDeployment_Snapshot(t *testing.T) {
	deploy := RenderLeaderDeployment(snapshotBackend(), "mooncake:v0.3.13")
	podSpec := deploy.Spec.Template.Spec
	container := podSpec.Containers[0]

	volumeIndex := slices.IndexFunc(podSpec.Volumes, func(v core.Volume) bool {
		return v.Name == leaderSnapshotVolumeName
	})
	require.GreaterOrEqual(t, volumeIndex, 0, "the claim has to reach the pod as a volume")
	volume := podSpec.Volumes[volumeIndex]
	require.NotNil(t, volume.PersistentVolumeClaim,
		"a claim, not an emptyDir: the point is storage that outlives the pod writing it")
	assert.Equal(t, "mooncake-snapshots", volume.PersistentVolumeClaim.ClaimName)

	assert.Contains(t, container.VolumeMounts, core.VolumeMount{
		Name:      leaderSnapshotVolumeName,
		MountPath: leaderSnapshotDir,
	}, "writable, because which replica serves moves without the pod spec changing")

	assert.Contains(t, container.Env, core.EnvVar{
		Name:  LeaderSnapshotLocalPathEnv,
		Value: leaderSnapshotDir,
	}, "the store reads its snapshot root from here and has no default for it")
}

// TestRenderLeaderDeployment_SnapshotSitsBesideTheQuotaPolicy pins that the two volume-bearing
// features compose.
//
// They are rendered by separate branches over one list, which is the arrangement where an
// assignment in place of an append drops whichever ran first. Nothing else would report it: the
// Deployment is valid either way, and the feature whose volume vanished fails at runtime as a
// missing mount.
func TestRenderLeaderDeployment_SnapshotSitsBesideTheQuotaPolicy(t *testing.T) {
	kvcb := snapshotBackend()
	kvcb.Spec.Connection.Managed.Leader.MultiTenancy = true

	deploy := RenderLeaderDeployment(kvcb, "mooncake:v0.3.13")
	names := make([]string, 0, len(deploy.Spec.Template.Spec.Volumes))
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		names = append(names, v.Name)
	}

	assert.ElementsMatch(t, []string{
		quotaPolicyVolumeName,
		quotaPolicySeedVolumeName,
		leaderSnapshotVolumeName,
	}, names)

	mounts := deploy.Spec.Template.Spec.Containers[0].VolumeMounts
	assert.Len(t, mounts, 2, "the leader mounts the policy it writes and the claim it snapshots to")
}

// TestRenderLeaderDeployment_NoSnapshotRendersNoStorage is the negative half of the three
// assertions above, kept separate because each of them can pass on an object that asked for
// nothing.
func TestRenderLeaderDeployment_NoSnapshotRendersNoStorage(t *testing.T) {
	deploy := RenderLeaderDeployment(haBackend(), "mooncake:v0.3.13")
	podSpec := deploy.Spec.Template.Spec

	assert.Empty(t, podSpec.Volumes)
	assert.Empty(t, podSpec.Containers[0].VolumeMounts)
	for _, env := range podSpec.Containers[0].Env {
		assert.NotEqual(t, LeaderSnapshotLocalPathEnv, env.Name)
	}
}

// TestSnapshotKeysAreReserved names every key the snapshot work adds to the escape hatch's lists,
// one literal per line, and says which list it belongs in.
//
// Listing them individually rather than counting them is the point: a count passes when one key is
// swapped for another, and these two classes refuse for different reasons that an operator reading
// the message has to be able to tell apart. Derived means this API renders the flag; Forbidden
// means nothing collides by name and the key costs more than the hatch is worth anyway.
func TestSnapshotKeysAreReserved(t *testing.T) {
	for _, key := range []string{
		"enable_snapshot",
		"enable_snapshot_restore",
		"snapshot_object_store_type",
		"snapshot_interval_seconds",
		"snapshot_retention_count",
	} {
		assert.Contains(t, LeaderExtraArgsRules.Derived, key,
			"rendered from leader.highAvailability.snapshot, so two sources would be ambiguous")
		assert.NotContains(t, LeaderExtraArgsRules.Forbidden, key,
			"a key cannot be in both lists: the operator would hear only one of the two messages")
	}

	for _, key := range []string{
		// Decides whether snapshots are generated at all, silently, under a name that mentions none
		// of this.
		"memory_allocator",
		// Turns a failed upload from an error into a local copy the store steps over.
		"snapshot_backup_dir",
		// The two deprecated spellings the canonical flag wins over.
		"snapshot_payload_store_type",
		"snapshot_payload_backend_type",
		// The catalog, which this operator renders nothing for because the default puts the index on
		// the same claim as the payloads.
		"snapshot_catalog_store_type",
		"snapshot_catalog_backend_type",
		// Read only under a catalog kind that is itself refused.
		"snapshot_catalog_store_connstring",
		"snapshot_catalog_backend_connstring",
	} {
		assert.Contains(t, LeaderExtraArgsRules.Forbidden, key,
			"reaches the snapshot arrangement without colliding with any flag this API renders")
		assert.NotContains(t, LeaderExtraArgsRules.Derived, key,
			"a key cannot be in both lists: the operator would hear only one of the two messages")
	}
}
