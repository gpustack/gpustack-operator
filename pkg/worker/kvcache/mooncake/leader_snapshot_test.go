package mooncake

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// TestRenderLeader_RendersNoSnapshot holds the rendered leader free of every part of the store's
// snapshot: no flag, no volume, no mount and no environment variable.
//
// The fixtures cover each branch that adds to the command line or the pod spec, because a snapshot
// piece added under one of them is invisible from the others: the election, one leader under a
// high-availability block, and the tenant quota policy, which is the one feature that does mount a
// volume.
func TestRenderLeader_RendersNoSnapshot(t *testing.T) {
	cases := []struct {
		name string
		kvcb *workercore.KVCacheBackend
	}{
		{"a plain backend", testBackend()},
		{"a backend electing its leader", haBackend()},
		{"one leader under a high-availability block", testBackend(func(kvcb *workercore.KVCacheBackend) {
			kvcb.Spec.Connection.Managed.Leader.HighAvailability = &workercore.KVCacheBackendLeaderHighAvailability{}
		})},
		{"an elected leader under multi-tenancy", haBackend(func(kvcb *workercore.KVCacheBackend) {
			kvcb.Spec.Connection.Managed.Leader.MultiTenancy = true
		})},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, flag := range RenderLeaderFlags(c.kvcb) {
				assert.NotContains(t, flag, "snapshot", "the leader renders no snapshot flag")
			}

			podSpec := RenderLeaderDeployment(c.kvcb, "mooncake:v0.3.13").Spec.Template.Spec
			for _, v := range podSpec.Volumes {
				assert.NotContains(t, v.Name, "snapshot", "the leader mounts no snapshot storage")
				assert.Nil(t, v.PersistentVolumeClaim, "the leader mounts no claim at all")
			}
			for _, m := range podSpec.Containers[0].VolumeMounts {
				assert.NotContains(t, m.MountPath, "snapshot", "the leader mounts no snapshot directory")
			}
			for _, e := range podSpec.Containers[0].Env {
				assert.NotContains(t, strings.ToUpper(e.Name), "SNAPSHOT",
					"the leader sets no snapshot variable")
			}
		})
	}
}

// TestSnapshotKeysAreForbidden names every snapshot key the leader's escape hatch refuses, one
// literal per line, and the reason each is refused with.
//
// Listing them individually rather than counting them is the point: a count passes when one key is
// swapped for another. The two switches carry the wrong-data reason because they are what would turn
// the snapshot on; every other key is read only under one of them, and its message says so.
func TestSnapshotKeysAreForbidden(t *testing.T) {
	cases := []struct {
		key    string
		reason string
	}{
		{"enable_snapshot", snapshotKeyReason},
		{"enable_snapshot_restore", snapshotKeyReason},
		{"snapshot_interval_seconds", snapshotCompanionKeyReason},
		{"snapshot_retention_count", snapshotCompanionKeyReason},
		{"snapshot_object_store_type", snapshotCompanionKeyReason},
		{"snapshot_payload_store_type", snapshotCompanionKeyReason},
		{"snapshot_payload_backend_type", snapshotCompanionKeyReason},
		{"snapshot_backup_dir", snapshotCompanionKeyReason},
		{"snapshot_catalog_store_type", snapshotCompanionKeyReason},
		{"snapshot_catalog_backend_type", snapshotCompanionKeyReason},
		{"snapshot_catalog_store_connstring", snapshotCompanionKeyReason},
		{"snapshot_catalog_backend_connstring", snapshotCompanionKeyReason},
	}

	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			assert.Equal(t, c.reason, LeaderExtraArgsRules.Forbidden[c.key])
			assert.NotContains(t, LeaderExtraArgsRules.Derived, c.key,
				"nothing renders this key, so the Derived message would be untrue")
		})
	}
}
