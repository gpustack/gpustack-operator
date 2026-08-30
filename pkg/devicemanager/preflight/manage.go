package preflight

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// The two management behaviors, one row each per accelerator. Unlike a driver-read row neither is
// a capability the manufacturer's vocabulary has a word for: no driver is asked, two allocations
// are made and compared, and -- where the environment allows it -- started.
//
// They are separate rows because they are separate behaviors with separate remedies. A sidecar
// that is granted something other than what its owner holds is a workload that sees the wrong
// device; two slices that cannot coexist on one accelerator is a node that serves one tenant.
const (
	capSidecarVisibility = "sidecar-visibility"
	capCoTenancy         = "co-tenancy"
)

// The containers the synthetic Pod carries alongside the owner. They are the second container of
// the two Pod shapes at issue -- the SSH-enabled Instance's visibility sidecar, and a second
// independent tenant of the same accelerator -- and they are in the Pod from the start, because
// that is how the kubelet sees such a Pod and how the visibility resolution finds an owner to
// co-allocate from.
const (
	preflightSidecarContainer  = "preflight-visibility"
	preflightCoTenantContainer = "preflight-cotenant"
)

// visibleDevicesEnvSuffix is how an injection says which accelerators its container may see. Seven
// of the nine manufacturers name a variable ending this way -- ASCEND_VISIBLE_DEVICES,
// NVIDIA_VISIBLE_DEVICES and so on -- and the rest name device nodes instead, which is why
// grantedDevices reads both and neither on its own.
const visibleDevicesEnvSuffix = "_VISIBLE_DEVICES"

// The two reasons a management answer stops short, said in words on the row that stopped.
//
// Neither is a failure. A behavior that could not be taken deeper is an answer about how far this
// environment let the pass go, and F6 is explicit that it is reported at the depth it reached with
// the reason -- never as a pass and never as a failure.
const (
	sidecarNoDeeper = "the measured form needs the owner container to still be running when the " +
		"sidecar starts, and the container this command starts is a one-shot that exits as soon as " +
		"it has printed its evidence, so there would be nothing left for a sidecar to co-allocate from"
	partitionNotDriven = "this accelerator is partition-backed, so a sidecar is granted the " +
		"partition its owner holds rather than the accelerator itself. That response is served by " +
		"the responder's partition capability, which this pass does not drive: reaching it means " +
		"asserting the responder to the interface that also creates a hardware partition, and " +
		"preflight reads partitions and never creates one"
)

// checkManagement answers, for one manufacturer, whether its accelerators can be managed while
// sliced -- one row per behavior per accelerator.
//
// It is asked of the same preflighter that answered the detection and the slice, for the same reason measureSliced
// is: it is the value that already holds this manufacturer's library, and building a second would
// load it again to ask a question the first can answer.
//
// Each accelerator is answered according to what it can host. One that hosts a logical slice is
// driven -- both behaviors are about slices sharing an accelerator, and a logical slice is the one
// this pass can produce without changing the hardware. A partition-backed accelerator is reported
// rather than driven, because both of its answers would have to create a partition or drive the
// interface that does.
func (p *Preflighter) checkManagement(
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
			switch {
			case accel.Status.LogicalSliced.Count > 0:
				checks = append(checks,
					p.sidecarVisibility(ctx, manufacturer, injector, grp, accel),
					p.coTenancy(ctx, manufacturer, injector, staged, grp, accel))
			case accel.Status.PhysicalSliced.Count > 0:
				checks = append(checks, partitionBackedRows(accel.ID)...)
			}
		}
	}
	return checks
}

// partitionBackedRows are the two answers a partition-backed accelerator gets: what a sidecar would
// be granted on it, and why this pass did not drive either behavior there.
//
// They are ok at the shallowest depth. Nothing was simulated -- no responder was driven at all --
// and calling that a failure would report every MIG-capable node as broken for a limit of this
// command rather than of the node.
// The mode each row carries is the mode its capability is a precondition for, which is not the mode
// of the accelerator that produced the row: what a sidecar is granted is a visibility question on
// any accelerator, while co-tenancy here is asked of a partition rather than of a slice. Filing both
// under the slicing mode -- which this accelerator does not even have -- would put them beside rows
// answering a different question, and mode is the one column that makes two manufacturers' answers
// comparable.
func partitionBackedRows(acceleratorID string) []device.PreflightCheck {
	modes := map[string]workercore.DeviceAllocationMode{
		capSidecarVisibility: workercore.DeviceAllocationModeVisibility,
		capCoTenancy:         workercore.DeviceAllocationModePartitioned,
	}

	rows := make([]device.PreflightCheck, 0, len(modes))
	for _, capability := range []string{capSidecarVisibility, capCoTenancy} {
		rows = append(rows, device.PreflightCheck{
			Accelerator: acceleratorID,
			Capability:  capability,
			Mode:        device.PreflightModeOf(modes[capability]),
			State:       device.PreflightStateOK,
			Depth:       device.PreflightDepthDeclared,
			Detail:      partitionNotDriven,
		})
	}
	return rows
}

// sidecarVisibility drives the two allocations the kubelet makes for an SSH-enabled Instance, in
// that order, and reports whether the second names anything the first was not granted.
//
// It is answered at the simulated depth: the allocator's own code produces both injections and the
// two are compared, while nothing on the hardware changes. sidecarNoDeeper says why it stops there.
func (p *Preflighter) sidecarVisibility(
	ctx context.Context,
	manufacturer string,
	injector deviceplugin.AcceleratorInjectionPreflighter,
	grp *workercore.DevicesGroup,
	accel *workercore.Accelerator,
) device.PreflightCheck {
	row := device.PreflightCheck{
		Accelerator: accel.ID,
		Capability:  capSidecarVisibility,
		Mode:        device.PreflightModeOf(workercore.DeviceAllocationModeVisibility),
		State:       device.PreflightStateOK,
		Depth:       device.PreflightDepthSimulated,
	}

	owner, sidecar, err := p.simulateSidecarPair(ctx, manufacturer, injector, grp, accel)
	if err != nil {
		row.State = device.PreflightStateUnavailable
		row.Depth = device.PreflightDepthDeclared
		row.Reason = err.Error()
		return row
	}

	// The question is containment, not equality. What F6 guards against is a sidecar reaching an
	// accelerator its owner was never granted, and that is the sidecar naming something the owner
	// does not. Naming fewer things is a different fact: measured on hardware, AMD's owner carries
	// ROCR_VISIBLE_DEVICES because its sliced response adds it, while the visibility response the
	// sidecar is served does not -- both name the same card through the same device nodes, and
	// calling that a failure would report a working node as broken.
	granted, seen := grantedDevices(owner), grantedDevices(sidecar)
	unheld := namesNotIn(seen, granted)
	unnamed := namesNotIn(granted, seen)
	switch {
	case len(granted) == 0:
		row.Detail = "the owner allocation names no visible device and no device node, so there was " +
			"nothing for the sidecar's own to be compared against. " + sidecarNoDeeper
	case len(seen) == 0:
		row.State = device.PreflightStateUnavailable
		row.Reason = "the owner was granted " + describeDevices(granted) +
			" and the sidecar allocation names no visible device and no device node at all, so a " +
			"sidecar on this node would be handed its owner's accelerator by nothing"
	case len(unheld) != 0:
		row.State = device.PreflightStateUnavailable
		row.Reason = "the sidecar allocation names " + describeDevices(unheld) +
			", which the owner was not granted (" + describeDevices(granted) +
			"), so a sidecar on this node would see a device its owner does not hold"
	case len(unnamed) != 0:
		row.Detail = "the sidecar allocation names " + describeDevices(seen) +
			", all of it the owner's own grant, and does not carry " + describeDevices(unnamed) +
			" -- which the owner's allocation adds for itself and a sidecar reaching the same " +
			"accelerator does not need. " + sidecarNoDeeper
	default:
		row.Detail = "the sidecar allocation names exactly what the owner was granted (" +
			describeDevices(granted) + "). " + sidecarNoDeeper
	}
	return row
}

// namesNotIn returns the names in a that b does not carry, in a's order. Both come from
// grantedDevices and are therefore already sorted.
func namesNotIn(a, b []string) []string {
	var out []string
	for _, name := range a {
		if !slices.Contains(b, name) {
			out = append(out, name)
		}
	}
	return out
}

// coTenancy places two independent slices on one accelerator and, where the environment permits,
// starts them together and reads back what each one sees.
//
// Everything short of that is an answer rather than a failure, and each says which one it was --
// the same ladder measureSliced walks, for the same reasons: a manufacturer with no container
// probe, a library tree that could not be staged, a probe image that cannot be resolved, and a
// container step that had to be emitted instead of run.
func (p *Preflighter) coTenancy(
	ctx context.Context,
	manufacturer string,
	injector deviceplugin.AcceleratorInjectionPreflighter,
	staged StageResult,
	grp *workercore.DevicesGroup,
	accel *workercore.Accelerator,
) device.PreflightCheck {
	row := device.PreflightCheck{
		Accelerator: accel.ID,
		Capability:  capCoTenancy,
		Mode:        device.PreflightModeOf(workercore.DeviceAllocationModeSliced),
		State:       device.PreflightStateOK,
		Depth:       device.PreflightDepthSimulated,
	}

	first, second, err := p.simulateCoTenants(ctx, manufacturer, injector, grp, accel)
	if err != nil {
		row.State = device.PreflightStateUnavailable
		row.Depth = device.PreflightDepthDeclared
		row.Reason = err.Error()
		return row
	}

	const produced = "the allocator produced two independent slice injections for this accelerator"

	probe, known := sliceProbes[manufacturer]
	if !known {
		row.Detail = produced + "; no container probe has been established for " + manufacturer +
			", so the two slices were not started together"
		return row
	}

	image, err := ResolveProbeImage(manufacturer, grp.Family, grp.RuntimeVersion, p.probeImage)
	if err != nil {
		row.Detail = produced + "; they were not started together because " + err.Error()
		return row
	}

	for _, injection := range []*deviceplugin.ContainerAllocateResponse{first, second} {
		for k, v := range probe.LogEnv {
			injection.Envs[k] = v
		}
	}

	runs, err := p.runTogether(ctx, image, probe, staged, accel.ID, first, second)
	row.Command = togetherCommand(runs[0], runs[1])
	switch {
	case err != nil:
		// Containers that could not be started say nothing about whether two slices coexist, so this
		// is not the state that exits non-zero. See measureAccelerator for the same distinction.
		row.State = device.PreflightStateOK
		row.Detail = produced + "; the two co-tenant containers could not both be started, so " +
			"nothing was measured: " + err.Error()
		if p.runtime != nil && p.runtime.NetworkWarning != "" {
			row.Detail += ". " + p.runtime.NetworkWarning
		}
		row.Evidence = string(runs[0].Output) + string(runs[1].Output)
		return row
	case runs[0].Emitted:
		row.Detail = produced + "; the container step was emitted rather than run because " +
			runs[0].Reason
		if p.dryRun {
			// Only on a dry run, which is the one path that reaches here without having staged.
			row.Detail += ". The command mounts " +
				filepath.Join(deviceplugin.OperatorLibDir, manufacturer) +
				", which a dry run deliberately does not write: stage it, or re-run without --dry-run"
		}
		return row
	}

	row.Evidence = "tenant 1:\n" + string(runs[0].Output) + "\ntenant 2:\n" + string(runs[1].Output)

	// Both containers have to have seen the other still running. Two one-shot containers started at
	// the same moment do not necessarily overlap -- the first can be finished before the runtime has
	// created the second -- and without an overlap there is no co-tenancy to have measured, however
	// well each of them ran on its own.
	met := bytes.Contains(runs[0].Output, []byte(coTenantsMet)) &&
		bytes.Contains(runs[1].Output, []byte(coTenantsMet))

	firstQuota, firstAbsent := memoryQuota(first, probe, p.host.root)
	secondQuota, secondAbsent := memoryQuota(second, probe, p.host.root)
	switch {
	case runs[0].ExitError != "" || runs[1].ExitError != "":
		// A co-tenant that printed the barrier marker and its cap and then died still printed both,
		// so every clause below would find what it looks for and call this measured. It is not: an
		// accelerator whose tenants do not survive holding it is not one two slices coexist on.
		// judgeProbeOutput makes the same call for a single container, for the same reason.
		row.State = device.PreflightStateUnavailable
		row.Depth = device.PreflightDepthMeasured
		row.Reason = "both slice injections started, and a container then exited non-zero under " +
			"them, so this accelerator was not observed holding two slices that both stayed " +
			"alive -- " + coTenantExits(runs)
	case !met:
		row.Detail = "both slices were started and each ran, but neither waited out the other at " +
			"the barrier, so they were not observed holding this accelerator at the same time; " +
			"the containers' own output is carried as evidence"
	case firstQuota == "" || secondQuota == "":
		// The same call judgeProbeOutput makes for a single container: this manufacturer's probe
		// names a carrier for the cap and the allocator was asked for one, so a slice with no
		// readable cap is a slice that bounds nothing -- and two of those on one accelerator is not
		// co-tenancy, it is two containers sharing a card with no quota between them.
		row.State = device.PreflightStateUnavailable
		row.Depth = device.PreflightDepthMeasured
		row.Reason = "two slices ran together on this accelerator and at least one of them bounds " +
			"nothing this run could read -- " + cmp.Or(firstAbsent, secondAbsent)
	case !reportsFigure(readerSection(runs[0].Output), firstQuota) ||
		!reportsFigure(readerSection(runs[1].Output), secondQuota):
		row.Detail = "two slices ran together on this accelerator, capped at " + firstQuota +
			" and " + secondQuota + ", and at least one of them did not report its cap back, so the " +
			"two coexist and each holding its own quota was not observed; the containers' own " +
			"output is carried as evidence"
	default:
		row.Depth = device.PreflightDepthMeasured
		row.Detail = "two slices ran together on this accelerator, each reporting its own cap (" +
			firstQuota + " and " + secondQuota + ") rather than the whole accelerator"
	}
	return row
}

// readerSection is the part of one probe's output the vendor reader produced, which is the only part
// a reported cap may be read out of -- for the same reason judgeProbeOutput reads it there.
func readerSection(out []byte) string {
	_, readerOut := probeSections(string(out))
	return readerOut
}

// barrierComponent is one accelerator's directory name under the barrier root.
//
// Hashed, because an accelerator ID is whatever the vendor driver returned and nothing validates it
// -- Ascend's carry spaces, measured on hardware -- so joining one into a path unchecked lets a
// driver string decide where this writes, and the RemoveAll beside it decides what gets deleted:
// filepath.Join resolves "..", so an ID of "../../../etc" escapes the preflight tree entirely.
//
// Hashed rather than escaped, because an escape has to be injective to keep two accelerators in two
// directories -- the whole point of the per-accelerator split -- and a hash is that by construction
// while an escape has to be argued.
func barrierComponent(acceleratorID string) string {
	return stringx.SumByFNV64a(acceleratorID)
}

// tenantName is what one co-tenant calls itself at the barrier: an ordinal, so the two probes'
// commands differ only where they have to and a reader can tell which row came from which.
func tenantName(i int) string { return "tenant-" + strconv.Itoa(i+1) }

// coTenantExits names each co-tenant that exited non-zero, and with what.
//
// Both are named where both died: a pair that dies together points at the accelerator or the
// injection, and one that loses a single tenant points at what the two do to each other. A reader
// cannot tell those apart from a message that only says one container failed.
func coTenantExits(runs []emitResult) string {
	failed := make([]string, 0, len(runs))
	for i := range runs {
		if runs[i].ExitError != "" {
			failed = append(failed, tenantName(i)+": "+runs[i].ExitError)
		}
	}
	return strings.Join(failed, "; ")
}

// runTogether starts one container per injection at the same time, and returns what each produced.
//
// At the same time, and not one after another: these containers are one-shot, so a second started
// after the first has exited would measure two slices that never met. Co-tenancy is the claim that
// they can hold the accelerator at once, and only an overlap establishes it.
func (p *Preflighter) runTogether(
	ctx context.Context, image string, probe sliceProbe, staged StageResult, acceleratorID string,
	injections ...*deviceplugin.ContainerAllocateResponse,
) ([]emitResult, error) {
	results := make([]emitResult, len(injections))
	errs := make([]error, len(injections))
	force := forceEmitReason(p.dryRun, staged)

	// The directory the two probes signal each other through, mounted writable into both. It lives
	// under this run's own pod directory, so the sweep that removes what a responder rendered
	// removes it too -- and a dry run, which writes nothing, only names it in the command.
	//
	// One directory per accelerator, emptied before its tenants start. A marker in it is the only
	// evidence the two overlapped, and a tenant reports the overlap the instant it sees its peer's --
	// so a marker left by the accelerator before this one, or by a run killed before its sweep, is an
	// overlap that never happened, believed immediately by a container that has met no one.
	hostBarrier := filepath.Join(deviceplugin.OperatorPreflightDir,
		string(deviceplugin.PreflightPodUID), "barrier", barrierComponent(acceleratorID))
	if force == "" {
		dir := filepath.Join(p.host.root, hostBarrier)
		if err := os.RemoveAll(dir); err != nil {
			return results, fmt.Errorf("clear the co-tenancy barrier directory: %w", err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return results, fmt.Errorf("create the co-tenancy barrier directory: %w", err)
		}
	}
	for i := range injections {
		injections[i].Mounts = append(injections[i].Mounts, &deviceplugin.Mount{
			ContainerPath: coTenancyBarrierDir,
			HostPath:      hostBarrier,
		})
	}

	var wg sync.WaitGroup
	for i := range injections {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = emitOrRun(ctx, p.host, p.runtime, p.noRuntime, force, containerRunSpec{
				Image:     image,
				Injection: injections[i],
				Runtime:   probe.Runtime,
				Label:     preflightLabel,
				Args: coTenantProbeShellCommand(probe.Reader, logEnvNames(probe),
					tenantName(i), tenantName((i+1)%len(injections))),
			})
		}(i)
	}
	wg.Wait()

	return results, errors.Join(errs...)
}

// togetherCommand prints the two container steps as one fragment that reproduces what was run.
//
// The two are backgrounded and waited on rather than listed one after the other, because running
// them in sequence is a different experiment: two slices that each had the accelerator to
// themselves, which is the very thing co-tenancy is not about.
func togetherCommand(first, second emitResult) string {
	return first.Command + " &\n" + second.Command + "\nwait"
}

// simulateSidecarPair drives the owner allocation and then the sidecar's, against one responder and
// in the order the kubelet makes them.
//
// The sidecar is handed the owner's allocated map verbatim, because that is what a visibility
// Allocate hands the responder: ResourceServer.allocateVisibility selects no device of its own, it
// reuses the reservation the owner container already holds. Driving it any other way would answer a
// question the node is never asked.
func (p *Preflighter) simulateSidecarPair(
	ctx context.Context,
	manufacturer string,
	injector deviceplugin.AcceleratorInjectionPreflighter,
	grp *workercore.DevicesGroup,
	accel *workercore.Accelerator,
) (owner, sidecar *deviceplugin.ContainerAllocateResponse, err error) {
	alloc, err := newCoAllocation(manufacturer, grp, accel, preflightSidecarContainer)
	if err != nil {
		return nil, nil, err
	}
	// A visibility container asks for the device-only resource and for no accelerator unit of its
	// own, which is what tells the allocation apart from the owner's in the Pod the kubelet sees.
	if name := nodefeature.GetAcceleratableResourceName(
		manufacturer, workercore.DeviceAllocationModeVisibility); name != "" {
		alloc.Second.Resources.Limits = core.ResourceList{
			name: *resource.NewQuantity(int64(len(alloc.Allocated)), resource.DecimalSI),
		}
	}

	pair, err := p.driveResponder(manufacturer, injector,
		func(open responderOpener) ([]*deviceplugin.ContainerAllocateResponse, error) {
			// Two responders, because on a node the kubelet's two Allocate calls reach two servers
			// and a response depends on the mode of the server answering it. The owner holds a slice
			// and is rendered the way a slice is rendered; the sidecar's visibility Allocate goes
			// through GetContainerAllocateResponse on a visibility server, which is what makes it
			// answer with plain device visibility rather than taking the slicing path.
			slicedSrv, err := open(workercore.DeviceAllocationModeSliced)
			if err != nil {
				return nil, err
			}
			own, _, err := slicedResponse(ctx, slicedSrv, alloc.Pod, alloc.Owner, alloc.Devs, alloc.Allocated, nil)
			if err != nil {
				return nil, fmt.Errorf("drive the owner allocation: %w", err)
			}

			visibilitySrv, err := open(workercore.DeviceAllocationModeVisibility)
			if err != nil {
				return nil, err
			}
			side, err := visibilitySrv.GetContainerAllocateResponse(
				ctx, alloc.Pod, alloc.Second, alloc.Devs, alloc.Allocated)
			if err != nil {
				return nil, fmt.Errorf("drive the sidecar allocation: %w", err)
			}
			return []*deviceplugin.ContainerAllocateResponse{own, side}, nil
		})
	if err != nil {
		return nil, nil, err
	}
	return pair[0], pair[1], nil
}

// simulateCoTenants drives two independent slice allocations for one accelerator, against one
// responder, as two containers of one Pod.
//
// Two containers rather than two Pods because the synthetic Pod's identity is fixed -- two calls
// with the same inputs return the same request by design -- so the container is the only axis a
// second tenant can differ on. It is the axis that matters here anyway: a responder that renders
// per-tenant host state keys it by Pod UID and container name, so two containers is what puts two
// tenants' artifacts in two places.
func (p *Preflighter) simulateCoTenants(
	ctx context.Context,
	manufacturer string,
	injector deviceplugin.AcceleratorInjectionPreflighter,
	grp *workercore.DevicesGroup,
	accel *workercore.Accelerator,
) (first, second *deviceplugin.ContainerAllocateResponse, err error) {
	alloc, err := newCoAllocation(manufacturer, grp, accel, preflightCoTenantContainer)
	if err != nil {
		return nil, nil, err
	}
	// A co-tenant asks for exactly what the owner asked for: the two are independent slices of the
	// same size, and together they are the whole accelerator.
	alloc.Second.Resources = alloc.Owner.Resources

	pair, err := p.driveResponder(manufacturer, injector,
		func(open responderOpener) ([]*deviceplugin.ContainerAllocateResponse, error) {
			r, err := open(workercore.DeviceAllocationModeSliced)
			if err != nil {
				return nil, err
			}
			// The second tenant is placed around the first. Placing both against an empty occupancy
			// hands them the same geometry, and two containers holding the same slice of one
			// accelerator demonstrate nothing about two slices sharing it.
			one, held, err := slicedResponse(ctx, r, alloc.Pod, alloc.Owner, alloc.Devs, alloc.Allocated, nil) //nolint:govet
			if err != nil {
				return nil, fmt.Errorf("drive the first tenant's allocation: %w", err)
			}
			two, _, err := slicedResponse(ctx, r, alloc.Pod, alloc.Second, alloc.Devs, alloc.Allocated,
				mergePlacements(nil, held))
			if err != nil {
				return nil, fmt.Errorf("drive the second tenant's allocation: %w", err)
			}
			return []*deviceplugin.ContainerAllocateResponse{one, two}, nil
		})
	if err != nil {
		return nil, nil, err
	}
	return pair[0], pair[1], nil
}

// coAllocation is the synthetic two-container request both behaviors drive: the Pod shape they are
// about, with the second container present from the start.
//
// From the start, because that is what the kubelet sees and what the visibility resolution reads --
// it finds the owner by looking at the Pod's other containers, so a Pod that grew its second
// container between the two allocations would answer a question no node asks.
type coAllocation struct {
	// Pod is the synthetic Pod, carrying both containers.
	Pod *core.Pod
	// Owner is the container holding the accelerator: the first allocation, and the one the second
	// co-allocates from.
	Owner *core.Container
	// Second is the container allocated after it -- the visibility sidecar, or the co-tenant.
	Second *core.Container
	// Devs is the ledger snapshot both allocations are answered over.
	Devs *workercore.Devices
	// Allocated is what the owner was granted, and what a visibility allocation reuses verbatim.
	Allocated map[deviceplugin.Resource]int32
}

// newCoAllocation builds one accelerator's synthetic allocation request and adds the second
// container to its Pod.
//
// One accelerator per request, for the same reason measureSliced does it: handing the responder the whole
// group would grant every accelerator of it to one container and answer a node-wide allocation
// rather than a slice.
func newCoAllocation(
	manufacturer string,
	grp *workercore.DevicesGroup,
	accel *workercore.Accelerator,
	secondName string,
) (*coAllocation, error) {
	one := *grp
	one.Accelerators = []workercore.Accelerator{*accel}

	pod, _, devs, allocated, err := deviceplugin.NewPreflightAllocationRequest(
		[]workercore.DevicesGroup{one}, manufacturer, workercore.DeviceAllocationModeSliced, sliceQuota)
	if err != nil {
		return nil, fmt.Errorf("build the allocation request: %w", err)
	}

	// Both containers are named for the accelerator, for the reason probeContainerName gives: two
	// cards whose containers share a name render their per-container artifacts over one path.
	pod.Spec.Containers[0].Name = probeContainerName(pod.Spec.Containers[0].Name, accel.ID)
	pod.Spec.Containers = append(pod.Spec.Containers,
		core.Container{Name: probeContainerName(secondName, accel.ID)})
	// Both taken after the append: appending may move the backing array, and the container pointer
	// the request returned would then address a container this Pod no longer carries.
	return &coAllocation{
		Pod:       pod,
		Owner:     &pod.Spec.Containers[0],
		Second:    &pod.Spec.Containers[1],
		Devs:      devs,
		Allocated: allocated,
	}, nil
}

// grantedDevices names, in a stable order, the accelerators an injection lets its container see.
//
// Both carriers are read because manufacturers do not agree on one: most name a visibility variable
// the vendor runtime's hook resolves, and the rest pass device nodes directly. Reading only one
// would compare two injections on a field neither of them uses and call them equal.
func grantedDevices(injection *deviceplugin.ContainerAllocateResponse) []string {
	var names []string
	for k, v := range injection.GetEnvs() {
		if strings.HasSuffix(k, visibleDevicesEnvSuffix) {
			names = append(names, k+"="+v)
		}
	}
	for _, d := range injection.GetDevices() {
		names = append(names, d.GetHostPath())
	}
	slices.Sort(names)
	return names
}

// describeDevices renders what grantedDevices returned into a row's own words, and says so in words
// when it returned nothing -- an empty list printed into a sentence reads as a device named "".
func describeDevices(names []string) string {
	if len(names) == 0 {
		return "no device at all"
	}
	return strings.Join(names, ", ")
}
