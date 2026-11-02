// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	field "k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlapiutil "sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlintercept "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmgr "sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	apiv1 "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/utils/json"
)

// selectionNodeName is the node every observer fixture reconciles.
const selectionNodeName = "node-observe"

// selectionNodeLabels is the label set the fixture policies select on.
var selectionNodeLabels = map[string]string{"rdma.example.com/pool": "compute"}

// selectionInventory returns a complete local verbs observation: three managed HCAs and one
// externally owned device, the smallest inventory that distinguishes default, policy, invalid
// and conflict resolutions from one another.
func selectionInventory() *device.RDMASnapshot {
	return &device.RDMASnapshot{
		Complete: true,
		Revision: "rev-1",
		Devices: []device.RDMADevice{
			{Name: "mlx5_0", Owner: device.RDMADeviceOwnerGPUStack},
			{Name: "mlx5_4", Owner: device.RDMADeviceOwnerGPUStack},
			{Name: "mlx5_5", Owner: device.RDMADeviceOwnerGPUStack},
			{Name: "efa0", Owner: device.RDMADeviceOwnerExternal, Driver: "efa"},
		},
	}
}

// selectionNode returns the local Node the resolver's selector matches against.
func selectionNode() *core.Node {
	return &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name:   selectionNodeName,
			UID:    "node-uid-1",
			Labels: selectionNodeLabels,
		},
	}
}

// selectionPolicy builds one policy targeting the fixture node.
func selectionPolicy(
	name, uid string, generation int64, mutate func(*workercore.DevicesRDMASelectionPolicy),
) *workercore.DevicesRDMASelectionPolicy {
	p := &workercore.DevicesRDMASelectionPolicy{
		ObjectMeta: meta.ObjectMeta{Name: name, UID: types.UID(uid), Generation: generation},
		Spec: workercore.DevicesRDMASelectionPolicySpec{
			NodeSelector: &meta.LabelSelector{MatchLabels: selectionNodeLabels},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

// observationDevices returns this node's Devices with a generation, so observedGeneration
// pins against a real value.
func observationDevices() *workercore.Devices {
	return &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: selectionNodeName, Generation: 7},
	}
}

// foreignGroups is the accelerator ledger a concurrent writer owns on the same status.
func foreignGroups() []workercore.DevicesAllocationGroup {
	return []workercore.DevicesAllocationGroup{{
		ID:           "grp-0",
		Manufacturer: "nvidia",
		Accelerators: []workercore.AcceleratorAllocation{{
			ID:        "acc-0",
			Mode:      workercore.DeviceAllocationModeShared,
			Remaining: 8,
		}},
	}}
}

// foreignState is the status a concurrent writer owns on the same object: the accelerator
// ledger and a foreign condition of a type this observer does not own. The selection
// section it also seeds — a reference and an exclusion — is this writer's own stale state
// from an earlier pass, which the observer replaces per its fail-closed contract rather
// than preserves.
func foreignState() (workercore.DevicesInterfacesStatus, []apiv1.Condition) {
	transition := meta.NewTime(time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC))
	ifaces := workercore.DevicesInterfacesStatus{
		RDMASelectionPolicy: &workercore.DevicesRDMASelectionPolicyStatus{
			PolicyRef: &workercore.DevicesRDMASelectionPolicyReference{
				Name: "edge-select", UID: "uid-applied", Generation: 2,
			},
			Excluded: []workercore.DevicesRDMAExclusion{
				{Name: "mlx5_5", Reason: "ExcludedByPolicy"},
			},
		},
	}
	conditions := []apiv1.Condition{
		{
			Type:               "MetricsReported",
			Status:             meta.ConditionTrue,
			Reason:             "Reported",
			Message:            "metrics exporter reported",
			ObservedGeneration: 6,
			LastTransitionTime: transition,
		},
	}
	return ifaces, conditions
}

// seedForeignState writes the foreign state onto a Devices status.
func seedForeignState(devs *workercore.Devices) {
	ifaces, conditions := foreignState()
	devs.Status.Interfaces = &ifaces
	devs.Status.Conditions = conditions
	devs.Status.Groups = foreignGroups()
}

// observerClientOptions shapes the failures one observer test client simulates.
type observerClientOptions struct {
	listErr         error // the cached policy List
	nodeGetErr      error // the local Node read
	devicesGetErr   error // the Devices read
	pruneInterfaces bool  // drop status.interfaces after every status patch, as an old CRD does
	// rejectOversizedConditionMessage makes the status writer validate the patched object
	// against the message bound the generated Devices CRD carries on
	// status.conditions[].message, the way a validating API server does, and reject the
	// whole patch with an Invalid error when a condition message exceeds it.
	rejectOversizedConditionMessage bool
}

// newSelectionObserverClient builds the shared fake the observer's Client and APIReader both
// point at: the tracker keeps resource versions and patch application, the spy captures every
// status write, and the options shape the read failures. They are held by pointer, so a test
// can flip a failure off mid-test and watch recovery land through the same client.
func newSelectionObserverClient(
	t *testing.T, spy *ledgerWriteSpy, opts *observerClientOptions, objs ...ctrlcli.Object,
) ctrlcli.Client {
	t.Helper()
	bound := devicesConditionMessageBound(t)
	tracker := k8stesting.NewObjectTracker(
		scheme.Scheme, serializer.NewCodecFactory(scheme.Scheme).UniversalDecoder(),
	)
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjectTracker(tracker).
		WithStatusSubresource(&workercore.Devices{}).
		WithObjects(objs...).
		WithInterceptorFuncs(ctrlintercept.Funcs{
			List: func(
				ctx context.Context, wrapped ctrlcli.WithWatch, list ctrlcli.ObjectList,
				listOpts ...ctrlcli.ListOption,
			) error {
				if _, ok := list.(*workercore.DevicesRDMASelectionPolicyList); ok && opts.listErr != nil {
					return opts.listErr
				}
				return wrapped.List(ctx, list, listOpts...)
			},
			Get: func(
				ctx context.Context, wrapped ctrlcli.WithWatch, key ctrlcli.ObjectKey,
				obj ctrlcli.Object, getOpts ...ctrlcli.GetOption,
			) error {
				if opts.nodeGetErr != nil {
					if _, ok := obj.(*core.Node); ok {
						return opts.nodeGetErr
					}
				}
				if opts.devicesGetErr != nil {
					if _, ok := obj.(*workercore.Devices); ok {
						return opts.devicesGetErr
					}
				}
				return wrapped.Get(ctx, key, obj, getOpts...)
			},
			SubResourcePatch: func(
				ctx context.Context, wrapped ctrlcli.Client, sub string, obj ctrlcli.Object,
				patch ctrlcli.Patch, popts ...ctrlcli.SubResourcePatchOption,
			) error {
				spy.writes++
				spy.patches++
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				spy.lastPatch = data
				spy.lastPatchType = patch.Type()
				if opts.rejectOversizedConditionMessage {
					// A validating API server rejects the whole patch before it applies
					// anything when any condition message on the result exceeds the schema
					// bound; the stale preceding condition stays displayed.
					if errs := oversizedConditionMessageErrors(obj, bound); len(errs) > 0 {
						return apierrors.NewInvalid(
							schema.GroupKind{Group: workercore.GroupName, Kind: "Devices"},
							obj.GetName(), errs,
						)
					}
				}
				if spy.failWrite {
					return errors.New("the api server is unreachable")
				}
				if spy.raceForeign != nil {
					spy.raceForeign(ctx, wrapped)
				}
				if err := wrapped.SubResource(sub).Patch(ctx, obj, patch, popts...); err != nil {
					return err
				}
				if opts.pruneInterfaces {
					live := new(workercore.Devices)
					if err := wrapped.Get(ctx, ctrlcli.ObjectKey{Name: obj.GetName()}, live); err != nil {
						return err
					}
					live.Status.Interfaces = nil
					return wrapped.Status().Update(ctx, live)
				}
				return nil
			},
		}).
		Build()
	return cli
}

// newSelectionObserver builds an observer over the shared fake, its inventory source pointing
// at the given snapshot, so a reconcile runs the production read-resolve-publish path against
// the fixture store.
func newSelectionObserver(
	t *testing.T, inventory *device.RDMASnapshot, opts *observerClientOptions, objs ...ctrlcli.Object,
) (*RDMASelectionObserver, *ledgerWriteSpy) {
	t.Helper()
	spy := new(ledgerWriteSpy)
	cli := newSelectionObserverClient(t, spy, opts, objs...)
	obs := &RDMASelectionObserver{
		NodeName:  selectionNodeName,
		Client:    cli,
		APIReader: cli,
	}
	if inventory != nil {
		obs.RDMAInterfaceInventory = func() *device.RDMASnapshot { return inventory }
	}
	return obs, spy
}

// devicesConditionMessageBound reads the bound the generated Devices CRD carries on
// status.conditions[].message — the schema a real API server validates the observer's
// status patch against, at the same path the production limit is pinned by.
func devicesConditionMessageBound(t *testing.T) int64 {
	t.Helper()
	crd := workercore.GetCustomResourceDefinitions()["Devices"]
	require.NotNil(t, crd, "the Devices custom resource definition is not registered")
	require.NotEmpty(t, crd.Spec.Versions, "the Devices custom resource definition has no versions")
	conditions := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.
		Properties["status"].Properties["conditions"]
	require.NotNil(t, conditions.Items, "the status.conditions schema has no items")
	require.NotNil(t, conditions.Items.Schema, "the status.conditions schema has no item schema")
	message := conditions.Items.Schema.Properties["message"]
	require.NotNil(t, message.MaxLength,
		"the generated schema carries no MaxLength at status.conditions[].message")
	return *message.MaxLength
}

// oversizedConditionMessageErrors returns one field error for every condition message on
// the patched object longer than the schema bound — the rejection a validating API server
// answers the whole patch with, before it applies anything.
func oversizedConditionMessageErrors(obj ctrlcli.Object, bound int64) field.ErrorList {
	devs, ok := obj.(*workercore.Devices)
	if !ok {
		return nil
	}
	var errs field.ErrorList
	for i := range devs.Status.Conditions {
		length := int64(utf8.RuneCountInString(devs.Status.Conditions[i].Message))
		if length <= bound {
			continue
		}
		errs = append(errs, field.Invalid(
			field.NewPath("status").Child("conditions").Index(i).Child("message"),
			fmt.Sprintf("<%d characters>", length),
			fmt.Sprintf("should be at most %d characters long", bound),
		))
	}
	return errs
}

// oversizedUnknownIncludePolicy builds one policy whose include names 10,000 devices the
// complete fixture inventory does not know: the reproduced report whose resolver message
// exceeds the condition schema bound.
func oversizedUnknownIncludePolicy() *workercore.DevicesRDMASelectionPolicy {
	const unknownNames = 10000
	names := make([]string, 0, unknownNames)
	for i := 0; i < unknownNames; i++ {
		names = append(names, fmt.Sprintf("hca-%05d", i))
	}
	return selectionPolicy("huge-include", "uid-huge", 3,
		func(p *workercore.DevicesRDMASelectionPolicy) { p.Spec.Include = names })
}

// exactlyAtBoundUnknownIncludePolicy builds one policy whose invalid report renders to
// exactly the schema bound: the resolver's 43-rune prefix plus a bracketed list of 3,636
// eight-rune names and their separators — 3636 x 9 + 1 = 32,725 code points — totals
// 32,768, the MaxLength the generated Devices CRD carries on status.conditions[].message.
// The row built on it pins that an exactly-at-bound message passes the validating writer
// unmarked and uncut; a resolver wording change moves the length and fails the fixture
// check first.
func exactlyAtBoundUnknownIncludePolicy() *workercore.DevicesRDMASelectionPolicy {
	const unknownNames = 3636
	names := make([]string, 0, unknownNames)
	for i := 0; i < unknownNames; i++ {
		names = append(names, fmt.Sprintf("hca-%04d", i))
	}
	return selectionPolicy("exact-include", "uid-exact", 3,
		func(p *workercore.DevicesRDMASelectionPolicy) { p.Spec.Include = names })
}

// atBoundResolution resolves one policy through the production resolver the way the
// observer's pass does, so a test can compare the published message against the exact
// diagnostic the resolver produced.
func atBoundResolution(
	policy *workercore.DevicesRDMASelectionPolicy,
) RDMAPolicyResolution {
	return ResolveRDMAPolicy(RDMAPolicyInputs{
		Synced:    true,
		Node:      RDMAPolicyNodeInput{Name: selectionNodeName, Labels: selectionNodeLabels},
		Policies:  []workercore.DevicesRDMASelectionPolicy{*policy},
		Inventory: selectionInventory(),
	})
}

// reconcileSelection runs one reconcile pass over the node's Devices and reports the result.
func reconcileSelection(
	t *testing.T, obs *RDMASelectionObserver,
) (ctrl.Result, error) {
	t.Helper()
	return obs.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: selectionNodeName},
	})
}

// observationStoredDevices reads the node's Devices back from the store the writer shares.
func observationStoredDevices(t *testing.T, obs *RDMASelectionObserver) *workercore.Devices {
	t.Helper()
	got := new(workercore.Devices)
	require.NoError(t, obs.Client.Get(
		context.Background(), ctrlcli.ObjectKey{Name: selectionNodeName}, got,
	))
	return got
}

// observerCondition finds one condition of a Devices status, or nil when absent.
func observerCondition(
	conditions []apiv1.Condition, conditionType string,
) *apiv1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// assertResolvedCondition pins one resolved condition's content: everything the writer owns
// is exact, the message names the given cause, and the instant it stamped is present,
// wherever the codec renders it.
func assertResolvedCondition(t *testing.T, have *apiv1.Condition, want apiv1.Condition) {
	t.Helper()
	require.NotNil(t, have, "the resolved condition must be present")
	assert.Equal(t, RDMASelectionResolvedConditionType, have.Type)
	assert.Equal(t, want.Status, have.Status)
	assert.Equal(t, want.Reason, have.Reason)
	assert.Equal(t, want.ObservedGeneration, have.ObservedGeneration)
	assert.False(t, have.LastTransitionTime.IsZero(),
		"a published condition carries its transition instant")
	assert.Contains(t, have.Message, want.Message,
		"the condition message must carry this resolution's cause")
}

// assertForeignConditionSurvived compares the foreign condition and accelerator state
// semantically, the part of the foreign state that no pruning store can drop.
func assertForeignConditionSurvived(t *testing.T, got *workercore.DevicesStatus) {
	t.Helper()
	_, wantConditions := foreignState()
	foreign := observerCondition(got.Conditions, "MetricsReported")
	require.NotNil(t, foreign, "the foreign condition must survive")
	assert.Equal(t, wantConditions[0].Status, foreign.Status)
	assert.Equal(t, wantConditions[0].Reason, foreign.Reason)
	assert.Equal(t, wantConditions[0].Message, foreign.Message)
	assert.True(t, wantConditions[0].LastTransitionTime.Time.Equal(foreign.LastTransitionTime.Time),
		"the foreign condition reports a different instant")
	assert.True(t, kubemeta.DeepEqual(foreignGroups(), got.Groups),
		"the accelerator groups changed")
}

// assertUnknownObservation pins the failure vocabulary: the resolved condition reads
// Unknown/ResolutionUnknown, the stale reference and exclusions are withdrawn rather than
// kept, and the failure cause is named in the message.
func assertUnknownObservation(t *testing.T, obs *RDMASelectionObserver, msgHas string) {
	t.Helper()
	got := observationStoredDevices(t, obs)
	require.NotNil(t, got.Status.Interfaces)
	require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy)
	assert.Nil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef,
		"a failed read withdraws the policy reference instead of keeping or defaulting it")
	assert.Empty(t, got.Status.Interfaces.RDMASelectionPolicy.Excluded,
		"a failed read clears the stored exclusions instead of keeping them stale")
	assertResolvedCondition(t,
		observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType),
		apiv1.Condition{
			Status:             meta.ConditionUnknown,
			Reason:             RDMASelectionUnknownReason,
			ObservedGeneration: 7,
			Message:            msgHas,
		})
	assertForeignConditionSurvived(t, &got.Status)
}

// assertOwnedPatch opens the captured write and checks the transport contract the observer
// must keep: exactly one merge patch, conditioned on the resource version the observation
// was read at, whose status touches only its owned fields — the selection section beneath
// interfaces.rdmaSelectionPolicy, and the resolved condition in the conditions list.
func assertOwnedPatch(
	t *testing.T, spy *ledgerWriteSpy, rvAtRead string,
) map[string]json.RawMessage {
	t.Helper()
	assert.Equal(t, 1, spy.patches, "one observation change is one status patch")
	assert.Equal(t, types.MergePatchType, spy.lastPatchType,
		"the observation is published as a merge patch, so it cannot carry foreign fields")
	patchFields := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal(spy.lastPatch, &patchFields))
	require.Len(t, patchFields, 2,
		"the patch carries its precondition and its status fields, and nothing else")
	require.Contains(t, patchFields, "metadata")
	require.Contains(t, patchFields, "status")
	patchMeta := map[string]any{}
	require.NoError(t, json.Unmarshal(patchFields["metadata"], &patchMeta))
	assert.Equal(t, rvAtRead, patchMeta["resourceVersion"],
		"the precondition is the resource version the observation was read at")
	patchStatus := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal(patchFields["status"], &patchStatus))
	require.NotEmpty(t, patchStatus, "a patch that carries no status change is a churn bug")
	for key := range patchStatus {
		assert.Contains(t, []string{"interfaces", "conditions"}, key,
			"the observer owns interfaces.rdmaSelectionPolicy and the resolved condition alone, got %q", key)
	}
	if raw, ok := patchStatus["interfaces"]; ok {
		ifaces := map[string]json.RawMessage{}
		require.NoError(t, json.Unmarshal(raw, &ifaces))
		require.Contains(t, ifaces, "rdmaSelectionPolicy")
		selection := map[string]json.RawMessage{}
		require.NoError(t, json.Unmarshal(ifaces["rdmaSelectionPolicy"], &selection))
		require.NotEmpty(t, selection)
		for key := range selection {
			assert.Contains(t, []string{"policyRef", "excluded"}, key,
				"policyRef and excluded are the only selection keys this writer owns; the patch carried %q", key)
		}
	}
	if raw, ok := patchStatus["conditions"]; ok {
		var conditions []apiv1.Condition
		require.NoError(t, json.Unmarshal(raw, &conditions))
		require.NotNil(t, observerCondition(conditions, RDMASelectionResolvedConditionType),
			"a moved conditions list must carry the resolved condition")
	}
	return patchStatus
}

// TestRDMASelectionObserver_PublishesResolutionTable pins the resolver-status to wire mapping:
// every resolution publishes its status, reason, message and desired reference, and only its
// desired reference — the applied state, the exclusions and the accelerator groups are foreign
// and ride through every publish unchanged.
func TestRDMASelectionObserver_PublishesResolutionTable(t *testing.T) {
	computeRef := func() *workercore.DevicesRDMASelectionPolicyReference {
		return &workercore.DevicesRDMASelectionPolicyReference{
			Name: "compute-hcas", UID: "uid-a", Generation: 3,
		}
	}

	tests := []struct {
		name      string
		inventory func() *device.RDMASnapshot
		policies  []*workercore.DevicesRDMASelectionPolicy
		want      apiv1.Condition
		wantRef   *workercore.DevicesRDMASelectionPolicyReference
	}{
		{
			name:      "no policy resolves the default with no desired reference",
			inventory: selectionInventory,
			want: apiv1.Condition{
				Status: meta.ConditionTrue, Reason: RDMASelectionNoPolicyReason,
				Message: "no RDMA selection policy targets this node", ObservedGeneration: 7,
			},
		},
		{
			name:      "one targeting policy publishes its reference",
			inventory: selectionInventory,
			policies: []*workercore.DevicesRDMASelectionPolicy{
				selectionPolicy("compute-hcas", "uid-a", 3, func(p *workercore.DevicesRDMASelectionPolicy) {
					p.Spec.Exclude = []string{"mlx5_5"}
				}),
			},
			want: apiv1.Condition{
				Status: meta.ConditionTrue, Reason: RDMASelectionPolicyResolvedReason,
				Message: "the RDMA selection resolves from policy compute-hcas", ObservedGeneration: 7,
			},
			wantRef: computeRef(),
		},
		{
			name:      "an invalid rule reports false with the resolver reference",
			inventory: selectionInventory,
			policies: []*workercore.DevicesRDMASelectionPolicy{
				selectionPolicy("compute-hcas", "uid-a", 3, func(p *workercore.DevicesRDMASelectionPolicy) {
					p.Spec.Include = []string{"mlx5_9"}
				}),
			},
			want: apiv1.Condition{
				Status: meta.ConditionFalse, Reason: RDMASelectionPolicyInvalidReason,
				Message: "mlx5_9", ObservedGeneration: 7,
			},
			wantRef: computeRef(),
		},
		{
			name:      "two targeting policies conflict with no reference",
			inventory: selectionInventory,
			policies: []*workercore.DevicesRDMASelectionPolicy{
				selectionPolicy("compute-hcas", "uid-a", 3, nil),
				selectionPolicy("edge-select", "uid-b", 1, nil),
			},
			want: apiv1.Condition{
				Status: meta.ConditionFalse, Reason: RDMASelectionPolicyConflictReason,
				Message: "merge or priority", ObservedGeneration: 7,
			},
		},
		{
			name: "an incomplete inventory is unknown, never a default",
			inventory: func() *device.RDMASnapshot {
				snap := selectionInventory()
				snap.Complete = false
				snap.Message = "the verbs read failed"
				return snap
			},
			want: apiv1.Condition{
				Status: meta.ConditionUnknown, Reason: RDMASelectionUnknownReason,
				Message: "the verbs inventory is missing or incomplete", ObservedGeneration: 7,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			devs := observationDevices()
			seedForeignState(devs)
			objs := []ctrlcli.Object{devs, selectionNode()}
			for i := range tc.policies {
				objs = append(objs, tc.policies[i])
			}
			obs, spy := newSelectionObserver(t, tc.inventory(), &observerClientOptions{}, objs...)
			rvAtRead := observationStoredDevices(t, obs).ResourceVersion

			result, err := reconcileSelection(t, obs)
			require.NoError(t, err)
			assert.Equal(t, rdmaSelectionResyncPeriod, result.RequeueAfter,
				"every settled pass arms the bounded polling interval")

			got := observationStoredDevices(t, obs)
			require.NotNil(t, got.Status.Interfaces)
			require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy)
			haveRef := got.Status.Interfaces.RDMASelectionPolicy.PolicyRef
			if tc.wantRef == nil {
				assert.Nil(t, haveRef, "this resolution publishes no desired reference")
			} else {
				require.NotNil(t, haveRef)
				assert.True(t, kubemeta.DeepEqual(tc.wantRef, haveRef),
					"the published reference must be the resolver's, verbatim")
			}
			assertResolvedCondition(t,
				observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType),
				tc.want)
			assertForeignConditionSurvived(t, &got.Status)

			assertOwnedPatch(t, spy, rvAtRead)
		})
	}
}

// TestRDMASelectionObserver_FailureIsUnknownNeverDefault pins the read-failure vocabulary:
// every failed read publishes Unknown with the desired reference withdrawn — never the
// no-policy default — while a matching policy sits in the store a broken reader would have
// answered the default from. Every row settles without an error, so the polling interval
// stays armed and a later successful read recovers without a restart.
func TestRDMASelectionObserver_FailureIsUnknownNeverDefault(t *testing.T) {
	notFoundList := apierrors.NewNotFound(
		schema.GroupResource{Group: "worker.gpustack.ai", Resource: "devicesrdmaselectionpolicies"}, "",
	)
	noKindMatch := &apimeta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: "worker.gpustack.ai", Kind: "DevicesRDMASelectionPolicy"},
	}
	unreachable := errors.New("the api server is unreachable")

	tests := []struct {
		name    string
		opts    observerClientOptions
		seed    func(objs []ctrlcli.Object) []ctrlcli.Object
		wantHas string
	}{
		{
			name:    "a missing policy CRD is unknown",
			opts:    observerClientOptions{listErr: notFoundList},
			wantHas: "devicesrdmaselectionpolicies",
		},
		{
			name:    "a failed discovery is unknown",
			opts:    observerClientOptions{listErr: noKindMatch},
			wantHas: "no match",
		},
		{
			name: "a forbidden list is unknown",
			opts: observerClientOptions{listErr: apierrors.NewForbidden(
				schema.GroupResource{Group: "worker.gpustack.ai", Resource: "devicesrdmaselectionpolicies"},
				"", errors.New("denied"),
			)},
			wantHas: "forbidden",
		},
		{
			name:    "an api error is unknown",
			opts:    observerClientOptions{listErr: unreachable},
			wantHas: "api server is unreachable",
		},
		{
			name:    "a failed node read is unknown",
			opts:    observerClientOptions{nodeGetErr: unreachable},
			wantHas: "api server is unreachable",
		},
		{
			name: "a missing node is unknown",
			seed: func(objs []ctrlcli.Object) []ctrlcli.Object {
				kept := make([]ctrlcli.Object, 0, len(objs))
				for i := range objs {
					if _, ok := objs[i].(*core.Node); !ok {
						kept = append(kept, objs[i])
					}
				}
				return kept
			},
			wantHas: "not found",
		},
		{
			name:    "an unwired inventory is unknown",
			seed:    nil, // the runner passes a nil inventory source for this row
			wantHas: "inventory is missing or incomplete",
		},
		{
			name:    "an incomplete inventory is unknown",
			seed:    nil, // the runner passes an incomplete snapshot for this row
			wantHas: "inventory is missing or incomplete",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The matching policy must be in the store: an implementation that maps a failed
			// read to an empty policy list answers this store with the default, which is
			// exactly the false success this table exists to catch.
			devs := observationDevices()
			seedForeignState(devs)
			objs := []ctrlcli.Object{devs, selectionNode(), selectionPolicy("compute-hcas", "uid-a", 3, nil)}

			inventory := selectionInventory()
			if tc.name == "an unwired inventory is unknown" {
				inventory = nil
			}
			if tc.name == "an incomplete inventory is unknown" {
				inventory = &device.RDMASnapshot{Complete: false, Message: "the verbs read failed"}
			}
			if tc.seed != nil {
				objs = tc.seed(objs)
			}

			obs, spy := newSelectionObserver(t, inventory, &tc.opts, objs...)

			result, err := reconcileSelection(t, obs)
			require.NoError(t, err, "a failed read must settle the observation, not fail the pass")
			assert.Equal(t, rdmaSelectionResyncPeriod, result.RequeueAfter,
				"the polling interval stays armed across failed reads, so recovery is bounded")

			assertUnknownObservation(t, obs, tc.wantHas)
			assert.Positive(t, spy.patches, "the first failed observation replaces the stale state")
		})
	}

	t.Run("a later successful read recovers without a restart", func(t *testing.T) {
		devs := observationDevices()
		seedForeignState(devs)
		policy := selectionPolicy("compute-hcas", "uid-a", 3, nil)
		opts := observerClientOptions{listErr: unreachable}
		obs, _ := newSelectionObserver(t, selectionInventory(), &opts, devs, selectionNode(), policy)

		_, err := reconcileSelection(t, obs)
		require.NoError(t, err)
		got := observationStoredDevices(t, obs)
		unknownCondition := observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType)
		require.NotNil(t, unknownCondition)
		assert.Equal(t, meta.ConditionUnknown, unknownCondition.Status,
			"the failed read publishes unknown first")

		// The very same observer, no process restart: the next read succeeds and the truth
		// the store already held is published over the unknown.
		opts.listErr = nil
		_, err = reconcileSelection(t, obs)
		require.NoError(t, err)
		got = observationStoredDevices(t, obs)
		require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
		assert.Equal(t, "uid-a", got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.UID)
		have := observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType)
		require.NotNil(t, have)
		assert.Equal(t, meta.ConditionTrue, have.Status)
		assert.Equal(t, RDMASelectionPolicyResolvedReason, have.Reason)
	})
}

// TestRDMASelectionObserver_ConditionMessageStaysWithinTheSchemaBound pins the message
// bound at the production reconcile/status boundary. The resolver's diagnostics are
// unbounded — one invalid policy can name tens of thousands of unknown devices — and the
// generated Devices CRD caps status.conditions[].message, so a message past that bound
// makes a validating API server reject the whole status patch, which returns the pass as
// an error and leaves the preceding condition, for example a stale resolved True,
// displayed. The bound here is read from the generated schema itself, not restated, the
// oversized fixtures genuinely exceed it before the observer bounds them, and every
// publish keeps the outcome, reason, reference and foreign fields intact.
func TestRDMASelectionObserver_ConditionMessageStaysWithinTheSchemaBound(t *testing.T) {
	bound := devicesConditionMessageBound(t)

	t.Run("an oversized invalid report publishes a bounded False condition", func(t *testing.T) {
		policy := oversizedUnknownIncludePolicy()
		resolution := atBoundResolution(policy)
		require.Greater(t, utf8.RuneCountInString(resolution.Message), int(bound),
			"the fixture must genuinely exceed the schema bound before the observer bounds it")

		devs := observationDevices()
		seedForeignState(devs)
		obs, spy := newSelectionObserver(t, selectionInventory(),
			&observerClientOptions{rejectOversizedConditionMessage: true},
			devs, selectionNode(), policy)
		rvAtRead := observationStoredDevices(t, obs).ResourceVersion

		result, err := reconcileSelection(t, obs)
		require.NoError(t, err,
			"an oversized diagnostic must publish bounded, not be rejected by the API server")
		assert.Equal(t, rdmaSelectionResyncPeriod, result.RequeueAfter,
			"the bounded pass settles and arms the polling interval")

		got := observationStoredDevices(t, obs)
		have := observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType)
		require.NotNil(t, have)
		assert.Equal(t, meta.ConditionFalse, have.Status)
		assert.Equal(t, RDMASelectionPolicyInvalidReason, have.Reason)
		assert.Equal(t, int64(7), have.ObservedGeneration)
		assert.LessOrEqual(t, utf8.RuneCountInString(have.Message), int(bound),
			"the published message must satisfy the generated schema bound")
		assert.Contains(t, have.Message, "include names the inventory does not know",
			"the leading diagnostic stays")
		assert.Contains(t, have.Message, "hca-00000", "the first unknown name stays")
		assert.Contains(t, have.Message, "truncated",
			"the truncation is marked in the published message")
		require.NotNil(t, got.Status.Interfaces)
		require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy)
		ref := got.Status.Interfaces.RDMASelectionPolicy.PolicyRef
		require.NotNil(t, ref, "the invalid policy's reference is still the desiredRef")
		assert.Equal(t, "uid-huge", ref.UID)
		assertForeignConditionSurvived(t, &got.Status)
		assertOwnedPatch(t, spy, rvAtRead)
	})

	t.Run("an oversized read error publishes a bounded Unknown condition", func(t *testing.T) {
		devs := observationDevices()
		seedForeignState(devs)
		opts := observerClientOptions{
			listErr:                         errors.New(strings.Repeat("x", int(bound)+64)),
			rejectOversizedConditionMessage: true,
		}
		policy := selectionPolicy("compute-hcas", "uid-a", 3, nil)
		obs, spy := newSelectionObserver(t, selectionInventory(), &opts,
			devs, selectionNode(), policy)
		rvAtRead := observationStoredDevices(t, obs).ResourceVersion

		result, err := reconcileSelection(t, obs)
		require.NoError(t, err,
			"an oversized read failure must publish bounded, not be rejected by the API server")
		assert.Equal(t, rdmaSelectionResyncPeriod, result.RequeueAfter,
			"the bounded pass settles and arms the polling interval")

		assertUnknownObservation(t, obs, "list the RDMA selection policies")
		got := observationStoredDevices(t, obs)
		have := observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType)
		require.NotNil(t, have)
		assert.LessOrEqual(t, utf8.RuneCountInString(have.Message), int(bound),
			"the published message must satisfy the generated schema bound")
		assert.Contains(t, have.Message, "truncated",
			"the truncation is marked in the published message")
		assertOwnedPatch(t, spy, rvAtRead)
	})

	t.Run("a report exactly at the bound publishes verbatim", func(t *testing.T) {
		policy := exactlyAtBoundUnknownIncludePolicy()
		resolution := atBoundResolution(policy)
		require.Equal(t, int(bound), utf8.RuneCountInString(resolution.Message),
			"the fixture must land exactly on the schema bound")

		devs := observationDevices()
		seedForeignState(devs)
		obs, spy := newSelectionObserver(t, selectionInventory(),
			&observerClientOptions{rejectOversizedConditionMessage: true},
			devs, selectionNode(), policy)

		_, err := reconcileSelection(t, obs)
		require.NoError(t, err)

		got := observationStoredDevices(t, obs)
		have := observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType)
		require.NotNil(t, have)
		assert.Equal(t, meta.ConditionFalse, have.Status)
		assert.Equal(t, RDMASelectionPolicyInvalidReason, have.Reason)
		assert.Equal(t, resolution.Message, have.Message,
			"an exactly-at-bound message is published verbatim")
		assert.NotContains(t, have.Message, "truncated",
			"an exactly-at-bound message carries no truncation mark")
		require.NotNil(t, got.Status.Interfaces)
		require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy)
		require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
		assert.Equal(t, "uid-exact", got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.UID)
		assertForeignConditionSurvived(t, &got.Status)
		assert.Positive(t, spy.patches, "the exactly-at-bound publish is one write")
	})
}

// TestRDMASelectionMessageBound pins the message clamp itself: the production limit equals
// the bound the generated Devices CRD carries on status.conditions[].message, messages at
// or under the limit pass through verbatim however many bytes their code points render to,
// and an oversized message is cut on a rune boundary to exactly the limit with its leading
// text kept and the truncation marked.
func TestRDMASelectionMessageBound(t *testing.T) {
	bound := int(devicesConditionMessageBound(t))
	require.Equal(t, int64(rdmaSelectionMessageLimit), int64(bound),
		"the production limit must equal the generated schema bound")

	tests := []struct {
		name   string
		msg    string
		expect *string // nil pins a bounded (truncated) result instead of an exact one
	}{
		{
			name:   "an empty message stays empty",
			msg:    "",
			expect: ptrOf(""),
		},
		{
			name:   "a short message passes through verbatim",
			msg:    "mlx5_9 is not known",
			expect: ptrOf("mlx5_9 is not known"),
		},
		{
			name:   "a message exactly at the limit in code points passes through verbatim",
			msg:    strings.Repeat("x", bound),
			expect: ptrOf(strings.Repeat("x", bound)),
		},
		{
			name: "a message exactly at the limit in code points passes through however many bytes it renders to",
			msg:  strings.Repeat("界", bound),
			// 32,768 three-byte runes: at the limit by code points, far over it by bytes,
			// so a byte-counting clamp would wrongly truncate this message.
			expect: ptrOf(strings.Repeat("界", bound)),
		},
		{
			name: "a message one code point over the limit is cut to the limit with a mark",
			msg:  strings.Repeat("x", bound+1),
		},
		{
			name: "an oversized multibyte message is cut on a rune boundary",
			msg:  strings.Repeat("界", bound+1),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rdmaSelectionBoundMessage(tc.msg)
			if tc.expect != nil {
				assert.Equal(t, *tc.expect, got)
				return
			}
			assert.Len(t, []rune(got), bound,
				"a truncated message is exactly at the limit, the mark included")
			assert.True(t, utf8.ValidString(got), "the published message is valid UTF-8")
			assert.Contains(t, got, "truncated", "the truncation is marked")
			assert.True(t, strings.HasPrefix(got, string([]rune(tc.msg)[:bound/2])),
				"the leading diagnostics stay")
		})
	}

	t.Run("a truncated message drops only the tail", func(t *testing.T) {
		msg := strings.Repeat("world-", 8000) + "the tail that must go"
		got := rdmaSelectionBoundMessage(msg)
		assert.True(t, strings.HasPrefix(got, strings.Repeat("world-", 4000)),
			"the leading text stays")
		assert.NotContains(t, got, "the tail that must go")
		assert.Len(t, []rune(got), bound)
		assert.Contains(t, got, fmt.Sprintf("truncated from %d characters", utf8.RuneCountInString(msg)),
			"the mark names the original length")
	})
}

// ptrOf returns a pointer to its argument, for optional table expectations.
func ptrOf(s string) *string { return &s }

// TestRDMASelectionObserver_DevicesFailuresAreRetryable pins that a Devices read or write
// that fails returns a retryable error and publishes nothing.
func TestRDMASelectionObserver_DevicesFailuresAreRetryable(t *testing.T) {
	unreachable := errors.New("the api server is unreachable")

	t.Run("a failed devices read retries without publishing", func(t *testing.T) {
		devs := observationDevices()
		seedForeignState(devs)
		policy := selectionPolicy("compute-hcas", "uid-a", 3, nil)
		obs, spy := newSelectionObserver(t, selectionInventory(),
			&observerClientOptions{devicesGetErr: unreachable}, devs, selectionNode(), policy)

		_, err := reconcileSelection(t, obs)
		require.Error(t, err, "an unreadable Devices must be reported, not settled")
		assert.ErrorContains(t, err, "api server")
		assert.Zero(t, spy.patches, "nothing may be published over a read that failed")
	})

	t.Run("a failed devices write is reported and retried from fresh state", func(t *testing.T) {
		devs := observationDevices()
		seedForeignState(devs)
		policy := selectionPolicy("compute-hcas", "uid-a", 3, nil)
		obs, spy := newSelectionObserver(t, selectionInventory(), &observerClientOptions{}, devs, selectionNode(), policy)
		spy.failWrite = true

		_, err := reconcileSelection(t, obs)
		require.Error(t, err, "a failed write must be reported, not claimed as settled")
		assert.ErrorContains(t, err, "api server")
		got := observationStoredDevices(t, obs)
		if policy := got.Status.Interfaces.RDMASelectionPolicy; policy != nil {
			assert.Equal(t, "uid-applied", policy.PolicyRef.UID,
				"the failed pass publishes nothing, so the stale reference stands")
		}

		spy.failWrite = false
		_, err = reconcileSelection(t, obs)
		require.NoError(t, err)
		got = observationStoredDevices(t, obs)
		require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
		assert.Equal(t, "uid-a", got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.UID)
	})
}

// TestRDMASelectionObserver_OwnedFieldsOnly pins the payload contract on a clean base: the
// patch touches interfaces.rdmaSelectionPolicy.desiredRef and the resolved condition, and no
// applied field, applied condition or group key ever enters the payload.
func TestRDMASelectionObserver_OwnedFieldsOnly(t *testing.T) {
	t.Run("a policy resolution on a clean base carries only owned keys", func(t *testing.T) {
		devs := observationDevices()
		policy := selectionPolicy("compute-hcas", "uid-a", 3, func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Exclude = []string{"mlx5_5"}
		})
		obs, spy := newSelectionObserver(t, selectionInventory(), &observerClientOptions{}, devs, selectionNode(), policy)
		rvAtRead := observationStoredDevices(t, obs).ResourceVersion

		_, err := reconcileSelection(t, obs)
		require.NoError(t, err)

		patchStatus := assertOwnedPatch(t, spy, rvAtRead)
		require.Contains(t, patchStatus, "interfaces")
		require.Contains(t, patchStatus, "conditions")
		// A clean base carries no foreign conditions, so the moved list is the writer's own
		// condition alone; the applied condition is never introduced.
		var patchConditions []apiv1.Condition
		require.NoError(t, json.Unmarshal(patchStatus["conditions"], &patchConditions))
		require.Len(t, patchConditions, 1)
		assert.Equal(t, RDMASelectionResolvedConditionType, patchConditions[0].Type)
		got := observationStoredDevices(t, obs)
		assert.Nil(t, got.Status.Groups, "a clean base gains no accelerator groups from the observer")
	})

	t.Run("withdrawing the reference nulls the owned keys alone", func(t *testing.T) {
		devs := observationDevices()
		seedForeignState(devs)
		seeded := devs.DeepCopy()
		// The resolved condition this pass would derive is already published beside the
		// foreign one; only the stale reference and exclusion from an earlier pass of this
		// very writer remain to move.
		seeded.Status.Conditions = append(seeded.Status.Conditions, apiv1.Condition{
			Type:               RDMASelectionResolvedConditionType,
			Status:             meta.ConditionTrue,
			Reason:             RDMASelectionNoPolicyReason,
			Message:            "no RDMA selection policy targets this node",
			ObservedGeneration: 7,
			LastTransitionTime: meta.NewTime(time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)),
		})
		seeded.Status.Interfaces.RDMASelectionPolicy.PolicyRef = &workercore.DevicesRDMASelectionPolicyReference{
			Name: "compute-hcas", UID: "uid-old", Generation: 1,
		}
		obs, spy := newSelectionObserver(t, selectionInventory(), &observerClientOptions{}, seeded, selectionNode())
		rvAtRead := observationStoredDevices(t, obs).ResourceVersion

		_, err := reconcileSelection(t, obs)
		require.NoError(t, err)

		patchStatus := assertOwnedPatch(t, spy, rvAtRead)
		require.Contains(t, patchStatus, "interfaces")
		assert.NotContains(t, patchStatus, "conditions",
			"only the selection section moved; the resolved condition the pass derived agrees with the seed")
		ifaces := map[string]json.RawMessage{}
		require.NoError(t, json.Unmarshal(patchStatus["interfaces"], &ifaces))
		selection := map[string]json.RawMessage{}
		require.NoError(t, json.Unmarshal(ifaces["rdmaSelectionPolicy"], &selection))
		require.Contains(t, selection, "policyRef")
		assert.Contains(t, selection, "excluded",
			"the resolved default replaces the stale exclusion list in the same patch")
		assert.Nil(t, observationStoredDevices(t, obs).Status.Interfaces.RDMASelectionPolicy.PolicyRef)
		assert.Equal(t, []workercore.DevicesRDMAExclusion{
			{Name: "efa0", Reason: "ExternallyOwned"},
		}, observationStoredDevices(t, obs).Status.Interfaces.RDMASelectionPolicy.Excluded,
			"the default resolution's externally owned exclusion replaces the stale list")
		assertForeignConditionSurvived(t, &observationStoredDevices(t, obs).Status)
	})
}

// TestRDMASelectionObserver_ForeignFieldsSurvive pins that a published observation preserves
// every foreign field, including through the patch payload itself, which carries the foreign
// conditions only because a merge patch replaces the list whole and the resource version
// precondition makes that safe.
func TestRDMASelectionObserver_ForeignFieldsSurvive(t *testing.T) {
	devs := observationDevices()
	seedForeignState(devs)
	policy := selectionPolicy("compute-hcas", "uid-a", 3, func(p *workercore.DevicesRDMASelectionPolicy) {
		p.Spec.Exclude = []string{"mlx5_5"}
	})
	obs, spy := newSelectionObserver(t, selectionInventory(), &observerClientOptions{}, devs, selectionNode(), policy)

	_, err := reconcileSelection(t, obs)
	require.NoError(t, err)

	got := observationStoredDevices(t, obs)
	assertForeignConditionSurvived(t, &got.Status)

	_, wantConditions := foreignState()
	var patchConditions []apiv1.Condition
	patchFields := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal(spy.lastPatch, &patchFields))
	require.NoError(t, json.Unmarshal(patchFields["status"], &patchFields))
	require.Contains(t, patchFields, "conditions")
	require.NoError(t, json.Unmarshal(patchFields["conditions"], &patchConditions))
	foreignInPatch := observerCondition(patchConditions, "MetricsReported")
	require.NotNil(t, foreignInPatch,
		"a whole-list merge patch carries the foreign condition")
	assert.Equal(t, wantConditions[0].Status, foreignInPatch.Status)
	assert.Equal(t, wantConditions[0].Reason, foreignInPatch.Reason)
	assert.Equal(t, wantConditions[0].Message, foreignInPatch.Message)
	assert.True(t, wantConditions[0].LastTransitionTime.Time.Equal(foreignInPatch.LastTransitionTime.Time),
		"the carried foreign condition keeps its instant")
}

// TestRDMASelectionObserver_ConflictRetriesFromFreshState pins the optimistic-lock contract:
// a foreign write landing between the read and the patch is a reported conflict, the stale
// pass publishes nothing, and a fresh pass completes over the raced foreign state.
func TestRDMASelectionObserver_ConflictRetriesFromFreshState(t *testing.T) {
	devs := observationDevices()
	seedForeignState(devs)
	policy := selectionPolicy("compute-hcas", "uid-a", 3, nil)
	obs, spy := newSelectionObserver(t, selectionInventory(), &observerClientOptions{}, devs, selectionNode(), policy)

	spy.raceForeign = func(_ context.Context, cli ctrlcli.Client) {
		live := new(workercore.Devices)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: selectionNodeName}, live))
		live.Status.Conditions[0].Message = "metrics exporter reported at generation 3"
		live.Status.Groups[0].Accelerators[0].Remaining = 7
		require.NoError(t, cli.Status().Update(context.Background(), live))
	}

	_, err := reconcileSelection(t, obs)
	require.Error(t, err, "a write that raced a foreign update must come back as an error")
	assert.True(t, apierrors.IsConflict(err),
		"the error is the conflict itself, not a publish that guessed its way past one")

	got := observationStoredDevices(t, obs)
	require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
	assert.Equal(t, "uid-applied", got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.UID,
		"the stale pass publishes nothing, so the stale reference stands")
	assert.Equal(t, "metrics exporter reported at generation 3",
		observerCondition(got.Status.Conditions, "MetricsReported").Message,
		"the foreign write that won the race keeps its condition")

	spy.raceForeign = nil
	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)
	got = observationStoredDevices(t, obs)
	require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
	assert.Equal(t, "uid-a", got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.UID,
		"the fresh pass completes the publish")
	assert.Equal(t, "metrics exporter reported at generation 3",
		observerCondition(got.Status.Conditions, "MetricsReported").Message,
		"completing the publish keeps the raced foreign condition")
	assert.Equal(t, int32(7), got.Status.Groups[0].Accelerators[0].Remaining,
		"completing the publish keeps the raced foreign ledger")
}

// TestRDMASelectionObserver_RepeatedIdenticalObservationsWriteNothing pins the no-churn
// contract: an agreeing pass writes nothing, however the stored instants are rendered, and
// however many times the same observation is delivered.
func TestRDMASelectionObserver_RepeatedIdenticalObservationsWriteNothing(t *testing.T) {
	devs := observationDevices()
	seedForeignState(devs)
	policy := selectionPolicy("compute-hcas", "uid-a", 3, func(p *workercore.DevicesRDMASelectionPolicy) {
		p.Spec.Exclude = []string{"mlx5_5"}
	})
	obs, spy := newSelectionObserver(t, selectionInventory(), &observerClientOptions{}, devs, selectionNode(), policy)

	_, err := reconcileSelection(t, obs)
	require.NoError(t, err)
	require.Equal(t, 1, spy.patches, "the first pass publishes the observation")

	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)
	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)
	assert.Equal(t, 1, spy.patches,
		"a re-delivered identical observation must not write again")

	// The same content rendered in another zone is the same content: shifting every stored
	// instant must not turn the next pass into a write.
	live := observationStoredDevices(t, obs)
	zone := time.FixedZone("UTC+8", 8*3600)
	for i := range live.Status.Conditions {
		live.Status.Conditions[i].LastTransitionTime = meta.NewTime(
			live.Status.Conditions[i].LastTransitionTime.In(zone),
		)
	}
	require.NoError(t, obs.Client.Status().Update(context.Background(), live))

	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)
	assert.Equal(t, 1, spy.patches,
		"an agreeing pass writes nothing, whatever zone the stored instants render in")
}

// TestRDMASelectionObserver_PrunedPublishDoesNotLoop pins the old-CRD pruning bound: a status
// patch the store cannot keep re-writes at pass cadence only, because the self-echo of the
// write is a status-only update the event predicate ignores.
func TestRDMASelectionObserver_PrunedPublishDoesNotLoop(t *testing.T) {
	devs := observationDevices()
	seedForeignState(devs)
	policy := selectionPolicy("compute-hcas", "uid-a", 3, nil)
	obs, spy := newSelectionObserver(t, selectionInventory(),
		&observerClientOptions{pruneInterfaces: true}, devs, selectionNode(), policy)

	_, err := reconcileSelection(t, obs)
	require.NoError(t, err)
	assert.Equal(t, 1, spy.patches, "the first pass writes against the pruning store")
	pruned := observationStoredDevices(t, obs)
	assert.Nil(t, pruned.Status.Interfaces,
		"the pruning store really dropped the field, standing in for the old CRD")

	// The next pass is a re-delivery of the same observation — the resync, or the echo event
	// a newer CRD would deliver. Each pass writes at most once, whatever the previous pass
	// wrote, and the foreign state the pruning store can keep stays whole.
	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)
	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)
	assert.Equal(t, 3, spy.patches,
		"three passes write three times at most: no pass amplifies the write it triggered")
	assertForeignConditionSurvived(t, &observationStoredDevices(t, obs).Status)
}

// TestRDMASelectionObserver_DevicesEventPredicate pins the production event predicate:
// creation and spec changes enqueue, status-only updates do not, and other nodes never do.
func TestRDMASelectionObserver_DevicesEventPredicate(t *testing.T) {
	obs := &RDMASelectionObserver{NodeName: selectionNodeName}
	predicate := obs.devicesEventPredicate()

	thisNode := observationDevices
	otherNode := func() *workercore.Devices {
		return &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: "node-other", Generation: 7}}
	}
	specBumped := func() *workercore.Devices {
		devs := observationDevices()
		devs.Generation = 8
		return devs
	}
	statusOnly := func() *workercore.Devices {
		devs := observationDevices()
		devs.Status.Conditions = []apiv1.Condition{{
			Type: RDMASelectionResolvedConditionType, Status: meta.ConditionTrue,
			Reason: RDMASelectionNoPolicyReason, ObservedGeneration: 7,
			LastTransitionTime: meta.NewTime(time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)),
		}}
		return devs
	}

	tests := []struct {
		name  string
		event func() bool
		want  bool
	}{
		{
			name: "a created devices object enqueues",
			event: func() bool {
				return predicate.Create(ctrlevent.CreateEvent{Object: thisNode()})
			},
			want: true,
		},
		{
			name: "another node's devices never enqueue",
			event: func() bool {
				return predicate.Create(ctrlevent.CreateEvent{Object: otherNode()})
			},
			want: false,
		},
		{
			name: "a spec change enqueues",
			event: func() bool {
				return predicate.Update(ctrlevent.UpdateEvent{ObjectOld: thisNode(), ObjectNew: specBumped()})
			},
			want: true,
		},
		{
			name: "a status-only update does not enqueue",
			event: func() bool {
				return predicate.Update(ctrlevent.UpdateEvent{ObjectOld: thisNode(), ObjectNew: statusOnly()})
			},
			want: false,
		},
		{
			name: "another node's spec change does not enqueue",
			event: func() bool {
				return predicate.Update(ctrlevent.UpdateEvent{
					ObjectOld: otherNode(),
					ObjectNew: &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: "node-other", Generation: 8}},
				})
			},
			want: false,
		},
		{
			name: "a deleted devices object still enqueues to settle the pass",
			event: func() bool {
				return predicate.Delete(ctrlevent.DeleteEvent{Object: thisNode()})
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.event())
		})
	}
}

// TestRDMASelectionObserver_PolicyLifecycle pins the reference across a policy's life:
// creation publishes it, a selector edit moves the generation, deletion withdraws it into
// the default, and a same-name replacement binds the new UID.
func TestRDMASelectionObserver_PolicyLifecycle(t *testing.T) {
	devs := observationDevices()
	seedForeignState(devs)
	obs, spy := newSelectionObserver(t, selectionInventory(), &observerClientOptions{}, devs, selectionNode())
	ctx := context.Background()

	// Nothing targets the node yet: the default publishes with no reference.
	_, err := reconcileSelection(t, obs)
	require.NoError(t, err)
	got := observationStoredDevices(t, obs)
	require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy)
	assert.Nil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
	have := observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType)
	require.NotNil(t, have)
	assert.Equal(t, meta.ConditionTrue, have.Status)
	assert.Equal(t, RDMASelectionNoPolicyReason, have.Reason)

	// Creation: the reference moves in.
	policy := selectionPolicy("compute-hcas", "uid-a", 1, nil)
	require.NoError(t, obs.Client.Create(ctx, policy))
	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)
	got = observationStoredDevices(t, obs)
	require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
	assert.Equal(t, int64(1), got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.Generation)

	// A selector edit is a new generation of the same object: the reference moves with it.
	edited := policy.DeepCopy()
	edited.Generation = 2
	edited.Spec.Exclude = []string{"mlx5_5"}
	require.NoError(t, obs.Client.Update(ctx, edited))
	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)
	got = observationStoredDevices(t, obs)
	require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
	assert.Equal(t, int64(2), got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.Generation,
		"the reference binds the generation it was read at")
	assert.Equal(t, 3, spy.patches, "each reference movement is one publish")

	// Deletion: the default returns and the reference is withdrawn.
	require.NoError(t, obs.Client.Delete(ctx, policy))
	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)
	got = observationStoredDevices(t, obs)
	assert.Nil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
	have = observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType)
	require.NotNil(t, have)
	assert.Equal(t, meta.ConditionTrue, have.Status)
	assert.Equal(t, RDMASelectionNoPolicyReason, have.Reason)

	// A same-name replacement is another policy: the reference binds the new UID.
	replacement := selectionPolicy("compute-hcas", "uid-replaced", 1, nil)
	require.NoError(t, obs.Client.Create(ctx, replacement))
	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)
	got = observationStoredDevices(t, obs)
	require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
	assert.Equal(t, "uid-replaced", got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.UID,
		"a same-name replacement must not keep the old binding")
	assertForeignConditionSurvived(t, &got.Status)
}

// TestRDMASelectionObserver_CachedReadsWithNodeNotFoundAPIReaderFallback pins the data path:
// the policy list and the local Node come from the cached client the manager hands the
// observer, and only a local Node the cache has not populated yet falls back to one read
// through the uncached reader.
func TestRDMASelectionObserver_CachedReadsWithNodeNotFoundAPIReaderFallback(t *testing.T) {
	t.Run("the cached client serves both reads while it holds the local node", func(t *testing.T) {
		devs := observationDevices()
		seedForeignState(devs)
		spy := new(ledgerWriteSpy)
		cached := newSelectionObserverClient(t, spy, &observerClientOptions{}, devs,
			selectionPolicy("cached-policy", "uid-cached", 9, nil), selectionNode())
		// The uncached reader refuses every read: if a pass reached it at all, the
		// resolution would collapse to Unknown instead of the cached policy below.
		reader := newSelectionObserverClient(t, new(ledgerWriteSpy), &observerClientOptions{
			listErr:    errors.New("the uncached reader serves no reads"),
			nodeGetErr: errors.New("the uncached reader serves no reads"),
		})

		obs := &RDMASelectionObserver{
			NodeName:               selectionNodeName,
			Client:                 cached,
			APIReader:              reader,
			RDMAInterfaceInventory: selectionInventory,
		}

		_, err := reconcileSelection(t, obs)
		require.NoError(t, err)

		got := new(workercore.Devices)
		require.NoError(t, cached.Get(context.Background(), ctrlcli.ObjectKey{Name: selectionNodeName}, got))
		require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
		assert.Equal(t, "cached-policy", got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.Name,
			"the published reference must come from the cached client")
		assert.Equal(t, "uid-cached", got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.UID)
		assert.Equal(t, 1, spy.patches, "the pass publishes through the cached client alone")
	})

	t.Run("a local node missing from the cache falls back to one uncached read", func(t *testing.T) {
		devs := observationDevices()
		seedForeignState(devs)
		spy := new(ledgerWriteSpy)
		// The cache holds the Devices and the policy but not the Node: its Get answers
		// NotFound the way an informer that has not synced yet does.
		cached := newSelectionObserverClient(t, spy, &observerClientOptions{}, devs,
			selectionPolicy("cached-policy", "uid-cached", 9, nil))
		reader := newSelectionObserverClient(t, new(ledgerWriteSpy), &observerClientOptions{}, selectionNode())

		obs := &RDMASelectionObserver{
			NodeName:               selectionNodeName,
			Client:                 cached,
			APIReader:              reader,
			RDMAInterfaceInventory: selectionInventory,
		}

		_, err := reconcileSelection(t, obs)
		require.NoError(t, err)

		// The node's labels arrived through the uncached reader — the targeting policy only
		// matches them there — and the policy list still came from the cached client.
		got := new(workercore.Devices)
		require.NoError(t, cached.Get(context.Background(), ctrlcli.ObjectKey{Name: selectionNodeName}, got))
		require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
		assert.Equal(t, "cached-policy", got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.Name,
			"the policy list still comes from the cached client")
		assert.Equal(t, "uid-cached", got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.UID)
		assertResolvedCondition(t,
			observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType),
			apiv1.Condition{
				Status:             meta.ConditionTrue,
				Reason:             RDMASelectionPolicyResolvedReason,
				ObservedGeneration: 7,
				Message:            "the RDMA selection resolves from policy cached-policy",
			})
	})
}

// _syncedChannel is the closed channel every fake sync check reports.
var _syncedChannel = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// _syncedChecker reports an already-synced DoneChecker for the fake informer.
type _syncedChecker struct{}

func (_syncedChecker) Name() string          { return "synced" }
func (_syncedChecker) Done() <-chan struct{} { return _syncedChannel }

// _syncedRegistration is an event-handler registration the fake informer reports as synced.
type _syncedRegistration struct{}

func (_syncedRegistration) HasSynced() bool                          { return true }
func (_syncedRegistration) HasSyncedChecker() toolscache.DoneChecker { return _syncedChecker{} }

// _syncedInformer is the informer the recording cache hands back: synced from birth.
type _syncedInformer struct{}

func (_syncedInformer) AddEventHandler(_ toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	return _syncedRegistration{}, nil
}

func (_syncedInformer) AddEventHandlerWithResyncPeriod(_ toolscache.ResourceEventHandler, _ time.Duration) (toolscache.ResourceEventHandlerRegistration, error) {
	return _syncedRegistration{}, nil
}

func (_syncedInformer) AddEventHandlerWithOptions(_ toolscache.ResourceEventHandler, _ toolscache.HandlerOptions) (toolscache.ResourceEventHandlerRegistration, error) {
	return _syncedRegistration{}, nil
}

func (_syncedInformer) RemoveEventHandler(_ toolscache.ResourceEventHandlerRegistration) error {
	return nil
}

func (_syncedInformer) AddIndexers(_ toolscache.Indexers) error  { return nil }
func (_syncedInformer) HasSynced() bool                          { return true }
func (_syncedInformer) HasSyncedChecker() toolscache.DoneChecker { return _syncedChecker{} }
func (_syncedInformer) IsStopped() bool                          { return false }

// _recordingCache stands in for the manager cache and records every informer kind the
// started controllers ask for, so the test can prove which kinds the wiring depends on.
type _recordingCache struct {
	mu        sync.Mutex
	requested []string
}

func (c *_recordingCache) record(obj runtime.Object) {
	gvk, err := ctrlapiutil.GVKForObject(obj, scheme.Scheme)
	if err != nil {
		panic("the recording cache only serves scheme-known objects: " + err.Error())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requested = append(c.requested, gvk.String())
}

func (c *_recordingCache) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requested...)
}

func (c *_recordingCache) Get(_ context.Context, _ ctrlcli.ObjectKey, _ ctrlcli.Object, _ ...ctrlcli.GetOption) error {
	return errors.New("the recording cache serves no reads")
}

func (c *_recordingCache) List(_ context.Context, _ ctrlcli.ObjectList, _ ...ctrlcli.ListOption) error {
	return errors.New("the recording cache serves no reads")
}

func (c *_recordingCache) GetInformer(_ context.Context, obj ctrlcli.Object, _ ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error) {
	c.record(obj)
	return _syncedInformer{}, nil
}

func (c *_recordingCache) GetInformerForKind(_ context.Context, gvk schema.GroupVersionKind, _ ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requested = append(c.requested, gvk.String())
	return _syncedInformer{}, nil
}

func (c *_recordingCache) RemoveInformer(_ context.Context, _ ctrlcli.Object) error { return nil }

func (c *_recordingCache) Start(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (c *_recordingCache) WaitForCacheSync(_ context.Context) bool { return true }

func (c *_recordingCache) IndexField(_ context.Context, _ ctrlcli.Object, _ string, _ ctrlcli.IndexerFunc) error {
	return nil
}

// _idleWebhookServer is a webhook server that serves nothing and binds nothing, so the
// manager under test starts no listener.
type _idleWebhookServer struct{}

func (_idleWebhookServer) NeedLeaderElection() bool          { return false }
func (_idleWebhookServer) Register(_ string, _ http.Handler) {}
func (_idleWebhookServer) Start(ctx context.Context) error   { <-ctx.Done(); return ctx.Err() }
func (_idleWebhookServer) StartedChecker() healthz.Checker {
	return func(_ *http.Request) error { return nil }
}
func (_idleWebhookServer) WebhookMux() *http.ServeMux { return http.NewServeMux() }

// _controllerLogSink is a logr sink that records every message logged through the manager
// under test, so the setup control can wait on controller-reported evidence instead of
// elapsed time.
type _controllerLogSink struct {
	mu       sync.Mutex
	messages []string
}

func (s *_controllerLogSink) Init(logr.RuntimeInfo) {}

func (s *_controllerLogSink) Enabled(int) bool { return true }

func (s *_controllerLogSink) Info(_ int, msg string, _ ...any) { s.append(msg) }

func (s *_controllerLogSink) Error(_ error, msg string, _ ...any) { s.append(msg) }

func (s *_controllerLogSink) WithName(_ string) logr.LogSink { return s }

func (s *_controllerLogSink) WithValues(_ ...any) logr.LogSink { return s }

func (s *_controllerLogSink) append(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
}

func (s *_controllerLogSink) saw(msg string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.messages, msg)
}

// TestRDMASelectionObserver_SetupWatchesDevicesPoliciesAndLocalNode pins the event surface:
// the observer's setup registers exactly three informer-backed watches — this node's Devices,
// the RDMA selection policy kind, and the local Node — plus the local wake-up channel, and
// contacts no API server during setup. This is a local wiring control, not a cluster proof:
// real API-server discovery and rollout behavior remain a separate acceptance gate.
func TestRDMASelectionObserver_SetupWatchesDevicesPoliciesAndLocalNode(t *testing.T) {
	rec := new(_recordingCache)
	sink := new(_controllerLogSink)
	// The controller name lands in a process-global registry on the first setup, so a second
	// -count iteration would fail validation against the first. This throwaway test manager
	// is the only place the skip is set; production managers keep the validation on.
	skipNameValidation := true
	rawMgr, err := ctrlmgr.New(&rest.Config{Host: "localhost:1"}, ctrlmgr.Options{
		Scheme:  scheme.Scheme,
		Logger:  logr.New(sink),
		Metrics: ctrlmetricsserver.Options{BindAddress: "0"},
		NewCache: func(_ *rest.Config, _ ctrlcache.Options) (ctrlcache.Cache, error) {
			return rec, nil
		},
		WebhookServer: _idleWebhookServer{},
		Controller:    ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
	})
	require.NoError(t, err, "the manager must come up without a cluster")

	obs := new(RDMASelectionObserver)
	require.NoError(t, obs.SetupController(context.Background(),
		controller.SetupOptions{Manager: manager.CtrlManager{Manager: rawMgr}}),
		"setup must not contact the api server")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- rawMgr.Start(ctx) }()

	// The internal controller logs "Starting workers" only after every registered source has
	// started and synced (errgroup.Wait), so by the time the line lands, every informer request
	// the watches make is already in the recording cache. Waiting on this controller-reported
	// line is what makes the inspection deterministic; a grace-period sleep is not, because a
	// late source could always have started after it elapsed.
	require.Eventually(t, func() bool {
		return sink.saw("Starting workers")
	}, 10*time.Second, 10*time.Millisecond,
		"the started controller must report that its sources have started")

	gvkOf := func(obj ctrlcli.Object) string {
		gvk, err := ctrlapiutil.GVKForObject(obj, scheme.Scheme)
		require.NoError(t, err)
		return gvk.String()
	}
	assert.ElementsMatch(t,
		[]string{
			gvkOf(&workercore.Devices{}),
			gvkOf(&workercore.DevicesRDMASelectionPolicy{}),
			gvkOf(&core.Node{}),
		},
		rec.snapshot(),
		"the observer watches exactly this node's Devices, the policy kind, and the local Node")

	cancel()
	select {
	case err := <-startErr:
		assert.True(t, err == nil || errors.Is(err, context.Canceled),
			"the manager must stop cleanly once its context is cancelled, got %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the manager did not stop after its context was cancelled")
	}
}

// TestRDMASelectionObserver_NodeEventPredicate pins the local-Node event filter: the node
// arriving, leaving, or changing labels wakes the pass, while annotation or status updates and
// any other node's label change do not.
func TestRDMASelectionObserver_NodeEventPredicate(t *testing.T) {
	obs := &RDMASelectionObserver{NodeName: selectionNodeName}
	predicate := obs.nodeEventPredicate()

	other := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name:   "node-elsewhere",
			Labels: map[string]string{"rdma.example.com/pool": "elsewhere"},
		},
	}

	assert.True(t, predicate.Create(ctrlevent.CreateEvent{Object: selectionNode()}),
		"the local node arriving must wake the pass")
	assert.True(t, predicate.Delete(ctrlevent.DeleteEvent{Object: selectionNode()}),
		"the local node leaving must wake the pass")
	assert.True(t, predicate.Update(ctrlevent.UpdateEvent{
		ObjectOld: selectionNode(),
		ObjectNew: func() *core.Node {
			// A fresh label map: selectionNode hands out the shared fixture map, and
			// mutating it in place would move the old and new objects together.
			n := selectionNode()
			n.Labels = map[string]string{"rdma.example.com/pool": "storage"}
			return n
		}(),
	}), "a label change on the local node must wake the pass")

	assert.False(t, predicate.Update(ctrlevent.UpdateEvent{
		ObjectOld: selectionNode(),
		ObjectNew: func() *core.Node {
			n := selectionNode()
			n.Annotations = map[string]string{"note": "touched"}
			return n
		}(),
	}), "an annotation-only update must not wake the pass")
	assert.False(t, predicate.Update(ctrlevent.UpdateEvent{
		ObjectOld: selectionNode(),
		ObjectNew: selectionNode(),
	}), "a status-only update carrying no change must not wake the pass")

	relabeled := *other
	relabeled.Labels = map[string]string{"rdma.example.com/pool": "moved"}
	assert.False(t, predicate.Update(ctrlevent.UpdateEvent{
		ObjectOld: other,
		ObjectNew: &relabeled,
	}), "a label change on another node must not wake the pass")
	assert.False(t, predicate.Create(ctrlevent.CreateEvent{Object: other}),
		"another node arriving must not wake the pass")
	assert.False(t, predicate.Delete(ctrlevent.DeleteEvent{Object: other}),
		"another node leaving must not wake the pass")
}

// TestRDMASelectionObserver_ActiveReconcilerPublishesAndActivatesSelection pins that when a
// DevicesReconciler is attached, the observer publishes the resolved policyRef, exclusions,
// and the single RDMASelectionResolved condition, immediately updates the reconciler's active
// selection for subsequent advertisements, and fails closed when a later resolution becomes
// invalid: the reference names the offending policy, the exclusions clear, and every HCA the
// previously applied selection served goes unhealthy until the resolution recovers.
func TestRDMASelectionObserver_ActiveReconcilerPublishesAndActivatesSelection(t *testing.T) {
	ctx := context.Background()
	devs := observationDevices()
	devs.Spec.Interfaces = []workercore.DeviceInterface{
		wholeFunctionIface("ib0", "0", "mlx5_0", nil),
		wholeFunctionIface("ib1", "0", "mlx5_4", nil),
		wholeFunctionIface("ib2", "1", "mlx5_5", nil),
	}
	policy := selectionPolicy("compute-hcas", "uid-a", 3, func(p *workercore.DevicesRDMASelectionPolicy) {
		p.Spec.Exclude = []string{"mlx5_5"}
	})
	obs, spy := newSelectionObserver(t, selectionInventory(), &observerClientOptions{}, devs, selectionNode(), policy)
	rec := &DevicesReconciler{
		NodeName:               selectionNodeName,
		Client:                 obs.Client,
		RDMAInterfaceInventory: obs.RDMAInterfaceInventory,
	}
	obs.Reconciler = rec

	notifier, release := rec.getReconcileNotifier(rdmaFamilyName, workercore.DeviceAllocationModeExclusive)
	defer release()

	_, err := reconcileSelection(t, obs)
	require.NoError(t, err)
	assert.Equal(t, 1, spy.patches)

	select {
	case <-notifier:
	default:
		t.Fatal("applying a changed RDMA selection must notify subscribed RDMA streams immediately")
	}

	got := observationStoredDevices(t, obs)
	require.NotNil(t, got.Status.Interfaces)
	require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy)
	wantRef := &workercore.DevicesRDMASelectionPolicyReference{
		Name: "compute-hcas", UID: "uid-a", Generation: 3,
	}
	assert.True(t, kubemeta.DeepEqual(wantRef, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef))
	assert.Equal(t, []workercore.DevicesRDMAExclusion{
		{Name: "efa0", Reason: "ExternallyOwned"},
		{Name: "mlx5_5", Reason: "ExcludedByPolicy"},
	}, got.Status.Interfaces.RDMASelectionPolicy.Excluded)

	resolvedCond := observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType)
	require.NotNil(t, resolvedCond)
	assert.Equal(t, meta.ConditionTrue, resolvedCond.Status)
	assert.Equal(t, RDMASelectionPolicyResolvedReason, resolvedCond.Reason)
	assert.Nil(t, observerCondition(got.Status.Conditions, "RDMASelectionApplied"),
		"the selection has exactly one condition; an applied condition never reappears")

	srv := rdmaTestServer(t, workercore.DeviceAllocationModeExclusive, rec)
	adv, err := srv.getListAndWatchResponse(ctx)
	require.NoError(t, err)
	require.Len(t, adv.Devices, 3)
	healthByID := map[string]string{}
	for _, d := range adv.Devices {
		healthByID[d.ID] = d.Health
	}
	assert.Equal(t, map[string]string{
		"ib0:ib0:0000": "Healthy",
		"ib1:ib1:0000": "Healthy",
		"ib2:ib2:0000": "Unhealthy",
	}, healthByID)

	// An invalid policy edit fails closed: the reference moves to the offending policy's new
	// generation, the exclusions clear, the resolved condition reads False with
	// PolicyInvalid, and every HCA the previously applied selection served goes unhealthy.
	invalid := policy.DeepCopy()
	invalid.Generation = 4
	invalid.Spec.Include = []string{"mlx5_99"}
	require.NoError(t, obs.Client.Update(ctx, invalid))

	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)

	got = observationStoredDevices(t, obs)
	assert.Equal(t, int64(4), got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.Generation,
		"an invalid resolution names the offending policy's reference")
	assert.Empty(t, got.Status.Interfaces.RDMASelectionPolicy.Excluded,
		"an invalid resolution clears the stored exclusions instead of keeping them stale")

	resolvedCond = observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType)
	require.NotNil(t, resolvedCond)
	assert.Equal(t, meta.ConditionFalse, resolvedCond.Status)
	assert.Equal(t, RDMASelectionPolicyInvalidReason, resolvedCond.Reason)
	assert.Nil(t, observerCondition(got.Status.Conditions, "RDMASelectionApplied"))

	advAfterInvalid, err := srv.getListAndWatchResponse(ctx)
	require.NoError(t, err)
	require.Len(t, advAfterInvalid.Devices, 3)
	for _, d := range advAfterInvalid.Devices {
		assert.Equal(t, "Unhealthy", d.Health,
			"an invalid resolution withholds every HCA the previously applied selection served")
	}

	// Deleting the policy resolves the default: the reference is cleared, only externally
	// owned exclusions remain, and every interface returns to Healthy.
	require.NoError(t, obs.Client.Delete(ctx, invalid))
	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)

	got = observationStoredDevices(t, obs)
	assert.Nil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
	assert.Equal(t, []workercore.DevicesRDMAExclusion{
		{Name: "efa0", Reason: "ExternallyOwned"},
	}, got.Status.Interfaces.RDMASelectionPolicy.Excluded)
	resolvedCond = observerCondition(got.Status.Conditions, RDMASelectionResolvedConditionType)
	require.NotNil(t, resolvedCond)
	assert.Equal(t, meta.ConditionTrue, resolvedCond.Status)
	assert.Equal(t, RDMASelectionNoPolicyReason, resolvedCond.Reason)

	advDefault, err := srv.getListAndWatchResponse(ctx)
	require.NoError(t, err)
	for _, d := range advDefault.Devices {
		assert.Equal(t, "Healthy", d.Health)
	}
}

// TestRDMASelectionObserver_StatusPublishFailureStillAppliesSelection pins the fail-closed
// order inside one pass: the resolved selection is applied to the wired reconciler BEFORE the
// status patch is attempted, so a status write that fails still withholds the HCAs the
// resolution excludes. An applied-last bug would leave every non-external HCA eligible for as
// long as the write kept failing, which is the fail-open this test exists to refuse.
func TestRDMASelectionObserver_StatusPublishFailureStillAppliesSelection(t *testing.T) {
	ctx := context.Background()
	devs := observationDevices()
	devs.Spec.Interfaces = []workercore.DeviceInterface{
		wholeFunctionIface("ib0", "0", "mlx5_0", nil),
		wholeFunctionIface("ib1", "0", "mlx5_4", nil),
		wholeFunctionIface("ib2", "1", "mlx5_5", nil),
	}
	policy := selectionPolicy("compute-hcas", "uid-a", 3, func(p *workercore.DevicesRDMASelectionPolicy) {
		p.Spec.Exclude = []string{"mlx5_5"}
	})
	obs, spy := newSelectionObserver(t, selectionInventory(), &observerClientOptions{}, devs, selectionNode(), policy)
	rec := &DevicesReconciler{
		NodeName:               selectionNodeName,
		Client:                 obs.Client,
		RDMAInterfaceInventory: obs.RDMAInterfaceInventory,
	}
	obs.Reconciler = rec

	spy.failWrite = true
	_, err := reconcileSelection(t, obs)
	require.Error(t, err, "a failed status write must be reported, not claimed as settled")
	assert.ErrorContains(t, err, "api server")
	assert.Equal(t, 1, spy.patches, "the pass attempts its one status patch even though it fails")

	// Nothing was published: the failed patch leaves the stored status without a selection.
	got := observationStoredDevices(t, obs)
	assert.Nil(t, got.Status.Interfaces,
		"the failed pass publishes nothing, so the selection below can only be the applied one")

	// The advertisement already reflects the applied selection: the excluded HCA is unhealthy
	// and the two selected ones healthy, though no status ever landed.
	srv := rdmaTestServer(t, workercore.DeviceAllocationModeExclusive, rec)
	adv, err := srv.getListAndWatchResponse(ctx)
	require.NoError(t, err)
	require.Len(t, adv.Devices, 3)
	healthByID := map[string]string{}
	for _, d := range adv.Devices {
		healthByID[d.ID] = d.Health
	}
	assert.Equal(t, map[string]string{
		"ib0:ib0:0000": "Healthy",
		"ib1:ib1:0000": "Healthy",
		"ib2:ib2:0000": "Unhealthy",
	}, healthByID)

	// A later pass whose write succeeds settles the stored status on the selection already
	// applied, so the recovered status and the enforced selection cannot disagree.
	spy.failWrite = false
	_, err = reconcileSelection(t, obs)
	require.NoError(t, err)
	got = observationStoredDevices(t, obs)
	require.NotNil(t, got.Status.Interfaces)
	require.NotNil(t, got.Status.Interfaces.RDMASelectionPolicy.PolicyRef)
	assert.Equal(t, "uid-a", got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.UID)
}
