package api

import (
	"bytes"
	"context"
	"fmt"
	"time"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	apireg "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubeclientset/review"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

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
		_, err = kubeclientset.Update(ctx, crdCli, crds[i],
			kubeclientset.WithCreateIfNotExisted[*apiext.CustomResourceDefinition]())
		if err != nil {
			return fmt.Errorf("install custom resource definition %q: %w",
				crds[i].GetName(), err)
		}
	}

	return nil
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

// WaitForServicesReady waits for the api services to be ready.
func WaitForServicesReady(ctx context.Context, cli kubernetes.Interface, getters []ServiceGetter) error {
	svcCli := cli.ApiregistrationV1().APIServices()
	svcs := MergeServices(apireg.ServiceReference{}, nil, getters)

	return waitx.PollUntilContextTimeout(ctx, 2*time.Second, 30*time.Second, false,
		func(ctx context.Context) error {
			for i := range svcs {
				svc, err := svcCli.Get(ctx, svcs[i].Name, meta.GetOptions{ResourceVersion: "0"})
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
		})
}
