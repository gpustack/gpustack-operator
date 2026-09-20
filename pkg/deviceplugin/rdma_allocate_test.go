package deviceplugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// rdmaAllocateFixture stands everything an Allocate reads: a sysfs tree mapping RDMA device names
// to verbs entries, and a device-node tree the response's existence check stats. Plain files stand
// in for character devices, which is the only fact that check reads, so a test never touches the
// host's real /sys or /dev/infiniband.
type rdmaAllocateFixture struct {
	sysfs  *verbsFixture
	devDir string
}

func newRdmaAllocateFixture(t *testing.T) *rdmaAllocateFixture {
	t.Helper()

	f := &rdmaAllocateFixture{
		sysfs:  newVerbsFixture(t),
		devDir: filepath.Join(t.TempDir(), "dev", "infiniband"),
	}
	require.NoError(t, os.MkdirAll(f.devDir, 0o755))

	origSysfs, origDev := rdmaSysfsRoot, rdmaDevNodesDir
	rdmaSysfsRoot, rdmaDevNodesDir = f.sysfs.root, f.devDir
	t.Cleanup(func() { rdmaSysfsRoot, rdmaDevNodesDir = origSysfs, origDev })

	return f
}

// mapVerbs plants the sysfs fact that one RDMA device name resolves to one verbs entry.
func (f *rdmaAllocateFixture) mapVerbs(name, entry string) {
	f.sysfs.addVerbsClassEntry(entry, name)
}

// writeNode stands one device node in the tree the response stats. Its absence from that tree is
// a genuine absence, so a case can name the host that lacks a node.
func (f *rdmaAllocateFixture) writeNode(name string) {
	f.sysfs.t.Helper()
	require.NoError(f.sysfs.t, os.WriteFile(filepath.Join(f.devDir, name), nil, 0o600))
}

// nodePath is where the fixture stands one device node.
func (f *rdmaAllocateFixture) nodePath(name string) string {
	return filepath.Join(f.devDir, name)
}

// deviceSpecsOf renders the specs a response carries as comparable plain values: each node's
// permissions and host path, in order. The proto messages themselves carry internal marshaling
// state once they have been logged, which a deep comparison would trip over without saying
// anything about the grant.
func deviceSpecsOf(ctrResp *ContainerAllocateResponse) []string {
	specs := make([]string, 0, len(ctrResp.Devices))
	for _, d := range ctrResp.Devices {
		specs = append(specs, d.Permissions+":"+d.HostPath)
	}
	return specs
}

// allocateRequest builds the request kubelet sends for one container holding the given device IDs.
func allocateRequest(ids ...string) *AllocateRequest {
	return &AllocateRequest{ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: ids}}}
}

// TestRDMAServer_Allocate pins the response a granted endpoint earns: its own verbs character
// device and never a sibling's, the node-level connection manager once however many endpoints were
// granted, and the environment variable naming the granted RDMA devices.
func TestRDMAServer_Allocate(t *testing.T) {
	const nodeName = "node-rdma-alloc"

	cases := []struct {
		name string
		mode workercore.DeviceAllocationMode
		// ifaces is the interface inventory.
		ifaces []workercore.DeviceInterface
		// ids are the granted device IDs, in whatever order the case wants to hand them.
		ids []string
		// verbs maps each RDMA device name to the sysfs entry resolving it.
		verbs map[string]string
		// nodes are the device nodes the host carries, by entry name.
		nodes []string
		// wantPaths are the injected device node paths, in order.
		wantPaths []string
		// wantHCA is the environment variable's value.
		wantHCA string
	}{
		{
			// The sibling's node is present on the host, so an implementation that injects
			// "some endpoint's" device produces a different, still-valid path and fails: one
			// endpoint cannot stand in for the other.
			name: "one granted endpoint injects its own verbs device and not a sibling's",
			mode: workercore.DeviceAllocationModeShared,
			ifaces: []workercore.DeviceInterface{
				wholeFunctionIface("ib0", "0", "mlx5_0", nil),
				wholeFunctionIface("eth1", "1", "rxe0_eth1", nil),
			},
			ids:       []string{"ib0:ib0:0000"},
			verbs:     map[string]string{"mlx5_0": "uverbs1", "rxe0_eth1": "uverbs0"},
			nodes:     []string{"uverbs0", "uverbs1", "rdma_cm"},
			wantPaths: []string{"uverbs1", "rdma_cm"},
			wantHCA:   "mlx5_0",
		},
		{
			name: "two granted endpoints carry the connection manager once",
			mode: workercore.DeviceAllocationModeShared,
			ifaces: []workercore.DeviceInterface{
				wholeFunctionIface("ib0", "0", "mlx5_0", nil),
				wholeFunctionIface("eth1", "1", "rxe0_eth1", nil),
			},
			ids:       []string{"ib0:ib0:0000", "eth1:eth1:0000"},
			verbs:     map[string]string{"mlx5_0": "uverbs1", "rxe0_eth1": "uverbs0"},
			nodes:     []string{"uverbs0", "uverbs1", "rdma_cm"},
			wantPaths: []string{"uverbs0", "uverbs1", "rdma_cm"},
			wantHCA:   "rxe0_eth1,mlx5_0",
		},
		{
			// A second token on one endpoint is a second claim on the same thing: the endpoint's
			// device is injected once and named once, never twice.
			name: "two tokens of one endpoint inject it once",
			mode: workercore.DeviceAllocationModeShared,
			ifaces: []workercore.DeviceInterface{
				wholeFunctionIface("ib0", "0", "mlx5_0", nil),
			},
			ids:       []string{"ib0:ib0:160000", "ib0:ib0:0000"},
			verbs:     map[string]string{"mlx5_0": "uverbs1"},
			nodes:     []string{"uverbs1", "rdma_cm"},
			wantPaths: []string{"uverbs1", "rdma_cm"},
			wantHCA:   "mlx5_0",
		},
		{
			name: "a host with no connection manager is not an error",
			mode: workercore.DeviceAllocationModeShared,
			ifaces: []workercore.DeviceInterface{
				wholeFunctionIface("ib0", "0", "mlx5_0", nil),
			},
			ids:       []string{"ib0:ib0:0000"},
			verbs:     map[string]string{"mlx5_0": "uverbs1"},
			nodes:     []string{"uverbs1"},
			wantPaths: []string{"uverbs1"},
			wantHCA:   "mlx5_0",
		},
		{
			name: "an exclusive grant injects its endpoint",
			mode: workercore.DeviceAllocationModeExclusive,
			ifaces: []workercore.DeviceInterface{
				wholeFunctionIface("ib0", "0", "mlx5_0", nil),
			},
			ids:       []string{"ib0:ib0:0000"},
			verbs:     map[string]string{"mlx5_0": "uverbs1"},
			nodes:     []string{"uverbs1", "rdma_cm"},
			wantPaths: []string{"uverbs1", "rdma_cm"},
			wantHCA:   "mlx5_0",
		},
		{
			// A partitioned grant names one virtual function; its sibling VF's node is present,
			// so a grant that wandered would inject a different, still-valid path and fail.
			name: "a partitioned grant injects its virtual function and not its sibling's",
			mode: workercore.DeviceAllocationModePartitioned,
			ifaces: []workercore.DeviceInterface{
				sriovPF("pf0", "0",
					rdmaVF("vf0", "", "mlx5_1", nil),
					rdmaVF("vf1", "", "mlx5_2", nil)),
			},
			ids:       []string{"pf0:vf0:0000"},
			verbs:     map[string]string{"mlx5_1": "uverbs2", "mlx5_2": "uverbs3"},
			nodes:     []string{"uverbs2", "uverbs3", "rdma_cm"},
			wantPaths: []string{"uverbs2", "rdma_cm"},
			wantHCA:   "mlx5_1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRdmaAllocateFixture(t)
			for name, entry := range c.verbs {
				f.mapVerbs(name, entry)
			}
			for _, node := range c.nodes {
				f.writeNode(node)
			}

			rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName, c.ifaces...))}
			resp, err := rdmaTestServer(t, c.mode, rec).Allocate(context.Background(), allocateRequest(c.ids...))
			require.NoError(t, err)
			require.Len(t, resp.ContainerResponses, 1)

			ctrResp := resp.ContainerResponses[0]
			want := make([]string, 0, len(c.wantPaths))
			for _, p := range c.wantPaths {
				want = append(want, "rw:"+f.nodePath(p))
			}
			assert.Equal(t, want, deviceSpecsOf(ctrResp))
			assert.Equal(t, map[string]string{"NCCL_IB_HCA": c.wantHCA}, ctrResp.Envs)
			assert.Empty(t, ctrResp.Mounts)
			assert.Empty(t, ctrResp.Annotations)
			assert.Empty(t, ctrResp.CdiDevices)
		})
	}
}

// TestRDMAServer_Allocate_Refuses pins the refusals: a token that does not parse, or that names no
// endpoint of the family being served, is refused rather than silently dropped, and so is an
// endpoint whose verbs device cannot be resolved or is absent from the host. A response that
// quietly grants less than was asked is the silent half-grant this resource exists to end.
func TestRDMAServer_Allocate_Refuses(t *testing.T) {
	const nodeName = "node-rdma-refuse"

	cases := []struct {
		name   string
		mode   workercore.DeviceAllocationMode
		ifaces []workercore.DeviceInterface
		ids    []string
		verbs  map[string]string
		nodes  []string
		// wantErr holds substrings the error must carry, every one of them.
		wantErr []string
	}{
		{
			name:    "an unparseable device id",
			mode:    workercore.DeviceAllocationModeShared,
			ifaces:  []workercore.DeviceInterface{wholeFunctionIface("ib0", "0", "mlx5_0", nil)},
			ids:     []string{"ib0"},
			verbs:   map[string]string{"mlx5_0": "uverbs1"},
			nodes:   []string{"uverbs1"},
			wantErr: []string{"invalid device id", `"ib0"`},
		},
		{
			name:   "a device id naming no endpoint of the inventory",
			mode:   workercore.DeviceAllocationModeShared,
			ifaces: []workercore.DeviceInterface{wholeFunctionIface("ib0", "0", "mlx5_0", nil)},
			ids:    []string{"ib9:ib9:0000"},
			verbs:  map[string]string{"mlx5_0": "uverbs1"},
			nodes:  []string{"uverbs1"},
			wantErr: []string{
				"names no RDMA endpoint",
				`"ib9:ib9:0000"`,
			},
		},
		{
			// A partitioned endpoint is not served under a whole-function family, so its token
			// resolves to nothing here and must be refused, not half-granted.
			name: "a token of a partitioned endpoint under a whole-function family",
			mode: workercore.DeviceAllocationModeShared,
			ifaces: []workercore.DeviceInterface{
				sriovPF("pf0", "0", rdmaVF("vf0", "", "mlx5_1", nil)),
			},
			ids:     []string{"pf0:vf0:0000"},
			verbs:   map[string]string{"mlx5_1": "uverbs2"},
			nodes:   []string{"uverbs2"},
			wantErr: []string{"names no RDMA endpoint", `"pf0:vf0:0000"`},
		},
		{
			// The resolution's own refusal names both layouts it tried, so an operator can tell
			// a host with no RDMA from a distribution with a layout this code does not read.
			name:   "a name resolving under neither sysfs layout fails naming both",
			mode:   workercore.DeviceAllocationModeShared,
			ifaces: []workercore.DeviceInterface{wholeFunctionIface("ib0", "0", "mlx5_gone", nil)},
			ids:    []string{"ib0:ib0:0000"},
			verbs:  map[string]string{"mlx5_0": "uverbs1"},
			nodes:  []string{"uverbs1"},
			wantErr: []string{
				"resolve the verbs character device",
				sysfsVerbsClassDir,
				sysfsVerbsClassName + `" directory under`,
				filepath.Join(sysfsInfinibandDir, "mlx5_gone"),
			},
		},
		{
			// The sysfs tree resolves the name, but the host carries no node for it: the
			// grant would hand over a token whose device the container cannot open, so the
			// allocation fails naming the path rather than succeeding with less.
			name:   "a resolved endpoint with no device node on the host",
			mode:   workercore.DeviceAllocationModeShared,
			ifaces: []workercore.DeviceInterface{wholeFunctionIface("ib0", "0", "mlx5_0", nil)},
			ids:    []string{"ib0:ib0:0000"},
			verbs:  map[string]string{"mlx5_0": "uverbs1"},
			nodes:  []string{"uverbs0"},
			wantErr: []string{
				"has no verbs device node",
				"uverbs1",
			},
		},
		{
			name:    "an empty grant",
			mode:    workercore.DeviceAllocationModeShared,
			ifaces:  []workercore.DeviceInterface{wholeFunctionIface("ib0", "0", "mlx5_0", nil)},
			ids:     nil,
			verbs:   map[string]string{"mlx5_0": "uverbs1"},
			nodes:   []string{"uverbs1"},
			wantErr: []string{"grants no RDMA device"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRdmaAllocateFixture(t)
			for name, entry := range c.verbs {
				f.mapVerbs(name, entry)
			}
			for _, node := range c.nodes {
				f.writeNode(node)
			}

			rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName, c.ifaces...))}
			resp, err := rdmaTestServer(t, c.mode, rec).Allocate(context.Background(), allocateRequest(c.ids...))
			require.Error(t, err)
			require.Nil(t, resp)
			for _, want := range c.wantErr {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

// TestRDMAServer_Allocate_RefusesARequestWithNoContainer covers the request envelope rather than
// what it carries. The handler reads the first container request, and this is an external gRPC
// boundary where grpc-go does not recover a handler panic: an empty slice would end the whole
// device-manager process, every vendor allocator with it, rather than the one call. Indexing it
// panics, which this case reports as a failure on its own -- no assertion needed for that half.
func TestRDMAServer_Allocate_RefusesARequestWithNoContainer(t *testing.T) {
	const nodeName = "node-rdma-no-container"

	rec := &DevicesReconciler{
		NodeName: nodeName,
		Client:   nodeFixture(rdmaDevices(nodeName, wholeFunctionIface("ib0", "0", "mlx5_0", nil))),
	}

	resp, err := rdmaTestServer(t, workercore.DeviceAllocationModeExclusive, rec).
		Allocate(context.Background(), &AllocateRequest{})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "no container requests")
}

// TestRDMAServer_Allocate_NoCrossModeExclusion is the behavioral criterion that no cross-mode
// exclusion was introduced between the two pooled families: a real Allocate on one family's server,
// which must succeed -- an errored Allocate crosses no boundary and proves nothing -- followed by
// the other family's next ListAndWatch advertisement, which must still carry the endpoint's full
// token count, every token Healthy.
//
// This is deliberately an assertion about the advertisement an allocation leaves behind, not about
// the absence of a call: the wrong implementation this guards against is one that counts live
// allocations in process and flips the sibling family's tokens Unhealthy, which would halve the
// node's capacity with nothing to show for it. The watching family's stream is driven for real and
// its next response read after a broadcast, so the case observes what kubelet would observe.
func TestRDMAServer_Allocate_NoCrossModeExclusion(t *testing.T) {
	const nodeName = "node-rdma-cross"

	runHalf := func(t *testing.T, allocating, watching workercore.DeviceAllocationMode) {
		t.Helper()

		f := newRdmaAllocateFixture(t)
		f.mapVerbs("mlx5_0", "uverbs1")
		f.writeNode("uverbs1")
		f.writeNode("rdma_cm")

		rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(rdmaDevices(nodeName,
			wholeFunctionIface("ib0", "1", "mlx5_0", nil)))}
		allocatingServer := rdmaTestServer(t, allocating, rec)
		watchingServer := rdmaTestServer(t, watching, rec)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sent := make(chan *ListAndWatchResponse, 4)
		returned := make(chan error, 1)
		go func() {
			returned <- watchingServer.ListAndWatch(&Empty{}, &fakeListAndWatchStream{ctx: ctx, sent: sent})
		}()
		t.Cleanup(func() {
			select {
			case <-returned:
			case <-time.After(15 * time.Second):
				t.Error("ListAndWatch did not return after its context was canceled")
			}
		})

		// The pre-allocation advertisement is the baseline the next one is judged against.
		select {
		case resp := <-sent:
			require.Len(t, resp.Devices, nodefeature.SharedResourceMaxSize)
			for _, d := range resp.Devices {
				require.Equal(t, deviceplugin.Healthy, d.Health)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("no initial list and watch response")
		}

		// The allocation crossing the family boundary, for real.
		resp, err := allocatingServer.Allocate(context.Background(), allocateRequest("ib0:ib0:0000"))
		require.NoError(t, err, "the allocation must really succeed: an errored Allocate crosses no boundary")
		require.Equal(t, map[string]string{"NCCL_IB_HCA": "mlx5_0"}, resp.ContainerResponses[0].Envs)

		// The watching family's next advertisement, pushed by a live-pod sweep broadcast the
		// way a real reconcile would.
		rec.notifiersMutex.Lock()
		rec.lastLivePodUIDs = []string{"pod-live"}
		rec.notifiersMutex.Unlock()
		rec.notifyListeners()

		select {
		case resp := <-sent:
			require.Len(t, resp.Devices, nodefeature.SharedResourceMaxSize,
				"the endpoint keeps its full token count after the sibling family allocated it")
			for _, d := range resp.Devices {
				require.Equal(t, deviceplugin.Healthy, d.Health,
					"an allocation on the %s family must not withhold the endpoint from the %s family",
					allocating, watching)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("no list and watch response after the allocation")
		}
	}

	t.Run("a shared allocation leaves the sliced advertisement healthy", func(t *testing.T) {
		runHalf(t, workercore.DeviceAllocationModeShared, workercore.DeviceAllocationModeSliced)
	})
	t.Run("a sliced allocation leaves the shared advertisement healthy", func(t *testing.T) {
		runHalf(t, workercore.DeviceAllocationModeSliced, workercore.DeviceAllocationModeShared)
	})
}

// TestRDMAServer_Allocate_ReadsCurrentInventory pins that the response is built from the record as
// it stands: the detector correcting an interface's bound RDMA device between two allocations
// changes what the second allocation injects, because nothing was remembered from the first.
func TestRDMAServer_Allocate_ReadsCurrentInventory(t *testing.T) {
	const nodeName = "node-rdma-fresh"

	f := newRdmaAllocateFixture(t)
	f.mapVerbs("mlx5_0", "uverbs1")
	f.mapVerbs("mlx5_9", "uverbs4")
	f.writeNode("uverbs1")
	f.writeNode("uverbs4")
	f.writeNode("rdma_cm")

	devs := rdmaDevices(nodeName, wholeFunctionIface("ib0", "0", "mlx5_0", nil))
	rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(devs)}
	s := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec)

	first, err := s.Allocate(context.Background(), allocateRequest("ib0:ib0:0000"))
	require.NoError(t, err)
	assert.Equal(t, "mlx5_0", first.ContainerResponses[0].Envs["NCCL_IB_HCA"])

	// The detector rebinds the interface to another device; the next allocation hands over the
	// new one, not the remembered old one.
	devs.Spec.Interfaces[0].RDMADevice = "mlx5_9"
	require.NoError(t, rec.Client.Update(context.Background(), devs))

	second, err := s.Allocate(context.Background(), allocateRequest("ib0:ib0:0000"))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"NCCL_IB_HCA": "mlx5_9"}, second.ContainerResponses[0].Envs)
	assert.Equal(t,
		[]string{"rw:" + f.nodePath("uverbs4"), "rw:" + f.nodePath("rdma_cm")},
		deviceSpecsOf(second.ContainerResponses[0]))
}

// TestRDMAServer_Allocate_WritesNothing pins that an allocation leaves no trace in the record it
// read: the Devices object is byte-identical after an Allocate, and no reservation appears in the
// reconciler. The advertisement's freedom from allocation state is what lets the two pooled
// families draw on the same HCAs, and that freedom rests on nothing being written here.
func TestRDMAServer_Allocate_WritesNothing(t *testing.T) {
	const nodeName = "node-rdma-write"

	f := newRdmaAllocateFixture(t)
	f.mapVerbs("mlx5_0", "uverbs1")
	f.writeNode("uverbs1")
	f.writeNode("rdma_cm")

	devs := rdmaDevices(nodeName, wholeFunctionIface("ib0", "1", "mlx5_0", nil))
	rec := &DevicesReconciler{NodeName: nodeName, Client: nodeFixture(devs)}
	s := rdmaTestServer(t, workercore.DeviceAllocationModeShared, rec)

	_, err := s.Allocate(context.Background(), allocateRequest("ib0:ib0:0000"))
	require.NoError(t, err)

	after := &workercore.Devices{}
	require.NoError(t, rec.Client.Get(context.Background(), ctrlcli.ObjectKey{Name: nodeName}, after))
	assert.Equal(t, devs, after, "the allocation wrote to the record it read")

	rec.reservationsMutex.RLock()
	defer rec.reservationsMutex.RUnlock()
	assert.Empty(t, rec.reservations, "the allocation recorded an in-process reservation")
}
