package preflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// kubeletConfigSources are the places this node's kubelet configuration is looked for, in order,
// relative to the host root. The first place that names the setting being read answers.
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
//
// flags marks the file carrying the kubelet's command line rather than its YAML configuration,
// which is what decides which of a setting's two spellings appears in it.
//
// dropInDir marks a source naming the kubelet's drop-in directory rather than a file in it. Such a
// source is walked, for the reason kubeletDropInFiles gives: the files that decide a node's policy
// are usually not the directory's direct children.
var kubeletConfigSources = []struct {
	pattern   string
	flags     bool
	dropInDir bool
}{
	{pattern: "var/lib/kubelet/kubeadm-flags.env", flags: true},
	{pattern: "var/lib/kubelet/config.yaml"},
	{pattern: "var/lib/rancher/*/agent/etc/kubelet.conf.d", dropInDir: true},
}

// kubeletDropInExtension is the suffix a kubelet merges out of its drop-in directory. It ignores
// every other file there, so reading one would report a value the kubelet never applied.
const kubeletDropInExtension = ".conf"

// kubeletDropInFiles lists the files a kubelet pointed at dir merges, in the order it merges them.
//
// Walked rather than globbed because the kubelet walks. It reads every .conf below the directory,
// subdirectories included, and the subdirectories are where an administrator's own configuration
// lands: a distribution embedding the kubelet does not let callers write into this tree, because it
// regenerates it on every start, and instead offers a flag naming a directory of the caller's own,
// whose contents it copies into a subdirectory here. A pattern matching only the direct children
// therefore sees the distribution's generated defaults and never the settings that override them,
// which is the shape that reports a node as having no policy while its kubelet is running one.
//
// The walk is also what keeps the order right. Later files override earlier ones, and the kubelet's
// order is the traversal order of this same standard-library walk rather than a sort of the names,
// so walking reproduces it exactly instead of approximating it.
func kubeletDropInFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != kubeletDropInExtension {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// kubeletSetting names one kubelet setting in both the spellings its configuration uses: the flag
// on the kubelet's command line, and the field in its YAML. A reading needs both, because which
// one appears depends on the file it is read from rather than on the setting.
type kubeletSetting struct{ flag, field string }

// key returns the spelling this setting takes in a source of the given form.
func (s kubeletSetting) key(flags bool) string {
	if flags {
		return s.flag
	}
	return s.field
}

var (
	// criEndpointSetting is the runtime this node's kubelet talks to.
	criEndpointSetting = kubeletSetting{
		flag:  "--container-runtime-endpoint=",
		field: "containerRuntimeEndpoint:",
	}
	// topologyPolicySetting is the placement guarantee this node's kubelet demands before it
	// admits a container asking for NUMA-affine devices.
	topologyPolicySetting = kubeletSetting{
		flag:  "--topology-manager-policy=",
		field: "topologyManagerPolicy:",
	}
)

// kubeletReading is what one pass over this node's kubelet configuration established about one
// setting. At most one of its outcomes holds, and what each one means is the caller's to decide:
// the CRI read refuses a host it cannot interpret, because it drives what preflight measures; the
// topology read degrades to unknown and says why, because it drives nothing.
type kubeletReading struct {
	// Value is the setting's value and Found reports whether a source named one. A reading with
	// no error and no conflict and Found false means every source was searched and none carries
	// the setting, which is "this host has nothing to say" rather than an answer.
	Value string
	Found bool
	// UnreadablePath names a configuration file that matched a pattern but could not be read, and
	// UnreadableErr says why. A match that cannot be read may be the one that decides, so it ends
	// the reading rather than being skipped: skipping it would answer as though every file had
	// been searched.
	UnreadablePath string
	UnreadableErr  error
	// UnsearchablePattern names a source whose search could not run or could not finish, and
	// UnsearchableErr says why. It is kept apart from the unreadable pair because no configuration
	// file was opened: reporting it as a configuration that could not be read sends whoever is
	// diagnosing it looking for a permissions problem on a file that was never reached.
	UnsearchablePattern string
	UnsearchableErr     error
	// Conflict carries the differing values when two of the configurations ConflictPattern
	// matches name the setting differently. Two distribution trees on one machine are two
	// configurations and only one belongs to the kubelet that is running, so neither value is
	// taken.
	Conflict        []string
	ConflictPattern string
}

// readKubeletSetting reads one setting out of this node's kubelet configuration under root.
//
// Read from the kubelet's own files rather than from its process, because this container shares no
// PID namespace with the host and the files are reachable through the mounted host root either way.
// None of the places read is universal -- a distribution is free to keep its configuration
// elsewhere -- so finding nothing is an outcome of its own rather than a failure.
//
// Every reading of a kubelet setting goes through here, so that two readings cannot drift apart
// about where a kubelet keeps its configuration, what a repeated setting means, or when two
// configurations are a conflict rather than an override. Adding a setting is a kubeletSetting, not a
// second reader.
func readKubeletSetting(root string, setting kubeletSetting) kubeletReading {
	for _, src := range kubeletConfigSources {
		matches, globErr := filepath.Glob(filepath.Join(root, src.pattern))
		if globErr != nil {
			// The patterns are constants, so this needs a root that is itself a malformed
			// pattern. Continuing would leave the caller reporting that every source was
			// searched and none carried the setting, which would be false: this one was never
			// looked at.
			return kubeletReading{
				UnsearchablePattern: filepath.Join(root, src.pattern),
				UnsearchableErr:     globErr,
			}
		}

		// Grouped by match, because a match is one configuration and the pattern's matches are
		// several: every file a drop-in directory holds is merged into a single configuration, the
		// later overriding the earlier, while separate distribution trees are separate
		// configurations and a difference between them is a conflict rather than an override.
		//
		// Grouping by the directory a file sits in would look equivalent and is not. A drop-in
		// directory's subdirectories belong to the same configuration as its root -- the kubelet
		// merges the whole tree in one pass -- so splitting on directory turns an override into a
		// conflict and reports a well-configured node as one whose setting cannot be established.
		byUnit := map[string]string{}
		var units []string
		for _, match := range matches {
			files := []string{match}
			if src.dropInDir {
				walked, walkErr := kubeletDropInFiles(match)
				if walkErr != nil {
					// The walk covers one configuration, so a tree it could not finish leaves
					// this source half-searched. Reported as unsearchable rather than carried on
					// from, because continuing would answer as though the whole tree had been
					// read and the files it could not reach may be the ones that decide.
					return kubeletReading{
						UnsearchablePattern: match,
						UnsearchableErr:     walkErr,
					}
				}
				files = walked
			}
			for _, path := range files {
				body, readErr := os.ReadFile(path)
				if readErr != nil {
					return kubeletReading{UnreadablePath: path, UnreadableErr: readErr}
				}
				value, ok := valueAfter(string(body), setting.key(src.flags))
				if !ok {
					continue
				}
				if _, seen := byUnit[match]; !seen {
					units = append(units, match)
				}
				byUnit[match] = value
			}
		}

		var answers []string
		for _, unit := range units {
			if !slices.Contains(answers, byUnit[unit]) {
				answers = append(answers, byUnit[unit])
			}
		}
		switch len(answers) {
		case 0:
			continue
		case 1:
			return kubeletReading{Value: answers[0], Found: true}
		default:
			return kubeletReading{Conflict: answers, ConflictPattern: src.pattern}
		}
	}
	return kubeletReading{}
}

// kubeletCRIEndpoint returns the CRI endpoint this node's kubelet is configured against.
//
// The caller treats "not found" as "this host has nothing to say", not as an error.
//
// It errors, rather than choosing, when two configurations under one source disagree. Two distribution
// trees on one machine are two configurations and only one belongs to the kubelet that is running;
// taking either would drive preflight against a socket that node's workloads may never touch, and
// would do it silently. Saying so instead drops the affected steps to being emitted, which is the
// answer a node nothing here can read already gets.
func (h *hostExec) kubeletCRIEndpoint() (endpoint string, found bool, err error) {
	reading := readKubeletSetting(h.root, criEndpointSetting)
	switch {
	case reading.UnsearchableErr != nil:
		// The same answer an unreadable file gets, worded for what happened: the search itself
		// could not run, which takes a host root that is a malformed pattern rather than a file
		// anything failed to open.
		return "", false, fmt.Errorf(
			"the kubelet configuration search under %s could not run, so which runtime its "+
				"kubelet talks to cannot be established: %w",
			reading.UnsearchablePattern, reading.UnsearchableErr)
	case reading.UnreadableErr != nil:
		// The same answer a conflict gets, for the same reason. These patterns name the kubelet's
		// own configuration paths rather than a general tree, so a match that cannot be read may
		// be the one that decides -- and skipping it falls through to the probe order, which picks
		// a runtime by what is installed rather than by what this node's kubelet talks to. On a
		// node whose kubelet uses containerd and which also carries docker, that silently measures
		// a runtime no workload here uses.
		return "", false, fmt.Errorf(
			"this host carries a kubelet configuration at %s that could not be read, so "+
				"which runtime its kubelet talks to cannot be established: %w",
			reading.UnreadablePath, reading.UnreadableErr)
	case reading.Conflict != nil:
		return "", false, fmt.Errorf(
			"this host names more than one kubelet CRI endpoint under %s (%s), and only one of them "+
				"belongs to the kubelet that is running: name the runtime with --runtime",
			reading.ConflictPattern, strings.Join(reading.Conflict, ", "))
	default:
		return reading.Value, reading.Found, nil
	}
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
