package deviceplugin

import (
	"context"
	"errors"
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
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlintercept "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// rdmaServedModes are the four allocation modes an RDMA server exists for. Visibility is not
// among them: it names an endpoint another container of the same Pod holds, and the constructor
// refuses it.
var rdmaServedModes = []workercore.DeviceAllocationMode{
	workercore.DeviceAllocationModeExclusive,
	workercore.DeviceAllocationModeShared,
	workercore.DeviceAllocationModeSliced,
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
			name:       "sliced registers the sliced key and its own socket",
			mode:       workercore.DeviceAllocationModeSliced,
			wantName:   "device.gpustack.ai/rdma.sliced",
			wantSocket: "rdma.sliced.sock",
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
				workercore.DeviceAllocationModeShared:      2 * nodefeature.SharedResourceMaxSize,
				workercore.DeviceAllocationModeSliced:      2 * nodefeature.SharedResourceMaxSize,
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
				workercore.DeviceAllocationModeSliced:      0,
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

		// The exact ten IDs one endpoint advertises in the shared family: the index steps by the
		// shared-owner stride, not by one, and kubelet matches the exact string it was offered.
		shared := ids(t, workercore.DeviceAllocationModeShared)
		ib0 := make([]string, 0, nodefeature.SharedResourceMaxSize)
		for _, id := range shared {
			if strings.HasPrefix(id, "ib0:") {
				ib0 = append(ib0, id)
			}
		}
		assert.Equal(t, []string{
			"ib0:ib0:0000", "ib0:ib0:160000", "ib0:ib0:320000", "ib0:ib0:480000", "ib0:ib0:640000",
			"ib0:ib0:800000", "ib0:ib0:960000", "ib0:ib0:1120000", "ib0:ib0:1280000", "ib0:ib0:1440000",
		}, ib0)

		// The sliced family counts the same ceiling as flat indices instead.
		sliced := ids(t, workercore.DeviceAllocationModeSliced)
		eth1 := make([]string, 0, nodefeature.SharedResourceMaxSize)
		for _, id := range sliced {
			if strings.HasPrefix(id, "eth1:") {
				eth1 = append(eth1, id)
			}
		}
		assert.Equal(t, []string{
			"eth1:eth1:0000", "eth1:eth1:0001", "eth1:eth1:0002", "eth1:eth1:0003", "eth1:eth1:0004",
			"eth1:eth1:0005", "eth1:eth1:0006", "eth1:eth1:0007", "eth1:eth1:0008", "eth1:eth1:0009",
		}, eth1)
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
	require.Len(t, resp.Devices, nodefeature.SharedResourceMaxSize)
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
				workercore.DeviceAllocationModeShared:    nodefeature.SharedResourceMaxSize,
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
		require.Len(t, devices, 2*nodefeature.SharedResourceMaxSize)
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
	sliced := rdmaTestServer(t, workercore.DeviceAllocationModeSliced, rec)

	beforeShared, err := shared.getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	beforeSliced, err := sliced.getListAndWatchResponse(context.Background())
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
	afterSliced, err := sliced.getListAndWatchResponse(context.Background())
	require.NoError(t, err)

	assert.Equal(t, beforeShared, afterShared, "the ledger hold changed the shared advertisement")
	assert.Equal(t, beforeSliced, afterSliced, "the reservation or the sweep changed the sliced advertisement")
	for _, d := range afterShared.Devices {
		assert.Equal(t, deviceplugin.Healthy, d.Health)
	}
	for _, d := range afterSliced.Devices {
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
		require.Len(t, resp.Devices, nodefeature.SharedResourceMaxSize)
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
		require.Len(t, resp.Devices, nodefeature.SharedResourceMaxSize)
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

// TestRDMAServer_Start_RegistersAllFourSockets proves the four servers and an accelerator
// neighbor can serve the one plugin directory at once: each registers its own resource under its
// own socket, and a socket collision would fail a generation's listen and with it the
// registration. The resource names are asserted as literals, never recomposed from the constants
// the implementation uses.
func TestRDMAServer_Start_RegistersAllFourSockets(t *testing.T) {
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
	// presence beside the four is what makes the collision check meaningful.
	startResourceServer(t, kubeSocket)

	require.Eventually(t, func() bool {
		return len(kubelet.registrations()) == 5
	}, 15*time.Second, 50*time.Millisecond, "the four RDMA servers and the accelerator neighbour never all registered")

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
		"device.gpustack.ai/rdma.sliced",
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
	assert.Len(t, gotEndpoints, 5, "each server must own its own socket beside kubelet's")
}
