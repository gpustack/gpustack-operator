package systemkuberess

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/pkg/kubemeta"
)

// sliceResource is the resource of the endpoint slices in the tracker of the fake.
var sliceResource = schema.GroupVersionResource{
	Group:    "discovery.k8s.io",
	Version:  "v1",
	Resource: "endpointslices",
}

// serviceResource is the resource of the services in the tracker of the fake.
var serviceResource = schema.GroupVersionResource{
	Group:    "",
	Version:  "v1",
	Resource: "services",
}

// endpointsResource is the resource of the legacy endpoints in the tracker of the fake.
var endpointsResource = schema.GroupVersionResource{
	Group:    "",
	Version:  "v1",
	Resource: "endpoints",
}

// alignSliceReadOrdinal is the ordinal of the slice read that the write of the slice
// does.
//
// The install reads the slice itself, and the write of the slice reads it again through
// kubeclientset. The second read is the one another actor can put its own slice in
// place before, so the reactors that inject a replacement key on it.
const alignSliceReadOrdinal = 2

// Test_installFakeSystemRoutingServiceRefusesTheSliceReplacedBeforeTheWrite checks
// that the ownership of the slice is decided on the object the write reads.
//
// The install reads the slice, and the write of the slice reads it again. Another actor
// can replace it in between the two reads, and the slice it puts in place must be
// refused and left alone.
func Test_installFakeSystemRoutingServiceRefusesTheSliceReplacedBeforeTheWrite(t *testing.T) {
	cli := newFakeClient(nil, staleSlice("192.168.50.99", testPort))

	// The second read is the one the write of the slice does, so the slice of
	// another owner is put in place right before it.
	foreign := foreignOwnedSlice("192.168.50.99", testPort)
	foreign.SetUID(types.UID("EndpointSlice/another-owner"))
	var reads atomic.Int64
	cli.PrependReactor("get", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
		if reads.Add(1) != alignSliceReadOrdinal {
			return false, nil, nil
		}
		return true, foreign, nil
	})

	var writes atomic.Int64
	cli.PrependReactor("update", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
		writes.Add(1)
		return false, nil, nil
	})

	err := installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
		"192.168.50.10", testPort)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is controlled by")
	assert.Equal(t, int64(0), writes.Load(), "the slice of another owner must not be rewritten")

	// The cluster must still hold the slice it held before.
	slice, err := cli.DiscoveryV1().EndpointSlices(testNamespace).
		Get(t.Context(), routingEndpointSliceName(testService), meta.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.50.99"}, slice.Endpoints[0].Addresses)
}

// Test_installFakeSystemRoutingServiceRefusesTheSliceReplacedBeforeTheRetry checks
// that the ownership of the slice is decided again when the write is retried.
//
// The write of the slice reads it and finds the address type of the other family, and
// the retry reads it again. Another actor can replace the slice in between the two
// reads, and the slice it puts in place must be refused and left alone.
func Test_installFakeSystemRoutingServiceRefusesTheSliceReplacedBeforeTheRetry(t *testing.T) {
	testCases := []struct {
		name string
		// replace makes the slice another actor puts in place.
		replace func(slice *discovery.EndpointSlice)
		// wantErr is the part of the error the reconcile must report.
		wantErr string
	}{
		{
			name: "the managed by label changes",
			replace: func(slice *discovery.EndpointSlice) {
				slice.SetLabels(map[string]string{
					discovery.LabelServiceName: testService,
					discovery.LabelManagedBy:   "another-operator",
					routingLabel:               routingLabelValue,
				})
			},
			wantErr: "is managed by",
		},
		{
			name: "the controller names another service",
			replace: func(slice *discovery.EndpointSlice) {
				// The fixture of another owner names another service, which is
				// what this case puts in place.
				slice.SetOwnerReferences(
					foreignOwnedSlice("192.168.50.99", testPort).GetOwnerReferences(),
				)
			},
			wantErr: "is controlled by",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cli := newFakeClient(nil, staleSlice("192.168.50.99", testPort))

			// The slice of the other actor is put in place while the write of
			// the slice reads it, so that the retry reads the new one.
			var reads atomic.Int64
			cli.PrependReactor("get", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
				if reads.Add(1) != alignSliceReadOrdinal {
					return false, nil, nil
				}

				replaced := staleSlice("192.168.50.99", testPort)
				replaced.SetUID(types.UID("EndpointSlice/another-owner"))
				tc.replace(replaced)
				require.NoError(t, cli.Tracker().Create(sliceResource, replaced, testNamespace))

				// The write of the slice still reads the slice as it was
				// before the other actor put its own in place.
				return true, staleSlice("192.168.50.99", testPort), nil
			})

			var deleted []types.UID
			cli.PrependReactor("delete", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
				del, ok := action.(ktesting.DeleteAction)
				if ok && del.GetDeleteOptions().Preconditions != nil && del.GetDeleteOptions().Preconditions.UID != nil {
					deleted = append(deleted, *del.GetDeleteOptions().Preconditions.UID)
				}
				return false, nil, nil
			})

			err := installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
				"fd00::1", testPort)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)

			// The slice of the other actor must be left alone.
			slice, err := cli.DiscoveryV1().EndpointSlices(testNamespace).
				Get(t.Context(), routingEndpointSliceName(testService), meta.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, types.UID("EndpointSlice/another-owner"), slice.GetUID())

			for _, uid := range deleted {
				assert.NotEqual(t, types.UID("EndpointSlice/another-owner"), uid,
					"the slice of the other actor must not be deleted")
			}
		})
	}
}

// ipv6Slice builds the endpoint slice of this install for the other address family.
func ipv6Slice(address string, port int32) *discovery.EndpointSlice {
	slice := staleSlice(address, port)
	slice.AddressType = discovery.AddressTypeIPv6
	return slice
}

// Test_installFakeSystemRoutingServiceAdoptsTheSliceOfARecreatedService checks that
// the slice of a routing service that was recreated is adopted rather than refused.
//
// The routing service is recreated whenever its spec changes, and the slice that the
// service of the past owned outlives it. The slice belongs to this installation, so
// the reconcile takes it over and rewrites its owner reference with the service of
// now. Refusing it would leave the worker unable to repair the routing it owns.
func Test_installFakeSystemRoutingServiceAdoptsTheSliceOfARecreatedService(t *testing.T) {
	cli := newFakeClient(nil, staleSlice("192.168.50.99", testPort))

	// The routing service was recreated, so it carries another uid than the one
	// the slice names. Its spec matches the wanted one, so the reconcile keeps it.
	recreated := &core.Service{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testNamespace,
			Name:      testService,
			UID:       types.UID("Service/" + testService + "-2"),
		},
		Spec: core.ServiceSpec{
			Type:  core.ServiceTypeClusterIP,
			Ports: []core.ServicePort{{Name: routingPortName, Port: testPort}},
		},
	}
	require.NoError(t, cli.Tracker().Create(serviceResource, recreated, testNamespace))

	require.NoError(t, installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
		"192.168.50.10", testPort))

	slice, err := cli.DiscoveryV1().EndpointSlices(testNamespace).
		Get(t.Context(), routingEndpointSliceName(testService), meta.GetOptions{})
	require.NoError(t, err)

	owner := kubemeta.GetControllerOfNoCopy(slice)
	require.NotNil(t, owner, "the slice must be owned by the routing service")
	assert.Equal(t, types.UID("Service/"+testService+"-2"), owner.UID,
		"the owner reference must name the service of now")

	require.Len(t, slice.Endpoints, 1)
	assert.Equal(t, []string{"192.168.50.10"}, slice.Endpoints[0].Addresses,
		"the adopted slice must carry the wanted address")
}

// Test_installFakeSystemRoutingServiceRemovesTheMirrorOfRecreatedEndpoints checks that
// the mirror of the legacy endpoints is removed even when the endpoints were recreated.
//
// The legacy endpoints are recreated whenever they are deleted, and the mirror of the
// endpoints of the past outlives them. The mirror belongs to this installation, so the
// reconcile removes it, for the same reason it removes the slice of a recreated service.
func Test_installFakeSystemRoutingServiceRemovesTheMirrorOfRecreatedEndpoints(t *testing.T) {
	cli := newFakeClient(nil, legacyEndpoints("192.168.50.10", testPort), mirroredSlice("192.168.50.10", testPort))

	// The legacy endpoints were recreated, so they carry another uid than the one
	// the mirror names.
	recreated := &core.Endpoints{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testNamespace,
			Name:      testService,
			UID:       types.UID("Endpoints/" + testService + "-2"),
		},
	}
	require.NoError(t, cli.Tracker().Update(endpointsResource, recreated, testNamespace))

	require.NoError(t, installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
		"192.168.50.10", testPort))

	slices, err := cli.DiscoveryV1().EndpointSlices(testNamespace).List(t.Context(), meta.ListOptions{})
	require.NoError(t, err)
	names := make([]string, 0, len(slices.Items))
	for i := range slices.Items {
		names = append(names, slices.Items[i].GetName())
	}
	assert.ElementsMatch(t, []string{routingEndpointSliceName(testService)}, names,
		"the mirror of the legacy endpoints must be removed")
}

// Test_routingEndpointSliceAlignKeepsTheDesiredStateApart checks that the object the
// align writes does not share its endpoints and ports with the desired object.
//
// The desired object is reused by the attempts of a write, so a change to the object
// that is written must not reach the object the next attempt writes.
func Test_routingEndpointSliceAlignKeepsTheDesiredStateApart(t *testing.T) {
	desired, err := newRoutingEndpointSlice(testNamespace, testService, "192.168.50.10", testPort)
	require.NoError(t, err)

	svc := &core.Service{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testNamespace,
			Name:      testService,
			UID:       types.UID("Service/" + testService),
		},
	}

	aligned, skip, err := routingEndpointSliceAlign(desired, svc)(staleSlice("192.168.50.10", testPort).DeepCopy())
	require.NoError(t, err)
	require.False(t, skip)

	aligned.Endpoints[0].Addresses[0] = "203.0.113.7"
	aligned.Ports[0].Port = ptr.To(int32(1))

	assert.Equal(t, []string{"192.168.50.10"}, desired.Endpoints[0].Addresses,
		"the desired endpoints must not be shared with the written object")
	assert.Equal(t, testPort, *desired.Ports[0].Port,
		"the desired ports must not be shared with the written object")
}

// Test_installFakeSystemRoutingServiceConvergesOnTheSliceOfTheWantedFamily checks
// that the retry of the write converges on the slice that carries the wanted address
// family, and does not delete it.
//
// The slice of the other address family is written again with the wanted one while
// the write of the slice reads it, and the retry reads the new one.
func Test_installFakeSystemRoutingServiceConvergesOnTheSliceOfTheWantedFamily(t *testing.T) {
	cli := newFakeClient(nil, staleSlice("192.168.50.99", testPort))

	// The slice of this install is written again for the other address family
	// while the write of the slice reads the one of the family it replaces.
	wanted := ipv6Slice("192.168.50.99", testPort)
	wanted.SetUID(types.UID("EndpointSlice/gpustack-operator-worker-direct-2"))

	var reads atomic.Int64
	cli.PrependReactor("get", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
		if reads.Add(1) != alignSliceReadOrdinal {
			return false, nil, nil
		}

		require.NoError(t, cli.Tracker().Create(sliceResource, wanted, testNamespace))
		return true, staleSlice("192.168.50.99", testPort), nil
	})

	var deleted []types.UID
	cli.PrependReactor("delete", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
		del, ok := action.(ktesting.DeleteAction)
		if ok && del.GetDeleteOptions().Preconditions != nil && del.GetDeleteOptions().Preconditions.UID != nil {
			deleted = append(deleted, *del.GetDeleteOptions().Preconditions.UID)
		}
		return false, nil, nil
	})

	err := installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
		"fd00::1", testPort)
	require.NoError(t, err)

	// The slice of the wanted family must not be deleted.
	for _, uid := range deleted {
		assert.NotEqual(t, types.UID("EndpointSlice/gpustack-operator-worker-direct-2"), uid,
			"the slice of the wanted address family must not be deleted")
	}

	slice, err := cli.DiscoveryV1().EndpointSlices(testNamespace).
		Get(t.Context(), routingEndpointSliceName(testService), meta.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, discovery.AddressTypeIPv6, slice.AddressType)
	require.Len(t, slice.Endpoints, 1)
	assert.Equal(t, []string{"fd00::1"}, slice.Endpoints[0].Addresses)
}

// Test_installFakeSystemRoutingServiceBindsTheFamilyReplacementDeleteToTheSlice checks
// that the deletion of the slice of the other address family is bound to the slice that
// was read.
//
// A slice that was replaced before the deletion reached it must not be destroyed, and
// the reconcile must not report that it converged while the cluster holds a slice of
// the other address family.
func Test_installFakeSystemRoutingServiceBindsTheFamilyReplacementDeleteToTheSlice(t *testing.T) {
	cli := newFakeClient(nil, staleSlice("192.168.50.99", testPort))

	var deleted []types.UID
	cli.PrependReactor("delete", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
		del, ok := action.(ktesting.DeleteAction)
		if ok && del.GetDeleteOptions().Preconditions != nil && del.GetDeleteOptions().Preconditions.UID != nil {
			deleted = append(deleted, *del.GetDeleteOptions().Preconditions.UID)
		}
		// The slice was replaced before the deletion reached it.
		return true, nil, kerrors.NewConflict(discovery.Resource("endpointslices"),
			routingEndpointSliceName(testService), fmt.Errorf("the uid precondition does not hold"))
	})

	err := installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
		"fd00::1", testPort)
	require.Error(t, err, "a deletion that never succeeds must not be reported as a convergence")
	assert.Contains(t, err.Error(), "has address type")

	// Every deletion is bound to the slice that was read.
	require.NotEmpty(t, deleted)
	for _, uid := range deleted {
		assert.Equal(t, types.UID("EndpointSlice/"+routingEndpointSliceName(testService)), uid)
	}

	// The cluster still holds the slice of the other address family, and the
	// reconcile has said so.
	slice, err := cli.DiscoveryV1().EndpointSlices(testNamespace).
		Get(t.Context(), routingEndpointSliceName(testService), meta.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, discovery.AddressTypeIPv4, slice.AddressType)
}

// Test_installFakeSystemRoutingServiceBindsTheRetryDeleteToTheSlice checks that the
// deletion of the retry of the write is bound to the slice the retry read.
//
// The slice the retry reads is not the slice the install read, so the deletion must
// carry the uid of the one it read, and the reconcile must not report that it converged
// while the cluster holds a slice of the other address family.
func Test_installFakeSystemRoutingServiceBindsTheRetryDeleteToTheSlice(t *testing.T) {
	cli := newFakeClient(nil, staleSlice("192.168.50.99", testPort))

	// The slice of this install is written again under a uid of its own, the
	// way another worker of an earlier version of the operator writes it.
	rewritten := staleSlice("192.168.50.99", testPort)
	rewritten.SetUID(types.UID("EndpointSlice/gpustack-operator-worker-direct-2"))

	var reads atomic.Int64
	cli.PrependReactor("get", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
		if reads.Add(1) != alignSliceReadOrdinal {
			return false, nil, nil
		}

		// The write of the slice reads the slice of the other address family
		// as it was before it was deleted, and the slice of this install is
		// written again in the meantime.
		require.NoError(t, cli.Tracker().Create(sliceResource, rewritten, testNamespace))
		return true, staleSlice("192.168.50.99", testPort), nil
	})

	var deleted []types.UID
	var deletes atomic.Int64
	cli.PrependReactor("delete", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
		del, ok := action.(ktesting.DeleteAction)
		if ok && del.GetDeleteOptions().Preconditions != nil && del.GetDeleteOptions().Preconditions.UID != nil {
			deleted = append(deleted, *del.GetDeleteOptions().Preconditions.UID)
		}
		if deletes.Add(1) == 1 {
			// The deletion of the install reaches the slice it read.
			return false, nil, nil
		}
		// The slice was replaced again before the deletion of the retry reached
		// it.
		return true, nil, kerrors.NewConflict(discovery.Resource("endpointslices"),
			routingEndpointSliceName(testService), fmt.Errorf("the uid precondition does not hold"))
	})

	err := installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
		"fd00::1", testPort)
	require.Error(t, err, "a deletion that never succeeds must not be reported as a convergence")
	assert.Contains(t, err.Error(), "has address type")

	// The first deletion is bound to the slice the install read, and the ones
	// of the retry are bound to the slice the retry read.
	require.GreaterOrEqual(t, len(deleted), 2)
	assert.Equal(t, types.UID("EndpointSlice/gpustack-operator-worker-direct"), deleted[0])
	assert.Equal(t, types.UID("EndpointSlice/gpustack-operator-worker-direct-2"), deleted[1])

	// The cluster still holds the slice of the other address family, and the
	// reconcile has said so.
	slice, err := cli.DiscoveryV1().EndpointSlices(testNamespace).
		Get(t.Context(), routingEndpointSliceName(testService), meta.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, discovery.AddressTypeIPv4, slice.AddressType)
}
