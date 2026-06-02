package systemkuberess

import (
	"context"
	"fmt"
	"slices"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubeclientset/review"
	"gpustack.ai/gpustack/pkg/system"
)

// InstallFakeSystemRoutingService creates the fake routing service/endpoint for system.
//
// The service points to the SelfIP of the system.
func InstallFakeSystemRoutingService(ctx context.Context, cli kubernetes.Interface, nsName, svcName string, port int32) error {
	err := review.CanDoCreate(ctx,
		cli.AuthorizationV1().SelfSubjectAccessReviews(),
		review.Simples{
			{
				Group:    core.SchemeGroupVersion.Group,
				Version:  core.SchemeGroupVersion.Version,
				Resource: "services",
			},
			{
				Group:    core.SchemeGroupVersion.Group,
				Version:  core.SchemeGroupVersion.Version,
				Resource: "endpoints",
			},
		},
		review.WithRecreateIfDuplicated(),
		review.WithUpdateIfExisted(),
	)
	if err != nil {
		return err
	}

	svcCli := cli.CoreV1().Services(nsName)
	eSvc := &core.Service{
		ObjectMeta: meta.ObjectMeta{
			Name: svcName,
			Labels: map[string]string{
				"gpustack.ai/fake-routing": "true",
			},
		},
		Spec: core.ServiceSpec{
			Type: core.ServiceTypeClusterIP,
			Ports: []core.ServicePort{
				{
					Name: "https",
					Port: port,
				},
			},
		},
	}
	svcCompareFn := func(aSvc *core.Service) bool {
		return aSvc.Spec.Type == eSvc.Spec.Type &&
			aSvc.Spec.ClusterIP == eSvc.Spec.ClusterIP &&
			slices.ContainsFunc(aSvc.Spec.Ports, func(ap core.ServicePort) bool {
				return ap.Port == eSvc.Spec.Ports[0].Port
			})
	}

	eSvc, err = kubeclientset.Create(ctx, svcCli, eSvc,
		kubeclientset.WithRecreateIfDuplicated(svcCompareFn))
	if err != nil {
		return fmt.Errorf("install fake rounting service %q: %w", eSvc.GetName(), err)
	}

	epCli := cli.CoreV1().Endpoints(nsName)
	eEp := &core.Endpoints{
		ObjectMeta: meta.ObjectMeta{
			Name: eSvc.GetName(),
			Labels: map[string]string{
				"gpustack.ai/fake-routing": "true",
			},
		},
		Subsets: []core.EndpointSubset{
			{
				Addresses: []core.EndpointAddress{
					{
						IP: system.PrimaryIP.Get(),
					},
				},
				Ports: []core.EndpointPort{
					{
						Name: "https",
						Port: port,
					},
				},
			},
		},
	}
	epAlignFn := func(aEp *core.Endpoints) (*core.Endpoints, bool, error) {
		var found bool
		for i := range aEp.Subsets {
			for j := range aEp.Subsets[i].Addresses {
				if aEp.Subsets[i].Addresses[j].IP == eEp.Subsets[0].Addresses[0].IP {
					found = true
					break
				}
			}
			if found {
				found = false
				for j := range aEp.Subsets[i].Ports {
					if aEp.Subsets[i].Ports[j].Port == eEp.Subsets[0].Ports[0].Port {
						found = true
						break
					}
				}
			}
			if found {
				break
			}
		}
		if found {
			return nil, true, nil
		}

		// Append the existing subsets.
		aEp.Subsets = append(aEp.Subsets, eEp.Subsets...)
		return aEp, false, nil
	}

	_, err = kubeclientset.Create(ctx, epCli, eEp,
		kubeclientset.WithUpdateIfExisted(epAlignFn))
	if err != nil {
		return fmt.Errorf("install fake routing service %q: %w", eEp.GetName(), err)
	}

	return nil
}
