package v1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// TestDevicesPublicProxyCarriesSelectionStatus checks each conversion direction independently.
func TestDevicesPublicProxyCarriesSelectionStatus(t *testing.T) {
	resolvedAt := meta.NewTime(time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC))

	cases := []struct {
		name   string
		status workercore.DevicesStatus
	}{
		{
			name:   "unreported status stays unreported",
			status: workercore.DevicesStatus{},
		},
		{
			name: "a resolved selection carries its reference and condition",
			status: workercore.DevicesStatus{
				Groups: []workercore.DevicesAllocationGroup{
					{
						ID:           "group-a",
						Manufacturer: "NVIDIA",
						Accelerators: []workercore.AcceleratorAllocation{
							{ID: "acc-0", Index: 0, Mode: workercore.DeviceAllocationModeExclusive, Allocated: 1},
							{ID: "acc-1", Index: 1, Mode: workercore.DeviceAllocationModeShared, Allocated: 4, Remaining: 4},
						},
					},
				},
				Interfaces: &workercore.DevicesInterfacesStatus{
					RDMASelectionPolicy: &workercore.DevicesRDMASelectionPolicyStatus{
						PolicyRef: policyRef("compute-hcas", "uid-a", 3),
						Excluded:  []workercore.DevicesRDMAExclusion{exclusion("mlx5_1", "ExcludedByPolicy")},
					},
				},
				Conditions: []gpustack.Condition{
					selectionCondition("RDMASelectionResolved", meta.ConditionTrue, "PolicyResolved",
						"the RDMA selection resolves from policy compute-hcas", 5, resolvedAt),
				},
			},
		},
		{
			name: "same name with another uid is another policy",
			status: workercore.DevicesStatus{
				Interfaces: &workercore.DevicesInterfacesStatus{
					RDMASelectionPolicy: &workercore.DevicesRDMASelectionPolicyStatus{
						PolicyRef: policyRef("compute-hcas", "uid-b", 1),
					},
				},
			},
		},
		{
			name: "a failed resolution clears the reference and carries the cause",
			status: workercore.DevicesStatus{
				Conditions: []gpustack.Condition{
					selectionCondition("RDMASelectionResolved", meta.ConditionFalse, "PolicyConflict",
						"two policies claim the node", 6, resolvedAt),
				},
			},
		},
		{
			name: "resolved default is told by the condition only",
			status: workercore.DevicesStatus{
				Conditions: []gpustack.Condition{
					selectionCondition("RDMASelectionResolved", meta.ConditionTrue, "NoPolicy",
						"no policy targets the node", 4, resolvedAt),
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Run("public to internal", func(t *testing.T) {
				public := &Devices{Status: c.status}
				internal := &workercore.Devices{}
				require.NoError(t, Convert_v1_Devices_To_v1alpha1_Devices(public, internal, nil))
				assertDevicesStatusRoundtrip(t, c.status, internal.Status)
			})
			t.Run("internal to public", func(t *testing.T) {
				internal := &workercore.Devices{Status: c.status}
				public := &Devices{}
				require.NoError(t, Convert_v1alpha1_Devices_To_v1_Devices(internal, public, nil))
				assertDevicesStatusRoundtrip(t, c.status, public.Status)
			})
		})
	}
}

func policyRef(name, uid string, generation int64) *workercore.DevicesRDMASelectionPolicyReference {
	return &workercore.DevicesRDMASelectionPolicyReference{Name: name, UID: uid, Generation: generation}
}

func exclusion(name, reason string) workercore.DevicesRDMAExclusion {
	return workercore.DevicesRDMAExclusion{Name: name, Reason: reason}
}

func selectionCondition(conditionType string, status meta.ConditionStatus, reason, message string, observedGeneration int64, at meta.Time) gpustack.Condition {
	return gpustack.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
		LastTransitionTime: at,
	}
}

// Conditions are independent of optional interfaces; nil and empty lists are equivalent.
func assertDevicesStatusRoundtrip(t *testing.T, want, got workercore.DevicesStatus) {
	t.Helper()

	if want.Interfaces == nil {
		assert.Nil(t, got.Interfaces, "absent interfaces must not materialize")
	} else {
		require.NotNil(t, got.Interfaces, "interfaces must survive the proxy")
		assertSelectionPolicy(t, want.Interfaces.RDMASelectionPolicy, got.Interfaces.RDMASelectionPolicy)
	}

	require.Len(t, got.Conditions, len(want.Conditions),
		"conditions must roundtrip one-for-one; an absent condition is unreported, never success")
	for _, wantCondition := range want.Conditions {
		gotCondition, found := conditionByType(got.Conditions, wantCondition.Type)
		require.True(t, found, "condition %s must survive the proxy", wantCondition.Type)
		assert.Equal(t, wantCondition.Status, gotCondition.Status, "condition %s status", wantCondition.Type)
		assert.Equal(t, wantCondition.Reason, gotCondition.Reason, "condition %s reason", wantCondition.Type)
		assert.Equal(t, wantCondition.Message, gotCondition.Message, "condition %s message", wantCondition.Type)
		assert.Equal(t, wantCondition.ObservedGeneration, gotCondition.ObservedGeneration,
			"condition %s observedGeneration", wantCondition.Type)
		assert.True(t, wantCondition.LastTransitionTime.Time.Equal(gotCondition.LastTransitionTime.Time),
			"condition %s lastTransitionTime must roundtrip to the same instant", wantCondition.Type)
	}

	require.Len(t, got.Groups, len(want.Groups), "groups must roundtrip one-for-one")
	for i := range want.Groups {
		assert.Equal(t, want.Groups[i].ID, got.Groups[i].ID, "group %d id", i)
		assert.Equal(t, want.Groups[i].Manufacturer, got.Groups[i].Manufacturer, "group %d manufacturer", i)
		require.Len(t, got.Groups[i].Accelerators, len(want.Groups[i].Accelerators), "group %d accelerators", i)
		for j := range want.Groups[i].Accelerators {
			assert.Equal(t, want.Groups[i].Accelerators[j], got.Groups[i].Accelerators[j], "group %d accelerator %d", i, j)
		}
	}
}

func assertSelectionPolicy(t *testing.T, want, got *workercore.DevicesRDMASelectionPolicyStatus) {
	t.Helper()

	if want == nil {
		assert.Nil(t, got, "absent selection policy must not materialize")
		return
	}
	require.NotNil(t, got, "selection policy must survive the proxy")

	assertSelectionRef(t, "policyRef", want.PolicyRef, got.PolicyRef)

	require.Len(t, got.Excluded, len(want.Excluded),
		"exclusions must roundtrip one-for-one; an empty list never implies a resolved selection")
	for i := range want.Excluded {
		assert.Equal(t, want.Excluded[i].Name, got.Excluded[i].Name, "exclusion %d name", i)
		assert.Equal(t, want.Excluded[i].Reason, got.Excluded[i].Reason, "exclusion %d reason", i)
	}
}

func assertSelectionRef(t *testing.T, field string, want, got *workercore.DevicesRDMASelectionPolicyReference) {
	t.Helper()

	if want == nil {
		assert.Nil(t, got, "%s must stay absent", field)
		return
	}
	require.NotNil(t, got, "%s must survive the proxy", field)
	assert.Equal(t, want.Name, got.Name, "%s name", field)
	assert.Equal(t, want.UID, got.UID, "%s uid", field)
	assert.Equal(t, want.Generation, got.Generation, "%s generation", field)
}

func conditionByType(conditions []gpustack.Condition, conditionType string) (gpustack.Condition, bool) {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return gpustack.Condition{}, false
}
