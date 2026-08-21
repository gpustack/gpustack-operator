package api

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"go.uber.org/multierr"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"
	apireg "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubeclientset/review"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

// ensureInterval is how often EnsureCRDs and EnsureServices look for a missing object.
const ensureInterval = 30 * time.Second

type (
	// CRDGetter is the function type for getting custom resource definitions.
	CRDGetter func() map[string]*apiext.CustomResourceDefinition

	// ServiceGetter is the function type for getting api services.
	ServiceGetter func(apireg.ServiceReference, []byte) *apireg.APIService
)

// MergeCRDs merges the CRD from the getters and returns in one list.
func MergeCRDs(crdGetters []CRDGetter) []*apiext.CustomResourceDefinition {
	// Merge all the CRDs from the getters.
	var (
		ret = make([]map[string]*apiext.CustomResourceDefinition, len(crdGetters))
		csc int
	)
	for i, get := range crdGetters {
		ret[i] = get()
		csc += len(ret[i])
	}

	crds := make([]*apiext.CustomResourceDefinition, 0, csc)
	for i := range ret {
		if ret[i] == nil {
			continue
		}
		for _, n := range sets.List(sets.KeySet(ret[i])) {
			crds = append(crds, ret[i][n])
		}
	}

	return crds
}

// InstallCRDs installs the custom resource definitions.
func InstallCRDs(ctx context.Context, cli kubernetes.Interface, crdGetters []CRDGetter) error {
	err := review.CanDoUpdate(ctx,
		cli.AuthorizationV1().SelfSubjectAccessReviews(),
		review.Simples{
			{
				Group:    apiext.SchemeGroupVersion.Group,
				Version:  apiext.SchemeGroupVersion.Version,
				Resource: "customresourcedefinitions",
			},
		},
		review.WithCreateIfNotExisted(),
	)
	if err != nil {
		return err
	}

	crdCli := cli.ApiextensionsV1().CustomResourceDefinitions()

	crds := MergeCRDs(crdGetters)
	for i := range crds {
		eCRD := crds[i]
		// The align function is what makes a losing writer retry instead of returning the
		// conflict: every replica installs the definitions before leader election, so
		// without it a concurrent boot fails one of them.
		var terminating bool
		crdAlignFn := func(aCRD *apiext.CustomResourceDefinition) (_ *apiext.CustomResourceDefinition, skip bool, err error) {
			skip = true
			// A terminating definition still serves reads, but the api server drops it as soon as
			// its custom resources drain, so writing to it aligns something that is about to be
			// gone. Leave it and report it as not installed.
			if aCRD.GetDeletionTimestamp() != nil {
				terminating = true
				return aCRD, skip, err
			}
			// Align spec.
			if !kubemeta.DeepEqual(aCRD.Spec, eCRD.Spec) {
				aCRD.Spec = *eCRD.Spec.DeepCopy()
				skip = false
			}
			return aCRD, skip, err
		}
		_, err = kubeclientset.Update(ctx, crdCli, eCRD,
			kubeclientset.WithCreateIfNotExisted[*apiext.CustomResourceDefinition](),
			kubeclientset.WithUpdateAlign(crdAlignFn))
		if err != nil {
			return fmt.Errorf("install custom resource definition %q: %w",
				eCRD.GetName(), err)
		}
		if terminating {
			// Not an error: the finalizers holding the deletion are released by controllers that
			// only start once this returns, so failing here keeps the definition we are waiting
			// for alive forever.
			klog.InfoS("terminating custom resource definition, leaving it to drain",
				"crd", eCRD.GetName())
		}
	}

	return nil
}

// EnsureCRDs restores the custom resource definitions that go missing, until the given context is
// done. Every failed attempt is logged, and what it returns once the context is done is that
// cancellation or, where an attempt raced it, the last failure recorded — so a caller reads the
// context before it reads the error as a failure.
//
// Installing once at boot is not enough. A definition deleted afterwards, or one that was already
// terminating at boot and drained right after, stays gone for the life of the process, and every
// controller watching it fails forever. Run this next to the controllers rather than before them:
// the finalizers holding a terminating definition are released by those same controllers.
//
// EnsureCRDs reviews no permission of its own, and must be able to return for no reason other than
// the context being done: the caller runs it beside the tasks that keep the process alive, so a
// task returning early is a repair loop silently gone. InstallCRDs is what reviews the permission,
// and the boot fails without it.
func EnsureCRDs(ctx context.Context, cli kubernetes.Interface, crdGetters []CRDGetter) error {
	crds := MergeCRDs(crdGetters)

	return waitx.UntilContextCancel(ctx, ensureInterval, false,
		func(ctx context.Context) error {
			err := restoreCRDs(ctx, cli, crds)
			if err != nil {
				// Report every attempt. The poll keeps the failure to itself until the context is
				// done, which for a loop living as long as the process is at shutdown.
				klog.InfoS("retrying to restore the custom resource definitions", "err", err)
			}
			return err
		})
}

// restoreCRDs creates the given definitions where they are absent, and leaves the ones already
// there exactly as they are, terminating or not.
//
// Absence is all it repairs. A rolling update overlaps two replicas even at one replica, so a
// restore that aligned the spec would let the outgoing replica push its own version of every
// definition back over the incoming one, once per interval, for as long as it lives. Aligning the
// spec is what InstallCRDs does, on the boot of the replica that carries that version.
func restoreCRDs(ctx context.Context, cli kubernetes.Interface, crds []*apiext.CustomResourceDefinition) error {
	crdCli := cli.ApiextensionsV1().CustomResourceDefinitions()

	keepFn := func(aCRD *apiext.CustomResourceDefinition) (*apiext.CustomResourceDefinition, bool, error) {
		return aCRD, true, nil
	}
	// Every definition is attempted even after one fails: they are restored in name order, so
	// giving up on the first failure would let one that fails persistently starve every
	// definition behind it.
	var errs []error
	for i := range crds {
		_, err := kubeclientset.Update(ctx, crdCli, crds[i],
			kubeclientset.WithCreateIfNotExisted[*apiext.CustomResourceDefinition](),
			kubeclientset.WithUpdateAlign(keepFn))
		if err != nil {
			errs = append(errs, fmt.Errorf("restore custom resource definition %q: %w",
				crds[i].GetName(), err))
		}
	}

	return multierr.Combine(errs...)
}

// MergeServices merges the API services from the getters and returns in one list.
//
// The given service reference and CA data will be passed to the getters, which can be used to construct the API services.
func MergeServices(svc apireg.ServiceReference, ca []byte, getters []ServiceGetter) []*apireg.APIService {
	ret := make([]*apireg.APIService, 0, len(getters))
	for i := range getters {
		r := getters[i](svc, ca)
		if r != nil {
			ret = append(ret, r)
		}
	}
	return ret
}

// InstallServices installs the api services.
//
// The given service reference and CA data will be passed to the getters, which can be used to construct the API services.
func InstallServices(ctx context.Context, cli kubernetes.Interface, svc apireg.ServiceReference, ca []byte, getters []ServiceGetter) error {
	err := review.CanDoCreate(ctx,
		cli.AuthorizationV1().SelfSubjectAccessReviews(),
		review.Simples{
			{
				Group:    apireg.SchemeGroupVersion.Group,
				Version:  apireg.SchemeGroupVersion.Version,
				Resource: "apiservices",
			},
		},
		review.WithUpdateIfExisted(),
	)
	if err != nil {
		return err
	}

	svcCli := cli.ApiregistrationV1().APIServices()

	svcs := MergeServices(svc, ca, getters)
	for i := range svcs {
		eSvc := svcs[i]
		svcAlignFn := func(aSvc *apireg.APIService) (_ *apireg.APIService, skip bool, err error) {
			skip = true
			// Nothing to update if the service reference is not the same.
			if aSvc.Spec.Service.Namespace != eSvc.Spec.Service.Namespace ||
				aSvc.Spec.Service.Name != eSvc.Spec.Service.Name {
				return aSvc, skip, err
			}
			// Align service port.
			if !ptr.Equal(aSvc.Spec.Service.Port, eSvc.Spec.Service.Port) {
				aSvc.Spec.Service.Port = eSvc.Spec.Service.Port
				skip = false
			}
			// Align CA bundle.
			if !bytes.Equal(aSvc.Spec.CABundle, eSvc.Spec.CABundle) {
				aSvc.Spec.CABundle = eSvc.Spec.CABundle
				skip = false
			}
			// Align InsecureSkipTLSVerify.
			if aSvc.Spec.InsecureSkipTLSVerify != eSvc.Spec.InsecureSkipTLSVerify {
				aSvc.Spec.InsecureSkipTLSVerify = eSvc.Spec.InsecureSkipTLSVerify
				skip = false
			}
			return aSvc, skip, err
		}
		_, err = kubeclientset.Create(ctx, svcCli, eSvc,
			kubeclientset.WithUpdateIfExisted(svcAlignFn))
		if err != nil {
			return fmt.Errorf("install api service %q: %w",
				svcs[i].Name, err)
		}
	}

	return nil
}

// EnsureServices restores the api services that go missing, until the given context is done. Every
// failed attempt is logged, and what it returns once the context is done reads the same way as
// EnsureCRDs': the cancellation, or the last failure recorded where an attempt raced it.
//
// An api service deleted after the boot leaves the aggregated api it fronts answering nothing for
// the life of the process, which is what EnsureCRDs prevents for the definitions. The two carry
// the same constraints: no permission review of its own, no returning for a reason other than the
// context being done, and absence is all that is repaired.
func EnsureServices(ctx context.Context, cli kubernetes.Interface, svc apireg.ServiceReference, ca []byte, getters []ServiceGetter) error {
	svcs := MergeServices(svc, ca, getters)

	return waitx.UntilContextCancel(ctx, ensureInterval, false,
		func(ctx context.Context) error {
			err := restoreServices(ctx, cli, svcs)
			if err != nil {
				klog.InfoS("retrying to restore the api services", "err", err)
			}
			return err
		})
}

// restoreServices creates the given api services where they are absent, and leaves the ones
// already there exactly as they are.
//
// As with restoreCRDs, absence is all it repairs: an outgoing replica of a rolling update runs
// this too, and its service reference and CA bundle are its own. Aligning those is what
// InstallServices does, on the boot of the replica they belong to.
func restoreServices(ctx context.Context, cli kubernetes.Interface, svcs []*apireg.APIService) error {
	svcCli := cli.ApiregistrationV1().APIServices()

	keepFn := func(aSvc *apireg.APIService) (*apireg.APIService, bool, error) {
		return aSvc, true, nil
	}
	// Every service is attempted even after one fails, for the reason restoreCRDs carries.
	var errs []error
	for i := range svcs {
		_, err := kubeclientset.Update(ctx, svcCli, svcs[i],
			kubeclientset.WithCreateIfNotExisted[*apireg.APIService](),
			kubeclientset.WithUpdateAlign(keepFn))
		if err != nil {
			errs = append(errs, fmt.Errorf("restore api service %q: %w",
				svcs[i].GetName(), err))
		}
	}

	return multierr.Combine(errs...)
}

// DeleteServicesBackedBy deletes the api services whose backing service lives in the given
// namespace. This reaches the registrations the process never made: a chart-installed
// aggregated api — Kueue's visibility pair is the one at hand — is cluster-scoped too, so a
// namespace deletion that skips the chart's uninstall leaves it wedging the deletion on
// discovery it can no longer serve, exactly as the process-registered ones do. An absent one
// is already deleted, and every service is attempted even after one fails, for the reason
// restoreCRDs carries.
func DeleteServicesBackedBy(ctx context.Context, cli kubernetes.Interface, namespace string) error {
	svcCli := cli.ApiregistrationV1().APIServices()

	svcs, err := svcCli.List(ctx, meta.ListOptions{})
	if err != nil {
		return fmt.Errorf("list api services: %w", err)
	}

	var errs []error
	for i := range svcs.Items {
		svc := svcs.Items[i].Spec.Service
		if svc == nil || svc.Namespace != namespace {
			continue
		}
		err = svcCli.Delete(ctx, svcs.Items[i].GetName(), meta.DeleteOptions{})
		if err != nil && !kerrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete api service %q: %w",
				svcs.Items[i].GetName(), err))
		}
	}

	return multierr.Combine(errs...)
}

// IsServicesReady reports whether the api services are ready, checking each of them once. The error
// names the first service found unavailable.
//
// Prefer this over WaitForServicesReady when the caller already retries on a schedule of its own, so
// that the two do not stack and the caller's schedule is the rate the services are polled at.
func IsServicesReady(ctx context.Context, cli kubernetes.Interface, getters []ServiceGetter) error {
	svcCli := cli.ApiregistrationV1().APIServices()
	svcs := MergeServices(apireg.ServiceReference{}, nil, getters)

	for i := range svcs {
		svc, err := svcCli.Get(ctx, svcs[i].Name,
			meta.GetOptions{
				ResourceVersion: "0",
			})
		if err != nil {
			return err
		}

		ready := false
		for j := range svc.Status.Conditions {
			if svc.Status.Conditions[j].Type != apireg.Available {
				continue
			}
			ready = svc.Status.Conditions[j].Status == apireg.ConditionTrue
			break
		}
		if !ready {
			return fmt.Errorf("api service %q is not ready", svc.Name)
		}
	}

	return nil
}

// WaitForServicesReady waits for the api services to be ready, polling every 2 seconds and giving up
// after 30 seconds, in which case it returns the last failure of the run.
func WaitForServicesReady(ctx context.Context, cli kubernetes.Interface, getters []ServiceGetter) error {
	return waitx.PollUntilContextTimeout(ctx, 2*time.Second, 30*time.Second, false,
		func(ctx context.Context) error {
			return IsServicesReady(ctx, cli, getters)
		})
}
