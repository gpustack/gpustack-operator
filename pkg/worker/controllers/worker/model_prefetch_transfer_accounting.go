package worker

import (
	"context"
	"math"

	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/setting"
)

// observeSourceCharges counts retained reservations independently of the current
// desired placement, artifact and intent revision. Readiness uses stricter joins.
func (r *ModelPrefetchReconciler) observeSourceCharges(ctx context.Context, pf *workercore.ModelPrefetch, obs *sourceObservation) error {
	if pf.UID == "" {
		return nil
	}
	bindings := new(workercore.ModelStoreBindingList)
	if err := r.reader().List(ctx, bindings, ctrlcli.InNamespace(pf.Namespace)); err != nil {
		return err
	}
	verified := false
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		if binding.Annotations[broker.AuthorityAnnotation] == "" {
			continue
		}
		entries, err := broker.ReadPinnedAuthority(ctx, r.reader(), setting.DelegatedSecretNamespace, binding)
		if err != nil {
			obs.authorityUnknown = true
			continue
		}
		verified = true
		for _, entry := range entries {
			if entry.Namespace != pf.Namespace || entry.IntentUID != string(pf.UID) {
				continue
			}
			if entry.Bytes < 0 || obs.charge > math.MaxInt64-entry.Bytes {
				obs.authorityUnknown = true
				continue
			}
			obs.charge += entry.Bytes
			obs.admitted = true
		}
	}
	if !verified && pf.Status.Transfer != nil && pf.Status.Transfer.ReservedBytes > 0 {
		obs.authorityUnknown = true
	}
	if obs.authorityUnknown && pf.Status.Transfer != nil && obs.charge < pf.Status.Transfer.ReservedBytes {
		// Losing authority is not release proof. Preserve the last observed charge.
		obs.charge = pf.Status.Transfer.ReservedBytes
		obs.admitted = obs.charge > 0
	}
	return nil
}
