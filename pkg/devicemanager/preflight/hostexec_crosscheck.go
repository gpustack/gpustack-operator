package preflight

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// hostVendorCLI is how one manufacturer's own tool is asked, on the host, what it can see.
type hostVendorCLI struct {
	name string
	args []string
	// match is the substring that marks one accelerator in the CLI's output, and accelerators are
	// counted by the lines carrying it.
	//
	// Counting bare lines instead would count the header rules these tools draw, which is worse
	// than it sounds: an over-count on a host with no hardware, and — where the match is wrong — a
	// silent zero that withholds the remedy from the operator who most needs it.
	match string
	// mounts names what a container needs in order to see what this CLI sees. It is the remedy
	// offered when the host answers and the detect pass did not.
	mounts []string
}

// hostVendorCLIs carries only the manufacturers whose CLI output shape is established. A
// manufacturer is absent rather than guessed at: a wrong match counts zero, and a zero here reads
// as "the host sees nothing either", which is the one answer that would send an operator to debug
// the wrong layer. An absent manufacturer simply gets no cross-check and says so.
var hostVendorCLIs = map[string]hostVendorCLI{
	nodefeature.ManufacturerNVIDIA: {
		// Matched on the UUID's own prefix rather than on "UUID:", because nvidia-smi -L nests a
		// line per MIG partition under the card that carries it and both lines end in a UUID. Only
		// a physical device's is prefixed GPU-; a partition's is prefixed MIG-. Counting both would
		// report one MIG-enabled card as several accelerators, and a host that sees more than this
		// container is the shape this cross-check reads as a bring-up mistake.
		name: "nvidia-smi", args: []string{"-L"}, match: "(UUID: GPU-",
		mounts: []string{
			"/dev/nvidiactl", "/dev/nvidia<N> for each accelerator",
			"the NVIDIA container runtime, or the toolkit libraries it would have injected",
		},
	},
	nodefeature.ManufacturerAscend: {
		name: "npu-smi", args: []string{"info", "-l"}, match: "NPU ID",
		mounts: []string{
			"/dev/davinci_manager", "/dev/devmm_svm", "/dev/hisi_hdc",
			"/dev/davinci<N> for each accelerator", "/usr/local/Ascend/driver",
		},
	},
	nodefeature.ManufacturerAMD: {
		name: "rocm-smi", args: []string{"--showuniqueid"}, match: "Unique ID:",
		// The libraries are named alongside the device nodes because AMD, unlike the two above, has
		// no container runtime that injects them: measured on a host with both cards visible and
		// /dev bind-mounted whole, the detect pass still found nothing, because librocm_smi64.so,
		// libamd_smi.so and libhsa-runtime64.so.1 were not in the container. A remedy listing only
		// the device nodes sends the reader to fix what was never broken.
		mounts: []string{
			"/dev/kfd", "/dev/dri",
			"the ROCm user-space libraries (/opt/rocm), which nothing injects for you",
		},
	},
}

// crossCheckHost asks the host's own vendor CLI what it can see, and folds the answer into the
// detection this pass made from inside the container.
//
// It is the one thing `detect` cannot do for an operator: from inside a container with no device
// mounts, "this machine has no accelerators" and "this machine has eight you cannot reach" are the
// same sentence. Entering the host root separates them, because the host's own CLI answers with no
// /dev mount in this container at all.
func crossCheckHost(
	ctx context.Context, host *hostExec, manufacturer string, detection *device.PreflightDetection,
) {
	cli, known := hostVendorCLIs[manufacturer]
	if !known {
		return
	}

	// Rendered through the same shell-safe join the container steps use, because this field carries
	// the same promise they do: what it prints is what a reader pastes into a shell. Everything in
	// it but the host root is a constant of this package, and the host root is an operator's own
	// path -- one carrying a space would otherwise print a chroot command that runs somewhere else,
	// or nowhere, while the pass itself succeeded through argv and reported it as runnable.
	view := &device.PreflightHostView{
		Command: shellQuoteJoin(host.Command(cli.name, cli.args...)),
	}
	detection.Host = view

	if err := host.Validate(); err != nil {
		view.Reason = err.Error()
		return
	}
	if !host.Has(ctx, cli.name) {
		view.Reason = "the host carries no " + cli.name + ", so it cannot be asked what it sees"
		return
	}

	out, err := host.Run(ctx, cli.name, cli.args...)
	if err != nil {
		view.Reason = err.Error()
		return
	}

	view.Detail = strings.TrimSpace(string(out))
	view.Accelerators = countMatchingLines(view.Detail, cli.match)

	// The discrepancy that matters runs one way only. The host seeing fewer than this container is
	// not a bring-up mistake — the CLI may list differently, or not be installed on a host whose
	// driver is fine — while the host seeing hardware this container found none of is the mistake
	// nearly every first run makes.
	//
	// Partial visibility is the same mistake and is counted the same way. A container that reached
	// one of eight cards is not a node with one card: seven allocations that the scheduling chain
	// will offer cannot be served, and reporting the pass as ok because the count was not zero
	// hides exactly the case a per-device mount list gets wrong.
	if view.Accelerators <= detection.Accelerators {
		return
	}
	view.MissingMounts = absentHere(cli.mounts)

	// A detection that already failed keeps the reason it failed for. Its count is zero because the
	// detect pass could not answer at all, not because this container looked and saw nothing, so the
	// host seeing more is not evidence of anything here -- and overwriting the reason would send a
	// reader to add mounts when the thing to fix is the driver read that never completed. The host
	// view is still attached above, so what the host sees is on the record either way.
	if detection.State == device.PreflightStateUnavailable {
		return
	}

	// Said on the detection itself and not only in the host view, because the exit code is what
	// automation reads, and a node whose accelerators this container cannot reach is a node that
	// cannot serve them.
	detection.State = device.PreflightStateUnavailable
	detection.Reason = fmt.Sprintf(
		"the host's own %s reports %d accelerator(s) where this container detected %d, so what is "+
			"missing is this container's access to them rather than the hardware",
		cli.name, view.Accelerators, detection.Accelerators)
}

// countMatchingLines counts the lines of out that carry match.
func countMatchingLines(out, match string) int {
	var n int
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, match) {
			n++
		}
	}
	return n
}

// absentHere keeps only the entries this container really lacks.
//
// The list is what a manufacturer needs, and a reader takes it as what to go and mount. Measured on
// hardware, that is two different things: a run with /dev bind-mounted whole still found no
// accelerator -- the libraries were what was missing -- and the row nonetheless told its reader to
// mount two device nodes that were already there. Sending someone to fix what is not broken costs
// them the one entry that was.
//
// Only path-shaped entries can be checked, and only from inside this container, which is where the
// question lives: the entries are what THIS container is missing, not what the host lacks. Anything
// that is not a path -- the sentence naming a vendor container runtime, say -- is always kept,
// because nothing here can establish whether it is in force.
func absentHere(mounts []string) []string {
	var absent []string
	for _, mount := range mounts {
		if strings.HasPrefix(mount, "/") && !strings.Contains(mount, " ") {
			if _, err := os.Stat(mount); err == nil {
				continue
			}
		}
		absent = append(absent, mount)
	}
	return absent
}
