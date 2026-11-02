package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlintercept "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// rdmaServedModes are the four allocation modes an RDMA server exists for. Visibility is not
// among them: it names an endpoint another container of the same Pod holds, and the constructor
// refuses it.
var rdmaServedModes = []workercore.DeviceAllocationMode{
	workercore.DeviceAllocationModeExclusive,
	workercore.DeviceAllocationModeShared,
	workercore.DeviceAllocationModePartitioned,
}

// rdmaDevices builds the node's Devices record over the given interfaces.
func rdmaDevices(nodeName string, ifaces ...workercore.DeviceInterface) *workercore.Devices {
	return &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec:       workercore.DevicesSpec{Interfaces: ifaces},
	}
}

// wholeFunctionIface builds an interface that serves the whole-function modes: a NUMA affinity, a
// bound RDMA device and a link verdict.
func wholeFunctionIface(name, numa, rdmaDevice string, link *workercore.DeviceInterfaceLink) workercore.DeviceInterface {
	return workercore.DeviceInterface{
		Name:         name,
		NumaAffinity: numa,
		RDMA:         rdmaDevice != "",
		RDMADevice:   rdmaDevice,
		Link:         link,
	}
}

// sriovPF builds an SR-IOV physical function with its NUMA affinity, carrying the given virtual
// functions, itself bound to an RDMA device the way a real physical function is.
func sriovPF(name, numa string, vfs ...workercore.DeviceInterfaceVirtualFunction) workercore.DeviceInterface {
	return workercore.DeviceInterface{
		Name:             name,
		NumaAffinity:     numa,
		SRIOV:            true,
		RDMA:             true,
		RDMADevice:       "mlx5_bond",
		VirtualFunctions: vfs,
	}
}

// rdmaVF builds a virtual function with its own NUMA affinity, bound RDMA device and link verdict.
func rdmaVF(name, numa, rdmaDevice string, link *workercore.DeviceInterfaceLink) workercore.DeviceInterfaceVirtualFunction {
	return workercore.DeviceInterfaceVirtualFunction{
		Name:         name,
		NumaAffinity: numa,
		RDMA:         rdmaDevice != "",
		RDMADevice:   rdmaDevice,
		Link:         link,
	}
}

// rdmaTestServer builds one RDMA server over the reconciler, failing the test when the mode is
// one the constructor refuses.
func rdmaTestServer(t *testing.T, mode workercore.DeviceAllocationMode, rec *DevicesReconciler) *rdmaServer {
	t.Helper()

	s, err := NewRDMAServer(klog.Background(), mode, rec)
	require.NoError(t, err)
	rs, ok := s.(*rdmaServer)
	require.True(t, ok, "NewRDMAServer must return the RDMA server behind the Server interface")

	return rs
}

// numaIDs reads a NUMA hint's node ids as plain values.
func numaIDs(t *testing.T, topology *deviceplugin.TopologyInfo) []int64 {
	t.Helper()

	require.NotNil(t, topology)
	ids := make([]int64, 0, len(topology.Nodes))
	for _, n := range topology.Nodes {
		ids = append(ids, n.ID)
	}
	return ids
}

// rdmaPublicationClient serves revision A and, once, publishes revision B to the store right
// after the first read returns. The event boundary is the read itself: the request in flight
// already holds A when B lands, and every later read is served B. Nothing sleeps and no object
// with another reader is mutated.
func rdmaPublicationClient(
	t *testing.T, revA, revB *workercore.Devices,
) ctrlcli.WithWatch {
	t.Helper()

	var published bool
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(revA).
		WithInterceptorFuncs(ctrlintercept.Funcs{
			Get: func(
				ctx context.Context, c ctrlcli.WithWatch, key ctrlcli.ObjectKey,
				obj ctrlcli.Object, opts ...ctrlcli.GetOption,
			) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if published {
					return nil
				}
				published = true
				current, ok := obj.(*workercore.Devices)
				if !ok {
					return fmt.Errorf("the inventory read returned %T, not a Devices record", obj)
				}
				revised := revB.DeepCopy()
				revised.ResourceVersion = current.ResourceVersion
				return c.Update(ctx, revised)
			},
		}).
		Build()
}

// TestNewRDMAServer pins what a constructed server names itself by: the resource it registers and
// the socket it owns, asserted as literals — a test that recomposed either from the same pieces
// the constructor uses would only compare that composition with itself. A mode with no key of its
// own must be refused, so an unknown mode cannot silently serve under the whole-function key, and
// two modes cannot land on one socket.
func TestNewRDMAServer(t *testing.T) {
	cases := []struct {
		name       string
		mode       workercore.DeviceAllocationMode
		wantName   core.ResourceName
		wantSocket string
	}{
		{
			name:       "exclusive registers the bare key and its own socket",
			mode:       workercore.DeviceAllocationModeExclusive,
			wantName:   "device.gpustack.ai/rdma",
			wantSocket: "rdma.exclusive.sock",
		},
		{
			name:       "shared registers the shared key and its own socket",
			mode:       workercore.DeviceAllocationModeShared,
			wantName:   "device.gpustack.ai/rdma.shared",
			wantSocket: "rdma.shared.sock",
		},
		{
			name:       "partitioned registers the partitioned key and its own socket",
			mode:       workercore.DeviceAllocationModePartitioned,
			wantName:   "device.gpustack.ai/rdma.partitioned",
			wantSocket: "rdma.partitioned.sock",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := NewRDMAServer(klog.Background(), c.mode, nil)
			require.NoError(t, err)
			rs, ok := s.(*rdmaServer)
			require.True(t, ok)

			assert.Equal(t, c.wantName, rs.ResourceName)
			opts := rs.servingOptions()
			assert.Equal(t, c.wantName, opts.ResourceName)
			assert.Equal(t, c.wantSocket, opts.SocketName)
			assert.Same(t, rs, opts.Plugin, "the serving loop must answer on this very server")
		})
	}

	for _, mode := range []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeVisibility,
		workercore.DeviceAllocationModeNone,
	} {
		_, err := NewRDMAServer(klog.Background(), mode, nil)
		assert.Error(t, err, "mode %s must not construct a server", mode)
	}
}

// TestRDMAServer_ListAndWatch_Counts pins the token count each family advertises over a fixed
// inventory, and the exact shape of the vocabulary the counts are made of: which endpoints a
// partitioned interface contributes, and which IDs a shared pool steps through.
func TestRDMAServer_ListAndWatch_Counts(t *testing.T) {
	const nodeName = "node-rdma-counts"

	cases := []struct {
		name       string
		ifaces     []workercore.DeviceInterface
		wantCounts map[workercore.DeviceAllocationMode]int
	}{
		{
			name: "two whole-function interfaces",
			ifaces: []workercore.DeviceInterface{
				wholeFunctionIface("ib0", "0", "mlx5_0", nil),
				wholeFunctionIface("eth1", "1", "rxe0_eth1", nil),
			},
			wantCounts: map[workercore.DeviceAllocationMode]int{
				workercore.DeviceAllocationModeExclusive:   2,
				workercore.DeviceAllocationModeShared:      2 * nodefeature.RDMAEndpointPoolSize,
				workercore.DeviceAllocationModePartitioned: 0,
			},
		},
		{
			// A physical function with virtual functions configured serves Partitioned only,
			// one endpoint per virtual function, and none of the whole-function modes.
			name: "an SR-IOV physical function with two virtual functions",
			ifaces: []workercore.DeviceInterface{
				sriovPF("pf0", "",
					rdmaVF("vf0", "", "mlx5_1", nil),
					rdmaVF("vf1", "", "mlx5_2", nil),
				),
			},
			wantCounts: map[workercore.DeviceAllocationMode]int{
				workercore.DeviceAllocationModeExclusive:   0,
				workercore.DeviceAllocationModeShared:      0,
				workercore.DeviceAllocationModePartitioned: 2,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName, c.ifaces...))}
			for _, mode := range rdmaServedModes {
				resp, err := rdmaTestServer(t, mode, rec).getListAndWatchResponse(context.Background())
				require.NoError(t, err)
				assert.Len(t, resp.Devices, c.wantCounts[mode], "mode %s", mode)
			}
		})
	}

	t.Run("the token vocabulary the counts are made of", func(t *testing.T) {
		rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(
			rdmaDevices(nodeName,
				wholeFunctionIface("ib0", "0", "mlx5_0", nil),
				wholeFunctionIface("eth1", "1", "rxe0_eth1", nil),
				sriovPF("pf0", "",
					rdmaVF("vf0", "", "mlx5_1", nil),
					rdmaVF("vf1", "", "mlx5_2", nil),
				)),
		)}

		ids := func(t *testing.T, mode workercore.DeviceAllocationMode) []string {
			t.Helper()

			resp, err := rdmaTestServer(t, mode, rec).getListAndWatchResponse(context.Background())
			require.NoError(t, err)
			got := make([]string, 0, len(resp.Devices))
			for _, d := range resp.Devices {
				got = append(got, d.ID)
			}
			return got
		}

		// Each whole-function endpoint names itself; a virtual function is named under its
		// physical function. Spelled out rather than rebuilt from the endpoint record, so a
		// wrong group or device component cannot pass against its own source.
		assert.Equal(t, []string{"ib0:ib0:0000", "eth1:eth1:0000"}, ids(t, workercore.DeviceAllocationModeExclusive))
		assert.Equal(t, []string{"pf0:vf0:0000", "pf0:vf1:0000"}, ids(t, workercore.DeviceAllocationModePartitioned))

		// The shared family steps one index across every endpoint before the next index, so the
		// advertisement interleaves its endpoints in hardware order: the first tokens kubelet
		// reads are one per endpoint, the same spread GetPreferredAllocation prefers. Each
		// endpoint's own tokens still ascend by index under the interleave, and the second
		// element of an endpoint is 0001 -- not the 160000 stride the accelerator id builder
		// would have stepped -- so the values are checked rather than left to the count.
		shared := ids(t, workercore.DeviceAllocationModeShared)
		require.GreaterOrEqual(t, len(shared), 4)
		assert.Equal(t, []string{
			"ib0:ib0:0000", "eth1:eth1:0000", "ib0:ib0:0001", "eth1:eth1:0001",
		}, shared[:4])

		ib0 := make([]string, 0, nodefeature.RDMAEndpointPoolSize)
		for _, id := range shared {
			if strings.HasPrefix(id, "ib0:") {
				ib0 = append(ib0, id)
			}
		}
		require.Len(t, ib0, nodefeature.RDMAEndpointPoolSize)
		assert.Equal(t, []string{"ib0:ib0:0000", "ib0:ib0:0001", "ib0:ib0:0002"}, ib0[:3])
		assert.Equal(t, "ib0:ib0:0063", ib0[len(ib0)-1])

		// Distinct, because kubelet keys its checkpoint on the string: a repeated ID silently
		// collapses two tokens into one and the count above would not notice.
		assert.Len(t, sets.New(ib0...), len(ib0), "the shared family advertises a repeated device ID")
	})
}

// TestRDMAServer_ListAndWatch_ZeroPair pins the paired zero rows of the name gate. The pair, not
// either half alone, is what discriminates: the second inventory carries a link verdict but no
// bound device name, so an implementation gating on the verdict instead of the name would
// advertise it. A nameless interface that also carries no verdict cannot be made to fail against
// any implementation that derives its tokens from the inventory — fabricated devices are caught
// by the counts case — so its worth here is the pair's baseline.
func TestRDMAServer_ListAndWatch_ZeroPair(t *testing.T) {
	const nodeName = "node-rdma-zero"

	cases := []struct {
		name   string
		iface  workercore.DeviceInterface
		reason string
	}{
		{
			name:   "a plain network interface with no bound RDMA device",
			iface:  workercore.DeviceInterface{Name: "eth0"},
			reason: "no endpoint carries a bound device name",
		},
		{
			name: "an unverified link with no device name",
			iface: workercore.DeviceInterface{
				Name: "eth0",
				Link: &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateUnverified},
			},
			reason: "the gate is the bound device name, not the link verdict",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName, c.iface))}
			for _, mode := range rdmaServedModes {
				resp, err := rdmaTestServer(t, mode, rec).getListAndWatchResponse(context.Background())
				require.NoError(t, err)
				assert.Empty(t, resp.Devices, "mode %s: %s", mode, c.reason)
			}
		})
	}

	// The discriminating half: the same inventory with the name present advertises the full
	// count, Healthy — so the zero above is the name gate's doing and not a verdict gate's.
	rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName,
		wholeFunctionIface("eth0", "", "rxe0_eth0",
			&workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateUnverified})))}
	resp, err := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec).getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	require.Len(t, resp.Devices, nodefeature.RDMAEndpointPoolSize)
	for _, d := range resp.Devices {
		assert.Equal(t, deviceplugin.Healthy, d.Health)
	}
}

// TestRDMAServer_ListAndWatch_LinkGate pins the health rule over the four link records an
// interface can carry: only a verdict of failure withholds an endpoint from new Pods, by marking
// its tokens, and every other record — a passing verdict, an unverified one, none at all —
// leaves them Healthy. Every row also holds the full token count, so the failed row proves the
// tokens are marked rather than dropped, which is what keeps the kubelet's checkpointed
// allocation for an existing holder intact.
func TestRDMAServer_ListAndWatch_LinkGate(t *testing.T) {
	const nodeName = "node-rdma-link"

	cases := []struct {
		name       string
		link       *workercore.DeviceInterfaceLink
		wantHealth string
	}{
		{
			name:       "a verified link leaves tokens healthy",
			link:       &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK},
			wantHealth: deviceplugin.Healthy,
		},
		{
			// Unverified is healthy deliberately: a link this node could not interrogate must
			// not silently exclude itself, which would turn "the port's state could not be read"
			// into "this node has no RDMA".
			name:       "an unverified link leaves tokens healthy",
			link:       &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateUnverified},
			wantHealth: deviceplugin.Healthy,
		},
		{
			// An endpoint reaches this gate only by carrying a bound RDMA device, so a missing
			// link record is the unverified case arriving by a different route, not a verdict of
			// failure.
			name:       "no link record leaves tokens healthy",
			link:       nil,
			wantHealth: deviceplugin.Healthy,
		},
		{
			name:       "a failed link marks tokens unhealthy and keeps them advertised",
			link:       &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateFailed},
			wantHealth: deviceplugin.Unhealthy,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName,
				wholeFunctionIface("ib0", "", "mlx5_0", c.link)))}

			wantCounts := map[workercore.DeviceAllocationMode]int{
				workercore.DeviceAllocationModeExclusive: 1,
				workercore.DeviceAllocationModeShared:    nodefeature.RDMAEndpointPoolSize,
			}
			for mode, wantCount := range wantCounts {
				resp, err := rdmaTestServer(t, mode, rec).getListAndWatchResponse(context.Background())
				require.NoError(t, err)
				require.Len(t, resp.Devices, wantCount,
					"mode %s keeps the full token count whatever the verdict", mode)
				for _, d := range resp.Devices {
					assert.Equal(t, c.wantHealth, d.Health)
				}
			}
		})
	}
}

// TestRDMAServer_ListAndWatch_Topology pins where each token's NUMA hint comes from: the
// endpoint's own affinity, a virtual function's own with its parent's as fallback, and no hint at
// all when neither answers — never node 0, which under the single-numa-node policy would decide
// admission against a proximity nobody measured.
func TestRDMAServer_ListAndWatch_Topology(t *testing.T) {
	const nodeName = "node-rdma-numa"

	sharedOver := func(t *testing.T, ifaces ...workercore.DeviceInterface) []*deviceplugin.Device {
		t.Helper()

		rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName, ifaces...))}
		resp, err := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec).
			getListAndWatchResponse(context.Background())
		require.NoError(t, err)
		return resp.Devices
	}

	t.Run("an affinity names its node on every token", func(t *testing.T) {
		for _, d := range sharedOver(t, wholeFunctionIface("ib0", "1", "mlx5_0", nil)) {
			assert.Equal(t, []int64{1}, numaIDs(t, d.Topology))
		}
	})

	t.Run("an empty affinity attaches no hint rather than node 0", func(t *testing.T) {
		for _, d := range sharedOver(t, wholeFunctionIface("ib0", "", "mlx5_0", nil)) {
			assert.Nil(t, d.Topology)
		}
	})

	t.Run("two interfaces carry their own nodes", func(t *testing.T) {
		devices := sharedOver(t,
			wholeFunctionIface("ib0", "0", "mlx5_0", nil),
			wholeFunctionIface("eth1", "1", "rxe0_eth1", nil))
		require.Len(t, devices, 2*nodefeature.RDMAEndpointPoolSize)
		for _, d := range devices {
			want := int64(1)
			if strings.HasPrefix(d.ID, "ib0:") {
				want = 0
			}
			assert.Equal(t, []int64{want}, numaIDs(t, d.Topology))
		}
	})

	t.Run("a virtual function falls back to its parent's affinity", func(t *testing.T) {
		rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName,
			sriovPF("pf0", "0", rdmaVF("vf0", "", "mlx5_1", nil))))}
		resp, err := rdmaTestServer(t, workercore.DeviceAllocationModePartitioned, rec).
			getListAndWatchResponse(context.Background())
		require.NoError(t, err)
		require.Len(t, resp.Devices, 1)
		assert.Equal(t, []int64{0}, numaIDs(t, resp.Devices[0].Topology))
	})
}

// TestRDMAServer_ListAndWatch_Deterministic asserts two consecutive responses over an unchanged
// inventory are equal, in every family. The guarantee is structural — the response is built by
// pure functions over the record's slice order, with no map iteration, clock or allocation state
// in the path — so this is the tripwire for anyone who introduces one, not the proof.
func TestRDMAServer_ListAndWatch_Deterministic(t *testing.T) {
	const nodeName = "node-rdma-det"

	ifaces := []workercore.DeviceInterface{
		wholeFunctionIface("ib0", "0", "mlx5_0",
			&workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateFailed}),
		wholeFunctionIface("eth1", "", "rxe0_eth1", nil),
		sriovPF("pf0", "",
			rdmaVF("vf0", "", "mlx5_1", nil),
			rdmaVF("vf1", "1", "mlx5_2",
				&workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK})),
	}
	rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName, ifaces...))}

	for _, mode := range rdmaServedModes {
		t.Run(mode.String(), func(t *testing.T) {
			s := rdmaTestServer(t, mode, rec)
			first, err := s.getListAndWatchResponse(context.Background())
			require.NoError(t, err)
			second, err := s.getListAndWatchResponse(context.Background())
			require.NoError(t, err)
			require.Equal(t, first, second)
			require.NotEmpty(t, first.Devices, "the fixture must advertise something in mode %s", mode)
		})
	}
}

// TestRDMAServer_ListAndWatch_IgnoresAllocationState asserts an RDMA advertisement is a pure
// function of the interface inventory: allocation state anywhere in the process — the ledger
// status, the in-process reservations, a live-pod sweep — cannot change it.
//
// This is deliberately not the no-cross-mode-exclusion criterion, which needs a real Allocate to
// move allocation state across the family boundary; that pair lives beside the Allocate it drives,
// in rdma_allocate_test.go. What this case guards is the defect one step upstream: an
// advertisement that consults a hold register at all.
func TestRDMAServer_ListAndWatch_IgnoresAllocationState(t *testing.T) {
	const nodeName = "node-rdma-hold"

	// The hold, fabricated in every register an implementation could consult. The ledger status
	// names the endpoint itself, in the shared family:
	hold := workercore.DevicesStatus{
		Groups: []workercore.DevicesAllocationGroup{{
			ID: "ib0",
			Accelerators: []workercore.AcceleratorAllocation{{
				ID:   "ib0",
				Mode: workercore.DeviceAllocationModeShared,
			}},
		}},
	}
	devs := rdmaDevices(nodeName, wholeFunctionIface("ib0", "1", "mlx5_0",
		&workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK}))
	devs.Status = hold

	rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(devs)}
	shared := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec)
	exclusive := rdmaTestServer(t, workercore.DeviceAllocationModeExclusive, rec)

	beforeShared, err := shared.getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	beforeExclusive, err := exclusive.getListAndWatchResponse(context.Background())
	require.NoError(t, err)

	// ...an in-process reservation of the same shape, which also fires the notifier broadcast...
	rec.reserveDevices(types.UID("pod-hold"), "worker", hold, []string{"ib0:ib0:0000"})

	// ...and a live-pod sweep, seeded directly so no Pod object is needed.
	rec.notifiersMutex.Lock()
	rec.lastLivePodUIDs = []string{"pod-hold"}
	rec.notifiersMutex.Unlock()
	rec.notifyListeners()

	afterShared, err := shared.getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	afterExclusive, err := exclusive.getListAndWatchResponse(context.Background())
	require.NoError(t, err)

	assert.Equal(t, beforeShared, afterShared, "the ledger hold changed the shared advertisement")
	assert.Equal(t, beforeExclusive, afterExclusive, "the reservation or the sweep changed the exclusive advertisement")
	for _, d := range afterShared.Devices {
		assert.Equal(t, deviceplugin.Healthy, d.Health)
	}
	for _, d := range afterExclusive.Devices {
		assert.Equal(t, deviceplugin.Healthy, d.Health)
	}
}

// TestRDMAServer_ListAndWatch_UnsubscribesWhenTheStreamEnds is the wiring half of the unsubscribe
// guard: the release closure removes a subscription, and this proves ListAndWatch reaches it —
// kubelet opens a fresh stream on every registration, so a stream that kept its subscription
// would leak one per kubelet restart for the life of the process.
func TestRDMAServer_ListAndWatch_UnsubscribesWhenTheStreamEnds(t *testing.T) {
	const nodeName = "node-rdma-unsub"

	rec := &DevicesReconciler{
		NodeName: nodeName,
		Client:   nodeFixture(rdmaDevices(nodeName, wholeFunctionIface("ib0", "", "mlx5_0", nil))),
	}
	s := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec)

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		returned <- s.ListAndWatch(&Empty{}, &fakeListAndWatchStream{
			ctx:  ctx,
			sent: make(chan *ListAndWatchResponse, 1),
		})
	}()

	notifierCount := func() int {
		rec.notifiersMutex.RLock()
		defer rec.notifiersMutex.RUnlock()

		return len(rec.notifiers)
	}
	require.Eventually(t, func() bool {
		return notifierCount() == 1
	}, 15*time.Second, 20*time.Millisecond, "ListAndWatch never subscribed to the broadcast")

	cancel()
	select {
	case <-returned:
	case <-time.After(15 * time.Second):
		t.Fatal("ListAndWatch did not return after its stream context was canceled")
	}

	assert.Zero(t, notifierCount(), "the stream kept its subscription after returning")
}

// TestRDMAServer_ListAndWatch_RetriesTheInitialResponse covers the first half of the stream. The
// poll the initial response is built under ends as soon as its condition returns nil, so a read
// failure that is only logged ends it as a success having sent nothing: the stream goes on to the
// watch loop, and kubelet holds no device list until some later reconcile happens to fire the
// notifier. The recovery is what discriminates -- a server that left the poll never sends the
// initial response at all, however healthy the inventory becomes afterwards.
func TestRDMAServer_ListAndWatch_RetriesTheInitialResponse(t *testing.T) {
	const nodeName = "node-rdma-retry"

	devs := rdmaDevices(nodeName, wholeFunctionIface("ib0", "", "mlx5_0", nil))
	var (
		failGet  atomic.Bool
		failures atomic.Int32
	)
	failGet.Store(true)
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs).
		WithInterceptorFuncs(ctrlintercept.Funcs{
			Get: func(
				ctx context.Context, c ctrlcli.WithWatch, key ctrlcli.ObjectKey,
				obj ctrlcli.Object, opts ...ctrlcli.GetOption,
			) error {
				if failGet.Load() {
					failures.Add(1)
					return errors.New("simulated inventory read failure")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	s := rdmaTestServer(t, workercore.DeviceAllocationModeExclusive, rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sent := make(chan *ListAndWatchResponse, 1)
	go func() {
		_ = s.ListAndWatch(&Empty{}, &fakeListAndWatchStream{ctx: ctx, sent: sent})
	}()

	// The recovery must come after a failure, or the first read succeeds and the case passes
	// whether or not the poll would have retried.
	require.Eventually(t, func() bool {
		return failures.Load() > 0
	}, 15*time.Second, 20*time.Millisecond, "the inventory was never read at all")
	failGet.Store(false)
	select {
	case resp := <-sent:
		assert.Len(t, resp.GetDevices(), 1,
			"the retried initial response does not carry the node's one endpoint")
	case <-time.After(30 * time.Second):
		t.Fatal("the initial list and watch response was never sent after the inventory read recovered")
	}
}

// TestRDMAServer_ListAndWatch_SendsUpdateOnBroadcast covers the watch half of the stream: a
// broadcast sends a fresh response built from the current inventory, and an inventory that cannot
// be read on a later broadcast ends the stream — returning the error restarts the plugin server,
// where serving a stale list would advertise endpoints the record no longer carries.
func TestRDMAServer_ListAndWatch_SendsUpdateOnBroadcast(t *testing.T) {
	const nodeName = "node-rdma-watch"

	devs := rdmaDevices(nodeName, wholeFunctionIface("ib0", "", "mlx5_0", nil))
	var failGet atomic.Bool
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs).
		WithInterceptorFuncs(ctrlintercept.Funcs{
			Get: func(
				ctx context.Context, c ctrlcli.WithWatch, key ctrlcli.ObjectKey,
				obj ctrlcli.Object, opts ...ctrlcli.GetOption,
			) error {
				if failGet.Load() {
					return errors.New("simulated inventory read failure")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	s := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := make(chan *ListAndWatchResponse, 4)
	returned := make(chan error, 1)
	go func() {
		returned <- s.ListAndWatch(&Empty{}, &fakeListAndWatchStream{ctx: ctx, sent: sent})
	}()

	// The initial response lands before any broadcast does.
	select {
	case resp := <-sent:
		require.Len(t, resp.Devices, nodefeature.RDMAEndpointPoolSize)
	case <-time.After(15 * time.Second):
		t.Fatal("no initial list and watch response")
	}

	broadcast := func() {
		rec.notifiersMutex.Lock()
		rec.lastLivePodUIDs = []string{"pod-live"}
		rec.notifiersMutex.Unlock()
		rec.notifyListeners()
	}

	// A live-pod sweep broadcast sends a fresh response over the unchanged inventory.
	broadcast()
	select {
	case resp := <-sent:
		require.Len(t, resp.Devices, nodefeature.RDMAEndpointPoolSize)
	case <-time.After(15 * time.Second):
		t.Fatal("no list and watch response after the broadcast")
	}

	// An inventory the server cannot read ends the stream rather than serving a stale list.
	failGet.Store(true)
	broadcast()
	select {
	case err := <-returned:
		require.Error(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("ListAndWatch did not return after the inventory became unreadable")
	}

	rec.notifiersMutex.RLock()
	defer rec.notifiersMutex.RUnlock()
	assert.Zero(t, len(rec.notifiers), "the stream kept its subscription after returning")
}

// TestRDMAServer_ListAndWatch_InFlightRevisionPublication pins the publication boundary on the
// advertisement path. Revision A is read, the store publishes B while that request is still in
// flight, and the response under construction is entirely A — every token Healthy, each with its
// own NUMA hint — while the next request is served entirely B. The boundary is the read, not a
// point inside endpoint iteration, and no read count is asserted: the exact token surface is.
func TestRDMAServer_ListAndWatch_InFlightRevisionPublication(t *testing.T) {
	const nodeName = "node-rdma-publish-watch"

	// Revision A advertises both whole-function interfaces healthy; revision B fails both links,
	// which is the fact each token's health carries to kubelet.
	linkOK := &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK}
	linkFailed := &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateFailed}
	revA := rdmaDevices(nodeName,
		wholeFunctionIface("ib0", "0", "mlx5_0", linkOK),
		wholeFunctionIface("eth1", "1", "rxe0_eth1", linkOK))
	revB := rdmaDevices(nodeName,
		wholeFunctionIface("ib0", "0", "mlx5_0", linkFailed),
		wholeFunctionIface("eth1", "1", "rxe0_eth1", linkFailed))
	rec := &DevicesReconciler{NodeName: nodeName, Client: rdmaPublicationClient(t, revA, revB)}
	s := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec)

	// The complete literal contract of the shared family over these two interfaces: every token
	// of both pools, no more, no fewer.
	wantIDs := sets.New[string]()
	for _, prefix := range []string{"ib0:ib0:", "eth1:eth1:"} {
		for i := int32(0); i < nodefeature.RDMAEndpointPoolSize; i++ {
			wantIDs.Insert(fmt.Sprintf("%s%04d", prefix, i))
		}
	}

	rounds := []struct {
		name        string
		wantHealthy bool
	}{
		{name: "the in-flight advertisement is entirely its read revision", wantHealthy: true},
		{name: "the next advertisement is the published revision", wantHealthy: false},
	}
	for _, round := range rounds {
		t.Run(round.name, func(t *testing.T) {
			resp, err := s.getListAndWatchResponse(context.Background())
			require.NoError(t, err)

			// The complete token surface of the revision: exactly the contract IDs, each once,
			// each carrying its interface's hint and its revision's verdict.
			require.Len(t, resp.Devices, len(wantIDs))
			gotIDs := sets.New[string]()
			for _, d := range resp.Devices {
				gotIDs.Insert(d.ID)

				wantNuma := int64(1)
				if strings.HasPrefix(d.ID, "ib0:") {
					wantNuma = 0
				}
				assert.Equal(t, []int64{wantNuma}, numaIDs(t, d.Topology),
					"token %s: NUMA hint", d.ID)

				wantHealth := deviceplugin.Healthy
				if !round.wantHealthy {
					wantHealth = deviceplugin.Unhealthy
				}
				assert.Equal(t, wantHealth, d.Health, "token %s: health", d.ID)
			}
			assert.Equal(t, wantIDs, gotIDs)
		})
	}
}

// TestRDMAServer_Start_RegistersEverySocket proves the three RDMA servers and an accelerator
// neighbor can serve the one plugin directory at once: each registers its own resource under its
// own socket, and a socket collision would fail a generation's listen and with it the
// registration. The resource names are asserted as literals, never recomposed from the constants
// the implementation uses, so a key that changes shape has to be changed here too.
func TestRDMAServer_Start_RegistersEverySocket(t *testing.T) {
	const nodeName = "node-rdma-reg"

	dir := pluginDir(t)
	kubeSocket := filepath.Join(dir, "kubelet.sock")
	kubelet, stopKubelet := startFakeKubelet(t, dir)
	defer stopKubelet()

	rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName,
		wholeFunctionIface("ib0", "", "mlx5_0", nil)))}

	// startedServer tracks one running server so Stop can be exercised on it without its
	// cancel-and-drain cleanup double-reporting the return it already consumed.
	type startedServer struct {
		server   Server
		cancel   context.CancelFunc
		returned chan error
		settled  bool
	}
	started := make([]*startedServer, 0, len(rdmaServedModes))
	for _, mode := range rdmaServedModes {
		s, err := NewRDMAServer(klog.Background(), mode, rec)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		st := &startedServer{server: s, cancel: cancel, returned: make(chan error, 1)}
		started = append(started, st)
		go func() {
			st.returned <- s.Start(ctx, kubeSocket)
		}()
		t.Cleanup(func() {
			if st.settled {
				return
			}
			st.cancel()
			select {
			case err := <-st.returned:
				assert.ErrorIs(t, err, context.Canceled, "Start reported a failure of its own")
			case <-time.After(10 * time.Second):
				t.Error("Start did not return after its context was canceled")
			}
		})
	}

	// The accelerator neighbor sharing the directory. Its socket is a manufacturer's, so its
	// presence beside the RDMA ones is what makes the collision check meaningful.
	startResourceServer(t, kubeSocket)

	wantRegistrations := len(rdmaServedModes) + 1
	require.Eventually(t, func() bool {
		return len(kubelet.registrations()) == wantRegistrations
	}, 15*time.Second, 50*time.Millisecond,
		"the RDMA servers and the accelerator neighbour never all registered")

	// Stopping one server exercises the lifecycle contract an aggregator holds: Stop ends the
	// serving loop, and a Start that was canceled by nothing but the Stop returns cleanly.
	first := started[0]
	first.settled = true
	first.server.Stop()
	select {
	case err := <-first.returned:
		assert.NoError(t, err, "Start reported a failure of its own after a clean Stop")
	case <-time.After(10 * time.Second):
		t.Error("Start did not return after Stop")
	}

	wantNames := sets.New(
		"device.gpustack.ai/rdma",
		"device.gpustack.ai/rdma.shared",
		"device.gpustack.ai/rdma.partitioned",
		"nvidia.com/gpu.sliced",
	)
	gotNames := sets.New[string]()
	gotEndpoints := sets.New[string]()
	for _, req := range kubelet.registrations() {
		gotNames.Insert(req.GetResourceName())
		gotEndpoints.Insert(req.GetEndpoint())
	}
	assert.Equal(t, wantNames, gotNames)
	assert.Len(t, gotEndpoints, 4, "each server must own its own socket beside kubelet's")
}

// TestRDMAServer_ListAndWatch_SynthesizesNetdevFreeVerbsEndpoints pins that GPUStack-owned
// verbs devices in the local RDMA snapshot without a netdev in Devices.spec.interfaces are
// advertised in Exclusive, Shared, and Partitioned modes, while externally owned verbs devices
// are excluded.
func TestRDMAServer_ListAndWatch_SynthesizesNetdevFreeVerbsEndpoints(t *testing.T) {
	const nodeName = "node-rdma-netdev-free"

	inv := &device.RDMASnapshot{
		Complete: true,
		Revision: "rev-synth",
		Devices: []device.RDMADevice{
			{
				Name:         "mlx5_0",
				Owner:        device.RDMADeviceOwnerGPUStack,
				PciBusID:     "0000:10:00.0",
				NumaAffinity: "0",
				Ports: []device.RDMAPort{
					{Number: 1, State: "ACTIVE", PhysicalState: "LinkUp"},
				},
			},
			{
				Name:            "mlx5_1",
				Owner:           device.RDMADeviceOwnerGPUStack,
				PciBusID:        "0000:20:00.1",
				ParentPciBusID:  "0000:20:00.0",
				VirtualFunction: true,
				NumaAffinity:    "1",
				Ports: []device.RDMAPort{
					{Number: 1, State: "ACTIVE", PhysicalState: "LinkUp"},
				},
			},
			{
				Name:   "efa0",
				Owner:  device.RDMADeviceOwnerExternal,
				Driver: "efa",
			},
		},
	}

	rec := &DevicesReconciler{
		NodeName:               nodeName,
		Client:                 nodeFixture(rdmaDevices(nodeName)),
		RDMAInterfaceInventory: func() *device.RDMASnapshot { return inv },
	}

	excl, err := rdmaTestServer(t, workercore.DeviceAllocationModeExclusive, rec).
		getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	require.Len(t, excl.Devices, 1)
	assert.Equal(t, "mlx5_0:mlx5_0:0000", excl.Devices[0].ID)
	assert.Equal(t, deviceplugin.Healthy, excl.Devices[0].Health)
	assert.Equal(t, []int64{0}, numaIDs(t, excl.Devices[0].Topology))

	part, err := rdmaTestServer(t, workercore.DeviceAllocationModePartitioned, rec).
		getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	require.Len(t, part.Devices, 1)
	assert.Equal(t, "0000:20:00.0:mlx5_1:0000", part.Devices[0].ID)
	assert.Equal(t, deviceplugin.Healthy, part.Devices[0].Health)
	assert.Equal(t, []int64{1}, numaIDs(t, part.Devices[0].Topology))
}

// TestRDMAServer_GetPreferredAllocation pins the spread the hint promises: a claim of N shared
// tokens over N HCAs names one token per HCA before a second token of any one of them, the spread
// holds when a round is already consumed, a must-include token is honored ahead of the walk, and
// an endpoint a failed link or the active selection withholds is out of the spread. The IDs are
// spelled out as literals: a hint that rebuilt its answer from the same pool it was handed would
// only compare the walk with itself.
func TestRDMAServer_GetPreferredAllocation(t *testing.T) {
	const nodeName = "node-rdma-preferred"

	// fourHCAs is a node of four whole-function interfaces, one HCA each, ib1 carrying the
	// given link verdict.
	fourHCAs := func(ib1Link *workercore.DeviceInterfaceLink) []workercore.DeviceInterface {
		return []workercore.DeviceInterface{
			wholeFunctionIface("ib0", "0", "mlx5_0", nil),
			wholeFunctionIface("ib1", "1", "mlx5_1", ib1Link),
			wholeFunctionIface("ib2", "0", "mlx5_2", nil),
			wholeFunctionIface("ib3", "1", "mlx5_3", nil),
		}
	}

	// sharedPoolFrom is the token pool the named endpoints advertise from the given index up,
	// the offer kubelet sends when every advertised token from there is available.
	sharedPoolFrom := func(from int, endpoints ...string) []string {
		ids := make([]string, 0, len(endpoints)*(nodefeature.RDMAEndpointPoolSize-from))
		for _, name := range endpoints {
			for i := from; i < nodefeature.RDMAEndpointPoolSize; i++ {
				ids = append(ids, fmt.Sprintf("%s:%s:%04d", name, name, i))
			}
		}
		return ids
	}

	t.Run("preferred allocation is advertised", func(t *testing.T) {
		rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName, fourHCAs(nil)...))}
		opts, err := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec).
			GetDevicePluginOptions(context.Background(), &Empty{})
		require.NoError(t, err)
		assert.True(t, opts.GetPreferredAllocationAvailable)
	})

	cases := []struct {
		name string
		// ifaces is the interface inventory the hint reads.
		ifaces []workercore.DeviceInterface
		// selected is the active selection; nil applies none.
		selected []string
		// available is the request's available device IDs.
		available []string
		// mustIDs is the request's must-include device IDs.
		mustIDs []string
		// size is the request's allocation size.
		size int32
		// wantIDs is the single container response's device IDs, in order; nil for the empty
		// hint an unsatisfiable claim earns.
		wantIDs []string
	}{
		{
			name:      "four units spread one per HCA",
			ifaces:    fourHCAs(nil),
			available: sharedPoolFrom(0, "ib0", "ib1", "ib2", "ib3"),
			size:      4,
			wantIDs: []string{
				"ib0:ib0:0000", "ib1:ib1:0000", "ib2:ib2:0000", "ib3:ib3:0000",
			},
		},
		{
			// Every HCA's first token is already taken: the spread survives the consumed
			// round and lands on 0001 of each HCA, never four tokens of one.
			name:      "a consumed first round pushes the spread to the next index",
			ifaces:    fourHCAs(nil),
			available: sharedPoolFrom(1, "ib0", "ib1", "ib2", "ib3"),
			size:      4,
			wantIDs: []string{
				"ib0:ib0:0001", "ib1:ib1:0001", "ib2:ib2:0001", "ib3:ib3:0001",
			},
		},
		{
			// Four HCAs cannot honor six units one each: the claim fills the first round
			// everywhere, then reaches into the second round of the hardware order.
			name:      "six units fill the first round and reach into the second",
			ifaces:    fourHCAs(nil),
			available: sharedPoolFrom(0, "ib0", "ib1", "ib2", "ib3"),
			size:      6,
			wantIDs: []string{
				"ib0:ib0:0000", "ib1:ib1:0000", "ib2:ib2:0000", "ib3:ib3:0000",
				"ib0:ib0:0001", "ib1:ib1:0001",
			},
		},
		{
			// The named token is taken ahead of the walk, and the rounds then avoid its HCA:
			// the remaining three picks are one per remaining HCA, never a second token of ib0.
			name:      "a must-include token takes its HCA out of the round",
			ifaces:    fourHCAs(nil),
			available: sharedPoolFrom(0, "ib0", "ib1", "ib2", "ib3"),
			mustIDs:   []string{"ib0:ib0:0000"},
			size:      4,
			wantIDs: []string{
				"ib0:ib0:0000", "ib1:ib1:0000", "ib2:ib2:0000", "ib3:ib3:0000",
			},
		},
		{
			// ib1's link has failed: its HCA is out of the spread, and the fourth pick falls
			// to the second round of the first endpoint in hardware order.
			name: "a failed link takes its HCA out of the spread",
			ifaces: fourHCAs(&workercore.DeviceInterfaceLink{
				State: workercore.DeviceInterfaceLinkStateFailed,
			}),
			available: sharedPoolFrom(0, "ib0", "ib1", "ib2", "ib3"),
			size:      4,
			wantIDs: []string{
				"ib0:ib0:0000", "ib2:ib2:0000", "ib3:ib3:0000", "ib0:ib0:0001",
			},
		},
		{
			// The active selection withholds mlx5_1: the same spread as the failed link,
			// reached through the policy rather than the verdict.
			name:      "a policy-excluded HCA takes its endpoint out of the spread",
			ifaces:    fourHCAs(nil),
			selected:  []string{"mlx5_0", "mlx5_2", "mlx5_3"},
			available: sharedPoolFrom(0, "ib0", "ib1", "ib2", "ib3"),
			size:      4,
			wantIDs: []string{
				"ib0:ib0:0000", "ib2:ib2:0000", "ib3:ib3:0000", "ib0:ib0:0001",
			},
		},
		{
			// Two tokens cannot honor four units however the rounds walk them: the empty hint
			// is the answer, leaving the choice, and the reason, to Allocate.
			name:      "a claim the available tokens cannot satisfy hints nothing",
			ifaces:    fourHCAs(nil),
			available: []string{"ib0:ib0:0000", "ib1:ib1:0000"},
			size:      4,
			wantIDs:   nil,
		},
		{
			// A must-include naming the link-failed endpoint is not honored: the hint skips it
			// and the rounds replace its pick from the eligible endpoints in hardware order.
			name: "a must-include on a link-failed endpoint is skipped and replaced",
			ifaces: fourHCAs(&workercore.DeviceInterfaceLink{
				State: workercore.DeviceInterfaceLinkStateFailed,
			}),
			available: sharedPoolFrom(0, "ib0", "ib1", "ib2", "ib3"),
			mustIDs:   []string{"ib1:ib1:0000"},
			size:      4,
			wantIDs: []string{
				"ib0:ib0:0000", "ib2:ib2:0000", "ib3:ib3:0000", "ib0:ib0:0001",
			},
		},
		{
			// The same skip reaches through the policy: a must-include naming an endpoint
			// whose HCA the active selection withholds is replaced from the eligible set.
			name:      "a must-include on a policy-excluded endpoint is skipped and replaced",
			ifaces:    fourHCAs(nil),
			selected:  []string{"mlx5_0", "mlx5_2", "mlx5_3"},
			available: sharedPoolFrom(0, "ib0", "ib1", "ib2", "ib3"),
			mustIDs:   []string{"ib1:ib1:0000"},
			size:      4,
			wantIDs: []string{
				"ib0:ib0:0000", "ib2:ib2:0000", "ib3:ib3:0000", "ib0:ib0:0001",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName, c.ifaces...))}
			if c.selected != nil {
				rec.applyRDMASelection(RDMAPolicyResolution{
					Status:   RDMAPolicyResolutionPolicy,
					Selected: c.selected,
				})
			}

			resp, err := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec).
				GetPreferredAllocation(context.Background(), &PreferredAllocationRequest{
					ContainerRequests: []*ContainerPreferredAllocationRequest{{
						AvailableDeviceIDs:   c.available,
						MustIncludeDeviceIDs: c.mustIDs,
						AllocationSize:       c.size,
					}},
				})
			require.NoError(t, err)
			require.Len(t, resp.ContainerResponses, 1)
			assert.Equal(t, c.wantIDs, resp.ContainerResponses[0].DeviceIDs)
		})
	}

	t.Run("a request with no container requests is refused", func(t *testing.T) {
		rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName, fourHCAs(nil)...))}
		_, err := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec).
			GetPreferredAllocation(context.Background(), &PreferredAllocationRequest{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no container requests")
	})

	t.Run("an unparseable available device id is refused", func(t *testing.T) {
		rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName, fourHCAs(nil)...))}
		_, err := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec).
			GetPreferredAllocation(context.Background(), &PreferredAllocationRequest{
				ContainerRequests: []*ContainerPreferredAllocationRequest{{
					AvailableDeviceIDs: []string{"ib0"},
					AllocationSize:     1,
				}},
			})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "convert available device id")
	})
}

// TestRDMAServer_ListAndWatch_PortPinnedHealth pins the advertisement under a selection that
// pins a port: a record-carried HCA whose pinned port is down reports Unhealthy though the
// record's own verdict reads OK, the same HCA under the other port pinned reports Healthy, and
// a synthesized HCA with no observation of the pinned port stays Healthy on the unverified
// record — nothing to read is not a verdict of failure. Tokens are always kept, never dropped.
func TestRDMAServer_ListAndWatch_PortPinnedHealth(t *testing.T) {
	const nodeName = "node-rdma-port-health"

	inv := func() *device.RDMASnapshot {
		return &device.RDMASnapshot{
			Complete: true,
			Revision: "rev-port",
			Devices: []device.RDMADevice{
				{Name: "mlx5_0", Owner: device.RDMADeviceOwnerGPUStack, Ports: []device.RDMAPort{
					{Number: 1, State: "ACTIVE", PhysicalState: "LinkUp"},
					{Number: 2, State: "DOWN", PhysicalState: "LinkDown"},
				}},
				{Name: "mlx5_9", Owner: device.RDMADeviceOwnerGPUStack, Ports: []device.RDMAPort{
					{Number: 1, State: "ACTIVE", PhysicalState: "LinkUp"},
				}},
			},
		}
	}
	// ib0's record verdict reads OK on both runs; only the pinned port differs.
	rec := &DevicesReconciler{
		NodeName: nodeName,
		Client: nodeFixture(rdmaDevices(nodeName,
			wholeFunctionIface("ib0", "0", "mlx5_0", &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK}),
		)),
		RDMAInterfaceInventory: inv,
	}

	t.Run("the pinned port down marks its HCAs unhealthy and keeps the rest", func(t *testing.T) {
		rec.applyRDMASelection(RDMAPolicyResolution{
			Status:   RDMAPolicyResolutionPolicy,
			Selected: []string{"mlx5_0", "mlx5_9"},
			Port:     ptr.To(int32(2)),
		})
		resp, err := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec).
			getListAndWatchResponse(context.Background())
		require.NoError(t, err)
		require.Len(t, resp.Devices, 2*nodefeature.RDMAEndpointPoolSize)
		for _, d := range resp.Devices {
			switch {
			case strings.HasPrefix(d.ID, "ib0:"):
				assert.Equal(t, deviceplugin.Unhealthy, d.Health, "token %s", d.ID)
			case strings.HasPrefix(d.ID, "mlx5_9:"):
				assert.Equal(t, deviceplugin.Healthy, d.Health, "token %s: no observation of the pinned port is unverified, not failed", d.ID)
			default:
				t.Fatalf("unexpected token %s", d.ID)
			}
		}
	})

	t.Run("the other port pinned marks the same tokens healthy", func(t *testing.T) {
		rec.applyRDMASelection(RDMAPolicyResolution{
			Status:   RDMAPolicyResolutionPolicy,
			Selected: []string{"mlx5_0", "mlx5_9"},
			Port:     ptr.To(int32(1)),
		})
		resp, err := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec).
			getListAndWatchResponse(context.Background())
		require.NoError(t, err)
		require.Len(t, resp.Devices, 2*nodefeature.RDMAEndpointPoolSize)
		for _, d := range resp.Devices {
			assert.Equal(t, deviceplugin.Healthy, d.Health, "token %s", d.ID)
		}
	})
}
