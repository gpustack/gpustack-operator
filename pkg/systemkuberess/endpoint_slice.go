package systemkuberess

import (
	"context"
	"errors"
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

// addressTypeMismatchError reports that the endpoint slice in the cluster carries another
// address type than the routing wants.
//
// The address type of an endpoint slice cannot be changed, so such a slice is replaced
// rather than updated.
type addressTypeMismatchError struct {
	name     string
	actual   discovery.AddressType
	expected discovery.AddressType
}

func (e *addressTypeMismatchError) Error() string {
	return fmt.Sprintf("routing endpoint slice %q has address type %q, want %q",
		e.name, e.actual, e.expected)
}

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
//
// One worker owns the routing of a cluster at a time. The namespace and the routing
// service name are the same in every installation, so two workers pointing at one
// cluster write the same slice, and the last write wins. A slice this worker does not
// own is refused rather than taken over.
func installRoutingEndpointSlice(ctx context.Context, cli kubernetes.Interface, nsName string,
	desired *discovery.EndpointSlice, svc *core.Service,
) error {
	sliceCli := cli.DiscoveryV1().EndpointSlices(nsName)

	actual, err := sliceCli.Get(ctx, desired.GetName(), meta.GetOptions{})
	switch {
	case err == nil:
		err = checkRoutingSliceIdentity(actual, svc)
		if err != nil {
			return err
		}
		if actual.AddressType != desired.AddressType {
			err = deleteRoutingSlice(ctx, cli, nsName, actual)
			if err != nil && !kerrors.IsConflict(err) {
				return err
			}
			// A conflict means another actor replaced the slice in between. The
			// write below reads the slice that is there now and decides on it.
		}
	case kerrors.IsNotFound(err):
		// The slice does not exist yet, it is created below.
	default:
		return fmt.Errorf("get routing endpoint slice %q: %w", desired.GetName(), err)
	}

	kubemeta.ControlOn(desired, svc, core.SchemeGroupVersion.WithKind("Service"))

	// The read the create uses can still show the slice of the other address family
	// after it was deleted, so a family mismatch is retried with the slice gone.
	for attempt := 0; ; attempt++ {
		_, err = kubeclientset.Create(ctx, sliceCli, desired,
			kubeclientset.WithUpdateIfExisted(routingEndpointSliceAlign(desired, svc)))
		if err == nil {
			return nil
		}

		var mismatch *addressTypeMismatchError
		if !errors.As(err, &mismatch) || attempt >= 2 {
			return fmt.Errorf("install routing endpoint slice %q: %w", desired.GetName(), err)
		}

		stale, getErr := sliceCli.Get(ctx, desired.GetName(), meta.GetOptions{})
		if getErr != nil {
			if kerrors.IsNotFound(getErr) {
				continue
			}
			return fmt.Errorf("get routing endpoint slice %q: %w", desired.GetName(), getErr)
		}

		// The slice may have been replaced between the read that found the
		// mismatch and this one, so its identity is checked again before
		// anything is deleted. The uid precondition of the deletion binds it to
		// the slice that was read here, and that slice may be one another actor
		// owns.
		err = checkRoutingSliceIdentity(stale, svc)
		if err != nil {
			return err
		}

		if stale.AddressType == desired.AddressType {
			// The slice carries the wanted address family now, so the
			// reconcile converges on it without deleting it.
			continue
		}

		err = deleteRoutingSlice(ctx, cli, nsName, stale)
		if err != nil {
			if !kerrors.IsConflict(err) {
				return err
			}
			// Another actor replaced the slice in between. The write below reads
			// the slice that is there now and decides on it.
			continue
		}
	}
}

// checkRoutingSliceIdentity refuses an endpoint slice this worker does not own.
//
// A slice belongs to this worker when it carries the managed-by label of this operator
// and the routing service controls it, or when it carries the label and has no owner at
// all, which is the state a slice of an earlier version of this operator leaves behind.
// Any other slice is refused, and the refusal names its owner.
func checkRoutingSliceIdentity(slice *discovery.EndpointSlice, svc *core.Service) error {
	if slice.GetLabels()[discovery.LabelManagedBy] != routingManagedBy {
		return fmt.Errorf("routing endpoint slice %q is managed by %q, not by this operator",
			slice.GetName(), slice.GetLabels()[discovery.LabelManagedBy])
	}

	owner := kubemeta.GetControllerOfNoCopy(slice)
	switch {
	case owner == nil:
		// A slice of an earlier version of this operator, which left it without an owner.
	case routingServiceOwns(slice, svc):
		// The routing service controls the slice. The uid of the owner may be that
		// of the service this installation recreated, which the slice outlived; the
		// slice is adopted, and its owner reference is rewritten with the service
		// of now.
	default:
		return fmt.Errorf("routing endpoint slice %q is controlled by %s %q with uid %q, not by the routing service",
			slice.GetName(), owner.Kind, owner.Name, owner.UID)
	}

	return nil
}

// routingServiceOwns reports whether the routing service controls the slice.
//
// The owner is recognized by the name of the service it names, and not by its uid,
// because the routing service is recreated whenever its spec changes. The slice that
// the service of the past owned still belongs to this installation, and one service
// carries the name in the namespace, so the name proves the owner as well as the uid
// does, and it survives the recreation of the service.
func routingServiceOwns(slice *discovery.EndpointSlice, svc *core.Service) bool {
	if svc == nil {
		return false
	}

	return controlsRoutingOwner(slice, "Service", svc.GetName())
}

// routingEndpointsOwns reports whether the legacy endpoints control the slice.
//
// The owner is recognized by the name here for the same reason it is for the routing
// service: the legacy endpoints are recreated whenever they are deleted, and the
// mirror of the endpoints of the past still belongs to this installation.
func routingEndpointsOwns(slice *discovery.EndpointSlice, ep *core.Endpoints) bool {
	if ep == nil {
		return false
	}

	return controlsRoutingOwner(slice, "Endpoints", ep.GetName())
}

// controlsRoutingOwner reports whether the controller owner reference of the slice names
// the resource of the given kind and name.
func controlsRoutingOwner(slice *discovery.EndpointSlice, kind, name string) bool {
	owner := kubemeta.GetControllerOfNoCopy(slice)
	if owner == nil {
		return false
	}

	return owner.APIVersion == core.SchemeGroupVersion.String() &&
		owner.Kind == kind &&
		owner.Name == name
}

// routingEndpointSliceAlign returns the align function of the endpoint slice of the
// system routing.
//
// The endpoints and the ports are replaced, not appended, so that a system that moved
// to another address or another port does not keep the old one.
//
// The identity of the slice is checked on the object that is written, and not only on
// the one that was read before, so that a slice that was swapped in between the two is
// not rewritten.
func routingEndpointSliceAlign(desired *discovery.EndpointSlice, svc *core.Service) kubeclientset.AlignWithFn[*discovery.EndpointSlice] {
	return func(actual *discovery.EndpointSlice) (*discovery.EndpointSlice, bool, error) {
		err := checkRoutingSliceIdentity(actual, svc)
		if err != nil {
			return nil, false, err
		}

		if actual.AddressType != desired.AddressType {
			return nil, false, &addressTypeMismatchError{
				name:     actual.GetName(),
				actual:   actual.AddressType,
				expected: desired.AddressType,
			}
		}

		aligned := actual.DeepCopy()
		aligned.SetLabels(mergeLabels(actual.GetLabels(), desired.GetLabels()))
		kubemeta.ControlOn(aligned, svc, core.SchemeGroupVersion.WithKind("Service"))

		// The endpoints and the ports are copied, and not shared with the desired
		// object, so that a change to the object that is written cannot reach the
		// object the next attempt writes.
		wanted := desired.DeepCopy()
		aligned.Ports = wanted.Ports
		aligned.Endpoints = wanted.Endpoints

		if apiequality.Semantic.DeepEqual(actual, aligned) {
			return actual, true, nil
		}
		return aligned, false, nil
	}
}

// deleteRoutingSlice deletes an endpoint slice of the system routing.
//
// The deletion is bound to the uid of the slice that was read, so a slice that another
// actor replaced in between is not destroyed. The api server answers a conflict in
// that case, and the slice that is there then is read again by the caller.
func deleteRoutingSlice(ctx context.Context, cli kubernetes.Interface, nsName string, slice *discovery.EndpointSlice) error {
	err := cli.DiscoveryV1().EndpointSlices(nsName).Delete(ctx, slice.GetName(), meta.DeleteOptions{
		PropagationPolicy: ptr.To(meta.DeletePropagationBackground),
		Preconditions:     &meta.Preconditions{UID: ptr.To(slice.GetUID())},
	})
	if err != nil {
		if kerrors.IsNotFound(err) {
			// The slice is gone, which is the state the delete aimed at.
			return nil
		}
		return fmt.Errorf("delete routing endpoint slice %q: %w", slice.GetName(), err)
	}
	return nil
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
	selector := discovery.LabelServiceName + "=" + svc.GetName()
	slices, err := sliceCli.List(ctx, meta.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list routing endpoint slices: %w", err)
	}

	for i := range slices.Items {
		slice := &slices.Items[i]
		if slice.GetName() == keepName {
			continue
		}

		if !isObsoleteRoutingSlice(slice, svc, ep) {
			// Not owned by this installation, keep it.
			continue
		}

		err = deleteObsoleteRoutingSlice(ctx, cli, nsName, slice, svc, ep, keepName)
		if err != nil {
			return err
		}
	}

	return nil
}

// isObsoleteRoutingSlice reports whether the slice belongs to this installation and is no
// longer needed.
func isObsoleteRoutingSlice(slice *discovery.EndpointSlice, svc *core.Service, ep *core.Endpoints) bool {
	switch {
	case routingEndpointsOwns(slice, ep):
		// A mirror of the legacy endpoints of this installation. It carries the
		// same address as the direct slice, but the aggregator may pick either
		// of the two, so the direct slice replaces it.
	case slice.GetLabels()[discovery.LabelManagedBy] == routingManagedBy &&
		routingServiceOwns(slice, svc):
		// A direct slice of this installation under a name this version no
		// longer uses.
	default:
		return false
	}

	return true
}

// deleteObsoleteRoutingSlice removes the obsolete slice, and reads the slice that is
// there when another actor replaced it in between.
//
// The delete carries the uid of the slice that was read, so the slice that another
// actor put in its place is not destroyed. The slice that is there then is read again,
// and it is removed when it is ours and obsolete as well.
func deleteObsoleteRoutingSlice(ctx context.Context, cli kubernetes.Interface, nsName string,
	slice *discovery.EndpointSlice, svc *core.Service, ep *core.Endpoints, keepName string,
) error {
	err := deleteRoutingSlice(ctx, cli, nsName, slice)
	if err == nil || !kerrors.IsConflict(err) {
		return err
	}

	current, err := cli.DiscoveryV1().EndpointSlices(nsName).
		Get(ctx, slice.GetName(), meta.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			// The slice is gone, which is the state the delete aimed at.
			return nil
		}
		return fmt.Errorf("get routing endpoint slice %q: %w", slice.GetName(), err)
	}

	if current.GetName() == keepName || !isObsoleteRoutingSlice(current, svc, ep) {
		// The slice that is there now is kept, and it is not ours or it is the
		// one this installation uses.
		return nil
	}

	return deleteRoutingSlice(ctx, cli, nsName, current)
}
