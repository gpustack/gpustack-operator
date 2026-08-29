package preflight

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os/exec"
	"slices"
	"strings"

	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// containerRunSpec is what one container preflight step needs in order to run: an image, the
// injection that image is started with, and the command run inside it once it is up.
//
// T7 (probeimage*.go, stage*.go) resolves Image against the accelerator family the detect pass
// reported, and stages the on-host tree Injection's mounts point at. The manufacturer verticals
// (T4/T5) build Injection by driving the allocator's own ContainerAllocateResponder, so this type
// never fabricates one -- it only carries what those two produce into one command.
type containerRunSpec struct {
	// Image is the container image to run.
	Image string
	// Injection carries the envs, mounts, devices and annotations rendered into the run invocation.
	//
	// Its mount host paths MUST be the tree T7 stages onto the host, never a simulated pass's
	// scratch directory (deviceplugin.NewPreflightRedirect / RedirectHostWrites): that directory is
	// removed the moment the simulated pass restores, so a command built over it would describe a
	// container that no longer exists by the time anyone -- human or the xbuild skill this is a
	// contract with -- reads it. This type takes Injection as a plain value and cannot enforce that
	// on its own; a caller assembling it from a simulated responder must copy it onto the staged
	// paths first. Documented loudly here because nothing else stops the mistake.
	Injection *deviceplugin.ContainerAllocateResponse
	// Runtime is the OCI runtime the container is started with, empty where the manufacturer needs
	// none.
	//
	// It is not the host CLI -- that is resolved separately -- but the vendor runtime that CLI hands
	// the container to. Two manufacturers need one: NVIDIA's injection names devices through
	// NVIDIA_VISIBLE_DEVICES and relies on the vendor runtime's hook to bring the driver in, and
	// Ascend's does the same through ASCEND_VISIBLE_DEVICES. Without it the container gets device
	// nodes and no user-space driver, and the slicing runtime fails to initialize against a library
	// that is not there -- which would be reported as a slice that does not work on a node where it
	// does.
	Runtime string
	// Label is the "key=value" every container this command starts carries, so a container left
	// behind by a killed run can be found and removed by the next one.
	Label string
	// Args is the command run inside the container, after the image.
	Args []string
}

// emitResult is what one container preflight step produced: either what running it observed, or the
// command that would have run it, for the two cases F10 names as the fallback and for the case
// where a caller asked to see it rather than run it.
type emitResult struct {
	// Command is the full command line the step runs, or would run, joined and shell-quoted so it
	// survives being pasted into a shell and run exactly as printed.
	Command string
	// Emitted is true when Command was printed instead of executed. It is never a failure of the
	// step -- see Failed in preflight.go, which does not read an emitted row as one.
	Emitted bool
	// Reason says why the step was emitted, set only when Emitted is true.
	Reason string
	// Output is the container's own standard output, set only when the step actually ran.
	Output []byte
	// ExitError carries the failure of a container that started and then exited non-zero, which is
	// a different answer from one that never started and must not be collapsed into it.
	//
	// A container that could not be pulled or started says nothing about slicing -- an air-gapped
	// node is not a node that cannot slice -- while a probe whose process died under the injected
	// runtime is the very failure this step exists to catch. Told apart by whether the probe script
	// reached its first line rather than by an exit status, because the status is the container
	// CLI's own convention and each one has a different one.
	ExitError string
}

// buildContainerRunArgv builds the argv that runs spec as a one-shot, auto-removed container under
// the runtime named rtName, pointed at address and namespace where the runtime has them.
//
// This is the single construction behind every container preflight step. emitOrRun calls it exactly
// once and both of its outcomes -- the argv actually executed and the command line printed -- are
// built from that one call, so an executed step and a printed one can never describe two different
// commands.
//
// address is the containerd socket the runtime was resolved against, and it is passed rather than
// left to the runtime's own default because a k3s or RKE2 node carries its socket at neither path a
// containerd CLI looks in.
//
// Everything read out of a map is sorted on the way in. Go randomizes map iteration, so an unsorted
// argv reorders itself between two runs over one injection -- and this argv is printed as the command
// an operator reruns, and compared against in tests.
func buildContainerRunArgv(rtName, address, namespace string, spec containerRunSpec) []string {
	argv := []string{rtName}
	if address != "" {
		argv = append(argv, "--address", address)
	}
	if namespace != "" {
		argv = append(argv, "--namespace", namespace)
	}
	argv = append(argv, "run", "--rm")
	argv = append(argv, vendorRuntimeArgs(rtName, spec.Runtime)...)
	if spec.Label != "" {
		argv = append(argv, "--label", spec.Label)
	}

	injection := spec.Injection
	if injection == nil {
		injection = &deviceplugin.ContainerAllocateResponse{}
	}

	for _, k := range slices.Sorted(maps.Keys(injection.GetEnvs())) {
		argv = append(argv, "-e", k+"="+injection.GetEnvs()[k])
	}

	mounts := append([]*deviceplugin.Mount(nil), injection.GetMounts()...)
	slices.SortFunc(mounts, func(a, b *deviceplugin.Mount) int {
		return strings.Compare(a.GetContainerPath(), b.GetContainerPath())
	})
	for _, m := range mounts {
		v := m.GetHostPath() + ":" + m.GetContainerPath()
		if m.GetReadOnly() {
			v += ":ro"
		}
		argv = append(argv, "-v", v)
	}

	devices := append([]*deviceplugin.DeviceSpec(nil), injection.GetDevices()...)
	slices.SortFunc(devices, func(a, b *deviceplugin.DeviceSpec) int {
		return strings.Compare(a.GetContainerPath(), b.GetContainerPath())
	})
	for _, d := range devices {
		v := d.GetHostPath() + ":" + d.GetContainerPath()
		if d.GetPermissions() != "" {
			v += ":" + d.GetPermissions()
		}
		argv = append(argv, "--device", v)
	}

	for _, k := range slices.Sorted(maps.Keys(injection.GetAnnotations())) {
		argv = append(argv, "--annotation", k+"="+injection.GetAnnotations()[k])
	}

	argv = append(argv, spec.Image)
	argv = append(argv, spec.Args...)
	return argv
}

// dryRunReason is what a dry run gives emitOrRun as its forceReason.
const dryRunReason = "this is a dry run"

// forceEmitReason says why a step is printed rather than taken before the host is even consulted,
// or "" where nothing forces it.
//
// Staging is in here with the dry run because the two leave the host in the same state as far as
// this step is concerned -- the tree the command mounts is not there -- and what an operator needs
// in both cases is the same: the command that would have run, so they can stage the tree by hand
// and take the step themselves. Skipping the command instead left the row saying a preparation
// failed and nothing about what it was for.
func forceEmitReason(dryRun bool, staged StageResult) string {
	switch {
	case dryRun:
		return dryRunReason
	case staged.Failed:
		return "the libraries the command mounts could not be staged onto the host: " + staged.Reason
	}
	return ""
}

// probeRuntimeFor returns the runtime a container preflight step is run -- or printed -- with, and,
// when that is not the runtime that was resolved, the reason the step is emitted instead of taken.
//
// ctr is the one resolved runtime that cannot take the step. containerd's own flag set for `ctr run`
// offers --env, --mount, --annotation and --privileged and nothing at all that passes a device node,
// so the only way to give a probe container an accelerator through ctr is --privileged -- which
// hands over every device on the host. A probe started that way would report an isolation the
// injection never established: a measured answer that measured the wrong thing. Emitting is the
// honest outcome, and it is why ctr's inability is a fallback here rather than a failure.
//
// ctr stays a resolved runtime for everything that does not start a container, and the command
// printed for it names nerdctl -- the containerd CLI that does speak devices -- carrying the socket
// and namespace ctr resolved, so it is runnable as printed against the same daemon. That holds only
// where nerdctl can also carry this manufacturer's vendor runtime; where it cannot, the clause below
// moves the fallback on to docker, and the reason names ctr, which is what was actually resolved.
func probeRuntimeFor(rt *hostRuntime, vendorRuntime string) (name, emitReason string) {
	name = rt.Name
	if name == "ctr" {
		name = "nerdctl"
		emitReason = "the resolved container runtime is ctr, which has no flag that passes a device " +
			"node to a container; the command above takes the step with nerdctl against the same containerd"
		if rt.NerdctlAbsent {
			// ctr is resolved only where nerdctl was looked for and missing, so the command written
			// for nerdctl cannot run here as printed. Saying otherwise sends an operator to run a
			// command whose first word this host does not have.
			emitReason += " -- which this host does not carry, since that is why ctr was resolved: " +
				"install nerdctl to take the step, or take it from a host that has one"
		}
	}
	if _, ok := vendorRuntimeFlags[[2]string{name, vendorRuntime}]; vendorRuntime != "" && !ok {
		// docker, not name: the reason says the printed command is written for docker, and it has
		// to be one. Printing the resolved CLI here produced a command in the dialect that cannot
		// pass the vendor runtime, missing the very flag it was emitted for the want of -- nerdctl
		// + Ascend printed a bare `nerdctl run` with no --runtime ascend anywhere in it. Measured
		// on an Ascend host, ctr reached the same command by this clause being skipped for it.
		return "docker", "the resolved container runtime is " + rt.Name + ", which has no way to hand a " +
			"container to the " + vendorRuntime + " runtime this manufacturer's injection relies on for " +
			"its user-space driver; the command above is written for docker, which does"
	}
	return name, emitReason
}

// vendorRuntimeFlags translates a manufacturer's vendor runtime into the flag the host CLI takes for
// it, keyed by (host CLI, vendor runtime).
//
// A vendor runtime is not one flag. `--runtime` on docker names an entry in the daemon's own
// configuration, which is where nvidia-container-runtime and the Ascend runtime install themselves;
// `--runtime` on nerdctl names an OCI shim, and measured on hardware `nerdctl run --runtime nvidia`
// dies with "invalid runtime name nvidia, correct runtime name should be either format like
// io.containerd.runc.v1". nerdctl's own door to the same hook is `--gpus`, measured working on the
// same host and the same image.
//
// A pair absent from this table is a CLI that cannot take the step for that manufacturer, and
// probeRuntimeFor emits rather than guessing at a flag. That is the same refusal ctr gets, for the
// same reason: a command that cannot run as printed is worse than one that says it cannot.
var vendorRuntimeFlags = map[[2]string][]string{
	{"docker", "nvidia"}:  {"--runtime", "nvidia"},
	{"docker", "ascend"}:  {"--runtime", "ascend"},
	{"nerdctl", "nvidia"}: {"--gpus", "all"},
}

// vendorRuntimeArgs returns the flags that hand a probe container to vendorRuntime under rtName, and
// nothing at all where the manufacturer needs no vendor runtime.
func vendorRuntimeArgs(rtName, vendorRuntime string) []string {
	if vendorRuntime == "" {
		return nil
	}
	return vendorRuntimeFlags[[2]string{rtName, vendorRuntime}]
}

// emitOrRun builds spec's command exactly once and either runs it as the host through host, or emits
// it as text -- the fallback F10 names, and the explicit preview F10 also names.
//
// It emits instead of running when force is true (an explicit request to preview rather than let the
// step run), when rt is nil (ResolveRuntime found no container runtime on the host), when the
// resolved runtime cannot start a probe (see probeRuntimeFor), or when host carries no usable host
// root (Validate fails). None of the four is a failure of the step: the caller reports the row as
// emitted, and Failed in preflight.go does not count one as a failure.
//
// The returned error is set only for an actual execution failure -- the container ran and exited
// non-zero, or could not be started -- never for a fallback, which emitResult.Reason names instead.
// noRuntime, when rt is nil, is what the resolution said when it found nothing. It is carried rather
// than reconstructed here because the two are not the same fact: a host where every probe came up
// empty and a host whose kubelet names a runtime nothing here can drive both arrive as a nil rt, and
// telling the second it "carries no docker, nerdctl, ctr" sends its reader to install what is
// already there. Empty falls back to naming the probe order, which is honest when nothing else is
// known.
// forceReason, when non-empty, says why the step is printed rather than taken regardless of what the
// host could have done -- a dry run, or a preparation this pass declined to make. It is a reason
// rather than a flag because it is what the row ends up saying, and the two callers have different
// ones to give.
func emitOrRun(
	ctx context.Context, host *hostExec, rt *hostRuntime, noRuntime, forceReason string,
	spec containerRunSpec,
) (emitResult, error) {
	rtName, address, namespace := hostRuntimes[0], "", ""
	var warning, cannotRun string
	if rt != nil {
		address, namespace, warning = rt.Socket, rt.Namespace, rt.NetworkWarning
		rtName, cannotRun = probeRuntimeFor(rt, spec.Runtime)

		// The fallback can change the CLI, and a containerd socket and namespace mean nothing to
		// one that is not a containerd CLI -- docker reads its own daemon's configuration and
		// rejects --address outright. Carrying them across the switch would print a command that
		// cannot run as printed, which is the one thing an emitted step must not do.
		if !slices.Contains(containerdRuntimes, rtName) {
			address, namespace = "", ""
		}
	}

	argv := buildContainerRunArgv(rtName, address, namespace, spec)
	printed := shellQuoteJoin(host.Command(argv[0], argv[1:]...))
	if warning != "" {
		printed = "# " + warning + "\n" + printed
	}

	// Established before force is consulted, because a dry run can also be a fallback and the two
	// are different facts about the same row. Measured on a k3s node carrying no nerdctl: the row
	// said only "this is a dry run" while the command it printed had quietly fallen back to docker's
	// dialect, so a reader would have taken it for the command this node's kubelet would run.
	var fallback string
	switch {
	case rt == nil:
		if noRuntime == "" {
			noRuntime = "no container runtime was found on the host: probed " + strings.Join(hostRuntimes, ", ")
		}
		fallback = noRuntime + "; the command above assumes " + rtName
	case cannotRun != "":
		fallback = cannotRun
	}

	switch {
	case forceReason != "":
		reason := forceReason
		if fallback != "" {
			reason += ", and it would have been emitted regardless: " + fallback
		}
		return emitResult{Command: printed, Emitted: true, Reason: reason}, nil
	case fallback != "":
		return emitResult{Command: printed, Emitted: true, Reason: fallback}, nil
	}

	if err := host.Validate(); err != nil {
		// The nil is the contract, not an oversight: a host root that cannot be entered is one of the
		// two cases F10 names as the fallback, so the step is emitted and the row says so.
		return emitResult{Command: printed, Emitted: true, Reason: err.Error()}, nil //nolint:nilerr
	}

	out, err := host.Run(ctx, argv[0], argv[1:]...)
	res := emitResult{Command: printed, Output: out}
	if err == nil {
		return res, nil
	}
	if containerRan(rtName, out, err) {
		res.ExitError = err.Error()
		return res, nil
	}
	return res, err
}

// runtimeRefusals is the exit status each runtime uses for refusing to run a container at all, as
// against one passed through from whatever ran inside it.
//
// Measured on hardware, because the two do not agree and neither is documented as a contract:
// docker answers 125 for an image it could not pull and for a flag it would not take, while nerdctl
// answers 1 for both of those and for a container it could not create. Both pass a container
// process's own status through unchanged.
//
// ctr is absent because ctr never runs a probe: it has no flag that hands a device node to a
// container, so its step is always emitted instead. Only these two ever reach here.
var runtimeRefusals = map[string]int{
	"docker":  125,
	"nerdctl": 1,
}

// containerRan says whether a failed container command got as far as running a container, which
// decides whether its failure is an observation about this node or a limit of the environment
// probing it. An observation is reported; a limit falls back to emitting the step.
//
// The probe script opens by printing its marker, so output carrying one is the strongest evidence
// there is: that container ran, whatever it exited with afterwards. But the marker cannot be the
// only evidence, and assuming it was is the defect this exists to close. The injected preload
// library loads into every process in the container, the shell included, so a shim that aborts as
// it is loaded kills the probe before the script's first line. Measured with a library whose
// constructor calls abort: exit 139, no stdout, no stderr -- indistinguishable by output alone from
// an image that could not be pulled, and precisely the failure this command exists to catch. Read
// as "the container could not be started", a broken injected runtime is reported as a simulated
// pass on a node where slicing does not work.
//
// So a marker-less failure is read as a container that ran unless the status is the one that
// runtime keeps for itself. That direction is deliberate. The statuses overlap -- docker answers
// 127 both for a device path it would not take and for a command the image does not carry -- and
// of the two ways to be wrong, calling an environment limit a failure is visible in the reason it
// carries, while calling a broken shim a pass is the answer this whole command exists to prevent.
func containerRan(runtime string, out []byte, err error) bool {
	if bytes.Contains(out, []byte(mapsBegin)) {
		return true
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		// No status to read at all -- the command could not be launched on the host, which is the
		// environment rather than the node under test.
		return false
	}

	refusal, known := runtimeRefusals[runtime]
	if !known {
		// A runtime whose refusal status has not been established here. Nothing is claimed about
		// what its statuses mean, so the marker stays the only evidence, as it was before.
		return false
	}
	return exitErr.ExitCode() != refusal
}

// shellQuoteJoin joins argv into one shell-safe line: any element carrying whitespace or a shell
// metacharacter is single-quoted, so an env value with a space, a comma or a quote in it does not
// split the command a reader pastes into a shell.
func shellQuoteJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// shellSafe is every character that needs none of the quoting below.
const shellSafe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:=@"

// shellQuote returns s verbatim when it carries nothing a shell would treat specially, and in
// single-quoted POSIX form otherwise -- the form that survives a space, a comma, a double quote or a
// shell metacharacter without splitting or expanding.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, shellSafe) == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
