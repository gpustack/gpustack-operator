package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/setting"
)

func TestModelStoreBindingMeasuresAuthorityCharges(t *testing.T) {
	for _, tc := range []struct {
		name                                           string
		charges                                        []int64
		state                                          string
		missing, mismatched, foreignNamespace, noGrant bool
		want                                           int64
		prior                                          int64
		unknown                                        bool
	}{
		{name: "window and runtime overhead", charges: []int64{(5 << 30) + (128 << 20)}, want: (5 << 30) + (128 << 20)},
		{name: "window smaller than checkpoint", charges: []int64{1 << 30}, want: 1 << 30},
		{name: "multiple retained generations", charges: []int64{17, 23}, want: 40},
		{name: "filling reservation", charges: []int64{41}, state: "reserved", want: 41},
		{name: "drained reservation still retained", charges: []int64{43}, state: "drained", want: 43},
		{name: "grant section removed", charges: []int64{47}, noGrant: true, want: 47},
		{name: "released reservation", charges: nil},
		{name: "unknown retains last charge", missing: true, unknown: true, prior: 17, want: 17},
		{name: "verified release clears last charge", prior: 17},
		{name: "missing authority", charges: []int64{13}, missing: true, unknown: true},
		{name: "replaced authority", charges: []int64{13}, mismatched: true, unknown: true},
		{name: "wrong namespace", charges: []int64{13}, foreignNamespace: true, unknown: true},
		{name: "sum overflow", charges: []int64{math.MaxInt64, 1}, unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := testSourceGrantBinding()
			binding.UID = "binding-uid"
			if tc.prior > 0 {
				binding.Status.ReservedMemory = resource.NewQuantity(tc.prior, resource.BinarySI)
			}
			if tc.noGrant {
				binding.Spec.SourceMemory = nil
			}
			epoch := strings.Repeat("a", 64)
			pin, err := json.Marshal(broker.AuthorityPin{UID: "authority-uid", Epoch: epoch})
			require.NoError(t, err)
			binding.Annotations = map[string]string{broker.AuthorityAnnotation: string(pin)}
			entries := make([]broker.ReservationEntry, 0, len(tc.charges))
			for i, charge := range tc.charges {
				state := tc.state
				if state == "" {
					state = broker.ReservationStateAdmitted
				}
				namespace := binding.Namespace
				if tc.foreignNamespace {
					namespace = "other"
				}
				entries = append(entries, broker.ReservationEntry{
					ReservationID: fmt.Sprintf("reservation-%d", i), NodeScope: "nvidia", LedgerIdentity: "ledger",
					Generation: fmt.Sprintf("generation-%d", i), IntentUID: "deleted-prefetch-uid", IntentGeneration: 1,
					Digest: testDigest, Namespace: namespace, Bytes: charge, Registrations: 1, State: state,
				})
			}
			raw, err := json.Marshal(map[string]any{"apiVersion": 1, "bindingUID": string(binding.UID), "epoch": epoch, "entries": entries})
			require.NoError(t, err)
			cm := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{
				Name: broker.AuthorityName(binding.UID), Namespace: setting.DelegatedSecretNamespace, UID: "authority-uid",
			}, Data: map[string]string{"reservations.json": string(raw)}}
			if tc.mismatched {
				cm.UID = "another-authority"
			}
			objs := []ctrlcli.Object{binding, testModelStore("h100", nil, nil)}
			if !tc.missing {
				objs = append(objs, cm)
			}
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithStatusSubresource(&workercore.ModelStoreBinding{}).WithObjects(objs...).Build()
			r := &ModelStoreBindingReconciler{Client: cli, APIReader: cli}
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(binding)})
			require.NoError(t, err)
			require.Positive(t, result.RequeueAfter, "authority changes remain observable without node-report churn")
			got := new(workercore.ModelStoreBinding)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(binding), got))
			if tc.want == 0 {
				assert.Nil(t, got.Status.ReservedMemory)
				return
			}
			require.NotNil(t, got.Status.ReservedMemory)
			assert.Equal(t, tc.want, got.Status.ReservedMemory.Value())
		})
	}
}

func TestBindingGrantPredicateObservesDeletionStart(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		deleting, wasDeleting, changed bool
	}{
		{name: "status only"},
		{name: "deletion starts", deleting: true, changed: true},
		{name: "already deleting", deleting: true, wasDeleting: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := testSourceGrantBinding()
			current := old.DeepCopy()
			now := meta.Now()
			if tc.wasDeleting {
				old.DeletionTimestamp = &now
			}
			if tc.deleting {
				current.DeletionTimestamp = &now
			}
			assert.Equal(t, tc.changed, bindingGrantChanged.Update(ctrlevent.UpdateEvent{ObjectOld: old, ObjectNew: current}))
		})
	}
}
