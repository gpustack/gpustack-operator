package kuberess

import (
	"context"
	"fmt"
	"strings"
	"time"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"gpustack.ai/gpustack/pkg/kubeapp/helm"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

// kueueAPIGroup is the API group of every Kueue CRD.
const kueueAPIGroup = "kueue.x-k8s.io"

// kueueReleaseNames are the Helm releases that may own a Kueue this operator installed:
// the bundled chart the worker installs today, and the standalone Kueue release earlier
// versions installed. A Kueue belonging to any other release is a user's own and is left
// alone — chart mode never reaches this code, since a chart-deployed worker installs no
// application at all.
var kueueReleaseNames = []string{
	gpustackOperatorReleaseName,
	"gpustack-kueue",
}

// reapOrphanedKueue clears the state a torn-down Kueue controller leaves behind so
// the (re)install can recreate its CRDs. Ordering is load-bearing: the Kueue
// validating webhook (failurePolicy: Fail) rejects a finalizer-clearing update once
// its Service has no endpoints, so the webhook configurations must be deleted before
// the finalizers are stripped. No-op on a healthy cluster (acts only when a
// kueue.x-k8s.io CRD is Terminating). Safe to re-run.
func reapOrphanedKueue(ctx context.Context, helmCli *helm.Client) error {
	restCfg, err := helmCli.KubeRestClientGetter().ToRESTConfig()
	if err != nil {
		return fmt.Errorf("get rest config: %w", err)
	}
	dynCli, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	cli := helmCli.KubeClientSet()
	reaped, err := reapOrphanedKueueWith(ctx, cli, dynCli)
	if err != nil {
		return err
	}
	if !reaped {
		return nil
	}

	// Wait for the freed CRDs to drain so the install below recreates them cleanly.
	return waitKueueCRDsDrained(ctx, cli, 90*time.Second, 3*time.Second)
}

// reapOrphanedKueueWith is the testable core of reapOrphanedKueue. It reports
// whether it acted (a stuck Kueue CRD was found) so the caller can wait for the
// drain only when there is something to drain.
func reapOrphanedKueueWith(ctx context.Context, cli kubernetes.Interface, dynCli dynamic.Interface) (bool, error) {
	stuck, err := listTerminatingKueueCRDs(ctx, cli)
	if err != nil {
		return false, err
	}
	if len(stuck) == 0 {
		return false, nil
	}

	// 1. Delete the Kueue admission webhook configurations first — see the ordering
	//    note on reapOrphanedKueue.
	if err := deleteKueueWebhookConfigs(ctx, cli); err != nil {
		return true, err
	}

	// 2. Strip the finalizers pinning the Terminating CRs so each stuck CRD can drain.
	for i := range stuck {
		if err := stripKueueCRFinalizers(ctx, dynCli, &stuck[i]); err != nil {
			return true, err
		}
	}

	return true, nil
}

// listTerminatingKueueCRDs returns the Kueue CRDs that are stuck Terminating.
func listTerminatingKueueCRDs(ctx context.Context, cli kubernetes.Interface) ([]apiext.CustomResourceDefinition, error) {
	list, err := cli.ApiextensionsV1().CustomResourceDefinitions().List(ctx, meta.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list CRDs: %w", err)
	}

	var stuck []apiext.CustomResourceDefinition
	for i := range list.Items {
		crd := list.Items[i]
		if crd.Spec.Group == kueueAPIGroup && crd.DeletionTimestamp != nil {
			stuck = append(stuck, crd)
		}
	}
	return stuck, nil
}

// deleteKueueWebhookConfigs deletes the Kueue admission webhook configurations. They are
// selected by the Helm release-instance label rather than by name: the chart sets
// fullnameOverride=kueue, so their names are kueue-* whoever installed them, and only the
// release tells this operator's Kueue apart from one a user brought themselves.
func deleteKueueWebhookConfigs(ctx context.Context, cli kubernetes.Interface) error {
	sel := fmt.Sprintf("app.kubernetes.io/instance in (%s)", strings.Join(kueueReleaseNames, ","))
	listOpts := meta.ListOptions{LabelSelector: sel}
	reg := cli.AdmissionregistrationV1()

	if err := reg.ValidatingWebhookConfigurations().DeleteCollection(ctx, meta.DeleteOptions{}, listOpts); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete kueue validating webhook configurations: %w", err)
	}
	if err := reg.MutatingWebhookConfigurations().DeleteCollection(ctx, meta.DeleteOptions{}, listOpts); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete kueue mutating webhook configurations: %w", err)
	}
	return nil
}

// stripKueueCRFinalizers clears the finalizers on every Terminating custom resource
// of the given Kueue CRD so the CRD can finish deleting. Non-Terminating CRs are left
// untouched, so a live Kueue's queues keep their accounting finalizer.
func stripKueueCRFinalizers(ctx context.Context, dynCli dynamic.Interface, crd *apiext.CustomResourceDefinition) error {
	gvr := schema.GroupVersionResource{
		Group:    crd.Spec.Group,
		Version:  storageCRDVersion(crd),
		Resource: crd.Spec.Names.Plural,
	}

	list, err := dynCli.Resource(gvr).List(ctx, meta.ListOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("list %s: %w", gvr.Resource, err)
	}

	const patch = `{"metadata":{"finalizers":[]}}`
	for i := range list.Items {
		obj := list.Items[i]
		if obj.GetDeletionTimestamp() == nil || len(obj.GetFinalizers()) == 0 {
			continue
		}
		// An empty namespace addresses cluster-scoped resources (e.g. ClusterQueue).
		_, err := dynCli.Resource(gvr).Namespace(obj.GetNamespace()).
			Patch(ctx, obj.GetName(), types.MergePatchType, []byte(patch), meta.PatchOptions{})
		if err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("strip finalizers on %s/%s: %w", gvr.Resource, obj.GetName(), err)
		}
	}
	return nil
}

// storageCRDVersion returns the CRD's storage version, falling back to the first
// served then the first declared version. The storage version is listed and patched
// without conversion; a non-storage served version is materialized through the CRD's
// conversion webhook, which fails once that webhook's backing Service is gone (the
// exact state this reaper runs in).
func storageCRDVersion(crd *apiext.CustomResourceDefinition) string {
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Storage {
			return crd.Spec.Versions[i].Name
		}
	}
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Served {
			return crd.Spec.Versions[i].Name
		}
	}
	if len(crd.Spec.Versions) > 0 {
		return crd.Spec.Versions[0].Name
	}
	return ""
}

// waitKueueCRDsDrained blocks until no Kueue CRD is Terminating, bounded by timeout.
func waitKueueCRDsDrained(ctx context.Context, cli kubernetes.Interface, timeout, interval time.Duration) error {
	return waitx.PollUntilContextTimeout(ctx, interval, timeout, false,
		func(ctx context.Context) error {
			stuck, err := listTerminatingKueueCRDs(ctx, cli)
			if err != nil {
				return err
			}
			if len(stuck) > 0 {
				return fmt.Errorf("%d kueue CRD(s) still terminating", len(stuck))
			}
			return nil
		})
}
