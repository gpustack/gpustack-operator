package preflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultHostRoot is where the host's own root filesystem is expected to be bind-mounted.
const DefaultHostRoot = "/host"

// chrootPath is absolute on purpose. chroot ships in /usr/sbin on the distributions this runs on,
// which is not on a non-login shell's PATH, so resolving it by name finds nothing and reports a
// present tool as missing.
const chrootPath = "/usr/sbin/chroot"

// hostRootMarkers are directories every host root carries. They are checked together because any
// one of them can exist in an empty directory a caller created by mistake, and the point of the
// check is to tell a real host root from a path that merely exists.
var hostRootMarkers = []string{"etc", "proc", "usr"}

// containerdSockets are the paths a containerd socket is found at, in probe order. A k3s or RKE2
// node carries one at neither of the first two, which is why the list is not just the default.
var containerdSockets = []string{
	"/run/containerd/containerd.sock",
	"/run/k3s/containerd/containerd.sock",
	"/var/run/containerd/containerd.sock",
}

// hostRuntimes are the container runtimes probed on the host, in preference order.
var hostRuntimes = []string{"docker", "nerdctl", "ctr"}

// containerdRuntimes are the runtimes above that drive containerd, and therefore have to be told
// which socket and which namespace to work in. docker is not one of them: it has a daemon of its
// own, and reads the host's configuration to find it.
var containerdRuntimes = []string{"nerdctl", "ctr"}

// errNoHostRuntime is what a host carrying none of the runtimes above answers with. It is a named
// outcome rather than a failure of the node: the affected steps fall back to being emitted for the
// caller to run.
var errNoHostRuntime = errors.New("no container runtime found on the host")

// hostExec runs the host's own executables by entering the bind-mounted host root with chroot.
//
// This is how the command reaches the host without a hole punched for it: no runtime socket is
// mounted, no vendor CLI is shipped in our image, and every CLI parsed is the exact version that
// matches the daemon it is talking to. Only the host's executables go through it — our own binary
// stays in container context, because it is cgo and dynamically linked, and running it against an
// older host glibc fails at the loader in a way that reads like "no devices".
type hostExec struct {
	root string
	// run is the seam the tests substitute. Everything below composes an argv and reads output, so
	// substituting here is what makes the whole file testable without a host root.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func newHostExec(root string) *hostExec {
	return &hostExec{root: root, run: runCommand}
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	// A command's own stderr is what says why it failed, and most say something. One that failed
	// silently says nothing, and appending that nothing leaves the message ending in a colon with
	// no text after it -- which reads as output that went missing rather than output that was never
	// produced. Observed as "exit status 7: " in a preflight row.
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return out, fmt.Errorf("%w: %s", err, msg)
	}
	return out, err
}

// Validate reports whether the configured path is a mounted host root, naming what it looked for
// when it is not.
//
// A host that refuses the mount — SELinux enforcing without a relabel, or a hardened host that
// forbids mounting / into a container at all — arrives here as a missing or empty directory, and
// saying which marker was absent is what separates that from a caller who passed the wrong path.
func (h *hostExec) Validate() error {
	if h.root == "" {
		return errors.New("no host root configured")
	}

	info, err := os.Stat(h.root)
	if err != nil {
		return fmt.Errorf("host root %q is not readable: %w", h.root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("host root %q is not a directory", h.root)
	}

	for _, marker := range hostRootMarkers {
		if _, err := os.Stat(filepath.Join(h.root, marker)); err != nil {
			return fmt.Errorf("host root %q carries no %q, so it is not a host root: "+
				"mount the host's / into the container at this path", h.root, marker)
		}
	}
	return nil
}

// Command returns the argv that runs name with args as the host, which is also exactly what is
// printed when a step is emitted rather than taken. Building it here means the executed command and
// the printed one cannot be different commands.
func (h *hostExec) Command(name string, args ...string) []string {
	return append([]string{chrootPath, h.root, name}, args...)
}

// Run executes name with args as the host and returns its standard output.
func (h *hostExec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	argv := h.Command(name, args...)
	return h.run(ctx, argv[0], argv[1:]...)
}

// Has reports whether the host carries an executable called name.
//
// name is quoted because on one path it is the raw --runtime flag, which reaches a shell running as
// the host. Nothing unprivileged can pass that flag -- this command already runs privileged -- but
// the quoting costs a call and removes the question.
func (h *hostExec) Has(ctx context.Context, name string) bool {
	_, err := h.Run(ctx, "sh", "-c", "command -v "+shellQuote(name))
	return err == nil
}

// hostRuntime is the container runtime resolved on the host, and how to address it.
type hostRuntime struct {
	// Name is the executable, invoked as the host.
	Name string
	// Socket is the containerd socket ctr is pointed at, empty for the runtimes that find their
	// own. It is reported so that a container this command started is never looked for in the
	// wrong place.
	Socket string
	// Namespace is the containerd namespace containers are created in, empty where the runtime has
	// no such concept. It is explicit so that nothing preflight starts is collected by another
	// component that owns the default namespace.
	Namespace string
	// NetworkWarning names the network-namespace mismatch that will break any image pull this
	// runtime is asked for, and is empty when there is none or it could not be determined. It is
	// carried here rather than raised as a failure because every step that does not pull works
	// regardless, and refusing them all over a pull that may never happen would be its own wrong
	// answer.
	NetworkWarning string
	// NerdctlAbsent records that this runtime was resolved only because nerdctl is not installed
	// here. It matters because ctr cannot start a probe and the emitted command is written for
	// nerdctl instead -- against the same containerd, but with a CLI this host does not carry, so
	// the reason has to say to install it rather than presenting the command as ready to run.
	NerdctlAbsent bool
}

// networkWarning is the sentence a mismatched network namespace gets. It names the cause and the
// fix, because the failure it replaces — a DNS timeout against a loopback address — names neither.
const networkWarning = "this container does not share the host's network namespace, so a host CLI " +
	"asked to pull an image will fail to resolve names: the host's /etc/resolv.conf is read " +
	"through the chroot while nothing answers on it here. Re-run with host networking."

// ResolveRuntime picks the container runtime to drive on the host.
//
// The kubelet's own CRI endpoint decides it wherever there is a kubelet, because that is what starts
// a container on this node in production and reproducing production is the whole point. A node with
// both docker and containerd installed but a kubelet talking to containerd would otherwise be probed
// docker-first, and every container answer would describe a path no workload ever takes.
//
// A host that gives no such answer -- the bare machine this command is designed for, before a
// cluster exists, or a distribution that keeps its kubelet configuration somewhere neither file
// below covers -- falls through to the probe order, which is the honest answer there: whatever can
// start a container.
//
// want overrides both, including with a name the host does not carry: the no-runtime path has to
// stay exercisable, since it is the path that falls back to emitting the command rather than
// running it.
func (h *hostExec) ResolveRuntime(ctx context.Context, want string) (*hostRuntime, error) {
	if want != "" {
		// Checked before the host is asked whether it carries the name, because carrying it is not
		// what makes it a runtime: every argument built below is docker's dialect, so any
		// executable that happens to answer to the name would be handed `run --rm --label ...` and
		// have whatever it printed judged as probe evidence. A name this command does not drive is
		// refused rather than run.
		if !slices.Contains(hostRuntimes, want) {
			return nil, fmt.Errorf("%w: --runtime %s is not a container runtime this command "+
				"drives -- it drives %s", errNoHostRuntime, want, strings.Join(hostRuntimes, ", "))
		}
		if !h.Has(ctx, want) {
			return nil, fmt.Errorf("%w: --runtime %s is not on the host", errNoHostRuntime, want)
		}
		rt := h.describeRuntime(want, "")
		// Recorded here for the same reason the auto-resolved paths record it: a ctr step is printed
		// as a nerdctl command (probeRuntimeFor says why), so whether this host carries one decides
		// whether what is printed can be run at all. Named explicitly rather than resolved, it used
		// to go unrecorded -- and the one host where it matters, ctr present and nerdctl absent, was
		// handed a command naming an executable it does not have, with nothing saying so.
		if want == "ctr" && !h.Has(ctx, "nerdctl") {
			rt.NerdctlAbsent = true
		}
		return rt, nil
	}

	endpoint, found, err := h.kubeletCRIEndpoint()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNoHostRuntime, err)
	}
	if found {
		name, socket, known := runtimeForEndpoint(endpoint)
		if !known {
			return nil, fmt.Errorf("%w: the kubelet is configured against %s, which is a CRI this "+
				"command carries no client for -- it drives docker and containerd only",
				errNoHostRuntime, endpoint)
		}
		if h.Has(ctx, name) {
			return h.describeRuntime(name, socket), nil
		}
		// A containerd endpoint on a host with no nerdctl still resolves, to ctr, wherever ctr is
		// there. ctr cannot start a probe -- probeRuntimeFor says why, and every container step
		// falls back to being emitted -- but resolving it is what carries the kubelet's own socket
		// and namespace into that emitted command. Failing outright here instead threw away the one
		// thing the kubelet had told us, and the printed command then named a daemon this node does
		// not use.
		if name == "nerdctl" && h.Has(ctx, "ctr") {
			rt := h.describeRuntime("ctr", socket)
			rt.NerdctlAbsent = true
			return rt, nil
		}
		return nil, fmt.Errorf("%w: the kubelet is configured against %s, and this host carries no %s "+
			"to drive it with", errNoHostRuntime, endpoint, name)
	}

	for _, name := range hostRuntimes {
		if h.Has(ctx, name) {
			// ctr sits last in the order, so reaching it means nerdctl was probed and absent --
			// which the emitted command has to say, since it is written for nerdctl.
			if name == "ctr" {
				rt := h.describeRuntime(name, "")
				rt.NerdctlAbsent = true
				return rt, nil
			}
			return h.describeRuntime(name, ""), nil
		}
	}
	return nil, fmt.Errorf("%w: probed %s", errNoHostRuntime, strings.Join(hostRuntimes, ", "))
}

// kubeletCRISources are the places a kubelet's CRI endpoint is looked for, in order, relative to the
// host root. The first place that names one answers.
//
// The standard paths come before the distribution one because a machine that has hosted more than
// one distribution can carry both: a kubelet reading the standard path reads it whatever else is on
// disk, while a distribution drop-in means something only to the distribution that wrote it.
//
// The third is a glob because a distribution that embeds the kubelet configures it under its own
// tree rather than the two standard paths -- measured on a k3s node, where neither of the first two
// exists at all and the endpoint is a drop-in at
// var/lib/rancher/k3s/agent/etc/kubelet.conf.d/00-k3s-defaults.conf. Matching the distribution name
// rather than listing it keeps this from being a guess about any particular one.
var kubeletCRISources = []struct{ pattern, key string }{
	{"var/lib/kubelet/kubeadm-flags.env", "--container-runtime-endpoint="},
	{"var/lib/kubelet/config.yaml", "containerRuntimeEndpoint:"},
	{"var/lib/rancher/*/agent/etc/kubelet.conf.d/*.conf", "containerRuntimeEndpoint:"},
}

// kubeletCRIEndpoint returns the CRI endpoint this node's kubelet is configured against.
//
// Read from the kubelet's own files rather than from its process, because this container shares no
// PID namespace with the host and the files are reachable through the mounted host root either way.
// None of the places read is universal -- a distribution is free to keep it elsewhere -- and the
// caller treats "not found" as "this host has nothing to say", not as an error.
//
// It errors, rather than choosing, when two directories under one source disagree. Two distribution
// trees on one machine are two configurations and only one belongs to the kubelet that is running;
// taking either would drive preflight against a socket that node's workloads may never touch, and
// would do it silently. Saying so instead drops the affected steps to being emitted, which is the
// answer a node nothing here can read already gets.
func (h *hostExec) kubeletCRIEndpoint() (endpoint string, found bool, err error) {
	for _, src := range kubeletCRISources {
		matches, globErr := filepath.Glob(filepath.Join(h.root, src.pattern))
		if globErr != nil {
			continue
		}

		// Grouped by directory, because the two levels mean different things: files within one are a
		// single drop-in configuration applied in name order, so the later overrides the earlier;
		// separate directories are separate configurations, and a difference between them is a
		// conflict rather than an override. Glob returns sorted paths, so the within-group order is
		// already the kubelet's own.
		byDir := map[string]string{}
		var dirs []string
		for _, path := range matches {
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				// The same answer a conflict gets, for the same reason. These patterns name the
				// kubelet's own configuration paths rather than a general tree, so a match that
				// cannot be read may be the one that decides -- and skipping it falls through to
				// the probe order, which picks a runtime by what is installed rather than by what
				// this node's kubelet talks to. On a node whose kubelet uses containerd and which
				// also carries docker, that silently measures a runtime no workload here uses.
				return "", false, fmt.Errorf(
					"this host carries a kubelet configuration at %s that could not be read, so "+
						"which runtime its kubelet talks to cannot be established: %w", path, readErr)
			}
			value, ok := valueAfter(string(body), src.key)
			if !ok {
				continue
			}
			dir := filepath.Dir(path)
			if _, seen := byDir[dir]; !seen {
				dirs = append(dirs, dir)
			}
			byDir[dir] = value
		}

		var answers []string
		for _, dir := range dirs {
			if !slices.Contains(answers, byDir[dir]) {
				answers = append(answers, byDir[dir])
			}
		}
		switch len(answers) {
		case 0:
			continue
		case 1:
			return answers[0], true, nil
		default:
			return "", false, fmt.Errorf(
				"this host names more than one kubelet CRI endpoint under %s (%s), and only one of them "+
					"belongs to the kubelet that is running: name the runtime with --runtime",
				src.pattern, strings.Join(answers, ", "))
		}
	}
	return "", false, nil
}

// valueAfter returns the token following key in body, stripped of the quoting and the scheme either
// source may wrap it in.
//
// Read line by line rather than by searching the whole body, because a substring search cannot tell
// a setting from a comment mentioning it, and a stale endpoint left commented above the live one
// would then win. The last occurrence answers, within a line as well as across them: the kubelet
// applies a repeated setting last-wins, and the kubeadm form puts every flag on one line.
//
// A key with nothing after it is absent rather than a value. It is the shape that matters most: it
// is what a half-written configuration looks like, and reading a field out of a line that has none
// would panic and take the whole pass down before it could report anything.
func valueAfter(body, key string) (string, bool) {
	var (
		value string
		found bool
	)
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		i := strings.LastIndex(trimmed, key)
		if i < 0 {
			continue
		}
		fields := strings.Fields(trimmed[i+len(key):])
		if len(fields) == 0 {
			continue
		}
		token := strings.TrimPrefix(strings.Trim(fields[0], `"'`), "unix://")
		if token != "" {
			value, found = token, true
		}
	}
	return value, found
}

// runtimeForEndpoint maps a CRI endpoint onto the host CLI that speaks to it, and the socket to point
// that CLI at. known is false for a CRI this command has no CLI for.
//
// The two it recognizes are named rather than inferred from "not docker". A CRI socket is not a
// containerd socket: pointing nerdctl at CRI-O's /var/run/crio/crio.sock gives a command that cannot
// connect to that daemon at all, and reporting the runtime as unsupported is the answer F9 asks for
// -- a runtime that cannot be driven has to be named as such rather than driven wrongly.
//
// The socket comes from the endpoint rather than from probing the usual paths: a node that carries
// two containerd sockets has exactly one its kubelet uses, and the other belongs to something else.
func runtimeForEndpoint(endpoint string) (name, socket string, known bool) {
	switch {
	case strings.Contains(endpoint, "docker"):
		return "docker", "", true
	case strings.Contains(endpoint, "containerd"):
		return "nerdctl", endpoint, true
	}
	return "", "", false
}

// describeRuntime fills in how a resolved runtime is addressed. Only the containerd CLIs need
// telling where their daemon is and which namespace to work in; docker reads the host's own
// configuration, which is the whole reason it is invoked as the host rather than from a CLI we ship.
//
// nerdctl is addressed the same way ctr is, and for the same reason: it is a containerd CLI, so on a
// k3s or RKE2 node it defaults to a socket that node does not have, and to the namespace whichever
// component owns the default one is already using.
// socket, when non-empty, is the one the kubelet named, and is taken over probing the usual paths.
func (h *hostExec) describeRuntime(name, socket string) *hostRuntime {
	rt := &hostRuntime{Name: name}

	// Determined when the runtime is resolved rather than when a pull fails, which is the whole
	// point: the failure it replaces arrives as a DNS timeout naming neither the cause nor the fix.
	if shared, known := h.networkNamespaceShared(); known && !shared {
		rt.NetworkWarning = networkWarning
	}

	if !slices.Contains(containerdRuntimes, name) {
		return rt
	}

	rt.Namespace = containerdNamespace
	if socket != "" {
		rt.Socket = socket
		return rt
	}
	for _, sock := range containerdSockets {
		if _, err := os.Stat(filepath.Join(h.root, sock)); err == nil {
			rt.Socket = sock
			break
		}
	}
	return rt
}

// containerdNamespace is the containerd namespace preflight creates its containers in. It is its
// own rather than the default, so that a container this command started is never collected by
// another component that owns "default" or "k8s.io".
const containerdNamespace = "gpustack-preflight"

// networkNamespaceShared reports whether this container shares the host's network namespace, and
// whether that could be determined at all.
//
// chroot changes the root and not the network namespace, so a host CLI entered through it still
// resolves names in the container's namespace while reading the host's /etc/resolv.conf. On a host
// whose resolver is systemd-resolved that file names a loopback address which listens only in the
// host's namespace, so any host CLI asked to pull an image dies on DNS with an error naming neither
// the cause nor the fix. Detecting it up front is what turns that into a sentence.
//
// The comparison is exact rather than a network probe: a recursive bind mount of the host's root
// brings the host's procfs with it, so the host's own PID 1 names the namespace to compare against.
// A host root mounted non-recursively carries no procfs, and then this is unknown rather than false
// — reporting a mismatch nobody established would be its own wrong answer.
func (h *hostExec) networkNamespaceShared() (shared, known bool) {
	ours, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return false, false
	}
	theirs, err := os.Readlink(filepath.Join(h.root, "proc/1/ns/net"))
	if err != nil {
		return false, false
	}
	return ours == theirs, true
}
