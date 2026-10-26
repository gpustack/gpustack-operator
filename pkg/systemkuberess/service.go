package systemkuberess

import (
	"context"
	"fmt"
	"slices"

	authz "k8s.io/api/authorization/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubeclientset/review"
	"gpustack.ai/gpustack/pkg/kubereviewself"
	"gpustack.ai/gpustack/pkg/system"
)

const (
	// routingLabel marks the resources that carry the system routing service, so that
	// they can be told apart from the resources of the cluster itself.
	routingLabel = "gpustack.ai/fake-routing"

	// routingLabelValue is the value of the routingLabel.
	routingLabelValue = "true"

	// routingPortName is the name of the routing port. The service, the legacy
	// endpoints and the endpoint slice all name it the same, because the aggregator
	// matches the port of a slice against the name of the service port it serves.
	routingPortName = "https"
)

// InstallFakeSystemRoutingService creates the fake routing service/endpoint for system.
//
// The service points to the SelfIP of the system.
func InstallFakeSystemRoutingService(ctx context.Context, cli kubernetes.Interface, nsName, svcName string, port int32) error {
	return installFakeSystemRoutingService(ctx, cli, nsName, svcName, system.PrimaryIP.Get(), port)
}

// installFakeSystemRoutingService creates the fake routing service/endpoint for system.
//
// The address is the address of the system the service routes to. It is a parameter, and
// not read from the system package, so that the desired resources can be built and
// checked without the process wide state they normally come from.
func installFakeSystemRoutingService(ctx context.Context, cli kubernetes.Interface, nsName, svcName, address string, port int32) error {
	err := checkRoutingAccess(ctx, cli, nsName)
	if err != nil {
		return err
	}

	// The address decides the family of the endpoint slice, so it is checked before
	// anything is written.
	desiredSlice, err := newRoutingEndpointSlice(nsName, svcName, address, port)
	if err != nil {
		return err
	}

	svc, err := installRoutingService(ctx, cli, nsName, svcName, port, desiredSlice.AddressType)
	if err != nil {
		return err
	}

	ep, err := installRoutingEndpoints(ctx, cli, nsName, svcName, address, port)
	if err != nil {
		return err
	}

	err = installRoutingEndpointSlice(ctx, cli, nsName, desiredSlice, svc)
	if err != nil {
		return err
	}

	return cleanupObsoleteRoutingSlices(ctx, cli, nsName, svc, ep, desiredSlice.GetName())
}

// checkRoutingAccess reviews the operations the routing reconcile performs.
//
// The service and the legacy endpoints keep the operations they have always needed. The
// endpoint slice is reviewed on its own, because the operations of the other two do not
// cover the list the cleanup of the obsolete slices reads.
func checkRoutingAccess(ctx context.Context, cli kubernetes.Interface, nsName string) error {
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

	verbs := []string{"get", "list", "create", "update", "delete"}
	reviews := make(kubereviewself.Reviews, 0, len(verbs)+1)
	for _, verb := range verbs {
		reviews = append(reviews, kubereviewself.Review{
			ResourceAttributes: &authz.ResourceAttributes{
				Verb:      verb,
				Group:     discovery.SchemeGroupVersion.Group,
				Version:   discovery.SchemeGroupVersion.Version,
				Resource:  "endpointslices",
				Namespace: nsName,
			},
		})
	}

	// The owner reference of the slice blocks the deletion of the routing service,
	// so the api server asks for the permission to update the finalizers of the
	// service before it accepts the slice.
	reviews = append(reviews, kubereviewself.Review{
		ResourceAttributes: &authz.ResourceAttributes{
			Verb:        "update",
			Group:       core.SchemeGroupVersion.Group,
			Version:     core.SchemeGroupVersion.Version,
			Resource:    "services",
			Subresource: "finalizers",
			Namespace:   nsName,
		},
	})

	return kubereviewself.CanDo(ctx, cli.AuthorizationV1().SelfSubjectAccessReviews(), reviews)
}

// installRoutingService creates the selectorless service the extension api services are
// fronted by, and returns it as the cluster holds it.
func installRoutingService(ctx context.Context, cli kubernetes.Interface, nsName, svcName string, port int32,
	addressType discovery.AddressType,
) (*core.Service, error) {
	expected := newRoutingService(nsName, svcName, port)
	svcCompareFn := func(actual *core.Service) bool {
		if actual.Spec.Type != expected.Spec.Type {
			return false
		}
		// The cluster ip is compared only when the wanted service asks for one.
		// The operator does not manage it, and the api server allocates one for
		// the service of the cluster, so comparing it against the empty ip of the
		// wanted service would delete and recreate the service on every start.
		// The recreation takes the endpoint slice of this installation with it,
		// because the slice is a dependent of the service that blocks its
		// deletion, and it changes the address of the routing as well.
		if expected.Spec.ClusterIP != "" && actual.Spec.ClusterIP != expected.Spec.ClusterIP {
			return false
		}
		if actual.Spec.ClusterIP != "" && actual.Spec.ClusterIP != core.ClusterIPNone {
			if actualType, err := routingAddressType(actual.Spec.ClusterIP); err == nil && actualType != addressType {
				return false
			}
		}
		return slices.ContainsFunc(actual.Spec.Ports, func(ap core.ServicePort) bool {
			return ap.Port == expected.Spec.Ports[0].Port
		})
	}

	actual, err := kubeclientset.Create(ctx, cli.CoreV1().Services(nsName), expected,
		kubeclientset.WithRecreateIfDuplicated(svcCompareFn))
	if err != nil {
		return nil, fmt.Errorf("install fake routing service %q: %w", expected.GetName(), err)
	}

	return actual, nil
}

// newRoutingService builds the desired service of the system routing.
//
// The service has no selector, so no controller picks its endpoints for it, and the
// endpoint slice below is the only thing that routes to the system.
func newRoutingService(nsName, svcName string, port int32) *core.Service {
	return &core.Service{
		ObjectMeta: meta.ObjectMeta{
			Namespace: nsName,
			Name:      svcName,
			Labels: map[string]string{
				routingLabel: routingLabelValue,
			},
		},
		Spec: core.ServiceSpec{
			Type: core.ServiceTypeClusterIP,
			Ports: []core.ServicePort{
				{
					Name: routingPortName,
					Port: port,
				},
			},
		},
	}
}

// installRoutingEndpoints creates the legacy endpoints of the system routing, and returns
// them as the cluster holds them.
//
// The legacy endpoints are kept for the aggregators that still read them, which are the
// ones older than the Kubernetes release that moved the aggregator over to endpoint
// slices. The skip mirror label keeps the mirroring controller from making a second
// slice out of them, because the aggregator may then pick either of the two.
func installRoutingEndpoints(ctx context.Context, cli kubernetes.Interface, nsName, svcName, address string, port int32) (*core.Endpoints, error) {
	expected := newRoutingEndpoints(nsName, svcName, address, port)
	epAlignFn := func(actual *core.Endpoints) (*core.Endpoints, bool, error) {
		aligned := actual.DeepCopy()
		aligned.SetLabels(mergeLabels(actual.GetLabels(), expected.GetLabels()))
		// The address and the port of the legacy endpoints are replaced, not
		// appended, so that a system that moved to another address does not keep
		// the old one.
		aligned.Subsets = expected.Subsets

		if apiequality.Semantic.DeepEqual(actual.GetLabels(), aligned.GetLabels()) &&
			apiequality.Semantic.DeepEqual(actual.Subsets, aligned.Subsets) {
			return actual, true, nil
		}
		return aligned, false, nil
	}

	actual, err := kubeclientset.Create(ctx, cli.CoreV1().Endpoints(nsName), expected,
		kubeclientset.WithUpdateIfExisted(epAlignFn))
	if err != nil {
		return nil, fmt.Errorf("install fake routing endpoints %q: %w", expected.GetName(), err)
	}

	return actual, nil
}

// newRoutingEndpoints builds the desired legacy endpoints of the system routing.
func newRoutingEndpoints(nsName, svcName, address string, port int32) *core.Endpoints {
	return &core.Endpoints{
		ObjectMeta: meta.ObjectMeta{
			Namespace: nsName,
			Name:      svcName,
			Labels: map[string]string{
				routingLabel:              routingLabelValue,
				discovery.LabelSkipMirror: "true",
			},
		},
		Subsets: []core.EndpointSubset{
			{
				Addresses: []core.EndpointAddress{
					{
						IP: address,
					},
				},
				Ports: []core.EndpointPort{
					{
						Name: routingPortName,
						Port: port,
					},
				},
			},
		},
	}
}

// mergeLabels returns the labels of an object with the wanted ones applied on top, so
// that the labels another actor added are kept and the wanted ones always win.
func mergeLabels(actual, wanted map[string]string) map[string]string {
	if len(actual) == 0 && len(wanted) == 0 {
		return nil
	}

	merged := make(map[string]string, len(actual)+len(wanted))
	for k, v := range actual {
		merged[k] = v
	}
	for k, v := range wanted {
		merged[k] = v
	}
	return merged
}
