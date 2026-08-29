package preflight

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// manageCall is one allocation the runner made of the responder, as the responder saw it.
type manageCall struct {
	Container string
	Limits    core.ResourceList
	Allocated map[deviceplugin.Resource]int32
}

// manageInjector is a manufacturer's preflighter as checkManagement sees it. Unlike fakeInjector it
// answers per container, because that is exactly what these behaviors are about: what a second container of the
// same Pod is granted, and whether it is what the first holds.
//
// It opens a real deviceplugin.NewPreflightRedirect for the same reason fakeInjector does -- the
// redirect is half of what the promotion onto the host has to undo.
type manageInjector struct {
	build        func(call manageCall, libDir, podsDir string) *deviceplugin.ContainerAllocateResponse
	render       func(call manageCall, podsDir string) error
	responderErr error
	callErr      error

	calls []manageCall
	// modes records the allocation mode each responder was opened for, in call order. A response
	// depends on the mode of the server answering it, so this is what tells a runner that opened the
	// mode the node would have used from one that reused whichever it had.
	modes []workercore.DeviceAllocationMode
}

func (*manageInjector) PreflightAccelerator(device.DevicesGroupList) device.PreflightGroup {
	return device.PreflightGroup{}
}

func (f *manageInjector) PreflightResponder(
	mode workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	f.modes = append(f.modes, mode)
	if f.responderErr != nil {
		return nil, nil, f.responderErr
	}
	_, restore, err := deviceplugin.NewPreflightRedirect()
	if err != nil {
		return nil, nil, err
	}
	return f, restore, nil
}

func (f *manageInjector) GetContainerAllocateResponse(
	_ context.Context, _ *core.Pod, ctr *core.Container,
	_ *workercore.Devices, allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	call := manageCall{Container: ctr.Name, Limits: ctr.Resources.Limits, Allocated: allocated}
	f.calls = append(f.calls, call)
	if f.callErr != nil {
		return nil, f.callErr
	}
	if f.render != nil {
		if err := f.render(call, deviceplugin.OperatorPodsDir); err != nil {
			return nil, err
		}
	}
	return f.build(call, deviceplugin.OperatorLibDir, deviceplugin.OperatorPodsDir), nil
}

// visibleDevices is the injection every manufacturer with a visibility variable produces: the
// accelerators the container may see, named the same way for the owner and for whoever
// co-allocates them.
func visibleDevices(value string) *deviceplugin.ContainerAllocateResponse {
	return &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{"ASCEND_VISIBLE_DEVICES": value},
	}
}

// concurrentHost answers every command with out, and holds each caller until arrivals of them have
// arrived.
//
// It is not answeringHost with a lock bolted on: co-tenancy starts its containers together, and a
// host exec that let the first return before the second began would answer a different question --
// two slices that each had the accelerator to themselves. A run that starts them in sequence never
// reaches the barrier and fails on the deadline rather than passing quietly.
func concurrentHost(root, out string, arrivals int) (*hostExec, *[]string) {
	var (
		mu      sync.Mutex
		asked   []string
		arrived int
		barrier = make(chan struct{})
	)

	h := newHostExec(root)
	h.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != chrootPath || len(args) < 2 || args[0] != root {
			return nil, errors.New("not invoked as the host: " + name)
		}

		mu.Lock()
		asked = append(asked, strings.Join(args[1:], " "))
		arrived++
		if arrived == arrivals {
			close(barrier)
		}
		mu.Unlock()

		select {
		case <-barrier:
			return []byte(out), nil
		case <-time.After(10 * time.Second):
			return nil, errors.New("the co-tenant containers were not started together")
		}
	}
	return h, &asked
}

// tenantAnswer is what one co-tenant's container produced: what it printed, and what the runtime
// returned for it. A container that printed its markers and then exited non-zero is both at once,
// which is the case a single string cannot express.
type tenantAnswer struct {
	out string
	err error
}

// concurrentHostPerCall is concurrentHost with one answer per tenant rather than one for all of
// them, so a test can give the two co-tenants different output -- which is the only way to tell an
// overlap both of them saw from one only a single container reported.
//
// Keyed on the tenant name in the command rather than on arrival order: the two containers are
// started concurrently, so which of them reaches this fake first is not decided by the runner, and
// answering by arrival would hand a test's first answer to whichever goroutine won a race.
func concurrentHostPerCall(root string, outs map[string]tenantAnswer) (*hostExec, *[]string) {
	var (
		mu      sync.Mutex
		asked   []string
		arrived int
		barrier = make(chan struct{})
	)

	h := newHostExec(root)
	h.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != chrootPath || len(args) < 2 || args[0] != root {
			return nil, errors.New("not invoked as the host: " + name)
		}
		command := strings.Join(args[1:], " ")

		mu.Lock()
		asked = append(asked, command)
		arrived++
		if arrived == len(outs) {
			close(barrier)
		}
		mu.Unlock()

		// Matched on where the tenant writes its OWN marker, not on the name appearing anywhere: a
		// probe's command carries its peer's path too -- that is what it waits on -- so a looser
		// match hands a tenant the answer scripted for the other one, and a map's iteration order
		// decides which. That made this fake answer differently between runs of the same test.
		answer, found := tenantAnswer{}, false
		for tenant, a := range outs {
			if strings.Contains(command, ": > "+coTenancyBarrierDir+"/"+tenant+";") {
				answer, found = a, true
				break
			}
		}
		if !found {
			return nil, errors.New("no answer scripted for this tenant: " + command)
		}

		select {
		case <-barrier:
			return []byte(answer.out), answer.err
		case <-time.After(10 * time.Second):
			return nil, errors.New("the co-tenant containers were not started together")
		}
	}
	return h, &asked
}

// amdGroups is one sliceable AMD accelerator, which is the manufacturer whose probe image resolves
// for any family -- so a co-tenancy test is about co-tenancy rather than about image resolution.
func amdGroups() device.DevicesGroupList {
	groups := ascendGroups()
	groups[0].Manufacturer = nodefeature.ManufacturerAMD
	return groups
}

// The order is the contract: an SSH-enabled Instance's owner container is allocated first and its
// sidecar second, and the sidecar selects no device of its own -- it is handed the devices the
// owner was granted, verbatim, which is what ResourceServer.allocateVisibility does.
func TestCheckManagement_DrivesTheSidecarPairInKubeletOrder(t *testing.T) {
	host, _ := answeringHost(fakeHostRoot(t), "", nil)
	injector := &manageInjector{
		build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
			return visibleDevices("0")
		},
	}
	p := &Preflighter{host: host, dryRun: true, runtime: &hostRuntime{Name: "docker"}}

	p.checkManagement(context.Background(), nodefeature.ManufacturerAscend, injector, p.stageLibFor(nodefeature.ManufacturerAscend), ascendGroups())

	require.Len(t, injector.calls, 4, "two allocations per behaviour")
	sidecar := injector.calls[:2]

	assert.Equal(t, probeContainerName(deviceplugin.PreflightContainerName, "npu-0"), sidecar[0].Container,
		"the owner container is allocated first")
	assert.Equal(t, probeContainerName(preflightSidecarContainer, "npu-0"), sidecar[1].Container,
		"the sidecar is allocated after it")
	assert.Equal(t, sidecar[0].Allocated, sidecar[1].Allocated,
		"the sidecar is handed what the owner was granted rather than selecting a device of its own")
	assert.NotEmpty(t, sidecar[0].Allocated)

	// And it asks for the device-only resource, which is what tells the two allocations apart in
	// the Pod the kubelet sees -- a sidecar consumes no accelerator unit of its own.
	assert.Contains(t, sidecar[1].Limits,
		nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerAscend,
			workercore.DeviceAllocationModeVisibility))
	assert.NotContains(t, sidecar[1].Limits,
		nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerAscend,
			workercore.DeviceAllocationModeSliced))
}

// What the behavior concludes is read out of the two injections the allocator itself produced, and
// only out of them: the sidecar has to name exactly the accelerator its owner holds.
func TestCheckManagement_SidecarVisibility(t *testing.T) {
	testCases := []struct {
		name       string
		build      func(call manageCall, libDir, podsDir string) *deviceplugin.ContainerAllocateResponse
		wantState  device.PreflightState
		wantDetail string
		wantReason string
	}{
		{
			name: "the sidecar names exactly what the owner was granted",
			build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
				return visibleDevices("0")
			},
			wantState:  device.PreflightStateOK,
			wantDetail: "names exactly what the owner was granted",
		},
		{
			// The failure this whole behavior exists to catch: a sidecar handed a device its
			// owner does not hold sees another tenant's accelerator.
			name: "a sidecar granted something else is a failure of this accelerator",
			build: func(call manageCall, _, _ string) *deviceplugin.ContainerAllocateResponse {
				if strings.HasPrefix(call.Container, preflightSidecarContainer) {
					return visibleDevices("0,1")
				}
				return visibleDevices("0")
			},
			wantState:  device.PreflightStateUnavailable,
			wantReason: "ASCEND_VISIBLE_DEVICES=0,1",
		},
		{
			// A sidecar that names nothing is the case ResourceServer refuses outright, because a
			// runtime reads an empty visible-devices variable as every device on the host.
			name: "a sidecar naming nothing is a failure rather than a match",
			build: func(call manageCall, _, _ string) *deviceplugin.ContainerAllocateResponse {
				if strings.HasPrefix(call.Container, preflightSidecarContainer) {
					return &deviceplugin.ContainerAllocateResponse{}
				}
				return visibleDevices("0")
			},
			wantState:  device.PreflightStateUnavailable,
			wantReason: "no visible device and no device node at all",
		},
		{
			// Containment, not equality. Measured on hardware: AMD's owner carries
			// ROCR_VISIBLE_DEVICES because its sliced response adds it, while the visibility
			// response the sidecar is served does not. Both name the same card through the same
			// device nodes, so this is an answer rather than the failure the case above is.
			name: "a sidecar naming a subset of the owner's grant is not a failure",
			build: func(call manageCall, _, _ string) *deviceplugin.ContainerAllocateResponse {
				resp := &deviceplugin.ContainerAllocateResponse{
					Devices: []*deviceplugin.DeviceSpec{
						{HostPath: "/dev/kfd", ContainerPath: "/dev/kfd"},
					},
				}
				if !strings.HasPrefix(call.Container, preflightSidecarContainer) {
					resp.Envs = map[string]string{"ROCR_VISIBLE_DEVICES": "GPU-abc"}
				}
				return resp
			},
			wantState:  device.PreflightStateOK,
			wantDetail: "does not carry ROCR_VISIBLE_DEVICES=GPU-abc",
		},
		{
			// Device nodes rather than a visibility variable: the carrier a manufacturer without
			// one uses, and the reason grantedDevices reads both.
			name: "a manufacturer naming device nodes is compared on those",
			build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
				return &deviceplugin.ContainerAllocateResponse{
					Devices: []*deviceplugin.DeviceSpec{
						{HostPath: "/dev/davinci0", ContainerPath: "/dev/davinci0"},
					},
				}
			},
			wantState:  device.PreflightStateOK,
			wantDetail: "/dev/davinci0",
		},
		{
			// A variable that merely ends in "_DEVICES" is not a grant of visibility. Cambricon
			// carries one -- VIRTUAL_DEVICES, the runtime fallback naming the sMLU instance's own
			// device node, which is per container -- and the same node is in the injection's
			// devices anyway, so reading it here would only manufacture a mismatch.
			name: "a variable that does not grant visibility is not compared",
			build: func(call manageCall, _, _ string) *deviceplugin.ContainerAllocateResponse {
				return &deviceplugin.ContainerAllocateResponse{
					Envs: map[string]string{
						"CAMBRICON_VISIBLE_DEVICES": "0",
						"VIRTUAL_DEVICES":           "/dev/cambricon_dev-" + call.Container,
					},
					Devices: []*deviceplugin.DeviceSpec{
						{HostPath: "/dev/cambricon_dev0", ContainerPath: "/dev/cambricon_dev0"},
					},
				}
			},
			wantState:  device.PreflightStateOK,
			wantDetail: "CAMBRICON_VISIBLE_DEVICES=0",
		},
		{
			name: "an owner naming no device has nothing to compare against",
			build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
				return &deviceplugin.ContainerAllocateResponse{
					Envs: map[string]string{"ASCEND_RUNTIME_OPTIONS": "VIRTUAL"},
				}
			},
			wantState:  device.PreflightStateOK,
			wantDetail: "nothing for the sidecar's own to be compared against",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			host, _ := answeringHost(fakeHostRoot(t), "", nil)
			p := &Preflighter{host: host, dryRun: true, runtime: &hostRuntime{Name: "docker"}}

			checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAscend,
				&manageInjector{build: tc.build}, p.stageLibFor(nodefeature.ManufacturerAscend), ascendGroups())

			require.Len(t, checks, 2, "one row per behaviour per accelerator")
			row := checks[0]

			assert.Equal(t, capSidecarVisibility, row.Capability)
			assert.Equal(t, "npu-0", row.Accelerator)
			assert.Equal(t, tc.wantState, row.State)
			assert.Equal(t, device.PreflightDepthSimulated, row.Depth,
				"the artifacts were produced and compared, and nothing ran")
			if tc.wantDetail != "" {
				assert.Contains(t, row.Detail, tc.wantDetail)
			}
			if tc.wantReason != "" {
				assert.Contains(t, row.Reason, tc.wantReason)
			}
			if row.State == device.PreflightStateOK {
				assert.Contains(t, row.Detail, "one-shot", "an ok row says why it went no deeper")
				assert.Empty(t, row.Reason, "a reason is empty exactly when the state is ok")
			} else {
				assert.NotEmpty(t, row.Reason)
			}
		})
	}
}

// A partition-backed accelerator is reported and not driven. Reaching either behavior on one means
// creating a hardware partition, or asserting the responder to the interface that creates them --
// so the rows say what a sidecar would be granted there and why this pass stopped.
func TestCheckManagement_PartitionBackedAcceleratorIsReportedNotDriven(t *testing.T) {
	host, asked := answeringHost(fakeHostRoot(t), "", nil)
	injector := &manageInjector{
		build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
			return visibleDevices("0")
		},
	}
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	partitioned := ascendGroups()
	partitioned[0].Accelerators[0].Status.LogicalSliced.Count = 0
	partitioned[0].Accelerators[0].Status.PhysicalSliced.Count = 7

	checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAscend, injector, p.stageLibFor(nodefeature.ManufacturerAscend), partitioned)

	require.Len(t, checks, 2)
	assert.Empty(t, injector.calls, "no responder is driven for a partition-backed accelerator")
	assert.Empty(t, *asked, "and no container is started")

	for _, row := range checks {
		assert.Equal(t, device.PreflightStateOK, row.State,
			"a limit of this command is not a failure of the node")
		assert.Equal(t, device.PreflightDepthDeclared, row.Depth, "nothing was simulated")
		assert.Contains(t, row.Detail, "granted the partition its owner holds")
		assert.Empty(t, row.Reason)
	}

	// Mode is the only thing that makes two manufacturers' rows comparable, so each capability
	// carries the mode it is a precondition for -- not the mode of the path that produced it.
	// Filing the sidecar row under a slicing mode this accelerator does not even have would hide it
	// from a reader grouping the report by mode.
	byCapability := map[string]device.PreflightCheck{}
	for _, row := range checks {
		byCapability[row.Capability] = row
	}
	assert.Equal(t, device.PreflightModeOf(workercore.DeviceAllocationModeVisibility),
		byCapability[capSidecarVisibility].Mode,
		"what a sidecar is granted is a visibility question on any accelerator")
	assert.Equal(t, device.PreflightModeOf(workercore.DeviceAllocationModePartitioned),
		byCapability[capCoTenancy].Mode,
		"co-tenancy on a partition-backed accelerator is a partitioned-mode question")
}

// An accelerator that can host neither a slice nor a partition has no management behavior to
// answer for, so it contributes no row rather than a row saying it passed.
func TestCheckManagement_Skips(t *testing.T) {
	t.Run("an accelerator hosting neither a slice nor a partition", func(t *testing.T) {
		host, _ := answeringHost(fakeHostRoot(t), "", nil)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		bare := ascendGroups()
		bare[0].Accelerators[0].Status.LogicalSliced.Count = 0

		assert.Empty(t, p.checkManagement(context.Background(), nodefeature.ManufacturerAscend,
			&manageInjector{}, p.stageLibFor(nodefeature.ManufacturerAscend), bare))
	})

	t.Run("a preflighter carrying no injection seam", func(t *testing.T) {
		host, _ := answeringHost(fakeHostRoot(t), "", nil)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		assert.Nil(t, p.checkManagement(context.Background(), nodefeature.ManufacturerAscend,
			noSeamPreflighter{}, p.stageLibFor(nodefeature.ManufacturerAscend), ascendGroups()))
	})
}

// A responder that cannot be built at all leaves both behaviors unanswered at the shallowest
// depth: nothing was simulated, so nothing may claim to have been.
func TestCheckManagement_ResponderRefused(t *testing.T) {
	host, _ := answeringHost(fakeHostRoot(t), "", nil)
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAscend,
		&manageInjector{responderErr: errors.New("no sliced server")}, p.stageLibFor(nodefeature.ManufacturerAscend), ascendGroups())

	require.Len(t, checks, 2)
	for _, row := range checks {
		assert.Equal(t, device.PreflightStateUnavailable, row.State)
		assert.Equal(t, device.PreflightDepthDeclared, row.Depth)
		assert.Contains(t, row.Reason, "no sliced server")
	}
}

// The measured form of co-tenancy: two slices of one accelerator, started at the same time, each
// reporting its own cap rather than the whole card. The host they are started through releases
// neither until both have arrived, so a pass that started them in sequence never reaches measured.
func TestCheckManagement_CoTenancyMeasured(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	host, asked := concurrentHost(fakeHostRoot(t),
		coTenantsMet+"\n"+mapsBegin+"\n"+
			"7f00-7f01 r-xp 00000000 00:2f 12  /usr/local/vrocm/libvrocm.so\n"+mapsEnd+"\n"+
			"card=0 mem_quota_mib=32768\n", 2)
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	injector := &manageInjector{
		build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
			return &deviceplugin.ContainerAllocateResponse{
				Envs: map[string]string{
					"AMD_VISIBLE_DEVICES":         "0",
					"VROCM_DEVICE_MEMORY_LIMIT_0": "32768",
				},
			}
		},
	}

	checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, injector, p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

	require.Len(t, checks, 2)
	row := checks[1]

	assert.Equal(t, capCoTenancy, row.Capability)
	assert.Equal(t, device.PreflightStateOK, row.State)
	assert.Equal(t, device.PreflightDepthMeasured, row.Depth)
	assert.Contains(t, row.Detail, "each reporting its own cap (32768 and 32768)")
	assert.Empty(t, row.Reason)
	assert.Contains(t, row.Evidence, "tenant 1:")
	assert.Contains(t, row.Evidence, "tenant 2:")
	assert.Len(t, *asked, 2, "one container per tenant")

	// The two tenants are two containers of one Pod, which is the axis a second tenant can differ
	// on -- the synthetic Pod's identity is fixed by design.
	tenants := injector.calls[2:]
	require.Len(t, tenants, 2)
	assert.Equal(t, probeContainerName(deviceplugin.PreflightContainerName, "npu-0"), tenants[0].Container)
	assert.Equal(t, probeContainerName(preflightCoTenantContainer, "npu-0"), tenants[1].Container)
	assert.Equal(t, tenants[0].Limits, tenants[1].Limits,
		"a co-tenant asks for exactly what the owner asked for; together they are the whole accelerator")
	assert.NotEmpty(t, tenants[1].Limits)

	// And the log level the shim needs to say which cap it read is preflight's own addition on top
	// of the injection, on both of them.
	for _, command := range *asked {
		assert.Contains(t, command, "LIBVROCM_LOG_LEVEL=2")
		assert.Contains(t, command, "--label "+preflightLabel)
	}
}

// Two containers that each ran but never overlapped establish nothing about co-tenancy, which is
// the claim that one accelerator carries two slices *at once*. Starting them together does not make
// them overlap -- a warm first probe can be finished before the runtime has created the second --
// so each waits for the other at a barrier and says whether it got there, and a run with no such
// answer from both goes no deeper than simulated.
func TestCheckManagement_CoTenancyNeedsAnObservedOverlap(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	// Everything a measured row needs except the barrier's own answer.
	host, _ := concurrentHost(fakeHostRoot(t),
		mapsBegin+"\n7f00-7f01 r-xp 00000000 00:2f 12  /usr/local/vrocm/libvrocm.so\n"+mapsEnd+"\n"+
			"card=0 mem_quota_mib=32768\n", 2)
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, &manageInjector{
		build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
			return &deviceplugin.ContainerAllocateResponse{
				Envs: map[string]string{"VROCM_DEVICE_MEMORY_LIMIT_0": "32768"},
			}
		},
	}, p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

	require.Len(t, checks, 2)
	row := checks[1]

	assert.Equal(t, device.PreflightStateOK, row.State,
		"an overlap this run could not observe is not a node that cannot serve two tenants")
	assert.Equal(t, device.PreflightDepthSimulated, row.Depth)
	assert.Contains(t, row.Detail, "neither waited out the other at the barrier")
	assert.Empty(t, row.Reason)
}

// One tenant reporting the overlap is not the overlap being established. The barrier is symmetric --
// each probe announces itself before waiting -- so a run where only one of them says it met the
// other is a run where the second's answer is missing or truncated, and half an answer must not earn
// the depth the whole one does.
func TestCheckManagement_CoTenancyNeedsBothTenantsToReportIt(t *testing.T) {
	measurable := mapsBegin + "\n7f00-7f01 r-xp 00000000 00:2f 12  /usr/local/vrocm/libvrocm.so\n" +
		mapsEnd + "\ncard=0 mem_quota_mib=32768\n"

	// Both directions, because either tenant's answer alone is half of the evidence: which of the
	// two containers reported the overlap says nothing about whether there was one.
	testCases := []struct {
		name       string
		reportedBy int
	}{
		{name: "only the first tenant reported the overlap", reportedBy: 0},
		{name: "only the second tenant reported the overlap", reportedBy: 1},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stagedImageLib(t, nodefeature.ManufacturerAMD)

			outs := map[string]tenantAnswer{
				tenantName(0): {out: measurable},
				tenantName(1): {out: measurable},
			}
			outs[tenantName(tc.reportedBy)] = tenantAnswer{out: coTenantsMet + "\n" + measurable}

			host, _ := concurrentHostPerCall(fakeHostRoot(t), outs)
			p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

			checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, &manageInjector{
				build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
					return &deviceplugin.ContainerAllocateResponse{
						Envs: map[string]string{"VROCM_DEVICE_MEMORY_LIMIT_0": "32768"},
					}
				},
			}, p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

			require.Len(t, checks, 2)
			row := checks[1]

			assert.Equal(t, device.PreflightDepthSimulated, row.Depth,
				"one tenant's word for it is not two slices observed holding the accelerator at once")
			assert.Contains(t, row.Detail, "neither waited out the other at the barrier")
		})
	}
}

// A cap that was set and never echoed is not a broken node: it is a shallower answer. The two
// containers did run together, so what was established is that they coexist -- and no more.
func TestCheckManagement_CoTenancyCapNotObserved(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	host, _ := concurrentHost(fakeHostRoot(t),
		coTenantsMet+"\n"+mapsBegin+"\n"+mapsEnd+"\nrocm-monitor: no usage region\n", 2)
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, &manageInjector{
		build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
			return &deviceplugin.ContainerAllocateResponse{
				Envs: map[string]string{"VROCM_DEVICE_MEMORY_LIMIT_0": "32768"},
			}
		},
	}, p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

	require.Len(t, checks, 2)
	row := checks[1]

	assert.Equal(t, device.PreflightStateOK, row.State)
	assert.Equal(t, device.PreflightDepthSimulated, row.Depth)
	assert.Contains(t, row.Detail, "did not report its cap back")
	assert.Empty(t, row.Reason)
}

// A container that could not be started at all is a failure of this accelerator, and it names what
// the runtime said.
func TestCheckManagement_CoTenancyContainerFailed(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	root := fakeHostRoot(t)
	host := newHostExec(root)
	host.run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 125: could not select device driver")
	}
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, &manageInjector{
		build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
			return &deviceplugin.ContainerAllocateResponse{}
		},
	}, p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

	require.Len(t, checks, 2)
	row := checks[1]

	// Containers that could not be started say nothing about whether two slices coexist, so this is
	// not the state that exits non-zero: an air-gapped node, or one whose image pull fails, is not a
	// node whose accelerators cannot be shared.
	assert.Equal(t, device.PreflightStateOK, row.State)
	assert.Equal(t, device.PreflightDepthSimulated, row.Depth)
	assert.Contains(t, row.Detail, "could not both be started")
	assert.Contains(t, row.Detail, "could not select device driver")
}

// A co-tenant that started, printed everything the measured clauses look for, and then died under
// the injection is the failure this step exists to catch. The container above never ran; this one
// ran and did not survive, which is the accelerator failing the behavior rather than the node
// failing to reach it.
func TestCheckManagement_CoTenancyTenantExitedNonZero(t *testing.T) {
	measurable := coTenantsMet + "\n" + mapsBegin +
		"\n7f00-7f01 r-xp 00000000 00:2f 12  /usr/local/vrocm/libvrocm.so\n" +
		mapsEnd + "\ncard=0 mem_quota_mib=32768\n"

	// The empty case first, and on the same input: it is what establishes that every measured clause
	// is satisfied here, so the rows below fail for the exit status and nothing else.
	testCases := []struct {
		name    string
		failing []int
	}{
		{name: "both tenants survived", failing: nil},
		{name: "the first tenant died", failing: []int{0}},
		{name: "the second tenant died", failing: []int{1}},
		{name: "both tenants died", failing: []int{0, 1}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stagedImageLib(t, nodefeature.ManufacturerAMD)

			outs := map[string]tenantAnswer{
				tenantName(0): {out: measurable},
				tenantName(1): {out: measurable},
			}
			for _, i := range tc.failing {
				outs[tenantName(i)] = tenantAnswer{out: measurable, err: errors.New("exit status 7")}
			}

			host, _ := concurrentHostPerCall(fakeHostRoot(t), outs)
			p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

			checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, &manageInjector{
				build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
					return &deviceplugin.ContainerAllocateResponse{
						Envs: map[string]string{"VROCM_DEVICE_MEMORY_LIMIT_0": "32768"},
					}
				},
			}, p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

			require.Len(t, checks, 2)
			row := checks[1]

			if len(tc.failing) == 0 {
				assert.Equal(t, device.PreflightStateOK, row.State)
				assert.Equal(t, device.PreflightDepthMeasured, row.Depth)
				assert.Empty(t, row.Reason)
				return
			}

			assert.Equal(t, device.PreflightStateUnavailable, row.State)
			assert.Equal(t, device.PreflightDepthMeasured, row.Depth,
				"the containers did run, so what failed was measured rather than assumed")
			for _, i := range tc.failing {
				assert.Contains(t, row.Reason, tenantName(i)+": exit status 7")
			}
			assert.Equal(t, len(tc.failing), strings.Count(row.Reason, "exit status 7"),
				"a tenant that survived is not named among the ones that did not")
		})
	}
}

// Every step short of starting the two containers is an answer rather than a failure, and each says
// which one it was.
func TestCheckManagement_CoTenancyFallbacks(t *testing.T) {
	build := func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
		return &deviceplugin.ContainerAllocateResponse{}
	}

	t.Run("a manufacturer with no container probe stops at simulated", func(t *testing.T) {
		stagedImageLib(t, nodefeature.ManufacturerMetaX)
		host, asked := answeringHost(fakeHostRoot(t), "", nil)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		metax := ascendGroups()
		metax[0].Manufacturer = nodefeature.ManufacturerMetaX

		checks := p.checkManagement(context.Background(), nodefeature.ManufacturerMetaX,
			&manageInjector{build: build}, p.stageLibFor(nodefeature.ManufacturerMetaX), metax)

		require.Len(t, checks, 2)
		assert.Empty(t, *asked)
		assert.Equal(t, device.PreflightDepthSimulated, checks[1].Depth)
		assert.Contains(t, checks[1].Detail, "no container probe has been established")
	})

	t.Run("libraries that could not be staged stop at simulated", func(t *testing.T) {
		stagedImageLib(t, "some-other-manufacturer")
		host, asked := answeringHost(fakeHostRoot(t), "", nil)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAMD,
			&manageInjector{build: build}, p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

		require.Len(t, checks, 2)
		assert.Empty(t, *asked)
		assert.Equal(t, device.PreflightDepthSimulated, checks[1].Depth)
		assert.Contains(t, checks[1].Detail, "could not be staged onto the host")
	})

	t.Run("a family with no default probe image names the flag", func(t *testing.T) {
		stagedImageLib(t, nodefeature.ManufacturerAscend)
		host, asked := answeringHost(fakeHostRoot(t), "", nil)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		unknown := ascendGroups()
		unknown[0].Family = "910A"

		checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAscend,
			&manageInjector{build: build}, p.stageLibFor(nodefeature.ManufacturerAscend), unknown)

		require.Len(t, checks, 2)
		assert.Empty(t, *asked)
		assert.Contains(t, checks[1].Detail, "--probe-image")
	})

	t.Run("an emitted step prints both containers as one runnable fragment", func(t *testing.T) {
		stagedImageLib(t, nodefeature.ManufacturerAMD)
		root := fakeHostRoot(t)
		host, asked := answeringHost(root, "", nil)
		p := &Preflighter{host: host, dryRun: true, runtime: &hostRuntime{Name: "docker"}}

		checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAMD,
			&manageInjector{build: build}, p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

		require.Len(t, checks, 2)
		row := checks[1]

		assert.Empty(t, *asked, "emit takes no step")
		_, err := os.Stat(filepath.Join(root, deviceplugin.OperatorLibDir, nodefeature.ManufacturerAMD))
		assert.True(t, os.IsNotExist(err), "--emit wrote the library tree onto the host")

		assert.Equal(t, device.PreflightDepthSimulated, row.Depth)
		assert.Contains(t, row.Detail, "a dry run deliberately does not write")
		// Backgrounded and waited on: running the two in sequence is a different experiment, one
		// where each slice had the accelerator to itself.
		assert.Equal(t, 2, strings.Count(row.Command, amdProbeImage), "both containers are printed")
		assert.Contains(t, row.Command, " &\n")
		assert.True(t, strings.HasSuffix(row.Command, "\nwait"))
	})

	t.Run("a fallback emit stages and says nothing about staging", func(t *testing.T) {
		stagedImageLib(t, nodefeature.ManufacturerAMD)
		root := fakeHostRoot(t)
		host, asked := answeringHost(root, "", nil)
		p := &Preflighter{host: host} // no runtime resolved, so the step falls back to emit

		checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAMD,
			&manageInjector{build: build}, p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

		require.Len(t, checks, 2)
		assert.Empty(t, *asked)
		assert.DirExists(t, filepath.Join(root, deviceplugin.OperatorLibDir, nodefeature.ManufacturerAMD))
		assert.Contains(t, checks[1].Detail, "no container runtime")
		assert.NotContains(t, checks[1].Detail, "a dry run deliberately does not write")
	})
}

// Management drives more than one allocation against one responder, and what any of them rendered lands in
// the scratch directory the redirect removes on restore. Every one of the injections has to come
// back addressed as the host addresses it, or the containers mount paths that no longer exist.
func TestCheckManagement_PromotesEveryInjectionOntoTheHost(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	root := fakeHostRoot(t)
	host, asked := concurrentHost(root, "", 2)
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	var scratchPods string
	injector := &manageInjector{
		render: func(call manageCall, podsDir string) error {
			scratchPods = podsDir
			path := filepath.Join(podsDir, "preflight", call.Container, "quota.config")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("memory-quota=32768\n"), 0o644)
		},
		build: func(call manageCall, _, podsDir string) *deviceplugin.ContainerAllocateResponse {
			return &deviceplugin.ContainerAllocateResponse{
				Mounts: []*deviceplugin.Mount{{
					ContainerPath: "/etc/vrocm/quota.config",
					HostPath:      filepath.Join(podsDir, "preflight", call.Container, "quota.config"),
				}},
			}
		},
	}

	p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, injector, p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

	require.Len(t, *asked, 2)
	for _, command := range *asked {
		assert.NotContains(t, command, scratchPods,
			"the command names the scratch directory, which is gone by the time anyone runs it")
	}
	// Each tenant's own rendered file, on the host, under the path its own command mounts -- and
	// the accelerator is part of that path, so a second card cannot render over this one's.
	for _, base := range []string{deviceplugin.PreflightContainerName, preflightCoTenantContainer} {
		container := probeContainerName(base, "npu-0")
		hostPath := filepath.Join(deviceplugin.OperatorPreflightDir, "preflight", container, "quota.config")
		assert.FileExists(t, filepath.Join(root, hostPath))
		assert.Contains(t, strings.Join(*asked, "\n"), hostPath+":/etc/vrocm/quota.config")
	}
}

// Two accelerators must not render into one path. The request builder fixes the Pod's identity so
// two runs agree, and a responder derives its per-container host paths from it -- so with a fixed
// container name every card of a manufacturer writes over the last one's config, and the emitted
// commands all mount a file describing whichever card happened to go last. Measured on an 8-NPU
// host: two files on disk for eight cards, both holding card 7.
func TestCheckManagement_GivesEachAcceleratorItsOwnRenderedPath(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	root := fakeHostRoot(t)
	// Four container steps: a co-tenant pair per accelerator, and the pairs run concurrently.
	host, asked := concurrentHost(root, "", 2)
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	two := amdGroups()
	two[0].Accelerators = append(two[0].Accelerators, workercore.Accelerator{
		ID:     "npu-1",
		Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 8}},
	})

	injector := &manageInjector{
		render: func(call manageCall, podsDir string) error {
			path := filepath.Join(podsDir, "preflight", call.Container, "quota.config")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("card="+call.Container+"\n"), 0o644)
		},
		build: func(call manageCall, _, podsDir string) *deviceplugin.ContainerAllocateResponse {
			return &deviceplugin.ContainerAllocateResponse{
				Mounts: []*deviceplugin.Mount{{
					ContainerPath: "/etc/vrocm/quota.config",
					HostPath:      filepath.Join(podsDir, "preflight", call.Container, "quota.config"),
				}},
			}
		},
	}

	p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, injector,
		p.stageLibFor(nodefeature.ManufacturerAMD), two)

	joined := strings.Join(*asked, "\n")
	for _, accelerator := range []string{"npu-0", "npu-1"} {
		hostPath := filepath.Join(deviceplugin.OperatorPreflightDir, "preflight",
			probeContainerName(deviceplugin.PreflightContainerName, accelerator), "quota.config")
		assert.FileExists(t, filepath.Join(root, hostPath),
			"%s rendered nothing of its own", accelerator)
		assert.Contains(t, joined, hostPath+":/etc/vrocm/quota.config",
			"%s's command does not mount its own config", accelerator)
	}
}

// A marker in the barrier is the only evidence the two tenants overlapped, and a tenant reports the
// overlap the instant it sees its peer's. So a marker that outlived its run -- left by the
// accelerator before this one, or by a pass killed before its sweep -- is an overlap that never
// happened, and the container believing it has met nobody at all.
//
// Two guards, and this covers both: the directory is per-accelerator, and it is emptied before the
// tenants start.
func TestCheckManagement_AStaleBarrierMarkerIsNotAnOverlap(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	root := fakeHostRoot(t)

	// Neither tenant ever signals, so the only markers that can be found are the ones planted below.
	// A pass that reads them reports an overlap; a pass that clears them cannot.
	measurable := mapsBegin + "\n7f00-7f01 r-xp 00000000 00:2f 12  /usr/local/vrocm/libvrocm.so\n" +
		mapsEnd + "\ncard=0 mem_quota_mib=32768\n"
	host, _ := concurrentHost(root, measurable, 2)
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	stale := filepath.Join(root, deviceplugin.OperatorPreflightDir,
		string(deviceplugin.PreflightPodUID), "barrier", barrierComponent("npu-0"))
	require.NoError(t, os.MkdirAll(stale, 0o755))
	for i := range 2 {
		require.NoError(t, os.WriteFile(filepath.Join(stale, tenantName(i)), nil, 0o644))
	}

	checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, &manageInjector{
		build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
			return &deviceplugin.ContainerAllocateResponse{
				Envs: map[string]string{"VROCM_DEVICE_MEMORY_LIMIT_0": "32768"},
			}
		},
	}, p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

	require.Len(t, checks, 2)
	row := checks[1]

	assert.Equal(t, device.PreflightDepthSimulated, row.Depth,
		"a marker from an earlier run is not this pair meeting")
	assert.Contains(t, row.Detail, "neither waited out the other at the barrier")

	entries, err := os.ReadDir(stale)
	require.NoError(t, err)
	assert.Empty(t, entries, "the stale markers were still there for the tenants to find")
}

// An accelerator ID is whatever the vendor driver returned -- Ascend's carry spaces, measured on
// hardware -- and this one is joined into a host path that is then RemoveAll'd. filepath.Join
// resolves "..", so an unchecked ID lets a driver string choose what gets deleted.
func TestCheckManagement_ABarrierPathCannotEscapeThePreflightTree(t *testing.T) {
	t.Run("a traversing id stays inside the barrier root", func(t *testing.T) {
		root := filepath.Join(deviceplugin.OperatorPreflightDir,
			string(deviceplugin.PreflightPodUID), "barrier")

		for _, id := range []string{
			"../../../../etc",
			"../../../../../../../../var/lib/gpustack/operator/pods",
			"a/../../b",
			"E2962A64 8080D745 27D0D6D2 808080E0 104301E3",
		} {
			joined := filepath.Join(root, barrierComponent(id))

			assert.True(t, strings.HasPrefix(joined, root+"/"),
				"%q escaped the barrier root as %q", id, joined)
			assert.Equal(t, joined, filepath.Clean(joined),
				"%q produced a path that still needs cleaning", id)
			assert.NotContains(t, barrierComponent(id), "/", "%q produced more than one component", id)
		}
	})

	// The split is only worth having if two accelerators cannot land in one directory -- which is
	// what an escaping scheme has to prove and a hash gives for free.
	t.Run("two accelerators keep two directories", func(t *testing.T) {
		first := barrierComponent("GPU-5c88007d760374f3")
		second := barrierComponent("GPU-d99e7fe92c7bdf75")

		assert.NotEqual(t, first, second)
		assert.Equal(t, first, barrierComponent("GPU-5c88007d760374f3"), "the same card must be stable")
	})
}

// Each accelerator signals in a directory of its own, so the pair on the second card cannot be
// handed the first card's overlap -- which is what one shared directory did, and it reported
// measured co-tenancy for every card after the first.
func TestCheckManagement_EachAcceleratorGetsItsOwnBarrier(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	root := fakeHostRoot(t)
	host, asked := concurrentHost(root, "", 2)
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	two := amdGroups()
	two[0].Accelerators = append(two[0].Accelerators, workercore.Accelerator{
		ID:     "npu-1",
		Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 8}},
	})

	p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, &manageInjector{
		build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
			return &deviceplugin.ContainerAllocateResponse{}
		},
	}, p.stageLibFor(nodefeature.ManufacturerAMD), two)

	joined := strings.Join(*asked, "\n")
	for _, accelerator := range []string{"npu-0", "npu-1"} {
		assert.Contains(t, joined,
			filepath.Join(deviceplugin.OperatorPreflightDir, "preflight", "barrier",
				barrierComponent(accelerator))+":"+coTenancyBarrierDir,
			"%s's tenants do not signal in a barrier of their own", accelerator)
	}
}

// The management rows are part of what a run reports, and not only of what this file can call
// directly. They are asked of the same preflighter the detection and the slice were asked of, and
// asked whatever those two concluded.
func TestPreflight_ReportsTheManagementRows(t *testing.T) {
	injector := &manageInjector{
		build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
			return visibleDevices("0")
		},
	}
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{
		nodefeature.ManufacturerAscend: func(device.PreflighterOptions) device.AcceleratorPreflighter {
			return injector
		},
	})

	host, _ := answeringHost(fakeHostRoot(t), "", nil)
	p := &Preflighter{host: host, dryRun: true, runtime: &hostRuntime{Name: "docker"}}

	grp := p.preflight(context.Background(), nodefeature.ManufacturerAscend,
		detection(ascendGroups(), false), ascendGroups(), false)

	capabilities := make([]string, 0, len(grp.Checks))
	for _, c := range grp.Checks {
		capabilities = append(capabilities, c.Capability)
	}
	assert.Contains(t, capabilities, capSidecarVisibility)
	assert.Contains(t, capabilities, capCoTenancy)
	assert.Empty(t, grp.Note, "a group carrying checks says nothing about carrying none")
}

// grantedDevices reads how an injection says which accelerators its container may see, and the
// suffix it matches on is a convention rather than a table -- so a manufacturer that spells its
// variable some other way would be compared on nothing and silently agree with everyone.
//
// The allocator sources are read rather than imported, so this holds off linux too, where the
// registry those packages are reached through is empty.
func TestVisibleDevicesSuffixCoversEveryAllocatorThatNamesOne(t *testing.T) {
	entries, err := os.ReadDir("../allocator")
	require.NoError(t, err)

	var named int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sources, globErr := filepath.Glob(filepath.Join("../allocator", e.Name(), "*.go"))
		require.NoError(t, globErr)

		for _, source := range sources {
			body, readErr := os.ReadFile(source)
			require.NoError(t, readErr)

			for _, field := range strings.FieldsFunc(string(body), func(r rune) bool {
				return !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZ_", r)
			}) {
				if !strings.Contains(field, "VISIBLE_DEVICE") {
					continue
				}
				named++
				assert.True(t, strings.HasSuffix(field, visibleDevicesEnvSuffix),
					"%s names %q, which grantedDevices does not read", e.Name(), field)
			}
		}
	}
	assert.NotZero(t, named, "no allocator names a visibility variable; this test now checks nothing")
}

// Two co-tenants are placed one around the other. Placing both against an empty occupancy hands them
// the same geometry, and two containers holding one slice demonstrate nothing about sharing.
func TestCheckManagement_PlacesTheSecondCoTenantAroundTheFirst(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	host, _ := answeringHost(fakeHostRoot(t), "", nil)
	p := &Preflighter{host: host, dryRun: true, runtime: &hostRuntime{Name: "docker"}}

	amd := ascendGroups()
	amd[0].Manufacturer = nodefeature.ManufacturerAMD
	injector := &fakeSlicedInjector{}

	checks := p.checkManagement(context.Background(), nodefeature.ManufacturerAMD,
		injector, p.stageLibFor(nodefeature.ManufacturerAMD), amd)

	require.NotEmpty(t, checks)
	// Owner, then first co-tenant, then second co-tenant: the last is the only one placed against a
	// non-empty occupancy, and what it is given is what the one before it took.
	require.Len(t, injector.occupied, 3)
	assert.Empty(t, injector.occupied[0], "the sidecar case's owner is placed on a clear accelerator")
	assert.Empty(t, injector.occupied[1], "the first co-tenant is placed on a clear accelerator")
	assert.Equal(t,
		deviceplugin.Placements{{Group: "grp-0", Device: "npu-0"}: {{Start: 1, Length: 1}}},
		injector.occupied[2],
		"the second co-tenant is placed around what the first took")
}

// A container's response depends on the mode of the server answering it, not on what the container
// asked for -- NVIDIA's reads the server's own AllocationMode and takes the slicing path for every
// container a sliced server serves. On a node the kubelet's two Allocate calls reach two servers, so
// the sidecar's must be opened for visibility. Measured on hardware: driving it against the sliced
// responder sent it down the slicing path, where it failed for want of a memory percentage the
// product deliberately never puts on a sidecar, and every NVIDIA run exited non-zero on a healthy
// node.
func TestCheckManagement_OpensTheModeTheNodeWouldHaveUsed(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	host, _ := concurrentHost(fakeHostRoot(t), "", 2)
	p := &Preflighter{host: host, dryRun: true, runtime: &hostRuntime{Name: "docker"}}

	injector := &manageInjector{
		build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
			return visibleDevices("0")
		},
	}

	p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, injector,
		p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

	assert.Contains(t, injector.modes, workercore.DeviceAllocationModeVisibility,
		"the sidecar was never served by a visibility responder")
	assert.Contains(t, injector.modes, workercore.DeviceAllocationModeSliced,
		"the owner was never served by a sliced responder")
}

// --emit exists to show what would run before anything does. Writing is part of taking the step, and
// the rendered artifacts are the half that was not gated: measured on an Ascend node, an --emit run
// left two rendered npu_info.config files on the host while every row claimed nothing was written.
func TestCheckManagement_EmitWritesNothingRendered(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	root := fakeHostRoot(t)
	host, asked := concurrentHost(root, "", 2)
	p := &Preflighter{host: host, dryRun: true, runtime: &hostRuntime{Name: "docker"}}

	injector := &manageInjector{
		render: func(call manageCall, podsDir string) error {
			path := filepath.Join(podsDir, "preflight", call.Container, "quota.config")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("rendered\n"), 0o644)
		},
		build: func(manageCall, string, string) *deviceplugin.ContainerAllocateResponse {
			return visibleDevices("0")
		},
	}

	p.checkManagement(context.Background(), nodefeature.ManufacturerAMD, injector,
		p.stageLibFor(nodefeature.ManufacturerAMD), amdGroups())

	assert.Empty(t, *asked, "emit takes no step")
	_, err := os.Stat(filepath.Join(root, deviceplugin.OperatorPreflightDir, "preflight"))
	assert.True(t, os.IsNotExist(err), "--emit carried a rendered artifact onto the host")
}
