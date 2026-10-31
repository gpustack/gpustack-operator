package v1

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func strings7x64() string { return strings.Repeat("7", 64) }

// TestModelDeploymentRemoteDeliveryPublicStorageConversionRoundTrip pins that the remote
// delivery view converts between this public storage view and the stored API with nothing
// dropped. The aggregated handler proxies every write through these conversion functions, so a
// field that converted lossily would corrupt status on its first read. Absence converts too: a
// legacy object's missing view must round-trip as missing, never as an empty one.
func TestModelDeploymentRemoteDeliveryPublicStorageConversionRoundTrip(t *testing.T) {
	legacy := &ModelDeployment{
		Status: workercore.ModelDeploymentStatus{
			Model: &workercore.ModelDeploymentModelStatus{
				Artifact: "weights",
				Delivery: workercore.ModelDeploymentModelDeliveryNode,
			},
		},
	}
	remote := &ModelDeployment{
		Status: workercore.ModelDeploymentStatus{
			Model: &workercore.ModelDeploymentModelStatus{
				Artifact:       "weights",
				ManifestDigest: "sha256:" + strings7x64(),
				Delivery:       workercore.ModelDeploymentModelDeliveryRemote,
				Remote: &workercore.ModelDeploymentModelRemoteStatus{
					Policy: workercore.ModelDeploymentRemotePolicyRequire,
					Roles: []workercore.ModelDeploymentRemoteRoleStatus{
						{
							Role: "prefill",
							Replicas: []workercore.ModelDeploymentRemoteReplicaStatus{
								{
									Ordinal:  0,
									Delivery: workercore.ModelDeploymentModelDeliveryRemote,
									Load:     workercore.ModelDeploymentRemoteLoadStateLoaded,
								},
								{
									Ordinal:          1,
									Delivery:         workercore.ModelDeploymentModelDeliveryEngine,
									Load:             workercore.ModelDeploymentRemoteLoadStateAwaitingSource,
									Reason:           "over the memory grant",
									FallbackAttempts: 2,
								},
							},
						},
					},
				},
			},
		},
	}

	cases := []struct {
		name   string
		public *ModelDeployment
	}{
		{name: "a deployment delivered the legacy way carries no remote view", public: legacy},
		{name: "the remote view converts whole, mixed delivery included", public: remote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var storage workercore.ModelDeployment
			require.NoError(t, Convert_v1_ModelDeployment_To_v1alpha1_ModelDeployment(tc.public, &storage, nil))

			require.NotNil(t, storage.Status.Model)
			if tc.public.Status.Model.Remote == nil {
				assert.Nil(t, storage.Status.Model.Remote)
			} else {
				require.NotNil(t, storage.Status.Model.Remote)
				assert.Equal(t, workercore.ModelDeploymentRemotePolicyRequire, storage.Status.Model.Remote.Policy)
				require.Len(t, storage.Status.Model.Remote.Roles, 1)
				require.Len(t, storage.Status.Model.Remote.Roles[0].Replicas, 2)
				assert.Equal(t, int32(2), storage.Status.Model.Remote.Roles[0].Replicas[1].FallbackAttempts)
			}

			var roundTrip ModelDeployment
			require.NoError(t, Convert_v1alpha1_ModelDeployment_To_v1_ModelDeployment(&storage, &roundTrip, nil))
			assert.True(t, equality.Semantic.DeepEqual(tc.public, &roundTrip),
				"the public view must survive the storage round trip unchanged")
		})
	}
}

// TestModelPrefetchModelTransferPublicStorageConversionRoundTrip pins that a prefetch's source intent and
// transfer status survive the public storage round trip, and that an ordinary prefetch's absence
// of both stays absence.
func TestModelPrefetchModelTransferPublicStorageConversionRoundTrip(t *testing.T) {
	q := resource.MustParse("16Gi")
	preparing := &ModelPrefetch{
		Spec: workercore.ModelPrefetchSpec{
			ArtifactRef: workercore.ModelPrefetchArtifactReference{Name: "model"},
			BindingRef:  workercore.ModelPrefetchBindingReference{Name: "cache"},
			Source:      &workercore.ModelPrefetchSource{},
		},
		Status: workercore.ModelPrefetchStatus{
			Transfer: &workercore.ModelPrefetchTransferStatus{
				ReservedBytes: 5 << 30,
				Limits: &workercore.ModelPrefetchTransferLimits{
					Bytes:         q,
					Registrations: 4,
					Sessions:      8,
				},
				Generations: []workercore.ModelPrefetchTransferGeneration{
					{
						Generation:         "gen-1",
						ReadyNodes:         2,
						DrainingNodes:      1,
						ResidentBytes:      5 << 30,
						RejectedAdmissions: 1,
					},
				},
			},
		},
	}
	plain := &ModelPrefetch{
		Spec: workercore.ModelPrefetchSpec{
			ArtifactRef: workercore.ModelPrefetchArtifactReference{Name: "model"},
			BindingRef:  workercore.ModelPrefetchBindingReference{Name: "cache"},
		},
	}

	cases := []struct {
		name   string
		public *ModelPrefetch
	}{
		{name: "an ordinary prefetch carries neither intent nor transfer view", public: plain},
		{name: "the source intent and the transfer status convert whole", public: preparing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var storage workercore.ModelPrefetch
			require.NoError(t, Convert_v1_ModelPrefetch_To_v1alpha1_ModelPrefetch(tc.public, &storage, nil))

			var roundTrip ModelPrefetch
			require.NoError(t, Convert_v1alpha1_ModelPrefetch_To_v1_ModelPrefetch(&storage, &roundTrip, nil))
			assert.True(t, equality.Semantic.DeepEqual(tc.public, &roundTrip),
				"the public view must survive the storage round trip unchanged")
		})
	}
}

// TestModelStoreBindingModelTransferPublicStorageConversionRoundTrip pins that a binding's memory grant
// and its reservation survive the public storage round trip, and that no grant round-trips as no
// grant.
func TestModelStoreBindingModelTransferPublicStorageConversionRoundTrip(t *testing.T) {
	q := resource.MustParse("16Gi")
	granted := &ModelStoreBinding{
		Spec: workercore.ModelStoreBindingSpec{
			StoreRefs: []workercore.ModelStoreBindingStoreReference{{Name: "h100"}},
			Quota:     workercore.ModelStoreBindingQuota{Bytes: q},
			SourceMemory: &workercore.ModelStoreBindingSourceMemory{
				Bytes:         q,
				Registrations: 4,
				Sessions:      8,
			},
		},
		Status: workercore.ModelStoreBindingStatus{ReservedMemory: &q},
	}
	ungranted := &ModelStoreBinding{
		Spec: workercore.ModelStoreBindingSpec{
			StoreRefs: []workercore.ModelStoreBindingStoreReference{{Name: "h100"}},
			Quota:     workercore.ModelStoreBindingQuota{Bytes: q},
		},
	}

	cases := []struct {
		name   string
		public *ModelStoreBinding
	}{
		{name: "no grant round-trips as no grant", public: ungranted},
		{name: "the grant and the reservation convert whole", public: granted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var storage workercore.ModelStoreBinding
			require.NoError(t, Convert_v1_ModelStoreBinding_To_v1alpha1_ModelStoreBinding(tc.public, &storage, nil))

			var roundTrip ModelStoreBinding
			require.NoError(t, Convert_v1alpha1_ModelStoreBinding_To_v1_ModelStoreBinding(&storage, &roundTrip, nil))
			assert.True(t, equality.Semantic.DeepEqual(tc.public, &roundTrip),
				"the public view must survive the storage round trip unchanged")
		})
	}
}

// TestNodeModelStoreModelTransferPublicStorageConversionRoundTrip pins that a node's transfer report
// survives the public storage round trip, quarantine state and all.
func TestNodeModelStoreModelTransferPublicStorageConversionRoundTrip(t *testing.T) {
	public := &NodeModelStore{
		Status: workercore.NodeModelStoreStatus{
			ModelTransfer: &workercore.NodeModelStoreModelTransferStatus{
				Generations: []workercore.NodeModelStoreModelTransferGeneration{
					{
						Generation:         "gen-1",
						Digest:             "sha256:" + strings7x64(),
						State:              workercore.NodeModelStoreModelTransferGenerationStateQuarantined,
						ResidentBytes:      5 << 30,
						Registrations:      2,
						RejectedAdmissions: 3,
					},
				},
			},
		},
	}

	var storage workercore.NodeModelStore
	require.NoError(t, Convert_v1_NodeModelStore_To_v1alpha1_NodeModelStore(public, &storage, nil))
	require.NotNil(t, storage.Status.ModelTransfer)
	assert.Equal(t, workercore.NodeModelStoreModelTransferGenerationStateQuarantined,
		storage.Status.ModelTransfer.Generations[0].State)

	var roundTrip NodeModelStore
	require.NoError(t, Convert_v1alpha1_NodeModelStore_To_v1_NodeModelStore(&storage, &roundTrip, nil))
	assert.True(t, equality.Semantic.DeepEqual(public, &roundTrip),
		"the public view must survive the storage round trip unchanged")
}
