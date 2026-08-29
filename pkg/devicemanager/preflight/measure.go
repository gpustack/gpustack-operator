package preflight

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// The two things a container established about a slice, named for what the answer is about rather
// than for a driver flag: unlike a declared row, neither is a capability the manufacturer's
// vocabulary has a word for, because no driver is asked -- a container is started and looked into.
//
// They are separate rows because they fail separately and mean different things. A shim that did
// not load leaves a container with the whole card and no cap at all; a shim that loaded but whose
// cap could not be confirmed is a slice that is probably enforced and was not observed to be. One
// row carrying both would have to pick which of those to report.
const (
	capSlicedRuntimeLoaded = "sliced-runtime-loaded"
	capSlicedQuotaInForce  = "sliced-quota-in-force"
)

// sliceQuota is what one preflight slice asks for: half an accelerator.
//
// Half rather than a token sliver, because the figure reported back inside the container is the
// evidence, and a figure has to be unmistakable to be evidence -- a cap near the card's own size
// could be read as the card, and one near zero could be read as a shim that answered with nothing.
// Half is also the largest quota every accelerator with any logical slicing at all can serve, so
// the ask never fails for being too big on a small card.
const sliceQuota = nodefeature.ResourceMaxUnits / 2

// preflightLabel is carried by every container this command starts, so one left behind by a run
// that was killed between starting a container and reaping it can be found again. It is a label of
// our own rather than a name, because a name collides and a run probes one container per
// accelerator.
const preflightLabel = "gpustack.ai/preflight=true"

// sliceProbe is what makes one manufacturer's slice observable from inside a container that was
// granted one.
//
// Every field is taken from that manufacturer's case under
// .claude/skills/gpustack-operator-xbuild-and-verify/cases/, which reproduces this injection by
// hand on the manufacturer's own hardware. Those cases are where these values were measured; this
// table is the second reader of them, and drift between the two is a drift in what the product
// claims its slicing does.
//
// A manufacturer absent from the table has no measured answer: its rows stop at the simulated
// depth saying so. That is an answer -- the allocator still produced an injection, and that was
// established -- and it is why the table carries only what has been observed rather than a guess
// per manufacturer.
type sliceProbe struct {
	// Runtime is the OCI runtime the probe container is handed to, empty where the manufacturer
	// needs none. See containerRunSpec.Runtime for why two of them do.
	Runtime string
	// LogEnv raises the slicing runtime's own log level, so that it says on stderr which cap it
	// read out of the injection. It is preflight's own addition and not part of the injection: the
	// allocator has no reason to set it, and a container that says nothing about its cap can only
	// be reported as unconfirmed.
	LogEnv map[string]string
	// Reader is the vendor tool the injection mounts, run after the load evidence as a shell
	// fragment. Its output is evidence and its exit status is not a verdict: all three of the
	// monitors this names exit non-zero in a container that has allocated nothing yet, which is
	// exactly the container a preflight starts.
	Reader string
	// MemoryQuotaEnvPrefix names the environment variable this manufacturer's injection carries
	// the per-accelerator memory cap in, without the accelerator index the allocator appends.
	//
	// It is empty for a manufacturer that carries the cap somewhere other than the environment,
	// and exactly one does: see the two fields below.
	MemoryQuotaEnvPrefix string
	// MemoryQuotaConfigMount and MemoryQuotaConfigKey are the other place a cap is carried: the
	// container path of a configuration file the injection mounts, and the "key=" whose value is
	// the figure. Ascend renders one rather than setting an environment variable, so a table that
	// only knew about environment variables would report the manufacturer this whole branch is for
	// as having no cap to look for.
	MemoryQuotaConfigMount string
	MemoryQuotaConfigKey   string
}

// sliceProbes holds the four manufacturers whose slice has been observed from inside a container.
var sliceProbes = map[string]sliceProbe{
	nodefeature.ManufacturerNVIDIA: {
		Runtime:              "nvidia",
		LogEnv:               map[string]string{"LIBCUDA_LOG_LEVEL": "3"},
		Reader:               "nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits",
		MemoryQuotaEnvPrefix: "CUDA_DEVICE_MEMORY_LIMIT_",
	},
	nodefeature.ManufacturerAscend: {
		Runtime:                "ascend",
		LogEnv:                 map[string]string{"ENPU_LOG_LEVEL": "3"},
		Reader:                 "/opt/enpu/vcann-rt/tools/enpu-monitor",
		MemoryQuotaConfigMount: "/etc/enpu/vcann-rt/npu_info.config",
		MemoryQuotaConfigKey:   "memory-quota",
	},
	nodefeature.ManufacturerAMD: {
		LogEnv:               map[string]string{"LIBVROCM_LOG_LEVEL": "2"},
		Reader:               "/usr/local/vrocm/rocm-monitor",
		MemoryQuotaEnvPrefix: "VROCM_DEVICE_MEMORY_LIMIT_",
	},
	nodefeature.ManufacturerTHead: {
		LogEnv:               map[string]string{"LIBHGGC_LOG_LEVEL": "2"},
		Reader:               "/usr/local/vppu/ppu-monitor",
		MemoryQuotaEnvPrefix: "HGGC_DEVICE_MEMORY_LIMIT_",
	},
}

// stageLibFor puts this manufacturer's preload-library tree on the host, once for the whole run.
//
// Once per manufacturer rather than once per accelerator or once per question: the tree is the same
// for every accelerator of one manufacturer, a failure to write it stops all of them the same way,
// and both container questions mount the same files. It is the caller that holds the result and
// hands it to both,
// so neither question depends on the other having run.
//
// And staged only where a container will actually be started. Writing the tree is part of taking the
// step, not part of preparing to describe it, so --dry-run -- which exists to show what would run
// before anything does -- stages nothing, and the rows say so instead. A manufacturer with no
// container probe would have nothing to start either way.
//
// Whether this manufacturer's accelerators can host a slice at all is the caller's half of that
// condition: both container questions skip an accelerator that cannot, so a caller with none must
// not ask for the tree.
func (p *Preflighter) stageLibFor(manufacturer string) StageResult {
	if _, probed := sliceProbes[manufacturer]; !probed || p.dryRun || p.host == nil {
		return StageResult{Manufacturer: manufacturer}
	}
	// A path that is not a mounted host root is still a path, and StageLib would write the tree
	// happily -- into this container's own filesystem, reporting success. The step is then emitted
	// for a host root that could not be entered, and its command mounts a directory the host does
	// not have, with nothing in the row saying so.
	if err := p.host.Validate(); err != nil {
		return StageResult{Manufacturer: manufacturer, Failed: true, Reason: err.Error()}
	}
	return StageLib(p.host.root, manufacturer)
}

// measureSliced answers, for one manufacturer, whether its accelerators can actually be sliced --
// one row pair per accelerator that can host a logical slice.
//
// The depth each pair reaches is whatever the environment permitted, and every step that stops it
// short is an answer rather than a failure: a manufacturer with no injection seam, an accelerator
// that hosts no slice, a probe image that cannot be resolved, a library tree that could not be
// staged, and a container step that had to be emitted instead of run.
//
// pf is the manufacturer's already-built preflighter rather than a second one of our own: it is the
// same value that answered the driver read, and building another would load the manufacturer's
// library a second time to ask it a question the first one can answer.
func (p *Preflighter) measureSliced(
	ctx context.Context, manufacturer string, pf device.AcceleratorPreflighter,
	staged StageResult, groups device.DevicesGroupList,
) []device.PreflightCheck {
	injector, ok := pf.(deviceplugin.AcceleratorInjectionPreflighter)
	if !ok || p.host == nil {
		return nil
	}

	var checks []device.PreflightCheck
	for i := range groups {
		grp := &groups[i]
		for j := range grp.Accelerators {
			accel := &grp.Accelerators[j]
			if accel.Status.LogicalSliced.Count == 0 {
				continue
			}
			checks = append(checks, p.measureAccelerator(ctx, manufacturer, injector, staged, grp, accel)...)
		}
	}
	return checks
}

// measureAccelerator drives one accelerator's slice as far as it goes and returns the row pair.
func (p *Preflighter) measureAccelerator(
	ctx context.Context,
	manufacturer string,
	injector deviceplugin.AcceleratorInjectionPreflighter,
	staged StageResult,
	grp *workercore.DevicesGroup,
	accel *workercore.Accelerator,
) []device.PreflightCheck {
	injection, err := p.simulateInjection(ctx, manufacturer, injector, grp, accel)
	if err != nil {
		return unreachablePair(accel.ID, device.PreflightStateUnavailable,
			device.PreflightDepthDeclared, "", err.Error())
	}

	probe, known := sliceProbes[manufacturer]
	if !known {
		return unreachablePair(accel.ID, device.PreflightStateOK, device.PreflightDepthSimulated,
			"the allocator produced the slice injection; no container probe has been established for "+
				manufacturer+", so what the slice looks like from inside it was not observed", "")
	}

	image, err := ResolveProbeImage(manufacturer, grp.Family, grp.RuntimeVersion, p.probeImage)
	if err != nil {
		return unreachablePair(accel.ID, device.PreflightStateOK, device.PreflightDepthSimulated,
			"the allocator produced the slice injection; the container step was not taken because "+
				err.Error(), "")
	}

	for k, v := range probe.LogEnv {
		injection.Envs[k] = v
	}
	run, err := emitOrRun(ctx, p.host, p.runtime, p.noRuntime,
		forceEmitReason(p.dryRun, staged), containerRunSpec{
			Image:     image,
			Injection: injection,
			Runtime:   probe.Runtime,
			Label:     preflightLabel,
			Args:      probeShellCommand(probe.Reader, logEnvNames(probe)),
		})
	switch {
	case err != nil:
		// A container that could not be started says nothing about slicing, so it is not
		// `unavailable` -- the one state that exits non-zero. Every other environment limit here is
		// already reported this way; this one was the exception, and it turned an air-gapped node, or
		// a run this command's own network warning predicted, into a node reported as unable to
		// slice. `unavailable` is reserved for a container that ran and demonstrably lacked the slice.
		detail := "the allocator produced the slice injection, and the probe container could not be " +
			"started, so nothing was measured: " + err.Error()
		if p.runtime != nil && p.runtime.NetworkWarning != "" {
			detail += ". " + p.runtime.NetworkWarning
		}
		return unreachablePair(accel.ID, device.PreflightStateOK,
			device.PreflightDepthSimulated, detail, "").
			withCommand(run.Command).withEvidence(string(run.Output))
	case run.Emitted:
		detail := "the allocator produced the slice injection; the container step was emitted " +
			"rather than run because " + run.Reason
		if p.dryRun {
			// Only on a dry run, which is the one path that reaches here having written neither of
			// the two things the command mounts: the other fallbacks did stage, and telling their
			// reader to stage again would send them after a file that is already there.
			//
			// Both are named, not just the library tree. A dry run also withholds whatever the
			// manufacturer's own responder renders -- Ascend's npu_info.config, Hygon's vdev.conf --
			// while the command still mounts it from the host path it would have been promoted to,
			// so a reader told only about the library tree would run this and find a mount source
			// missing that nothing had mentioned.
			detail += ". The command mounts " + filepath.Join(deviceplugin.OperatorLibDir, manufacturer) +
				" and, where this manufacturer's responder renders one, a file under " +
				filepath.Join(deviceplugin.OperatorPreflightDir, string(deviceplugin.PreflightPodUID)) +
				"; a dry run deliberately writes neither: re-run without --dry-run, which writes both"
		}
		return unreachablePair(accel.ID, device.PreflightStateOK, device.PreflightDepthSimulated,
			detail, "").withCommand(run.Command)
	}

	return judgeProbeOutput(accel.ID, p.host.root, injection, probe, run)
}

// simulateInjection drives the manufacturer's own responder for one accelerator and returns the
// injection an allocation would emit for it, with its mount host paths pointing where the container
// will actually find them.
func (p *Preflighter) simulateInjection(
	ctx context.Context,
	manufacturer string,
	injector deviceplugin.AcceleratorInjectionPreflighter,
	grp *workercore.DevicesGroup,
	accel *workercore.Accelerator,
) (*deviceplugin.ContainerAllocateResponse, error) {
	// One accelerator per request, because the acceptance is one container per accelerator: handing
	// the responder the whole group would grant every accelerator of it to a single container and
	// measure a node-wide allocation rather than a slice.
	pod, ctr, devs, allocated, err := sliceRequest(manufacturer, grp, accel)
	if err != nil {
		return nil, err
	}

	injections, err := p.driveResponder(manufacturer, injector,
		func(open responderOpener) ([]*deviceplugin.ContainerAllocateResponse, error) {
			responder, driveErr := open(workercore.DeviceAllocationModeSliced)
			if driveErr != nil {
				return nil, driveErr
			}
			injection, _, driveErr := slicedResponse(ctx, responder, pod, ctr, devs, allocated, nil)
			if driveErr != nil {
				return nil, driveErr
			}
			return []*deviceplugin.ContainerAllocateResponse{injection}, nil
		})
	if err != nil {
		return nil, err
	}
	return injections[0], nil
}

// slicedResponse renders one sliced allocation's container response through whichever method this
// manufacturer serves the sliced mode with, and returns the geometry it was placed at.
//
// GetLogicalSlicedResponse "replaces GetContainerAllocateResponse for the sliced mode" in its own
// words, and for one manufacturer that is not a refinement but the only way to see a slice at all:
// AMD's GetContainerAllocateResponse never reads the allocation mode, so it answers every mode with
// the same whole-accelerator injection -- no cap, no preload library, nothing a probe could look
// for. Measured on hardware: driving the universal entry point there emitted a probe carrying two
// device nodes, no mounts and no memory limit, which would have reported a slice nobody asked for.
//
// The assertion is the one F11 permits and calls sometimes necessary. GetLogicalSlicedResponse
// renders files, under the paths the redirect neutralizes, and touches no hardware.
// PhysicalSlicedResponder is the interface that must never be asserted for, and is not.
//
// occupied is what other slices already hold on these accelerators. It is what makes a second
// co-tenant land where the first is not -- without it both are placed identically, and two
// containers that were handed the same geometry demonstrate nothing about sharing one.
func slicedResponse(
	ctx context.Context,
	responder deviceplugin.ContainerAllocateResponder,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
	occupied deviceplugin.Placements,
) (*deviceplugin.ContainerAllocateResponse, deviceplugin.Placements, error) {
	logical, ok := responder.(deviceplugin.LogicalSlicedResponder)
	if !ok {
		injection, err := responder.GetContainerAllocateResponse(ctx, pod, ctr, devs, allocated)
		if err != nil {
			return nil, nil, fmt.Errorf("drive the slice responder: %w", err)
		}
		return injection, nil, nil
	}

	placements, err := logical.PlaceLogicalSliced(ctx, pod, ctr, devs, allocated, occupied)
	if err != nil {
		return nil, nil, fmt.Errorf("place the logical slice: %w", err)
	}
	injection, err := logical.GetLogicalSlicedResponse(ctx, pod, ctr, devs, allocated, placements)
	if err != nil {
		return nil, nil, fmt.Errorf("drive the slice responder: %w", err)
	}
	return injection, placements, nil
}

// mergePlacements folds one allocation's geometry into what is already held, so the next allocation
// is placed around both. The runs are appended rather than replaced: two slices on one accelerator
// are two entries under one resource, which is the whole shape co-tenancy is about.
func mergePlacements(into, add deviceplugin.Placements) deviceplugin.Placements {
	if len(add) == 0 {
		return into
	}
	if into == nil {
		into = deviceplugin.Placements{}
	}
	for res, runs := range add {
		into[res] = append(into[res], runs...)
	}
	return into
}

// sliceRequest fabricates the allocation request for one logical slice of one accelerator.
func sliceRequest(manufacturer string, grp *workercore.DevicesGroup, accel *workercore.Accelerator) (
	*core.Pod, *core.Container, *workercore.Devices, map[deviceplugin.Resource]int32, error,
) {
	one := *grp
	one.Accelerators = []workercore.Accelerator{*accel}

	pod, ctr, devs, allocated, err := deviceplugin.NewPreflightAllocationRequest(
		[]workercore.DevicesGroup{one}, manufacturer, workercore.DeviceAllocationModeSliced, sliceQuota)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("build the allocation request: %w", err)
	}
	ctr.Name = probeContainerName(ctr.Name, accel.ID)
	return pod, ctr, devs, allocated, nil
}

// probeContainerName names the synthetic container that stands for one accelerator's probe.
//
// The request builder fixes the Pod's identity so that two calls with the same inputs agree, and a
// responder derives its per-container host paths from that identity. Fixing the container name too
// would make every accelerator of a manufacturer derive the *same* path: measured on an 8-NPU host,
// all eight cards rendered their quota config over one file, so seven of the eight emitted commands
// mounted a config describing the eighth card. The accelerator is what makes the name its own, and
// the name stays a function of its inputs, so two runs still agree.
//
// The ID is folded to what a path can carry: some manufacturers' accelerator IDs contain spaces.
//
// Folded rather than hashed, unlike the barrier directory beside it, because this name is what a
// person reads out of the container runtime while a preflight is running, and an accelerator nobody
// can identify there is worth less than the injectivity a hash would buy. The two properties a hash
// gives are argued here instead. Traversal: the allow-list keeps neither "." nor "/", so both fold
// to "-" and no ".." survives to be resolved. Injectivity: two IDs collide only by differing
// nowhere except in the characters that fold, and the IDs being told apart are one manufacturer's
// on one host, where the driver returns them in one shape -- Ascend's five hex fields split by
// spaces, a PCI address, a vendor prefix and a hex digest -- so two of them differ in a hex digit
// long before they differ in a separator.
func probeContainerName(base, acceleratorID string) string {
	folded := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '-'
		}
	}, acceleratorID)
	if folded == "" {
		return base
	}
	return base + "-" + folded
}

// driveResponder builds the manufacturer's own responder, hands it to drive, and returns the
// injections that came back addressed as the host addresses them.
//
// Two halves make this safe and usable at once. The responder runs inside the redirect its own
// package set up, so a manufacturer that renders host state while answering renders it into a
// scratch directory instead of onto the node -- that is what keeps a simulated pass from being an
// action. But the mount paths in the injection then name that scratch directory, which stops
// existing the moment the redirect is restored, so what the responder rendered is copied onto the
// host through the mounted host root and every host path is rewritten onto its real location. The
// injections that come back are therefore the allocator's own, addressed as the host addresses them.
//
// drive may produce more than one injection, and every allocation of one behavior is driven against
// one responder inside one redirect. Two responders would be two redirects, and a sidecar answered
// under a second one would co-allocate from an owner whose artifacts had already been taken away.
//
// This is the only place the promotion exists. A second copy of it is a copy that can be fixed in
// one place and not the other, and what that costs is a container mounting a directory that no
// longer exists -- reported not as a preflight bug but as a slice that does not work.
func (p *Preflighter) driveResponder(
	manufacturer string,
	injector deviceplugin.AcceleratorInjectionPreflighter,
	drive func(open responderOpener) ([]*deviceplugin.ContainerAllocateResponse, error),
) ([]*deviceplugin.ContainerAllocateResponse, error) {
	// The real paths are read before any redirect opens and the scratch ones while each is open,
	// which is the only window either is knowable in: both are package variables the manufacturer's
	// own seam moves, and neither the caller nor the manufacturer can name the other's.
	//
	// What a responder rendered is promoted into the preflight tree rather than back to the pods
	// tree it was addressed under: an allocator reads the latter as occupancy, and a pass killed
	// before its cleanup runs would leave entries there that shift a real allocation. See
	// deviceplugin.OperatorPreflightDir.
	realLib, preflightPods := deviceplugin.OperatorLibDir, deviceplugin.OperatorPreflightDir

	var (
		restores []func()
		// swaps is every (scratch, host) pair an open redirect established: the two shared roots for
		// each redirect, plus whatever private path the manufacturer handed over. They accumulate
		// rather than replace, because an injection is addressed under whichever redirect was open
		// when it was produced and a caller may drive more than one.
		swaps [][2]string
		// rendered is the scratch pods root of each open redirect, which is the only place a
		// responder writes a file of its own. Kept apart from swaps because promoting is addressed
		// by root while rewriting is addressed by pair, and one list cannot answer both.
		rendered []string
	)
	// Undone in reverse, and only once everything below has run: each redirect removes its own
	// directory, and what a responder rendered has to be carried onto the host before the directory
	// holding it stops existing.
	defer func() {
		for i := len(restores) - 1; i >= 0; i-- {
			restores[i]()
		}
	}()

	open := func(mode workercore.DeviceAllocationMode) (deviceplugin.ContainerAllocateResponder, error) {
		responder, restore, err := injector.PreflightResponder(mode)
		if err != nil {
			return nil, fmt.Errorf("build the %s responder: %w", mode, err)
		}
		restores = append(restores, restore)
		rendered = append(rendered, deviceplugin.OperatorPodsDir)
		swaps = append(swaps,
			[2]string{deviceplugin.OperatorLibDir, realLib},
			[2]string{deviceplugin.OperatorPodsDir, preflightPods},
		)
		// A manufacturer may carry a host path beyond the shared pair -- NVIDIA's HAMi-core lock
		// directory is one -- and only the redirect knows it moved. Measured on hardware: without
		// this, a dry run emitted a command mounting /tmp/gpustack-preflight-<n>/vgpulock, a
		// directory that exists in no node's /tmp and shares the lock with nothing.
		for scratch, host := range deviceplugin.PreflightRehosts() {
			swaps = append(swaps, [2]string{scratch, host})
		}
		return responder, nil
	}

	injections, err := drive(open)
	if err != nil {
		return nil, err
	}
	for _, injection := range injections {
		if injection == nil {
			return nil, fmt.Errorf("the %s responder produced no injection", manufacturer)
		}
		if injection.Envs == nil {
			injection.Envs = map[string]string{}
		}
	}

	// The library tree is staged separately (stageLibFor) and is already where realLib says; only
	// what a responder itself rendered has to be carried out of its scratch directory before it
	// goes. A responder that rendered nothing leaves no such directory, and that is not an error:
	// only one manufacturer's sliced path writes a host file at all.
	//
	// Gated on --dry-run for the same reason the library tree is: writing is part of taking the step.
	// Measured on hardware, this was the half that was not gated -- a dry run left two rendered
	// npu_info.config files on an Ascend node while every row claimed nothing had been written.
	if !p.dryRun {
		for _, scratchPods := range rendered {
			if err = copyTree(scratchPods, filepath.Join(p.host.root, preflightPods)); err != nil &&
				!errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("stage what the responder rendered onto the host: %w", err)
			}
		}
	}

	for _, injection := range injections {
		for _, m := range injection.GetMounts() {
			m.HostPath = rehostPath(m.GetHostPath(), swaps...)
		}
	}
	return injections, nil
}

// responderOpener builds one of the manufacturer's responders inside the window driveResponder holds
// open, and is how a caller that needs more than one gets them without closing the first.
//
// More than one is not a convenience. A container's response depends on the mode of the **server**
// answering it, not on what the container asked for — NVIDIA's reads `s.AllocationMode` and takes
// the sliced path for every container a sliced server serves — and on a node the kubelet's two
// Allocate calls reach two servers. Measured on hardware: driving a visibility sidecar against the
// sliced responder sent it down the slicing path, where it failed for want of a memory percentage
// the product deliberately never puts on a sidecar. Opening the mode the node would have used is
// what makes the answer the node's.
type responderOpener func(workercore.DeviceAllocationMode) (deviceplugin.ContainerAllocateResponder, error)

// rehostPath rewrites one host path built under a scratch redirect onto the location the host knows
// it by, given every (scratch, host) pair the open redirects established.
//
// A path under none of them is returned untouched: most of what an injection mounts -- a driver
// directory, /dev/shm -- was never redirected in the first place. The first matching pair wins, and
// the scratch roots are distinct temporary directories, so at most one of them can prefix any given
// path anyway.
func rehostPath(path string, swaps ...[2]string) string {
	for _, swap := range swaps {
		if rel, err := filepath.Rel(swap[0], path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join(swap[1], rel)
		}
	}
	return path
}

// probeShellCommand is what runs inside the probe container.
//
// It is one shell line rather than the reader alone, for three reasons that each cost a separate
// container otherwise. /proc/self/maps is read first because it is the load evidence: every
// dynamically linked program in a container carrying /etc/ld.so.preload has the slicing runtime in
// its own address space, so `cat` reading its own maps shows whether the shim loaded, in every
// image and with no vendor tool at all. Standard error is merged in because the shim says which cap
// it read there and nowhere else. And the reader's exit status is swallowed into a line of output
// because it is evidence rather than a verdict: every one of these monitors exits non-zero in a
// container that has allocated nothing, which is precisely the container this starts, and letting
// that fail the run would report a working slice as a broken node.
// mapsBegin and mapsEnd delimit the mapping section inside the probe's combined output, which is
// what lets the two clauses below read the part of it that answers them and nothing else.
//
// They are not decoration. Standard error is merged into this output, and the dynamic loader names
// the object it refused inside the refusal -- "object '<path>' from LD_PRELOAD cannot be preloaded
// ... ignored" -- so a plain search of the whole output finds the shim's path in the very message
// proving the shim did not load. The same merge puts the slicing runtime's own debug line, which
// echoes the cap it was configured with, in front of a reader that may be about to report the whole
// card. Each clause therefore reads one section: mappings between the markers, the vendor reader's
// answer after them.
const (
	mapsBegin = "gpustack-preflight-maps-begin"
	mapsEnd   = "gpustack-preflight-maps-end"
)

// quiet names the variables the shim's own log level was raised through, and they are unset before
// the reader runs.
//
// The reader inherits LD_PRELOAD, and has to: the question is what a capped process sees, so the shim
// must be loaded inside it. Left noisy, the shim then prints the cap it was configured with into the
// reader's own section, and the quota clause finds the figure it is looking for whatever the vendor
// tool goes on to report. Measured on an AMD host, where the figure in the reader's section came from
// the shim's banner while the tool's own body named no cap at all -- the section split alone does not
// separate the two, because both are printed by the reader's process.
func probeShellCommand(reader string, quiet []string) []string {
	script := "echo " + mapsBegin + "; cat /proc/self/maps; echo " + mapsEnd
	if reader != "" {
		if len(quiet) != 0 {
			script += "; unset " + strings.Join(quiet, " ")
		}
		script += "; " + reader + " || echo probe-reader-exit-$?"
	}
	return []string{"sh", "-c", "{ " + script + "; } 2>&1"}
}

// logEnvNames are the variables probe raises the shim's log level through, sorted so one injection
// always builds one command.
func logEnvNames(probe sliceProbe) []string {
	return slices.Sorted(maps.Keys(probe.LogEnv))
}

// The co-tenancy barrier: where the two probes signal each other inside the container, what a probe
// prints once it has seen its peer, and how long it waits in tenths of a second.
//
// Starting two one-shot containers at the same moment does not make them overlap -- the first can be
// finished before the runtime has created the second, and a warm image against a cold one widens
// that gap further. Co-tenancy is the claim that one accelerator carries two slices *at once*, so
// without an overlap there is nothing to have measured, however well both containers ran.
//
// Each probe announces itself and then waits for the other, so a reader runs only while both
// containers are alive. A probe that waited out the timeout reads anyway and prints no marker: its
// answer is still worth having, at the simulated depth that says the two were not seen together.
const (
	coTenancyBarrierDir  = "/gpustack-preflight-barrier"
	coTenantsMet         = "gpustack-preflight-co-tenants-met"
	coTenancyBarrierWait = 300 // 30 seconds, which is an image pull apart rather than a scheduling jitter
)

// coTenantProbeShellCommand is probeShellCommand behind that barrier, for the tenant called self
// waiting on the one called peer.
func coTenantProbeShellCommand(reader string, quiet []string, self, peer string) []string {
	selfPath := coTenancyBarrierDir + "/" + self
	peerPath := coTenancyBarrierDir + "/" + peer

	barrier := ": > " + selfPath + "; i=0; " +
		"while [ ! -e " + peerPath + " ] && [ $i -lt " + strconv.Itoa(coTenancyBarrierWait) + " ]; " +
		"do sleep 0.1; i=$((i+1)); done; " +
		"if [ -e " + peerPath + " ]; then echo " + coTenantsMet + "; fi"

	return []string{"sh", "-c", "{ " + barrier + "; } 2>&1; " + probeShellCommand(reader, quiet)[2]}
}

// probeSections splits the probe's combined output into the mapping section and everything the
// vendor reader printed after it.
//
// A missing marker fails closed on both counts: mappings comes back empty, so every mounted object
// reads as unloaded, and readerOut comes back empty, so no cap can be observed. That is the honest
// outcome for output this package cannot locate itself in -- the alternative is searching the whole
// stream, which is the failure mode the markers exist to remove.
func probeSections(evidence string) (mappings, readerOut string) {
	begin := strings.Index(evidence, mapsBegin)
	if begin < 0 {
		return "", ""
	}
	rest := evidence[begin+len(mapsBegin):]

	end := strings.Index(rest, mapsEnd)
	if end < 0 {
		return "", ""
	}
	return rest[:end], rest[end+len(mapsEnd):]
}

// judgeProbeOutput turns what the container printed into the two rows.
//
// Both clauses are answered by looking for something the runner already holds in the output the
// container produced, rather than by parsing a vendor tool's grammar: the shared objects the
// injection itself mounts, and the memory figure the injection itself set. That is what keeps this
// from having to know four output formats, and it is also what makes the verdict specific -- it is
// this injection's own shim and this injection's own cap that were looked for, not a plausible one.
func judgeProbeOutput(
	acceleratorID, hostRoot string,
	injection *deviceplugin.ContainerAllocateResponse,
	probe sliceProbe,
	run emitResult,
) []device.PreflightCheck {
	evidence := string(run.Output)
	mappings, readerOut := probeSections(evidence)

	loaded := device.PreflightCheck{
		Accelerator: acceleratorID,
		Capability:  capSlicedRuntimeLoaded,
		Mode:        device.PreflightModeOf(workercore.DeviceAllocationModeSliced),
		State:       device.PreflightStateOK,
		Depth:       device.PreflightDepthMeasured,
		Command:     run.Command,
		Evidence:    evidence,
	}
	switch missing := unloadedObjects(injection, mappings); {
	case len(missing) == 0 && injectedObjects(injection) == 0:
		// Not "this manufacturer declares no slicing": the detect pass called this accelerator
		// sliceable, this manufacturer has a container probe, and the allocator was driven for a
		// sliced request. An injection carrying no shared object at the end of that is a slice with
		// no runtime behind it, and reporting it as undeclared would say the hardware does not offer
		// slicing while the row above says it does -- and would exit zero on a node that cannot slice.
		loaded.State = device.PreflightStateUnavailable
		loaded.Reason = "the allocator produced a slice for this accelerator that mounts no shared " +
			"object, so there is no slicing runtime for the container to load"
	case len(missing) != 0:
		loaded.State = device.PreflightStateUnavailable
		loaded.Reason = "the container ran without " + strings.Join(missing, ", ") +
			" in its address space, so the injection mounted a slicing runtime that did not load"
	case run.ExitError != "":
		// The objects are mapped, so the runtime loaded -- and then the container died under it.
		// That is this accelerator failing the very behavior the step measures, not an environment
		// limit, so it is the one state that exits non-zero.
		loaded.State = device.PreflightStateUnavailable
		loaded.Reason = "the slicing runtime the injection mounts loaded, and the container then " +
			"exited non-zero under it: " + run.ExitError
	default:
		loaded.Detail = "the slicing runtime the injection mounts is loaded in the container"
	}

	quota := device.PreflightCheck{
		Accelerator: acceleratorID,
		Capability:  capSlicedQuotaInForce,
		Mode:        device.PreflightModeOf(workercore.DeviceAllocationModeSliced),
		State:       device.PreflightStateOK,
		Depth:       device.PreflightDepthMeasured,
		Command:     run.Command,
		Evidence:    evidence,
	}
	limit, absent := memoryQuota(injection, probe, hostRoot)
	switch {
	case run.ExitError != "":
		// The same container as the row above, so the same call: a figure printed by a process that
		// then died under the injection was not observed holding. Left out, the two rows disagree
		// about one container -- one saying the runtime killed it, the other that its cap is fine.
		quota.State = device.PreflightStateUnavailable
		quota.Reason = "the container exited non-zero under the slicing runtime the injection " +
			"mounts, so whatever it reported about its cap was not observed to hold: " + run.ExitError
	case limit == "":
		// Reached only for a manufacturer whose probe names a carrier for the cap, on an accelerator
		// the detect pass called sliceable, from an injection the allocator produced for a sliced
		// request. An empty cap here is therefore what the responder failed to put in, not a slice
		// that has none: nothing bounds this container, and a row saying so is the whole point.
		quota.State = device.PreflightStateUnavailable
		quota.Reason = "the allocator produced a slice whose per-accelerator memory cap this run " +
			"could not read, so nothing observed bounds this container -- " + absent
	case !reportsFigure(readerOut, limit):
		quota.Depth = device.PreflightDepthSimulated
		quota.Detail = "the injection caps this accelerator at " + limit +
			" and the container did not report that figure back, so the cap is applied but was not " +
			"observed in force; the container's own output is carried as evidence"
	default:
		quota.Detail = "the container reports " + limit +
			", which is the cap the injection set rather than the whole accelerator"
	}

	return []device.PreflightCheck{loaded, quota}
}

// injectedObjects counts the shared objects an injection mounts into the container.
func injectedObjects(injection *deviceplugin.ContainerAllocateResponse) int {
	var n int
	for _, m := range injection.GetMounts() {
		if strings.HasSuffix(m.GetContainerPath(), ".so") {
			n++
		}
	}
	return n
}

// unloadedObjects names the shared objects the injection mounts that are absent from the container's
// own address space, given the mapping section of the probe's output.
//
// The mounts are the question rather than a per-manufacturer library name, because the injection is
// the allocator's own: whatever it decided to preload is what has to be there, and a manufacturer
// that changes which object it ships changes this check with it.
//
// A mapping's pathname is its last field, and it is compared whole. A substring search over the
// section would count a path that merely appears inside a longer one, and the section itself is
// what keeps a loader's refusal -- which quotes the same path -- from reading as the object being
// mapped.
func unloadedObjects(injection *deviceplugin.ContainerAllocateResponse, mappings string) []string {
	mapped := sets.New[string]()
	for line := range strings.SplitSeq(mappings, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		mapped.Insert(fields[len(fields)-1])
	}

	var missing []string
	for _, m := range injection.GetMounts() {
		path := m.GetContainerPath()
		if strings.HasSuffix(path, ".so") && !mapped.Has(path) {
			missing = append(missing, path)
		}
	}
	return missing
}

// reportsFigure says whether the vendor reader's own output carries figure as a number of its own
// rather than as part of a longer one -- so a cap of 8184 is not read out of 18184, and the cap is
// looked for only where the reader answered.
func reportsFigure(readerOut, figure string) bool {
	if figure == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(readerOut[i:], figure)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(figure)
		beforeOK := start == 0 || !isASCIIDigit(readerOut[start-1])
		afterOK := end == len(readerOut) || !isASCIIDigit(readerOut[end])
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
	}
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// memoryQuota returns the per-accelerator cap the injection set, as the bare figure a vendor tool
// would print rather than the encoding the vendor library parses -- the unit suffix an allocator
// appends is dropped, because no reader prints it back in the same form.
//
// It reads whichever of the two carriers this manufacturer uses. The file is read from our side
// through the mounted host root rather than from inside the container: the injection already names
// its host path, we wrote it there a moment ago, and reading it here needs no second container and
// no assumption about what tools the probe image carries.
func memoryQuota(
	injection *deviceplugin.ContainerAllocateResponse, probe sliceProbe, hostRoot string,
) (limit, absent string) {
	if probe.MemoryQuotaEnvPrefix != "" {
		for k, v := range injection.GetEnvs() {
			if strings.HasPrefix(k, probe.MemoryQuotaEnvPrefix) {
				return strings.TrimRight(v, "mMbBiI"), ""
			}
		}
		return "", "the injection carries no " + probe.MemoryQuotaEnvPrefix +
			"* variable, which is where this manufacturer's allocator puts the cap"
	}
	if probe.MemoryQuotaConfigMount == "" {
		return "", "this package knows no carrier for this manufacturer's cap"
	}

	for _, m := range injection.GetMounts() {
		if m.GetContainerPath() != probe.MemoryQuotaConfigMount {
			continue
		}
		body, err := os.ReadFile(filepath.Join(hostRoot, m.GetHostPath()))
		if err != nil {
			return "", "the injection mounts " + probe.MemoryQuotaConfigMount +
				", and the host copy it mounts from could not be read: " + err.Error()
		}
		for _, line := range strings.Split(string(body), "\n") {
			if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok &&
				key == probe.MemoryQuotaConfigKey {
				return value, ""
			}
		}
		return "", "the injection mounts " + probe.MemoryQuotaConfigMount +
			", and it names no " + probe.MemoryQuotaConfigKey
	}
	return "", "the injection mounts no " + probe.MemoryQuotaConfigMount +
		", which is where this manufacturer's allocator renders the cap"
}

// checkPair is the two rows one accelerator's slice answer always produces, so that a run reports the same
// shape whatever depth it reached.
type checkPair []device.PreflightCheck

// withCommand records the container step on both rows, for the answers reached before or instead of
// running it -- where the command is the most useful part of the answer.
func (p checkPair) withCommand(command string) checkPair {
	for i := range p {
		p[i].Command = command
	}
	return p
}

// withEvidence records whatever the container managed to print on both rows.
//
// It matters most on the path where the container did not run to completion: whatever it said before
// it died is the operator's lead, and dropping it leaves them with an exit status and a command.
// Empty output records nothing rather than an empty field, so a row never suggests a container ran
// and said nothing when none ran at all.
func (p checkPair) withEvidence(evidence string) []device.PreflightCheck {
	if evidence == "" {
		return p
	}
	for i := range p {
		p[i].Evidence = evidence
	}
	return p
}

// unreachablePair builds both rows for a slice whose container step was never taken, carrying the
// same state, depth and words on each.
//
// A detail rather than a reason is where "it went no deeper" is said, and that is deliberate: a
// PreflightCheck's reason is empty exactly when its state is ok, and an answer that established the
// injection and stopped there is an ok answer at a shallower depth -- not a failure. The two are
// only ever both set where the state is genuinely not ok.
func unreachablePair(
	acceleratorID string, state device.PreflightState, depth device.PreflightDepth, detail, reason string,
) checkPair {
	pair := make(checkPair, 0, 2)
	for _, capability := range []string{capSlicedRuntimeLoaded, capSlicedQuotaInForce} {
		pair = append(pair, device.PreflightCheck{
			Accelerator: acceleratorID,
			Capability:  capability,
			Mode:        device.PreflightModeOf(workercore.DeviceAllocationModeSliced),
			State:       state,
			Depth:       depth,
			Detail:      detail,
			Reason:      reason,
		})
	}
	return pair
}

// sweepRenderedArtifacts removes what this run's responders rendered onto the host.
//
// Hygiene rather than correctness, and deliberately so. What it removes lives under
// deviceplugin.OperatorPreflightDir, a tree no allocator reads, so a run killed before this executes
// leaves nothing that costs a later allocation anything. It is a deferred call, and a deferred call
// does not run after a SIGKILL -- which is exactly why the promotion targets that tree rather than
// the pod directory an allocator reads as occupancy.
//
// A dry run rendered nothing, and a host root that cannot be entered has nothing reachable to
// remove.
func (p *Preflighter) sweepRenderedArtifacts() {
	if p.dryRun || p.host == nil || p.host.Validate() != nil {
		return
	}
	dir := filepath.Join(p.host.root, deviceplugin.OperatorPreflightDir, string(deviceplugin.PreflightPodUID))
	if err := os.RemoveAll(dir); err != nil {
		logger.Error(err, "could not remove what this pass rendered", "path", dir)
	}
}

// sweepStaleContainers removes any container an earlier run left labeled behind, before this run
// starts any of its own, and records every runtime whose sweep it could not complete.
//
// A run killed between starting a container and reaping it leaves one holding an accelerator, and
// the next run's allocation would then be measured against a card that is already occupied. That is
// why a sweep it could not complete is not swallowed: the measurement that follows would report a
// healthy accelerator as unavailable, which is the exact failure the sweep exists to prevent, and it
// would do so while naming the accelerator as the thing at fault. What this pass could not rule out,
// it says.
//
// The recorded failures are node-wide, not per runtime this pass happens to drive: the accelerator is
// a physical resource, so a leftover under docker holds the card a nerdctl probe is about to measure
// just as firmly. There is nothing to be gained by excusing a runtime this run did not resolve.
//
// A host with no runtime, no host root, or a dry run sweeps nothing and records nothing -- none of
// them is about to start a container either.
//
// It is two commands and not one because neither CLI's `rm` takes a filter -- only `ps` does -- so
// the sweep lists and then removes, which is why it goes through the host's shell rather than being
// built by buildContainerRunArgv like every step that starts something.
func (p *Preflighter) sweepStaleContainers(ctx context.Context) {
	if p.runtime == nil || p.dryRun || p.host.Validate() != nil {
		return
	}

	// Every runtime that could be holding one, not only the one this pass resolved. A pass killed
	// under --runtime=docker leaves a docker container holding the accelerator, and the next pass --
	// defaulting to the kubelet's nerdctl -- would measure its own slice against that leftover while
	// reporting the accelerator as the thing at fault. The guarantee this sweep gives is that an
	// earlier run's probes are gone, and it does not hold if the sweep only looks where this run
	// happens to be looking.
	//
	// ctr is not in the list for the same reason it never starts a probe: no pass ever created a
	// container through it. A nerdctl sweep reaches the same containerd regardless.
	// No vendor runtime: a sweep starts nothing, it only lists and removes.
	current, _ := probeRuntimeFor(p.runtime, "")

	for _, name := range []string{"docker", "nerdctl"} {
		if !p.host.Has(ctx, name) {
			continue
		}
		// The resolved runtime's socket and namespace are the kubelet's own, which is where its
		// containers were started.
		rt := p.runtime
		if name != current {
			// Any other runtime is addressed the way this command would have addressed it had it
			// resolved it -- which is what an earlier pass under --runtime <name> did, so it is where
			// that pass's containers are. Built rather than left empty because the addressing is not
			// a default: the namespace is this command's own (containerdNamespace), chosen so that
			// nothing else collects its containers, and a containerd CLI asked without it looks in
			// "default" and finds none of them however many are there. The socket is probed for the
			// same reason it is probed at resolution -- a k3s or RKE2 node carries one where the CLI
			// does not look by itself.
			rt = p.host.describeRuntime(name, "")
		}

		err := p.sweepWith(ctx, name, rt)
		if err == nil {
			continue
		}

		// A runtime this pass did not resolve, which could not be reached at all, is an absence
		// established rather than a failure tolerated. It is reached here exactly as this command
		// would have addressed it had it resolved it, so a CLI that cannot answer that address could
		// not have started a probe through it either -- there is nothing of ours behind a daemon
		// nobody can talk to. Measured on an Ascend host: it carries nerdctl and a containerd socket
		// at the RKE2 path, but the daemon behind it refuses the connection, so every pass would
		// otherwise have hung a permanent red row on all eight of its healthy accelerators -- and a
		// red light that is always on is one nobody reads.
		//
		// The same failure on the runtime this pass DID resolve is a failure, because that is the one
		// about to start the probes: a runtime that cannot be listed is not one to measure through.
		if name != current && errors.Is(err, errSweepUnreachable) {
			logger.Info("skipped the stale-container sweep of a runtime this host cannot reach, "+
				"which is therefore a runtime no earlier pass started a probe through",
				"runtime", name, "error", err.Error())
			continue
		}
		// Logged as well as recorded, because the recording only becomes visible as rows on the
		// accelerators this pass goes on to measure: a run asked about a manufacturer this node has
		// none of would otherwise record the failure and say nothing anywhere.
		logger.Error(err, "could not sweep the containers an earlier preflight may have left behind",
			"runtime", name)
		p.sweepFailures = append(p.sweepFailures, fmt.Sprintf("%s: %s", name, err.Error()))
	}
}

// errSweepUnreachable marks a sweep that never got as far as an answer: the runtime could not be
// asked what it is holding. It is distinct from a sweep that was answered and could not act on the
// answer, which is a failure wherever it happens.
var errSweepUnreachable = errors.New("this runtime could not be asked what it is holding")

// sweepWith removes every labeled container one runtime can see, addressed as rt says to address it,
// and returns what stopped it where it could not. A runtime with nothing to be told (docker) carries
// neither field and is invoked bare.
//
// Two commands and not one because neither CLI's `rm` takes a filter -- only `ps` does -- and they
// are two host executions rather than one shell line because the difference between them is the
// whole verdict. Asked as `ids=$(list) || exit $?; ... rm -f $ids`, both halves come back as one
// non-zero status, and "this runtime could not be asked" is then indistinguishable from "it answered
// and the leftovers would not go away" -- which are the case to excuse and the case to fail on. A
// pipe into `xargs -r` is worse still: it takes the pipeline's status from xargs, and xargs given
// nothing exits zero, so a list that failed outright reads as a node with nothing to sweep. Measured
// on an Ascend host, where that shape swept nothing, exited zero and logged nothing on every pass.
//
// Splitting them also takes the shell out of it: the ids are split here rather than by word
// splitting, so nothing on this path is quoted into a command line, and the host's `sh` being dash
// (no `set -o pipefail`) stops mattering.
func (p *Preflighter) sweepWith(ctx context.Context, name string, rt *hostRuntime) error {
	args := sweepRuntimeArgs(rt)

	out, err := p.host.Run(ctx, name,
		append(slices.Clone(args), "ps", "-aq", "--filter", "label="+preflightLabel)...)
	if err != nil {
		return fmt.Errorf("%w: %w", errSweepUnreachable, err)
	}

	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}

	if _, err := p.host.Run(ctx, name,
		append(append(slices.Clone(args), "rm", "-f"), ids...)...); err != nil {
		return fmt.Errorf("could not remove %d stale container(s) it is holding: %w", len(ids), err)
	}
	logger.Info("removed containers an earlier preflight left behind",
		"runtime", name, "containers", len(ids))
	return nil
}

// sweepRuntimeArgs is how a runtime is addressed, as leading arguments. Only the containerd CLIs
// carry any: docker reads the host's own configuration, which is the whole reason it is invoked as
// the host rather than from a CLI we ship.
func sweepRuntimeArgs(rt *hostRuntime) []string {
	var args []string
	if rt.Socket != "" {
		args = append(args, "--address", rt.Socket)
	}
	if rt.Namespace != "" {
		args = append(args, "--namespace", rt.Namespace)
	}
	return args
}
