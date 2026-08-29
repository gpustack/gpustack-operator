package preflight

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// answeringHost returns a host exec that answers every command with out, recording the argv it was
// asked. It is the counterpart to scriptedHost for the cases that cannot state the argv up front:
// what a container step runs depends on the injection the responder produced, which is the very
// thing under test.
func answeringHost(root, out string, err error) (*hostExec, *[]string) {
	var asked []string
	h := newHostExec(root)
	h.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != chrootPath || len(args) < 2 || args[0] != root {
			return nil, errors.New("not invoked as the host: " + name)
		}
		asked = append(asked, strings.Join(args[1:], " "))
		return []byte(out), err
	}
	return h, &asked
}

// fakeInjector is a manufacturer's preflighter as measureSliced sees it: a driver read it never
// calls, and a responder built inside the redirect its own package opened.
//
// It opens a real deviceplugin.NewPreflightRedirect rather than faking one, because the redirect is
// half of what is under test: the injection a responder builds names the scratch directory, and the
// promotion onto the host is what has to put it back.
type fakeInjector struct {
	build func(container, libDir, podsDir string) *deviceplugin.ContainerAllocateResponse
	// render, when non-nil, is what the responder writes into the redirected pods directory before
	// answering -- the second door a responder reaches the host through.
	render func(podsDir string) error
	// privatePath, when non-nil, stands in for a manufacturer's own host path -- NVIDIA's HAMi-core
	// lock directory is the real one. It is handed to the redirect rather than joined under the root
	// by hand, which is the contract the rewrite depends on.
	privatePath  *string
	responderErr error
}

func (fakeInjector) PreflightAccelerator(device.DevicesGroupList) device.PreflightGroup {
	return device.PreflightGroup{}
}

func (f *fakeInjector) PreflightResponder(
	workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	if f.responderErr != nil {
		return nil, nil, f.responderErr
	}
	var private []*string
	if f.privatePath != nil {
		private = append(private, f.privatePath)
	}
	_, restore, err := deviceplugin.NewPreflightRedirect(private...)
	if err != nil {
		return nil, nil, err
	}
	return f, restore, nil
}

func (f *fakeInjector) GetContainerAllocateResponse(
	_ context.Context, _ *core.Pod, ctr *core.Container, _ *workercore.Devices,
	_ map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	if f.render != nil {
		if err := f.render(deviceplugin.OperatorPodsDir); err != nil {
			return nil, err
		}
	}
	return f.build(ctr.Name, deviceplugin.OperatorLibDir, deviceplugin.OperatorPodsDir), nil
}

// ascendGroups is one 910B group carrying one sliceable accelerator, which is what a detect pass on
// the hardware T11 uses reports.
func ascendGroups() device.DevicesGroupList {
	return device.DevicesGroupList{{
		ID:             "grp-0",
		Manufacturer:   nodefeature.ManufacturerAscend,
		Name:           "Ascend910B2",
		Family:         "910B",
		RuntimeVersion: "8.0.0",
		Memory:         65536,
		Accelerators: []workercore.Accelerator{{
			ID:     "npu-0",
			Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 8}},
		}},
	}}
}

// stagedImageLib points inImageLibDir at a scratch tree carrying one manufacturer, so StageLib
// succeeds without the container image this normally runs in.
func stagedImageLib(t *testing.T, manufacturer string) {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, manufacturer), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, manufacturer, "ld.so.preload"),
		[]byte("/opt/enpu/vcann-rt/lib/libvruntime.so\n"), 0o644))

	original := inImageLibDir
	inImageLibDir = root
	t.Cleanup(func() { inImageLibDir = original })
}

// The promotion this whole task turns on: a responder answers inside a redirect that points the
// shared host paths at a scratch directory, so nothing it renders touches the node -- and then that
// directory is removed when the redirect is restored. A container started against the injection as
// the responder built it would mount paths that no longer exist. So what the responder rendered is
// carried onto the host through the mounted host root, and every host path is rewritten onto the
// location the host knows it by.
func TestMeasureSliced_PromotesTheSimulatedInjectionOntoTheHost(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAscend)
	root := fakeHostRoot(t)
	host, asked := answeringHost(root, "", nil)

	var scratchPods string
	injector := &fakeInjector{
		render: func(podsDir string) error {
			scratchPods = podsDir
			path := filepath.Join(podsDir, "preflight/preflight/etc/enpu/vcann-rt/npu_info.config")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("memory-quota=32768\n"), 0o644)
		},
		build: func(_, libDir, podsDir string) *deviceplugin.ContainerAllocateResponse {
			return &deviceplugin.ContainerAllocateResponse{
				Envs: map[string]string{"ASCEND_VISIBLE_DEVICES": "0"},
				Mounts: []*deviceplugin.Mount{
					{
						ContainerPath: "/opt/enpu/vcann-rt/lib/libvruntime.so",
						HostPath:      filepath.Join(libDir, "ascend/cann-8-910b/lib/libvruntime.so"),
						ReadOnly:      true,
					},
					{
						ContainerPath: "/etc/enpu/vcann-rt/npu_info.config",
						HostPath: filepath.Join(podsDir,
							"preflight/preflight/etc/enpu/vcann-rt/npu_info.config"),
						ReadOnly: true,
					},
					// Never redirected in the first place: it has to come through untouched.
					{ContainerPath: "/dev/shm", HostPath: "/dev/shm"},
				},
			}
		},
	}

	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}
	checks := p.measureSliced(context.Background(), nodefeature.ManufacturerAscend, injector, p.stageLibFor(nodefeature.ManufacturerAscend), ascendGroups())

	require.Len(t, checks, 2)
	require.Len(t, *asked, 1, "one container per sliceable accelerator")
	command := (*asked)[0]

	assert.NotContains(t, command, scratchPods,
		"the command names the scratch directory, which is gone by the time anyone runs it")
	assert.Contains(t, command,
		filepath.Join(deviceplugin.OperatorPreflightDir,
			"preflight/preflight/etc/enpu/vcann-rt/npu_info.config")+":/etc/enpu/vcann-rt/npu_info.config:ro",
		"the rendered config is mounted from where the host keeps it")
	assert.Contains(t, command,
		filepath.Join(deviceplugin.OperatorLibDir, "ascend/cann-8-910b/lib/libvruntime.so"),
		"the staged library is mounted from where StageLib put it")
	assert.Contains(t, command, "/dev/shm:/dev/shm", "a path under neither redirect is untouched")

	// And what the responder rendered really is on the host now, not only named there.
	promoted := filepath.Join(root, deviceplugin.OperatorPreflightDir,
		"preflight/preflight/etc/enpu/vcann-rt/npu_info.config")
	body, err := os.ReadFile(promoted)
	require.NoError(t, err, "the rendered config was not carried onto the host root")
	assert.Equal(t, "memory-quota=32768\n", string(body))

	// The redirect was restored and its directory removed, which is what made the promotion
	// necessary in the first place.
	_, err = os.Stat(scratchPods)
	assert.True(t, os.IsNotExist(err), "the scratch directory outlived the pass")
	assert.Equal(t, "/var/lib/gpustack/operator/pods", deviceplugin.OperatorPodsDir)
	assert.Equal(t, "/var/lib/gpustack/operator/preflight", deviceplugin.OperatorPreflightDir,
		"the promotion target is a tree of preflight's own, which is what makes a leftover harmless")
}

// A manufacturer's own host path is redirected exactly as the shared pair is, and has to come back
// out addressed the same way. Measured on hardware: NVIDIA's HAMi-core lock directory came out of a
// dry run as /tmp/gpustack-preflight-1194651507/vgpulock, so the emitted command mounted a directory
// that exists on no node -- a reader running it would get an empty one docker created on the spot,
// coordinating with nothing, which is precisely the failure the co-tenancy row claims to rule out.
func TestMeasureSliced_RehostsTheManufacturersPrivatePath(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAscend)
	root := fakeHostRoot(t)
	host, asked := answeringHost(root, "", nil)

	lock := "/tmp/vgpulock"
	var scratchLock string
	injector := &fakeInjector{
		privatePath: &lock,
		build: func(_, libDir, _ string) *deviceplugin.ContainerAllocateResponse {
			scratchLock = lock
			return &deviceplugin.ContainerAllocateResponse{
				Envs: map[string]string{"ASCEND_VISIBLE_DEVICES": "0"},
				Mounts: []*deviceplugin.Mount{
					{
						ContainerPath: "/opt/enpu/vcann-rt/lib/libvruntime.so",
						HostPath:      filepath.Join(libDir, "ascend/cann-8-910b/lib/libvruntime.so"),
					},
					{ContainerPath: "/tmp/vgpulock", HostPath: lock},
				},
			}
		},
	}

	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}
	p.measureSliced(context.Background(), nodefeature.ManufacturerAscend, injector,
		p.stageLibFor(nodefeature.ManufacturerAscend), ascendGroups())

	require.Len(t, *asked, 1)
	command := (*asked)[0]

	require.NotEqual(t, "/tmp/vgpulock", scratchLock, "the responder answered outside the redirect")
	assert.NotContains(t, command, scratchLock,
		"the command names the scratch lock directory, which is gone by the time anyone runs it")
	assert.Contains(t, command, "/tmp/vgpulock:/tmp/vgpulock",
		"the lock is mounted from where a real allocation keeps it")
	assert.Equal(t, "/tmp/vgpulock", lock, "the redirect was not restored")
}

// A container that could not be started is an environment this pass could not measure in, not an
// accelerator that cannot be sliced -- and only the second may exit non-zero. The failure this
// covers is one the command predicts itself: without host networking an image pull dies on DNS, and
// reporting that as `unavailable` tells an operator their hardware is broken.
func TestMeasureSliced_AContainerThatCannotStartIsNotAnUnavailableSlice(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAscend)
	root := fakeHostRoot(t)
	host := newHostExec(root)
	host.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("Unable to find image locally"), errors.New("exit status 125: pull access denied")
	}
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker", NetworkWarning: networkWarning}}

	checks := p.measureSliced(context.Background(), nodefeature.ManufacturerAscend, &fakeInjector{
		build: func(string, string, string) *deviceplugin.ContainerAllocateResponse {
			return &deviceplugin.ContainerAllocateResponse{}
		},
	}, p.stageLibFor(nodefeature.ManufacturerAscend), ascendGroups())

	require.Len(t, checks, 2)
	for _, c := range checks {
		assert.Equal(t, device.PreflightStateOK, c.State, c.Capability+" exits non-zero on a pull failure")
		assert.Equal(t, device.PreflightDepthSimulated, c.Depth)
		assert.Contains(t, c.Detail, "could not be started")
		assert.Contains(t, c.Detail, "Re-run with host networking", "the predicted cause is carried")
		assert.Contains(t, c.Evidence, "Unable to find image locally",
			"the container's own last words are the operator's lead")
	}
}

// Staging into a path that is not a mounted host root writes into this container and vanishes with
// it. Reporting that as staged hands the reader a command mounting a directory the host never had,
// with nothing in the row to say so.
func TestStageLibFor_RefusesAPathThatIsNotAHostRoot(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAscend)
	p := &Preflighter{host: newHostExec(t.TempDir())} // exists, but carries none of the markers

	staged := p.stageLibFor(nodefeature.ManufacturerAscend)

	assert.True(t, staged.Failed, "a tree was reported staged into a directory the host does not have")
	assert.Contains(t, staged.Reason, "host root")
}

// What a responder rendered has to reach the host for the container to mount it, and is cleared
// again when the pass finishes.
//
// The sweep reaches preflight's own tree and stops there. The pod tree beside it is what an
// allocator reads as its record of what other Pods hold -- Ascend derives the next free vNPU id
// from it, Hygon the free CU windows -- so a sweep that reached into it would hand a live
// workload's placement to the next allocation.
func TestPreflightAccelerator_RemovesWhatItRendered(t *testing.T) {
	root := fakeHostRoot(t)
	host := newHostExec(root)
	p := &Preflighter{host: host}

	ours := filepath.Join(root, deviceplugin.OperatorPreflightDir, string(deviceplugin.PreflightPodUID))
	// Under the pod tree, and under the very UID preflight fabricates for itself: this is what a
	// sweep addressing the wrong root would take with it, and an allocator reads it as a placement.
	theirs := filepath.Join(root, deviceplugin.OperatorPodsDir, string(deviceplugin.PreflightPodUID))
	for _, dir := range []string{ours, theirs} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "c/etc"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "c/etc/npu_info.config"), []byte("x"), 0o644))
	}

	p.sweepRenderedArtifacts()

	_, err := os.Stat(ours)
	assert.True(t, os.IsNotExist(err), "this pass left behind what it rendered")
	_, err = os.Stat(theirs)
	assert.NoError(t, err, "a live Pod's placement was taken away")
}

// A dry run rendered nothing, so there is nothing of its own to remove -- and a sweep that ran
// anyway would remove what an earlier, non-dry run legitimately left staged.
func TestPreflightAccelerator_DryRunSweepsNothing(t *testing.T) {
	root := fakeHostRoot(t)
	p := &Preflighter{host: newHostExec(root), dryRun: true}

	ours := filepath.Join(root, deviceplugin.OperatorPreflightDir, string(deviceplugin.PreflightPodUID))
	require.NoError(t, os.MkdirAll(ours, 0o755))

	p.sweepRenderedArtifacts()

	_, err := os.Stat(ours)
	assert.NoError(t, err, "a dry run removed what an earlier run had staged")
}

// The verdict is read out of what the container printed, and both clauses look for something the
// runner already holds: the shared objects this injection mounts, and the cap this injection set.
func TestJudgeProbeOutput(t *testing.T) {
	const shim = "/opt/enpu/vcann-rt/lib/libvruntime.so"

	injection := func(envs map[string]string) *deviceplugin.ContainerAllocateResponse {
		return &deviceplugin.ContainerAllocateResponse{
			Envs:   envs,
			Mounts: []*deviceplugin.Mount{{ContainerPath: shim}, {ContainerPath: "/etc/ld.so.preload"}},
		}
	}

	testCases := []struct {
		name        string
		injection   *deviceplugin.ContainerAllocateResponse
		probe       sliceProbe
		output      string
		exitError   string
		wantLoaded  device.PreflightState
		wantQuota   device.PreflightDepth
		wantQuotaIn string
		// Defaults to ok, which every case but the two exit-status ones expects.
		wantQuotaState device.PreflightState
	}{
		{
			name:      "the shim is mapped and the cap is reported back",
			injection: injection(map[string]string{"HGGC_DEVICE_MEMORY_LIMIT_0": "32768"}),
			probe:     sliceProbe{MemoryQuotaEnvPrefix: "HGGC_DEVICE_MEMORY_LIMIT_"},
			output: mapsBegin + "\n7f00-7f01 r-xp 00000000 00:2f 12  " + shim + "\n" + mapsEnd + "\n" +
				"card=0 mem_quota_mib=32768 mem_used_mib=0\n",
			wantLoaded:  device.PreflightStateOK,
			wantQuota:   device.PreflightDepthMeasured,
			wantQuotaIn: "32768",
		},
		{
			// The whole point of the load clause: a container whose maps do not carry the object
			// the injection mounted has the whole card and no cap at all.
			name:       "a shim that did not load is a failure of this accelerator",
			injection:  injection(map[string]string{"HGGC_DEVICE_MEMORY_LIMIT_0": "32768"}),
			probe:      sliceProbe{MemoryQuotaEnvPrefix: "HGGC_DEVICE_MEMORY_LIMIT_"},
			output:     mapsBegin + "\n7f00-7f01 r-xp 00000000 00:2f 12  /lib/x86_64-linux-gnu/libc.so.6\n" + mapsEnd + "\n",
			wantLoaded: device.PreflightStateUnavailable,
			// And the cap could not be observed either, for the same reason: the thing that would
			// have reported it never loaded. Reporting the quota as measured here would claim an
			// observation that the row above says did not happen.
			wantQuota: device.PreflightDepthSimulated,
		},
		{
			// A cap that is set and was not seen is not a broken slice: it is a slice that was not
			// observed, which is a shallower answer rather than a failing one.
			name:      "a cap the container never echoed goes no deeper than simulated",
			injection: injection(map[string]string{"HGGC_DEVICE_MEMORY_LIMIT_0": "32768"}),
			probe:     sliceProbe{MemoryQuotaEnvPrefix: "HGGC_DEVICE_MEMORY_LIMIT_"},
			output: mapsBegin + "\n7f00-7f01 r-xp 00000000 00:2f 12  " + shim + "\n" + mapsEnd + "\n" +
				"ppu-monitor: no usage region\nprobe-reader-exit-1\n",
			wantLoaded: device.PreflightStateOK,
			wantQuota:  device.PreflightDepthSimulated,
		},
		{
			// This probe names the carrier and the request asked for a cap, so an injection without
			// one is a slice that bounds nothing -- not a shallower answer. Reported ok, it exited
			// zero for a container the allocator failed to cap.
			name:           "an injection carrying no cap at all bounds nothing, and says so",
			injection:      injection(nil),
			probe:          sliceProbe{MemoryQuotaEnvPrefix: "HGGC_DEVICE_MEMORY_LIMIT_"},
			output:         mapsBegin + "\n7f00-7f01 r-xp 00000000 00:2f 12  " + shim + "\n" + mapsEnd + "\n",
			wantLoaded:     device.PreflightStateOK,
			wantQuota:      device.PreflightDepthMeasured,
			wantQuotaState: device.PreflightStateUnavailable,
			wantQuotaIn:    "carries no HGGC_DEVICE_MEMORY_LIMIT_* variable",
		},
		{
			// Reached only for a sliceable accelerator, a manufacturer with a container probe, and an
			// injection the allocator produced for a sliced request -- so no shared object is a slice
			// with no runtime behind it, not hardware that declares no slicing. Reported
			// not-declared, it contradicted the detect pass above it and exited zero.
			name: "an injection mounting no shared object has no slicing runtime, which is a failure",
			injection: &deviceplugin.ContainerAllocateResponse{
				Mounts: []*deviceplugin.Mount{{ContainerPath: "/etc/ld.so.preload"}},
			},
			probe:          sliceProbe{},
			output:         mapsBegin + "\n7f00-7f01 r-xp 00000000 00:2f 12  /lib/x86_64-linux-gnu/libc.so.6\n" + mapsEnd + "\n",
			wantLoaded:     device.PreflightStateUnavailable,
			wantQuota:      device.PreflightDepthMeasured,
			wantQuotaState: device.PreflightStateUnavailable,
		},
		{
			// The dynamic loader names the object it refused in the refusal itself, and stderr is
			// merged into this evidence -- so the one message that proves the shim did NOT load
			// contains the very path that is looked for as proof it did. Only the mapping section
			// counts, and only a mapping's own pathname field within it.
			name:      "a loader refusal naming the object is not evidence that it loaded",
			injection: injection(map[string]string{"HGGC_DEVICE_MEMORY_LIMIT_0": "32768"}),
			probe:     sliceProbe{MemoryQuotaEnvPrefix: "HGGC_DEVICE_MEMORY_LIMIT_"},
			output: "ERROR: ld.so: object '" + shim + "' from LD_PRELOAD cannot be preloaded " +
				"(cannot open shared object file): ignored.\n" +
				mapsBegin + "\n" +
				"7f00-7f01 r-xp 00000000 00:2f 12  /lib/x86_64-linux-gnu/libc.so.6\n" +
				mapsEnd + "\n",
			wantLoaded: device.PreflightStateUnavailable,
			wantQuota:  device.PreflightDepthSimulated,
		},
		{
			// The shim's own debug line echoes the cap it was configured with, which is the
			// injection's figure verbatim -- so finding it anywhere in the output proves only that
			// the shim read the environment, not that the vendor's reader saw a capped card. Only
			// the reader's section counts.
			name:      "the shim echoing its own configured cap is not the reader reporting it",
			injection: injection(map[string]string{"HGGC_DEVICE_MEMORY_LIMIT_0": "32768"}),
			probe:     sliceProbe{MemoryQuotaEnvPrefix: "HGGC_DEVICE_MEMORY_LIMIT_"},
			output: "[libvgpu] configured device memory limit: 32768 MiB\n" +
				mapsBegin + "\n" +
				"7f00-7f01 r-xp 00000000 00:2f 12  " + shim + "\n" +
				mapsEnd + "\n" +
				"card=0 mem_quota_mib=65536 mem_used_mib=0\n",
			wantLoaded: device.PreflightStateOK,
			wantQuota:  device.PreflightDepthSimulated,
		},
		{
			// A container that started, loaded the shim and then died under it is this accelerator
			// failing the behavior being measured -- the shim crashing the probe process is exactly
			// what the step exists to catch, and reporting it as ok let it pass as a slice that
			// works. It is told apart from a container that never started, which says nothing about
			// slicing, by the probe script having reached its first line.
			name:       "a container that died under the loaded runtime is a failure of this accelerator",
			injection:  injection(map[string]string{"HGGC_DEVICE_MEMORY_LIMIT_0": "32768"}),
			probe:      sliceProbe{MemoryQuotaEnvPrefix: "HGGC_DEVICE_MEMORY_LIMIT_"},
			output:     mapsBegin + "\n7f00-7f01 r-xp 00000000 00:2f 12  " + shim + "\n" + mapsEnd + "\n",
			exitError:  "exit status 139: Segmentation fault",
			wantLoaded: device.PreflightStateUnavailable,
			// The container ran, so both rows are measured, and both are failures: the cap row
			// cannot report a slice holding on a process that did not.
			wantQuota:      device.PreflightDepthMeasured,
			wantQuotaState: device.PreflightStateUnavailable,
		},
		{
			// The case the hardware produced: the reader printed its cap and the process then died
			// under the injection. Every clause the cap row asks was satisfied, so without the exit
			// status it read ok/measured -- while the row beside it, the same container and the same
			// run, read unavailable. One container cannot be both.
			name:      "a cap reported by a container that then died is not a cap observed in force",
			injection: injection(map[string]string{"HGGC_DEVICE_MEMORY_LIMIT_0": "32768"}),
			probe:     sliceProbe{MemoryQuotaEnvPrefix: "HGGC_DEVICE_MEMORY_LIMIT_"},
			output: mapsBegin + "\n" +
				"7f00-7f01 r-xp 00000000 00:2f 12  " + shim + "\n" +
				mapsEnd + "\n" +
				"card=0 mem_quota_mib=32768 mem_used_mib=0\n",
			exitError:      "exit status 7",
			wantLoaded:     device.PreflightStateUnavailable,
			wantQuota:      device.PreflightDepthMeasured,
			wantQuotaState: device.PreflightStateUnavailable,
		},
		{
			// And the same figure in the reader's own section is the observation being looked for.
			name:      "the reader reporting the cap is what earns measured",
			injection: injection(map[string]string{"HGGC_DEVICE_MEMORY_LIMIT_0": "32768"}),
			probe:     sliceProbe{MemoryQuotaEnvPrefix: "HGGC_DEVICE_MEMORY_LIMIT_"},
			output: mapsBegin + "\n" +
				"7f00-7f01 r-xp 00000000 00:2f 12  " + shim + "\n" +
				mapsEnd + "\n" +
				"card=0 mem_quota_mib=32768 mem_used_mib=0\n",
			wantLoaded:  device.PreflightStateOK,
			wantQuota:   device.PreflightDepthMeasured,
			wantQuotaIn: "32768",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			run := emitResult{Command: "docker run ...", Output: []byte(tc.output), ExitError: tc.exitError}

			got := judgeProbeOutput("npu-0", "", tc.injection, tc.probe, run)

			require.Len(t, got, 2)
			loaded, quota := got[0], got[1]

			assert.Equal(t, capSlicedRuntimeLoaded, loaded.Capability)
			assert.Equal(t, tc.wantLoaded, loaded.State)
			assert.Equal(t, device.PreflightDepthMeasured, loaded.Depth,
				"the container ran, so the load clause is always measured")

			assert.Equal(t, capSlicedQuotaInForce, quota.Capability)
			assert.Equal(t, tc.wantQuota, quota.Depth)
			wantQuotaState := tc.wantQuotaState
			if wantQuotaState == "" {
				wantQuotaState = device.PreflightStateOK
			}
			assert.Equal(t, wantQuotaState, quota.State)
			if tc.wantQuotaIn != "" {
				// Detail carries an ok row's words, Reason a failing one's -- the assertion follows
				// the state rather than pinning the field, which is the contract the loop below checks.
				said := quota.Detail
				if quota.State != device.PreflightStateOK {
					said = quota.Reason
				}
				assert.Contains(t, said, tc.wantQuotaIn)
			}

			for _, c := range got {
				assert.Equal(t, tc.output, c.Evidence, "the observation travels with the verdict")
				assert.Equal(t, "docker run ...", c.Command)
				if c.State == device.PreflightStateOK {
					assert.Empty(t, c.Reason, "a reason is empty exactly when the state is ok")
				} else {
					assert.NotEmpty(t, c.Reason)
				}
			}
		})
	}
}

// The cap is carried in one of two places and one manufacturer uses each, so reading only the
// environment would report the manufacturer that renders a file as having no cap to look for.
func TestMemoryQuota(t *testing.T) {
	hostRoot := t.TempDir()
	configHost := "/var/lib/gpustack/operator/pods/p/c/npu_info.config"
	require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, filepath.Dir(configHost)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hostRoot, configHost),
		[]byte("npu-id=0\nvirtual-npu-id=0\naicore-quota=10\nmemory-quota=32768\n"), 0o644))

	withConfig := &deviceplugin.ContainerAllocateResponse{
		Mounts: []*deviceplugin.Mount{
			{ContainerPath: "/etc/enpu/vcann-rt/npu_info.config", HostPath: configHost},
		},
	}

	testCases := []struct {
		name      string
		injection *deviceplugin.ContainerAllocateResponse
		probe     sliceProbe
		want      string
		// wantAbsent is a fragment of the reason an empty answer carries. Which of the four ways a
		// cap can go missing it was is what the row reports, so the answer cannot flatten them.
		wantAbsent string
	}{
		{
			name: "an environment carrier keeps the figure and drops the unit suffix",
			injection: &deviceplugin.ContainerAllocateResponse{
				Envs: map[string]string{"CUDA_DEVICE_MEMORY_LIMIT_0": "12288m"},
			},
			probe: sliceProbe{MemoryQuotaEnvPrefix: "CUDA_DEVICE_MEMORY_LIMIT_"},
			want:  "12288",
		},
		{
			name:      "a config carrier reads the figure out of the file the injection mounts",
			injection: withConfig,
			probe: sliceProbe{
				MemoryQuotaConfigMount: "/etc/enpu/vcann-rt/npu_info.config",
				MemoryQuotaConfigKey:   "memory-quota",
			},
			want: "32768",
		},
		{
			name:      "a config carrier whose key is absent answers nothing rather than a wrong figure",
			injection: withConfig,
			probe: sliceProbe{
				MemoryQuotaConfigMount: "/etc/enpu/vcann-rt/npu_info.config",
				MemoryQuotaConfigKey:   "vram-quota",
			},
			wantAbsent: "names no vram-quota",
		},
		{
			name:       "a probe naming neither carrier has nothing to read",
			injection:  withConfig,
			probe:      sliceProbe{},
			wantAbsent: "knows no carrier",
		},
		{
			name:       "an environment carrier the injection never set says which variable is missing",
			injection:  &deviceplugin.ContainerAllocateResponse{},
			probe:      sliceProbe{MemoryQuotaEnvPrefix: "CUDA_DEVICE_MEMORY_LIMIT_"},
			wantAbsent: "carries no CUDA_DEVICE_MEMORY_LIMIT_* variable",
		},
		{
			name:       "a config carrier the injection never mounted says so",
			injection:  &deviceplugin.ContainerAllocateResponse{},
			probe:      sliceProbe{MemoryQuotaConfigMount: "/etc/enpu/vcann-rt/npu_info.config"},
			wantAbsent: "mounts no /etc/enpu/vcann-rt/npu_info.config",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			limit, absent := memoryQuota(tc.injection, tc.probe, hostRoot)

			assert.Equal(t, tc.want, limit)
			if tc.want != "" {
				assert.Empty(t, absent, "a cap that was read explains nothing")
				return
			}
			assert.Contains(t, absent, tc.wantAbsent,
				"an empty cap has to say which of the ways it went missing this was")
		})
	}
}

// Only a redirected root is swapped, and a prefix that merely looks similar is not one -- rewriting
// a path that was never redirected would point a mount at nothing.
func TestRehostPath(t *testing.T) {
	const (
		scratchLib  = "/tmp/gpustack-preflight-1/lib"
		scratchPods = "/tmp/gpustack-preflight-1/pods"
		scratchLock = "/tmp/gpustack-preflight-1/vgpulock"
		realLib     = "/var/lib/gpustack/operator/lib"
		realPods    = "/var/lib/gpustack/operator/pods"
		realLock    = "/tmp/vgpulock"
	)
	swaps := [][2]string{{scratchLib, realLib}, {scratchPods, realPods}, {scratchLock, realLock}}

	testCases := []struct{ name, in, want string }{
		{
			name: "a path under the redirected lib root moves onto the real one",
			in:   scratchLib + "/ascend/ld.so.preload", want: realLib + "/ascend/ld.so.preload",
		},
		{
			name: "a path under the redirected pods root moves onto the real one",
			in:   scratchPods + "/p/c/npu_info.config", want: realPods + "/p/c/npu_info.config",
		},
		{
			// A private root sits beside the shared pair rather than under either, so a rewrite that
			// only knew the pair would leave it naming a scratch directory no node has.
			name: "a manufacturer's private root moves onto the path a real allocation uses",
			in:   scratchLock, want: realLock,
		},
		{
			name: "a path under neither is untouched",
			in:   "/usr/local/Ascend/driver", want: "/usr/local/Ascend/driver",
		},
		{
			name: "a sibling that merely shares a prefix is untouched",
			in:   scratchLib + "-backup/x", want: scratchLib + "-backup/x",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, rehostPath(tc.in, swaps...))
		})
	}
}

// What runs inside the container has to survive a reader that exits non-zero, because every monitor
// this names does exactly that in a container that has allocated nothing -- which is the container
// a preflight starts.
func TestProbeShellCommand(t *testing.T) {
	t.Run("the reader's failure becomes evidence rather than a failed step", func(t *testing.T) {
		argv := probeShellCommand("/usr/local/vrocm/rocm-monitor", nil)

		require.Equal(t, []string{"sh", "-c"}, argv[:2])
		assert.Contains(t, argv[2], "cat /proc/self/maps", "the load evidence is read first")
		assert.Contains(t, argv[2], "|| echo probe-reader-exit-$?")
		assert.True(t, strings.HasSuffix(argv[2], "2>&1"),
			"the shim says which cap it read on standard error and nowhere else")

		// Merging standard error is what makes the markers necessary: it is how the shim's cap
		// line and the loader's refusal reach this output at all, and each clause has to read its
		// own section rather than the stream they share.
		assert.Less(t, strings.Index(argv[2], mapsBegin), strings.Index(argv[2], "cat /proc/self/maps"),
			"the mapping section opens before the mappings")
		assert.Less(t, strings.Index(argv[2], "cat /proc/self/maps"), strings.Index(argv[2], mapsEnd),
			"and closes after them")
		assert.Less(t, strings.Index(argv[2], mapsEnd), strings.Index(argv[2], "rocm-monitor"),
			"so the reader answers outside it")
	})

	// The reader runs under the shim -- that is the whole question -- so a noisy shim prints the cap
	// it was configured with inside the reader's own section, and the quota clause finds the figure it
	// looks for whatever the vendor tool goes on to say. Observed on an AMD host: the figure in the
	// reader's section was the shim's banner, and the tool's body named no cap at all. The section
	// split cannot separate them, because one process printed both.
	t.Run("the shim is silenced before the reader answers", func(t *testing.T) {
		argv := probeShellCommand("/usr/local/vrocm/rocm-monitor",
			[]string{"LIBVROCM_LOG_LEVEL", "VROCM_DEBUG"})

		assert.Contains(t, argv[2], "unset LIBVROCM_LOG_LEVEL VROCM_DEBUG")
		assert.Less(t, strings.Index(argv[2], "unset "), strings.Index(argv[2], "rocm-monitor"),
			"the variables go before the reader, or the reader still runs under a talking shim")
		assert.Less(t, strings.Index(argv[2], mapsEnd), strings.Index(argv[2], "unset "),
			"and after the mappings, which are read while the shim is still loaded")
	})

	t.Run("a manufacturer with no reader still gets the load evidence", func(t *testing.T) {
		argv := probeShellCommand("", nil)

		assert.Contains(t, argv[2], "cat /proc/self/maps")
		assert.NotContains(t, argv[2], "probe-reader-exit")
	})
}

// Every step short of the container is an answer rather than a failure, and each says which one it
// was. The pair is always two rows so that a run reports one shape whatever depth it reached.
func TestMeasureSliced_Fallbacks(t *testing.T) {
	groups := ascendGroups()
	injection := func(_, libDir, _ string) *deviceplugin.ContainerAllocateResponse {
		return &deviceplugin.ContainerAllocateResponse{
			Mounts: []*deviceplugin.Mount{{
				ContainerPath: "/opt/enpu/vcann-rt/lib/libvruntime.so",
				HostPath:      filepath.Join(libDir, "ascend/lib/libvruntime.so"),
			}},
		}
	}

	t.Run("a manufacturer with no container probe stops at simulated", func(t *testing.T) {
		stagedImageLib(t, nodefeature.ManufacturerMetaX)
		root := fakeHostRoot(t)
		host, asked := answeringHost(root, "", nil)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		metax := ascendGroups()
		metax[0].Manufacturer = nodefeature.ManufacturerMetaX

		checks := p.measureSliced(context.Background(), nodefeature.ManufacturerMetaX,
			&fakeInjector{build: injection}, p.stageLibFor(nodefeature.ManufacturerMetaX), metax)

		require.Len(t, checks, 2)
		assert.Empty(t, *asked, "nothing is started for a manufacturer with no probe")
		// Staging is what a container step needs, and there is no container step here, so writing
		// the tree would leave a manufacturer's libraries on a host that will never mount them.
		_, err := os.Stat(filepath.Join(root, deviceplugin.OperatorLibDir, nodefeature.ManufacturerMetaX))
		assert.True(t, os.IsNotExist(err), "a manufacturer with no probe had its tree staged anyway")
		for _, c := range checks {
			assert.Equal(t, device.PreflightStateOK, c.State)
			assert.Equal(t, device.PreflightDepthSimulated, c.Depth)
			assert.Contains(t, c.Detail, "no container probe has been established")
			assert.Empty(t, c.Reason)
		}
	})

	t.Run("libraries that could not be staged stop at simulated", func(t *testing.T) {
		stagedImageLib(t, "some-other-manufacturer")
		host, asked := answeringHost(fakeHostRoot(t), "", nil)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		checks := p.measureSliced(context.Background(), nodefeature.ManufacturerAscend,
			&fakeInjector{build: injection}, p.stageLibFor(nodefeature.ManufacturerAscend), groups)

		require.Len(t, checks, 2)
		assert.Empty(t, *asked)
		assert.Equal(t, device.PreflightDepthSimulated, checks[0].Depth)
		assert.Contains(t, checks[0].Detail, "could not be staged onto the host")
	})

	t.Run("a family with no default probe image names the flag", func(t *testing.T) {
		stagedImageLib(t, nodefeature.ManufacturerAscend)
		host, asked := answeringHost(fakeHostRoot(t), "", nil)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		unknown := ascendGroups()
		unknown[0].Family = "910A"

		checks := p.measureSliced(context.Background(), nodefeature.ManufacturerAscend,
			&fakeInjector{build: injection}, p.stageLibFor(nodefeature.ManufacturerAscend), unknown)

		require.Len(t, checks, 2)
		assert.Empty(t, *asked)
		assert.Equal(t, device.PreflightDepthSimulated, checks[0].Depth)
		assert.Contains(t, checks[0].Detail, "--probe-image")
	})

	// --emit exists to show what would run before anything does, so it takes no step at all --
	// including the one that writes the library tree onto the host. What it prints is therefore
	// complete, and the row says the one thing its reader still has to do.
	t.Run("an emitted container step carries the command it did not take and writes nothing", func(t *testing.T) {
		stagedImageLib(t, nodefeature.ManufacturerAscend)
		root := fakeHostRoot(t)
		host, asked := answeringHost(root, "", nil)
		p := &Preflighter{host: host, dryRun: true, runtime: &hostRuntime{Name: "docker"}}

		checks := p.measureSliced(context.Background(), nodefeature.ManufacturerAscend,
			&fakeInjector{build: injection}, p.stageLibFor(nodefeature.ManufacturerAscend), groups)

		require.Len(t, checks, 2)
		assert.Empty(t, *asked, "emit takes no step")
		_, err := os.Stat(filepath.Join(root, deviceplugin.OperatorLibDir, nodefeature.ManufacturerAscend))
		assert.True(t, os.IsNotExist(err), "--emit wrote the library tree onto the host")

		for _, c := range checks {
			assert.Equal(t, device.PreflightStateOK, c.State)
			assert.Equal(t, device.PreflightDepthSimulated, c.Depth)
			assert.Contains(t, c.Command, "quay.io/ascend/cann:8.5.0-910b-ubuntu22.04-py3.11")
			assert.Contains(t, c.Command, "--runtime ascend")
			assert.Contains(t, c.Command, "--label "+preflightLabel)
			assert.Contains(t, c.Detail, "a dry run deliberately writes neither")
			// Both mount sources a dry run withholds are named, not only the library tree: the
			// command also mounts whatever the responder renders, from the host path it would have
			// been promoted to.
			assert.Contains(t, c.Detail, deviceplugin.OperatorLibDir,
				"the library tree the command mounts")
			assert.Contains(t, c.Detail, string(deviceplugin.PreflightPodUID),
				"and the rendered artifact under the pods directory")
		}
	})

	// A fallback is not --emit: those runs did stage, so telling their reader to stage again would
	// send them after a file that is already there.
	t.Run("a fallback emit stages and says nothing about staging", func(t *testing.T) {
		stagedImageLib(t, nodefeature.ManufacturerAscend)
		root := fakeHostRoot(t)
		host, asked := answeringHost(root, "", nil)
		p := &Preflighter{host: host} // no runtime resolved, so the step falls back to emit

		checks := p.measureSliced(context.Background(), nodefeature.ManufacturerAscend,
			&fakeInjector{build: injection}, p.stageLibFor(nodefeature.ManufacturerAscend), groups)

		require.Len(t, checks, 2)
		assert.Empty(t, *asked)
		assert.DirExists(t, filepath.Join(root, deviceplugin.OperatorLibDir, nodefeature.ManufacturerAscend))
		assert.Contains(t, checks[0].Detail, "no container runtime")
		assert.NotContains(t, checks[0].Detail, "a dry run deliberately writes neither")
	})

	t.Run("a responder that refuses is unavailable at the shallowest depth", func(t *testing.T) {
		stagedImageLib(t, nodefeature.ManufacturerAscend)
		host, _ := answeringHost(fakeHostRoot(t), "", nil)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		checks := p.measureSliced(context.Background(), nodefeature.ManufacturerAscend,
			&fakeInjector{responderErr: errors.New("no sliced server")}, p.stageLibFor(nodefeature.ManufacturerAscend), groups)

		require.Len(t, checks, 2)
		for _, c := range checks {
			assert.Equal(t, device.PreflightStateUnavailable, c.State)
			assert.Equal(t, device.PreflightDepthDeclared, c.Depth)
			assert.Contains(t, c.Reason, "no sliced server")
		}
	})

	// Staging failed, so the tree the command mounts is not on the host -- which is the same state
	// a dry run leaves, and gets the same treatment. Returning before the command was built left the
	// row saying a preparation had failed and nothing at all about what it was for, so an operator
	// could not stage the tree by hand and take the step themselves.
	t.Run("a step whose libraries could not be staged still carries its command", func(t *testing.T) {
		// No in-image tree for this manufacturer, so StageLib fails by its own contract.
		stagedImageLib(t, nodefeature.ManufacturerNVIDIA)
		host, asked := answeringHost(fakeHostRoot(t), "", nil)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		staged := p.stageLibFor(nodefeature.ManufacturerAscend)
		require.True(t, staged.Failed, "this case needs staging to have failed")

		checks := p.measureSliced(context.Background(), nodefeature.ManufacturerAscend,
			&fakeInjector{build: injection}, staged, groups)

		require.Len(t, checks, 2)
		assert.Empty(t, *asked, "nothing is run when what it would mount is not there")
		for _, c := range checks {
			assert.Equal(t, device.PreflightStateOK, c.State, "a preparation this pass could not make is not a node that cannot slice")
			assert.Equal(t, device.PreflightDepthSimulated, c.Depth)
			assert.Contains(t, c.Detail, "could not be staged onto the host",
				"the row says what could not be prepared")
			assert.Contains(t, c.Command, "quay.io/ascend/cann:8.5.0-910b-ubuntu22.04-py3.11",
				"and carries the command that would have run, so it can be taken by hand")
		}
	})

	t.Run("an accelerator that hosts no logical slice is not probed", func(t *testing.T) {
		stagedImageLib(t, nodefeature.ManufacturerAscend)
		host, asked := answeringHost(fakeHostRoot(t), "", nil)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		unsliceable := ascendGroups()
		unsliceable[0].Accelerators[0].Status.LogicalSliced.Count = 0

		checks := p.measureSliced(context.Background(), nodefeature.ManufacturerAscend,
			&fakeInjector{build: injection}, p.stageLibFor(nodefeature.ManufacturerAscend), unsliceable)

		assert.Empty(t, checks)
		assert.Empty(t, *asked)
	})
}

// A preflighter that carries no injection seam at all contributes no slice rows, rather than rows
// saying it failed: three manufacturers read no driver when they serve an allocation, and this is
// how a fourth kind -- one with no seam -- would be reported.
func TestMeasureSliced_NoInjectionSeam(t *testing.T) {
	host, asked := answeringHost(fakeHostRoot(t), "", nil)
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	checks := p.measureSliced(context.Background(), nodefeature.ManufacturerAscend,
		noSeamPreflighter{}, p.stageLibFor(nodefeature.ManufacturerAscend), ascendGroups())

	assert.Nil(t, checks)
	assert.Empty(t, *asked)
}

type noSeamPreflighter struct{}

func (noSeamPreflighter) PreflightAccelerator(device.DevicesGroupList) device.PreflightGroup {
	return device.PreflightGroup{}
}

// A container left behind by a killed run still holds the accelerator the next run wants to measure,
// so it is removed before anything is started. Neither CLI's rm takes a filter, which is why this
// lists and then removes.
func TestSweepStaleContainers(t *testing.T) {
	const label = "--filter label=" + preflightLabel

	// installed answers "yes" to the presence probe for each named CLI, and nothing else, so a case
	// states which runtimes the host carries and answers their sweep separately.
	installed := func(names ...string) map[string]string {
		answers := map[string]string{}
		for _, n := range names {
			answers["sh -c command -v "+n] = "/usr/bin/" + n
		}
		return answers
	}

	nerdctlAt := "nerdctl --address /run/k3s/containerd/containerd.sock --namespace " + containerdNamespace

	t.Run("it lists by label and removes what it found", func(t *testing.T) {
		answers := installed("nerdctl")
		answers[nerdctlAt+" ps -aq "+label] = "abc123\ndef456"
		answers[nerdctlAt+" rm -f abc123 def456"] = ""
		host, asked := scriptedHost(fakeHostRoot(t), answers)
		p := &Preflighter{host: host, runtime: &hostRuntime{
			Name: "nerdctl", Socket: "/run/k3s/containerd/containerd.sock", Namespace: containerdNamespace,
		}}

		p.sweepStaleContainers(context.Background())

		// The resolved runtime is swept with the kubelet's own socket and namespace, which is where
		// its containers were started. The ids are split here rather than by a shell, so the removal
		// is one argv naming exactly what the list returned.
		assert.Contains(t, *asked, nerdctlAt+" ps -aq "+label)
		assert.Contains(t, *asked, nerdctlAt+" rm -f abc123 def456")
		assert.Empty(t, p.sweepFailures)
	})

	// A pass killed under --runtime=docker leaves a docker container holding the accelerator, and the
	// next pass defaulting to the kubelet's nerdctl would measure its own slice against that leftover
	// while reporting the accelerator as the thing at fault.
	t.Run("a runtime this pass did not resolve is swept too", func(t *testing.T) {
		answers := installed("nerdctl", "docker")
		answers[nerdctlAt+" ps -aq "+label] = ""
		answers["docker ps -aq "+label] = ""
		host, asked := scriptedHost(fakeHostRoot(t), answers)
		p := &Preflighter{host: host, runtime: &hostRuntime{
			Name: "nerdctl", Socket: "/run/k3s/containerd/containerd.sock", Namespace: containerdNamespace,
		}}

		p.sweepStaleContainers(context.Background())

		// docker has neither a socket nor a namespace to be told about: it reads the host's own
		// configuration, which is why it is invoked as the host in the first place.
		assert.Contains(t, *asked, "docker ps -aq "+label,
			"a leftover started under another runtime would have been left holding the accelerator")
		assert.Empty(t, p.sweepFailures)
	})

	// The other direction, and the one where "swept with its defaults" would have swept nothing at
	// all: every probe this command starts is created in a namespace of its own choosing, so a
	// containerd CLI asked without it looks in "default" and reports success having seen none of them.
	t.Run("a containerd runtime this pass did not resolve is addressed, not left to its defaults", func(t *testing.T) {
		answers := installed("docker", "nerdctl")
		answers["docker ps -aq "+label] = ""
		answers[nerdctlAt+" ps -aq "+label] = ""
		host, asked := scriptedHost(fakeHostRoot(t, "/run/k3s/containerd/containerd.sock"), answers)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		p.sweepStaleContainers(context.Background())

		assert.Contains(t, *asked, nerdctlAt+" ps -aq "+label,
			"a nerdctl sweep in the default namespace cannot see a container this command created")
		assert.Empty(t, p.sweepFailures)
	})

	// The sweep is what makes a measured answer mean anything, so one that could not be completed is
	// not a log line. A leftover it failed to remove still holds the card, and the slice measured
	// against it reports a healthy accelerator as unavailable while naming the accelerator as the
	// fault -- exactly the failure the sweep exists to prevent, arrived at through the sweep's own
	// silence.
	t.Run("the resolved runtime failing to be asked fails the accelerators it could not clear", func(t *testing.T) {
		// docker answers the presence probe and nothing else, so its list fails. It is the runtime
		// this pass resolved, and the one the probes are about to run through.
		host, _ := scriptedHost(fakeHostRoot(t), installed("docker"))
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		p.sweepStaleContainers(context.Background())

		require.Len(t, p.sweepFailures, 1, "the runtime whose sweep failed has to be named")
		assert.Contains(t, p.sweepFailures[0], "docker")

		groups := device.DevicesGroupList{{
			ID: "grp-0", Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: []workercore.Accelerator{{ID: "GPU-0"}, {ID: "GPU-1"}},
		}}
		checks := p.staleSweepChecks(groups)

		require.Len(t, checks, 2, "one row per accelerator this pass went on to measure")
		for _, c := range checks {
			assert.Equal(t, capStaleSweep, c.Capability)
			assert.Equal(t, device.PreflightStateUnavailable, c.State,
				"a swept-nothing run that measured anyway and exited zero is what this rules out")
			assert.Contains(t, c.Reason, "may still be holding this accelerator")
		}
		assert.NotEmpty(t, Failed([]device.PreflightGroup{{
			Manufacturer: nodefeature.ManufacturerNVIDIA, Checks: checks,
		}}), "the failure has to reach the exit code, not only the document")
	})

	// A leftover that was seen and would not go away is a failure wherever it is: the accelerator it
	// holds is the same physical card either way, and unlike the case below there is no absence to
	// establish -- the list already said the container is there.
	t.Run("a removal that fails is a failure even on a runtime this pass did not resolve", func(t *testing.T) {
		answers := installed("nerdctl", "docker")
		answers[nerdctlAt+" ps -aq "+label] = ""
		answers["docker ps -aq "+label] = "stuck1"
		// No answer for the removal, so it fails.
		host, _ := scriptedHost(fakeHostRoot(t), answers)
		p := &Preflighter{host: host, runtime: &hostRuntime{
			Name: "nerdctl", Socket: "/run/k3s/containerd/containerd.sock", Namespace: containerdNamespace,
		}}

		p.sweepStaleContainers(context.Background())

		require.Len(t, p.sweepFailures, 1)
		assert.Contains(t, p.sweepFailures[0], "docker")
		assert.Contains(t, p.sweepFailures[0], "could not remove 1 stale container(s)")
	})

	// The other half of the same rule, and the reason it is a rule rather than "fail on anything":
	// absence is established rather than assumed. A runtime this command cannot ask, addressed the way
	// it would itself have addressed it, is one no earlier pass started a probe through.
	//
	// Measured on an Ascend host, which carries nerdctl and a containerd socket at the RKE2 path whose
	// daemon refuses the connection. Failing on it would hang a permanent red row on all eight of its
	// healthy accelerators, on every run.
	t.Run("a runtime this pass did not resolve and cannot reach is skipped, not recorded", func(t *testing.T) {
		answers := installed("docker", "nerdctl")
		answers["docker ps -aq "+label] = ""
		// nerdctl answers the presence probe and refuses everything else, as a CLI whose daemon is not
		// listening does.
		host, _ := scriptedHost(fakeHostRoot(t, "/run/k3s/containerd/containerd.sock"), answers)
		p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

		p.sweepStaleContainers(context.Background())

		assert.Empty(t, p.sweepFailures,
			"a permanent red row on a sound node is a red light nobody reads")
		assert.Empty(t, p.staleSweepChecks(device.DevicesGroupList{{
			Accelerators: []workercore.Accelerator{{ID: "GPU-0"}},
		}}))
	})

	t.Run("nothing is swept when no step will be taken", func(t *testing.T) {
		host, asked := answeringHost(fakeHostRoot(t), "", nil)

		for _, p := range []*Preflighter{
			{host: host}, // no runtime resolved
			{host: host, dryRun: true, runtime: &hostRuntime{Name: "docker"}}, // nothing will start
		} {
			p.sweepStaleContainers(context.Background())
		}
		assert.Empty(t, *asked)
	})
}

// A slicing runtime says which cap it read only when its own log level is raised, and each of the
// four names that knob differently. These values were measured on the manufacturer's own hardware by
// the verification cases under .claude/skills/gpustack-operator-xbuild-and-verify/cases/, and this
// table is the second reader of them: a level that drifted off its case leaves a probe that starts a
// container which enforces a cap and never says so, reported as a slice that could not be observed.
func TestSliceProbeLogLevelsMatchTheVerificationCases(t *testing.T) {
	const casesDir = "../../../.claude/skills/gpustack-operator-xbuild-and-verify/cases"

	entries, err := os.ReadDir(casesDir)
	require.NoError(t, err)

	var body strings.Builder
	for _, e := range entries {
		content, readErr := os.ReadFile(filepath.Join(casesDir, e.Name()))
		require.NoError(t, readErr)
		body.Write(content)
	}
	cases := body.String()
	require.Contains(t, cases, "docker run", "the cases changed shape; this test now checks nothing")

	for manufacturer, probe := range sliceProbes {
		assert.NotEmpty(t, probe.LogEnv, "%s names no log level, so its cap can never be observed",
			manufacturer)
		for name, value := range probe.LogEnv {
			assert.Contains(t, cases, name+"="+value,
				"%s's log level is not the one its verification case runs with", manufacturer)
		}
	}
}

// A reader is only reachable inside the container if the manufacturer's own allocator mounted it
// there, and the container path it mounts it at is a constant in that allocator. Naming a path the
// injection does not carry produces a probe whose reader is simply absent -- which reads exactly
// like a monitor that found nothing, and would be reported as a slice that could not be observed
// rather than as this table being wrong.
//
// The allocator sources are read rather than imported, so this holds off linux too, where the
// registry those packages are reached through is empty.
func TestSliceProbeReadersAreMountedByTheAllocator(t *testing.T) {
	for manufacturer, probe := range sliceProbes {
		reader := strings.Fields(probe.Reader)[0]
		if !strings.HasPrefix(reader, "/") {
			// Not something this repository mounts: nvidia-smi comes with the vendor runtime the
			// probe container is handed to, which is what sliceProbe.Runtime is for.
			continue
		}

		source, err := os.ReadFile(
			filepath.Join("../allocator", manufacturer, "deviceplugin.go"))
		require.NoError(t, err)
		assert.Contains(t, string(source), `"`+reader+`"`,
			"%s's allocator mounts no reader at %s", manufacturer, reader)
	}
}

// fakeSlicedInjector is a manufacturer whose slicing lives where AMD's does: its universal entry
// point answers every mode with the same whole-accelerator injection, and only
// GetLogicalSlicedResponse renders a slice.
//
// It exists because every other fake in this package implements ContainerAllocateResponder alone,
// and a suite made only of those cannot tell a runner that drives the right method from one that
// drives the universal entry point everywhere -- which is exactly the defect hardware caught.
type fakeSlicedInjector struct {
	// occupied records, in call order, the occupancy each placement call was given. It is what a
	// co-tenancy test asserts the second tenant was placed around the first with.
	occupied []deviceplugin.Placements
}

func (fakeSlicedInjector) PreflightAccelerator(device.DevicesGroupList) device.PreflightGroup {
	return device.PreflightGroup{}
}

func (f *fakeSlicedInjector) PreflightResponder(
	workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	_, restore, err := deviceplugin.NewPreflightRedirect()
	if err != nil {
		return nil, nil, err
	}
	return f, restore, nil
}

// GetContainerAllocateResponse answers with the whole accelerator, naming no cap and mounting no
// preload library -- exactly what AMD's does for every mode.
func (f *fakeSlicedInjector) GetContainerAllocateResponse(
	context.Context, *core.Pod, *core.Container, *workercore.Devices, map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	return &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{"VENDOR_VISIBLE_DEVICES": "none"},
	}, nil
}

func (f *fakeSlicedInjector) PlaceLogicalSliced(
	_ context.Context, _ *core.Pod, ctr *core.Container, _ *workercore.Devices,
	_ map[deviceplugin.Resource]int32, occupied deviceplugin.Placements,
) (deviceplugin.Placements, error) {
	f.occupied = append(f.occupied, occupied)
	return deviceplugin.Placements{
		{Group: "grp-0", Device: "npu-0"}: {{Start: int32(len(f.occupied) - 1), Length: 1}},
	}, nil
}

func (f *fakeSlicedInjector) GetLogicalSlicedResponse(
	_ context.Context, _ *core.Pod, ctr *core.Container, _ *workercore.Devices,
	_ map[deviceplugin.Resource]int32, placements deviceplugin.Placements,
) (*deviceplugin.ContainerAllocateResponse, error) {
	var run int32
	for _, runs := range placements {
		if len(runs) != 0 {
			run = runs[0].Start
		}
	}
	return &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"VROCM_DEVICE_MEMORY_LIMIT_0": "8184",
			"SLICE_RUN":                   strconv.Itoa(int(run)),
			"SLICE_CONTAINER":             ctr.Name,
		},
		Mounts: []*deviceplugin.Mount{{
			ContainerPath: "/usr/local/vrocm/libvrocm.so",
			HostPath:      filepath.Join(deviceplugin.OperatorLibDir, "amd/libvrocm.so"),
		}},
	}, nil
}

// A manufacturer that renders its slice in GetLogicalSlicedResponse must be driven there. Its
// universal entry point answers every mode alike, so driving that one emits a probe carrying no cap
// and no preload library -- a container that would report a slice nobody asked for.
func TestMeasureSliced_DrivesTheSlicedResponseWhereTheSliceLives(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAMD)
	host, asked := answeringHost(fakeHostRoot(t), "", nil)
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	amd := ascendGroups()
	amd[0].Manufacturer = nodefeature.ManufacturerAMD

	checks := p.measureSliced(context.Background(), nodefeature.ManufacturerAMD,
		&fakeSlicedInjector{}, p.stageLibFor(nodefeature.ManufacturerAMD), amd)

	require.Len(t, checks, 2)
	require.Len(t, *asked, 1)
	command := (*asked)[0]

	assert.Contains(t, command, "VROCM_DEVICE_MEMORY_LIMIT_0=8184",
		"the cap the sliced response set is what the probe carries")
	assert.Contains(t, command, "libvrocm.so",
		"the preload library the sliced response mounts is what the probe loads")
	assert.NotContains(t, command, "VENDOR_VISIBLE_DEVICES",
		"the universal entry point's whole-accelerator injection must not be what was measured")
}

// Two accelerators must not render into one path. The request builder fixes the Pod's identity so
// two runs agree, and a responder derives its per-container host paths from it -- so with a fixed
// container name every card of a manufacturer writes over the last one's artifacts, and the emitted
// commands all mount a file describing whichever card went last. Measured on an 8-NPU host: two
// files on disk for eight cards, both holding card 7.
func TestMeasureSliced_GivesEachAcceleratorItsOwnContainerName(t *testing.T) {
	stagedImageLib(t, nodefeature.ManufacturerAscend)
	root := fakeHostRoot(t)
	host, asked := answeringHost(root, "", nil)
	p := &Preflighter{host: host, runtime: &hostRuntime{Name: "docker"}}

	two := ascendGroups()
	two[0].Accelerators = append(two[0].Accelerators, workercore.Accelerator{
		ID:     "npu-1",
		Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 8}},
	})

	injector := &fakeInjector{
		build: func(container, _, podsDir string) *deviceplugin.ContainerAllocateResponse {
			return &deviceplugin.ContainerAllocateResponse{
				Mounts: []*deviceplugin.Mount{{
					ContainerPath: "/etc/enpu/vcann-rt/npu_info.config",
					HostPath:      filepath.Join(podsDir, "preflight", container, "npu_info.config"),
				}},
			}
		},
	}

	p.measureSliced(context.Background(), nodefeature.ManufacturerAscend, injector,
		p.stageLibFor(nodefeature.ManufacturerAscend), two)

	require.Len(t, *asked, 2, "one container per sliceable accelerator")
	assert.NotEqual(t, (*asked)[0], (*asked)[1],
		"two accelerators produced the same command, so they render over one another")
	for _, accelerator := range []string{"npu-0", "npu-1"} {
		assert.Contains(t, strings.Join(*asked, "\n"),
			filepath.Join(deviceplugin.OperatorPreflightDir, "preflight",
				probeContainerName(deviceplugin.PreflightContainerName, accelerator), "npu_info.config"),
			"%s's command does not mount a config of its own", accelerator)
	}
}
