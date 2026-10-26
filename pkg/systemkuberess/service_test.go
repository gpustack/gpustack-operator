package systemkuberess

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authz "k8s.io/api/authorization/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

const (
	testNamespace = "gpustack-system"
	testService   = "gpustack-operator-worker"
	testPort      = int32(8443)
)

// wantRouting is the state the cluster must hold after the reconcile.
type wantRouting struct {
	// service is true when the routing service must exist.
	service bool
	// endpoints is true when the legacy endpoints must exist.
	endpoints bool
	// slice is true when the endpoint slice must exist.
	slice bool
	// foreignSlice is true when the endpoint slice exists and belongs to somebody
	// else, so the reconcile must have left it alone.
	foreignSlice bool
	// address is the address the routing must point at.
	address string
	// port is the routing port.
	port int32
	// addressType is the address type the endpoint slice must carry.
	addressType discovery.AddressType
	// sliceNames are the names of the endpoint slices the cluster must hold.
	sliceNames []string
}

// routingInput is a routing the reconcile is asked for.
type routingInput struct {
	address string
	port    int32
}

// Test_installFakeSystemRoutingService drives the routing reconcile over the states a
// cluster can be in, and asserts the state it leaves behind.
func Test_installFakeSystemRoutingService(t *testing.T) {
	testCases := []struct {
		name string
		// objects are the resources the cluster holds before the reconcile.
		objects []runtime.Object
		// input is the routing the reconcile is asked for.
		input routingInput
		// second, when set, is a routing the reconcile is asked for after the
		// first one, so that a change of address or port is covered.
		second *routingInput
		// deny denies the access reviews it answers for.
		deny func(attrs *authz.ResourceAttributes) bool
		// reactors are the reactors of the fake, applied after the built-in ones.
		reactors func(cli *kubefake.Clientset)
		// want is the state the cluster must hold afterwards.
		want wantRouting
		// wantErr is the error the reconcile must report, empty when it must
		// succeed.
		wantErr string
	}{
		{
			name:  "installs the routing resources",
			input: routingInput{address: "192.168.50.10", port: testPort},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "192.168.50.10",
				port:        testPort,
				addressType: discovery.AddressTypeIPv4,
				sliceNames:  []string{"gpustack-operator-worker-direct"},
			},
		},
		{
			name:    "installs the routing resources of an IPv6 system",
			input:   routingInput{address: "fd00::1", port: testPort},
			objects: nil,
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "fd00::1",
				port:        testPort,
				addressType: discovery.AddressTypeIPv6,
				sliceNames:  []string{"gpustack-operator-worker-direct"},
			},
		},
		{
			name:  "converges when installed again",
			input: routingInput{address: "192.168.50.10", port: testPort},
			second: &routingInput{
				address: "192.168.50.10",
				port:    testPort,
			},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "192.168.50.10",
				port:        testPort,
				addressType: discovery.AddressTypeIPv4,
				sliceNames:  []string{"gpustack-operator-worker-direct"},
			},
		},
		{
			name:    "replaces a stale address",
			input:   routingInput{address: "192.168.50.20", port: testPort},
			second:  &routingInput{address: "192.168.50.10", port: testPort},
			objects: []runtime.Object{staleSlice("192.168.50.99", testPort)},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "192.168.50.10",
				port:        testPort,
				addressType: discovery.AddressTypeIPv4,
				sliceNames:  []string{"gpustack-operator-worker-direct"},
			},
		},
		{
			name:   "replaces a stale port",
			input:  routingInput{address: "192.168.50.10", port: 9443},
			second: &routingInput{address: "192.168.50.10", port: testPort},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "192.168.50.10",
				port:        testPort,
				addressType: discovery.AddressTypeIPv4,
				sliceNames:  []string{"gpustack-operator-worker-direct"},
			},
		},
		{
			name:   "replaces the slice of the other address family",
			input:  routingInput{address: "fd00::1", port: testPort},
			second: &routingInput{address: "fd00::1", port: testPort},
			objects: []runtime.Object{
				staleSlice("192.168.50.10", testPort),
			},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "fd00::1",
				port:        testPort,
				addressType: discovery.AddressTypeIPv6,
				sliceNames:  []string{"gpustack-operator-worker-direct"},
			},
		},
		{
			name:  "removes the mirrored slice of the legacy endpoints",
			input: routingInput{address: "192.168.50.10", port: testPort},
			objects: []runtime.Object{
				legacyEndpoints("192.168.50.10", testPort),
				mirroredSlice("192.168.50.10", testPort),
			},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "192.168.50.10",
				port:        testPort,
				addressType: discovery.AddressTypeIPv4,
				sliceNames:  []string{"gpustack-operator-worker-direct"},
			},
		},
		{
			name:  "removes the slice of an earlier version of this operator",
			input: routingInput{address: "192.168.50.10", port: testPort},
			objects: []runtime.Object{
				obsoleteSlice("192.168.50.10", testPort),
			},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "192.168.50.10",
				port:        testPort,
				addressType: discovery.AddressTypeIPv4,
				sliceNames:  []string{"gpustack-operator-worker-direct"},
			},
		},
		{
			name:  "keeps the slices of others",
			input: routingInput{address: "192.168.50.10", port: testPort},
			objects: []runtime.Object{
				foreignSlice(),
			},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "192.168.50.10",
				port:        testPort,
				addressType: discovery.AddressTypeIPv4,
				sliceNames: []string{
					"gpustack-operator-worker-direct",
					"other-service-abcde",
				},
			},
		},
		{
			name:  "adopts a slice without an owner",
			input: routingInput{address: "192.168.50.10", port: testPort},
			objects: []runtime.Object{
				ownerlessSlice("192.168.50.10", testPort),
			},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "192.168.50.10",
				port:        testPort,
				addressType: discovery.AddressTypeIPv4,
				sliceNames:  []string{"gpustack-operator-worker-direct"},
			},
		},
		{
			name:  "refuses a slice of another owner",
			input: routingInput{address: "192.168.50.10", port: testPort},
			objects: []runtime.Object{
				foreignOwnedSlice("192.168.50.10", testPort),
			},
			want: wantRouting{
				service:      true,
				endpoints:    true,
				foreignSlice: true,
				address:      "192.168.50.10",
				port:         testPort,
				addressType:  discovery.AddressTypeIPv4,
				sliceNames:   []string{"gpustack-operator-worker-direct"},
			},
			wantErr: "is controlled by",
		},
		{
			name:  "keeps the endpoints of others",
			input: routingInput{address: "192.168.50.10", port: testPort},
			objects: []runtime.Object{
				&core.Endpoints{
					ObjectMeta: meta.ObjectMeta{
						Namespace: testNamespace,
						Name:      "other-service",
					},
					Subsets: []core.EndpointSubset{
						{
							Addresses: []core.EndpointAddress{{IP: "10.0.0.1"}},
							Ports:     []core.EndpointPort{{Name: "https", Port: 443}},
						},
					},
				},
			},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "192.168.50.10",
				port:        testPort,
				addressType: discovery.AddressTypeIPv4,
				sliceNames:  []string{"gpustack-operator-worker-direct"},
			},
		},
		{
			name:  "reports a denied access review",
			input: routingInput{address: "192.168.50.10", port: testPort},
			deny: func(attrs *authz.ResourceAttributes) bool {
				return attrs.Resource == "endpointslices" && attrs.Verb == "list"
			},
			want: wantRouting{
				sliceNames: nil,
			},
			wantErr: "denied",
		},
		{
			// The owner reference of the slice blocks the deletion of the routing
			// service, so the api server asks for the permission to update the
			// finalizers of the service before it accepts the slice. A binding
			// without that permission stops the worker before it writes anything.
			name:  "reports a denied finalizers access review",
			input: routingInput{address: "192.168.50.10", port: testPort},
			deny: func(attrs *authz.ResourceAttributes) bool {
				return attrs.Resource == "services" &&
					attrs.Subresource == "finalizers" &&
					attrs.Verb == "update"
			},
			want: wantRouting{
				sliceNames: nil,
			},
			wantErr: "denied",
		},
		{
			name:  "reports a failed endpoint slice creation",
			input: routingInput{address: "192.168.50.10", port: testPort},
			reactors: func(cli *kubefake.Clientset) {
				cli.PrependReactor("create", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, kerrors.NewInternalError(fmt.Errorf("endpoint slice storage is broken"))
				})
			},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       false,
				address:     "192.168.50.10",
				port:        testPort,
				addressType: discovery.AddressTypeIPv4,
				sliceNames:  nil,
			},
			wantErr: "endpoint slice storage is broken",
		},
		{
			name:  "retries a lost write",
			input: routingInput{address: "192.168.50.10", port: testPort},
			second: &routingInput{
				address: "192.168.50.11",
				port:    testPort,
			},
			reactors: func(cli *kubefake.Clientset) {
				var lost atomic.Bool
				cli.PrependReactor("update", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
					if lost.CompareAndSwap(false, true) {
						return true, nil, kerrors.NewConflict(
							discovery.Resource("endpointslices"), testService, fmt.Errorf("the object was modified"),
						)
					}
					return false, nil, nil
				})
			},
			want: wantRouting{
				service:     true,
				endpoints:   true,
				slice:       true,
				address:     "192.168.50.11",
				port:        testPort,
				addressType: discovery.AddressTypeIPv4,
				sliceNames:  []string{"gpustack-operator-worker-direct"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cli := newFakeClient(tc.deny, tc.objects...)
			if tc.reactors != nil {
				tc.reactors(cli)
			}

			err := installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
				tc.input.address, tc.input.port)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}

			if tc.second != nil {
				err = installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
					tc.second.address, tc.second.port)
				if tc.wantErr != "" {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tc.wantErr)
				} else {
					require.NoError(t, err)
				}
			}

			checkRouting(t, cli, tc.want)
		})
	}
}

// Test_installFakeSystemRoutingServiceConvergesWithoutWrites checks that an install
// that finds the routing it wants writes nothing at all.
//
// The state after a repeated install is the same either way, so only the writes tell
// the two apart.
func Test_installFakeSystemRoutingServiceConvergesWithoutWrites(t *testing.T) {
	cli := newFakeClient(nil)

	require.NoError(t, installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
		"192.168.50.10", testPort))

	var writes atomic.Int64
	count := func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetResource().Resource != "selfsubjectaccessreviews" {
			writes.Add(1)
		}
		return false, nil, nil
	}
	cli.PrependReactor("create", "*", count)
	cli.PrependReactor("update", "*", count)
	cli.PrependReactor("delete", "*", count)

	require.NoError(t, installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
		"192.168.50.10", testPort))
	assert.Equal(t, int64(0), writes.Load(), "a converged install must not write")
}

// Test_installFakeSystemRoutingServiceKeepsTheRoutingServiceOfTheCluster checks that
// the routing service of the cluster is kept.
//
// The api server allocates a cluster ip for the service, and the operator does not
// manage it. Comparing the cluster ip of the wanted service, which asks for none, with
// the cluster ip of the service of the cluster would delete and recreate the service on
// every start. The recreation takes the endpoint slice of this installation with it,
// because the slice is a dependent of the service that blocks its deletion, and it
// changes the address of the routing as well.
func Test_installFakeSystemRoutingServiceKeepsTheRoutingServiceOfTheCluster(t *testing.T) {
	allocated := &core.Service{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testNamespace,
			Name:      testService,
		},
		Spec: core.ServiceSpec{
			Type:      core.ServiceTypeClusterIP,
			ClusterIP: "10.96.0.42",
			Ports: []core.ServicePort{
				{Name: routingPortName, Port: testPort},
			},
		},
	}
	cli := newFakeClient(nil, allocated)

	var deleted atomic.Int64
	cli.PrependReactor("delete", "services", func(action ktesting.Action) (bool, runtime.Object, error) {
		deleted.Add(1)
		return false, nil, nil
	})

	require.NoError(t, installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
		"192.168.50.10", testPort))

	assert.Equal(t, int64(0), deleted.Load(), "the routing service of the cluster must be kept")

	svc, err := cli.CoreV1().Services(testNamespace).Get(t.Context(), testService, meta.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "10.96.0.42", svc.Spec.ClusterIP,
		"the cluster ip of the service of the cluster must be kept")

	checkRouting(t, cli, wantRouting{
		service:     true,
		endpoints:   true,
		slice:       true,
		address:     "192.168.50.10",
		port:        testPort,
		addressType: discovery.AddressTypeIPv4,
		sliceNames:  []string{"gpustack-operator-worker-direct"},
	})

	t.Run("recreates the routing service when the cluster ip family differs from the routing address", func(t *testing.T) {
		cli := newFakeClient(nil, allocated.DeepCopy())

		var deleted atomic.Int64
		cli.PrependReactor("delete", "services", func(action ktesting.Action) (bool, runtime.Object, error) {
			deleted.Add(1)
			return false, nil, nil
		})

		require.NoError(t, installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
			"fd00::1", testPort))

		assert.Equal(t, int64(1), deleted.Load(),
			"the routing service must be recreated when its cluster ip family differs from the routing address")

		svc, err := cli.CoreV1().Services(testNamespace).Get(t.Context(), testService, meta.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, []core.IPFamily{core.IPv6Protocol}, svc.Spec.IPFamilies,
			"the recreated routing service must request the IPv6 family")

		checkRouting(t, cli, wantRouting{
			service:     true,
			endpoints:   true,
			slice:       true,
			address:     "fd00::1",
			port:        testPort,
			addressType: discovery.AddressTypeIPv6,
			sliceNames:  []string{"gpustack-operator-worker-direct"},
		})
	})

	t.Run("recreates the routing service when the port name differs", func(t *testing.T) {
		renamedPort := allocated.DeepCopy()
		renamedPort.Spec.Ports[0].Name = "http"
		cli := newFakeClient(nil, renamedPort)

		var deleted atomic.Int64
		cli.PrependReactor("delete", "services", func(action ktesting.Action) (bool, runtime.Object, error) {
			deleted.Add(1)
			return false, nil, nil
		})

		require.NoError(t, installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
			"192.168.50.10", testPort))

		assert.Equal(t, int64(1), deleted.Load(),
			"the routing service must be recreated when its port name differs")

		checkRouting(t, cli, wantRouting{
			service:     true,
			endpoints:   true,
			slice:       true,
			address:     "192.168.50.10",
			port:        testPort,
			addressType: discovery.AddressTypeIPv4,
			sliceNames:  []string{"gpustack-operator-worker-direct"},
		})
	})
}

// Test_installFakeSystemRoutingServiceCancelled checks that a canceled call is reported
// as it is, and that a context that is already done stops the reconcile from trying
// again.
//
// The fake client does not read the context itself, so the cancellation is carried by
// the error the call answers with, the way a client answers a canceled call.
func Test_installFakeSystemRoutingServiceCancelled(t *testing.T) {
	t.Run("reports the cancellation of a call", func(t *testing.T) {
		cli := newFakeClient(nil)
		cli.PrependReactor("create", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.Canceled
		})

		err := installFakeSystemRoutingService(t.Context(), cli, testNamespace, testService,
			"192.168.50.10", testPort)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)

		checkRouting(t, cli, wantRouting{
			service:   true,
			endpoints: true,
			slice:     false,
			address:   "192.168.50.10",
			port:      testPort,
		})
	})

	t.Run("stops trying again when the context is done", func(t *testing.T) {
		cli := newFakeClient(nil, staleSlice("192.168.50.99", testPort))

		var attempts atomic.Int64
		cli.PrependReactor("update", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
			attempts.Add(1)
			return true, nil, kerrors.NewConflict(
				discovery.Resource("endpointslices"), testService, fmt.Errorf("the object was modified"),
			)
		})

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := installFakeSystemRoutingService(ctx, cli, testNamespace, testService, "192.168.50.10", testPort)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the object was modified")
		assert.Equal(t, int64(1), attempts.Load(), "the write must not be tried again")
	})
}

// checkRouting asserts the observable state of the routing resources.
func checkRouting(t *testing.T, cli *kubefake.Clientset, want wantRouting) {
	t.Helper()

	svc, err := cli.CoreV1().Services(testNamespace).Get(t.Context(), testService, meta.GetOptions{})
	if want.service {
		require.NoError(t, err)
		assert.Equal(t, core.ServiceTypeClusterIP, svc.Spec.Type)
		assert.Nil(t, svc.Spec.Selector, "the routing service must have no selector")
		require.Len(t, svc.Spec.Ports, 1)
		assert.Equal(t, routingPortName, svc.Spec.Ports[0].Name)
		assert.Equal(t, want.port, svc.Spec.Ports[0].Port)
	} else {
		assert.True(t, kerrors.IsNotFound(err), "the routing service must not exist")
	}

	ep, err := cli.CoreV1().Endpoints(testNamespace).Get(t.Context(), testService, meta.GetOptions{})
	if want.endpoints {
		require.NoError(t, err)
		assert.Equal(t, "true", ep.GetLabels()[discovery.LabelSkipMirror],
			"the legacy endpoints must not be mirrored")
		require.Len(t, ep.Subsets, 1)
		assert.Equal(t, []core.EndpointAddress{{IP: want.address}}, ep.Subsets[0].Addresses)
		require.Len(t, ep.Subsets[0].Ports, 1)
		assert.Equal(t, routingPortName, ep.Subsets[0].Ports[0].Name)
		assert.Equal(t, want.port, ep.Subsets[0].Ports[0].Port)
	} else {
		assert.True(t, kerrors.IsNotFound(err), "the legacy endpoints must not exist")
	}

	slice, err := cli.DiscoveryV1().EndpointSlices(testNamespace).
		Get(t.Context(), routingEndpointSliceName(testService), meta.GetOptions{})
	switch {
	case want.foreignSlice:
		require.NoError(t, err)
		owner := kubemeta.GetControllerOfNoCopy(slice)
		require.NotNil(t, owner, "the slice of another owner must keep its owner")
		if want.service {
			assert.NotEqual(t, svc.GetUID(), owner.UID,
				"the reconcile must not take the slice of another owner over")
		}
	case want.slice:
		require.NoError(t, err)
		assert.Equal(t, testService, slice.GetLabels()[discovery.LabelServiceName])
		assert.Equal(t, routingManagedBy, slice.GetLabels()[discovery.LabelManagedBy])
		assert.Equal(t, want.addressType, slice.AddressType)
		require.Len(t, slice.Endpoints, 1)
		assert.Equal(t, []string{want.address}, slice.Endpoints[0].Addresses)
		require.NotNil(t, slice.Endpoints[0].Conditions.Ready)
		assert.True(t, *slice.Endpoints[0].Conditions.Ready)
		require.Len(t, slice.Ports, 1)
		require.NotNil(t, slice.Ports[0].Name)
		assert.Equal(t, routingPortName, *slice.Ports[0].Name)
		require.NotNil(t, slice.Ports[0].Port)
		assert.Equal(t, want.port, *slice.Ports[0].Port)

		owner := kubemeta.GetControllerOfNoCopy(slice)
		require.NotNil(t, owner, "the endpoint slice must be owned by the routing service")
		if want.service {
			assert.Equal(t, svc.GetUID(), owner.UID)
		}
	default:
		assert.True(t, kerrors.IsNotFound(err), "the endpoint slice must not exist")
	}

	slices, err := cli.DiscoveryV1().EndpointSlices(testNamespace).List(t.Context(), meta.ListOptions{})
	require.NoError(t, err)
	names := make([]string, 0, len(slices.Items))
	for i := range slices.Items {
		names = append(names, slices.Items[i].GetName())
	}
	assert.ElementsMatch(t, want.sliceNames, names)
}

// newFakeClient returns a fake clientset holding the given objects.
//
// Every object it creates gets a uid, so that the owner references of the fixtures can
// name it, and every access review is allowed unless the deny function denies it.
func newFakeClient(deny func(attrs *authz.ResourceAttributes) bool, objects ...runtime.Object) *kubefake.Clientset {
	for _, obj := range objects {
		if m, ok := obj.(meta.Object); ok && m.GetUID() == "" {
			m.SetUID(types.UID(fakeKind(m) + "/" + m.GetName()))
		}
	}

	cli := kubefake.NewSimpleClientset(objects...)

	cli.PrependReactor("create", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(ktesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		obj, ok := create.GetObject().(meta.Object)
		if !ok || obj.GetUID() != "" {
			return false, nil, nil
		}
		obj.SetUID(types.UID(fakeKind(obj) + "/" + obj.GetName()))
		return false, nil, nil
	})

	cli.PrependReactor("create", "selfsubjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(ktesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		sar, ok := create.GetObject().(*authz.SelfSubjectAccessReview)
		if !ok {
			return false, nil, nil
		}
		allowed := true
		if deny != nil && sar.Spec.ResourceAttributes != nil {
			allowed = !deny(sar.Spec.ResourceAttributes)
		}
		sar.Status = authz.SubjectAccessReviewStatus{Allowed: allowed}
		// The access reviews are answered here, and not stored, the way the api
		// server answers them.
		return true, sar, nil
	})

	return cli
}

// fakeKind returns the kind of an object, for the uids of the fake.
func fakeKind(obj meta.Object) string {
	switch obj.(type) {
	case *core.Service:
		return "Service"
	case *core.Endpoints:
		return "Endpoints"
	case *discovery.EndpointSlice:
		return "EndpointSlice"
	default:
		return "Object"
	}
}

// legacyEndpoints builds the legacy endpoints of the routing service, as an earlier
// version of this operator left them.
func legacyEndpoints(address string, port int32) *core.Endpoints {
	return &core.Endpoints{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testNamespace,
			Name:      testService,
			Labels: map[string]string{
				routingLabel: routingLabelValue,
			},
		},
		Subsets: []core.EndpointSubset{
			{
				Addresses: []core.EndpointAddress{{IP: address}},
				Ports:     []core.EndpointPort{{Name: routingPortName, Port: port}},
			},
		},
	}
}

// staleSlice builds the endpoint slice of the routing service, as an earlier address
// left it.
func staleSlice(address string, port int32) *discovery.EndpointSlice {
	return &discovery.EndpointSlice{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testNamespace,
			Name:      routingEndpointSliceName(testService),
			Labels: map[string]string{
				discovery.LabelServiceName: testService,
				discovery.LabelManagedBy:   routingManagedBy,
				routingLabel:               routingLabelValue,
			},
			OwnerReferences: []meta.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Service",
					Name:       testService,
					UID:        types.UID("Service/" + testService),
					Controller: ptr.To(true),
				},
			},
		},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints: []discovery.Endpoint{
			{
				Addresses:  []string{address},
				Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
			},
		},
		Ports: []discovery.EndpointPort{
			{Name: ptr.To(routingPortName), Port: ptr.To(port), Protocol: ptr.To(core.ProtocolTCP)},
		},
	}
}

// mirroredSlice builds the endpoint slice the mirroring controller mirrors from the
// legacy endpoints of the routing service.
func mirroredSlice(address string, port int32) *discovery.EndpointSlice {
	return &discovery.EndpointSlice{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testNamespace,
			Name:      testService + "-abcde",
			Labels: map[string]string{
				discovery.LabelServiceName: testService,
				discovery.LabelManagedBy:   "endpointslicemirroring-controller.k8s.io",
			},
			OwnerReferences: []meta.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Endpoints",
					Name:       testService,
					UID:        types.UID("Endpoints/" + testService),
					Controller: ptr.To(true),
				},
			},
		},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints: []discovery.Endpoint{
			{
				Addresses:  []string{address},
				Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
			},
		},
		Ports: []discovery.EndpointPort{
			{Name: ptr.To(routingPortName), Port: ptr.To(port), Protocol: ptr.To(core.ProtocolTCP)},
		},
	}
}

// obsoleteSlice builds the endpoint slice an earlier version of this operator created
// under a name it no longer uses.
func obsoleteSlice(address string, port int32) *discovery.EndpointSlice {
	slice := staleSlice(address, port)
	slice.SetName(testService + "-legacy")
	return slice
}

// ownerlessSlice builds the endpoint slice an earlier version of this operator left
// without an owner.
func ownerlessSlice(address string, port int32) *discovery.EndpointSlice {
	slice := staleSlice(address, port)
	slice.SetOwnerReferences(nil)
	return slice
}

// foreignOwnedSlice builds the endpoint slice of another owner, carrying the labels of
// this operator.
func foreignOwnedSlice(address string, port int32) *discovery.EndpointSlice {
	slice := staleSlice(address, port)
	slice.SetOwnerReferences([]meta.OwnerReference{
		{
			APIVersion: "v1",
			Kind:       "Service",
			Name:       "other-service",
			UID:        types.UID("Service/other-service"),
			Controller: ptr.To(true),
		},
	})
	return slice
}

// foreignSlice builds the endpoint slice of another service.
func foreignSlice() *discovery.EndpointSlice {
	return &discovery.EndpointSlice{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testNamespace,
			Name:      "other-service-abcde",
			Labels: map[string]string{
				discovery.LabelServiceName: "other-service",
				discovery.LabelManagedBy:   "endpointslicemirroring-controller.k8s.io",
			},
			OwnerReferences: []meta.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Endpoints",
					Name:       "other-service",
					UID:        types.UID("Endpoints/other-service"),
					Controller: ptr.To(true),
				},
			},
		},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints: []discovery.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
			},
		},
		Ports: []discovery.EndpointPort{
			{Name: ptr.To(routingPortName), Port: ptr.To(int32(443)), Protocol: ptr.To(core.ProtocolTCP)},
		},
	}
}
