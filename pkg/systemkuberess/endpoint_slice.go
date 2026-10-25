package systemkuberess

import (
	"context"
	"fmt"
	"net"

	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

const (
	// routingManagedBy is the manager of the endpoint slice of the system routing.
	//
	// It is a value of its own, and not the one of the endpoint slice controller or
	// the one of the endpoint slice mirroring controller, so that neither of them
	// takes the slice over, and so that the slice of this operator can be told from
	// a mirrored one.
	routingManagedBy = "gpustack-operator-worker"

	// routingEndpointSliceNameSuffix is the suffix of the name of the endpoint slice
	// of the system routing.
	//
	// The slice is named apart from the service it serves, because a mirrored slice
	// of the legacy endpoints is named after the endpoints with a short random
	// suffix, and the two must never be the same object.
	routingEndpointSliceNameSuffix = "-direct"
)

// newRoutingEndpointSlice builds the desired endpoint slice of the system routing.
//
// The aggregator reads the slices that carry the name of the service, and takes the
// first one whose port is named after the service port and whose endpoint is ready.
// The slice therefore names the service, names the port of the service and carries a
// ready endpoint on the address of the system.
func newRoutingEndpointSlice(nsName, svcName, address string, port int32) (*discovery.EndpointSlice, error) {
	addressType, err := routingAddressType(address)
	if err != nil {
		return nil, err
	}

	ready, serving, terminating := true, true, false

	return &discovery.EndpointSlice{
		ObjectMeta: meta.ObjectMeta{
			Namespace: nsName,
			Name:      routingEndpointSliceName(svcName),
			Labels: map[string]string{
				discovery.LabelServiceName: svcName,
				discovery.LabelManagedBy:   routingManagedBy,
				routingLabel:               routingLabelValue,
			},
		},
		AddressType: addressType,
		Endpoints: []discovery.Endpoint{
			{
				Addresses: []string{address},
				Conditions: discovery.EndpointConditions{
					Ready:       &ready,
					Serving:     &serving,
					Terminating: &terminating,
				},
			},
		},
		Ports: []discovery.EndpointPort{
			{
				Name:     ptr.To(routingPortName),
				Port:     ptr.To(port),
				Protocol: ptr.To(core.ProtocolTCP),
			},
		},
	}, nil
}

// routingEndpointSliceName returns the name of the endpoint slice of the system routing.
func routingEndpointSliceName(svcName string) string {
	return svcName + routingEndpointSliceNameSuffix
}

// routingAddressType returns the address type of an endpoint slice that can carry the
// given address.
//
// The aggregator prefers the slices of the primary address family of the service, and
// an endpoint slice only accepts the addresses of its own type, so the type has to
// follow the address.
func routingAddressType(address string) (discovery.AddressType, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return "", fmt.Errorf("routing address %q is not an IP address", address)
	}
	if ip.To4() != nil {
		return discovery.AddressTypeIPv4, nil
	}
	return discovery.AddressTypeIPv6, nil
}

// installRoutingEndpointSlice creates or updates the endpoint slice of the system
// routing.
//
// The address type of an endpoint slice cannot be changed, so a slice that was made for
// the other address family is replaced instead of updated.
func installRoutingEndpointSlice(ctx context.Context, cli kubernetes.Interface, nsName string,
	desired *discovery.EndpointSlice, svc *core.Service,
) error {
	sliceCli := cli.DiscoveryV1().EndpointSlices(nsName)

	actual, err := sliceCli.Get(ctx, desired.GetName(), meta.GetOptions{})
	switch {
	case err == nil:
		if actual.GetLabels()[discovery.LabelManagedBy] != routingManagedBy {
			return fmt.Errorf("routing endpoint slice %q is managed by %q, not by this operator",
				desired.GetName(), actual.GetLabels()[discovery.LabelManagedBy])
		}
		if actual.AddressType != desired.AddressType {
			err = sliceCli.Delete(ctx, desired.GetName(), meta.DeleteOptions{
				PropagationPolicy: ptr.To(meta.DeletePropagationBackground),
			})
			if err != nil && !kerrors.IsNotFound(err) {
				return fmt.Errorf("delete routing endpoint slice %q: %w", desired.GetName(), err)
			}
		}
	case kerrors.IsNotFound(err):
		// The slice does not exist yet, it is created below.
	default:
		return fmt.Errorf("get routing endpoint slice %q: %w", desired.GetName(), err)
	}

	kubemeta.ControlOn(desired, svc, core.SchemeGroupVersion.WithKind("Service"))

	_, err = kubeclientset.Create(ctx, sliceCli, desired,
		kubeclientset.WithUpdateIfExisted(routingEndpointSliceAlign(desired, svc)))
	if err != nil {
		return fmt.Errorf("install routing endpoint slice %q: %w", desired.GetName(), err)
	}

	return nil
}

// routingEndpointSliceAlign returns the align function of the endpoint slice of the
// system routing.
//
// The endpoints and the ports are replaced, not appended, so that a system that moved
// to another address or another port does not keep the old one.
func routingEndpointSliceAlign(desired *discovery.EndpointSlice, svc *core.Service) kubeclientset.AlignWithFn[*discovery.EndpointSlice] {
	return func(actual *discovery.EndpointSlice) (*discovery.EndpointSlice, bool, error) {
		if actual.AddressType != desired.AddressType {
			return nil, false, fmt.Errorf("routing endpoint slice %q has address type %q, want %q",
				actual.GetName(), actual.AddressType, desired.AddressType)
		}

		aligned := actual.DeepCopy()
		aligned.SetLabels(mergeLabels(actual.GetLabels(), desired.GetLabels()))
		kubemeta.ControlOn(aligned, svc, core.SchemeGroupVersion.WithKind("Service"))
		aligned.Ports = desired.Ports
		aligned.Endpoints = desired.Endpoints

		if apiequality.Semantic.DeepEqual(actual, aligned) {
			return actual, true, nil
		}
		return aligned, false, nil
	}
}

// cleanupObsoleteRoutingSlices removes the endpoint slices this installation owns and no
// longer needs: the slice an earlier version of this operator created under another name,
// and the slice the endpoint slice mirroring controller mirrored from the legacy
// endpoints.
//
// Only the slices whose owner reference names the service or the legacy endpoints of
// this installation are removed. A slice that carries the name of the routing service
// but belongs to somebody else is left alone, because a label is not a proof of
// ownership.
func cleanupObsoleteRoutingSlices(ctx context.Context, cli kubernetes.Interface, nsName string,
	svc *core.Service, ep *core.Endpoints, keepName string,
) error {
	if svc == nil || ep == nil {
		// Without the owners there is no proof of ownership, so nothing is removed.
		return nil
	}

	sliceCli := cli.DiscoveryV1().EndpointSlices(nsName)
	slices, err := sliceCli.List(ctx, meta.ListOptions{})
	if err != nil {
		return fmt.Errorf("list routing endpoint slices: %w", err)
	}

	for i := range slices.Items {
		slice := &slices.Items[i]
		if slice.GetName() == keepName {
			continue
		}

		switch {
		case kubemeta.IsControlledBy(slice, ep):
			// A mirror of the legacy endpoints of this installation. It carries
			// the same address as the direct slice, but the aggregator may pick
			// either of the two, so the direct slice replaces it.
		case slice.GetLabels()[discovery.LabelManagedBy] == routingManagedBy &&
			kubemeta.IsControlledBy(slice, svc):
			// A direct slice of this installation under a name this version no
			// longer uses.
		default:
			// Not owned by this installation, keep it.
			continue
		}

		err = sliceCli.Delete(ctx, slice.GetName(), meta.DeleteOptions{
			PropagationPolicy: ptr.To(meta.DeletePropagationBackground),
		})
		if err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("delete obsolete routing endpoint slice %q: %w", slice.GetName(), err)
		}
	}

	return nil
}
