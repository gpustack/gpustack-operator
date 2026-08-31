package preflight

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// fakeHostRoot builds a directory that passes the host-root check, optionally carrying extra files
// -- a containerd socket, say -- named relative to the root.
func fakeHostRoot(t *testing.T, extra ...string) string {
	t.Helper()

	root := t.TempDir()
	for _, marker := range hostRootMarkers {
		require.NoError(t, os.MkdirAll(filepath.Join(root, marker), 0o755))
	}
	for _, name := range extra {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	}
	return root
}

// scriptedHost returns a host exec whose commands are answered from a table keyed by the full argv,
// so a test states what the host answers rather than what our code did to ask.
func scriptedHost(root string, answers map[string]string) (*hostExec, *[]string) {
	var asked []string
	h := newHostExec(root)
	h.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		// The chroot and the root it enters are stripped, so a case states what the *host* was
		// asked rather than restating how this package enters it -- which the test above already
		// pins on its own.
		if name != chrootPath || len(args) < 2 || args[0] != root {
			return nil, errors.New("not invoked as the host: " + name)
		}
		argv := strings.Join(args[1:], " ")
		asked = append(asked, argv)

		out, ok := answers[argv]
		if !ok {
			return nil, errors.New("command not found")
		}
		return []byte(out), nil
	}
	return h, &asked
}

// A path that merely exists is not a host root, and the difference decides whether every later step
// runs or is emitted for the caller. A refusal therefore has to say what it looked for -- a hardened
// or SELinux-enforcing host that refused the mount arrives here as an empty directory, which is not
// the same mistake as passing the wrong path.
func TestHostExec_Validate(t *testing.T) {
	populated := fakeHostRoot(t)
	empty := t.TempDir()

	file := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	testCases := []struct {
		name     string
		root     string
		wantErr  bool
		wantSays string
	}{
		{name: "a mounted host root is accepted", root: populated},
		{name: "no host root configured is refused", wantErr: true, wantSays: "no host root"},
		{
			name: "a path that does not exist is refused by name",
			root: filepath.Join(populated, "nope"), wantErr: true, wantSays: "not readable",
		},
		{
			name: "a file is refused as not a directory",
			root: file, wantErr: true, wantSays: "not a directory",
		},
		{
			// The case a refused bind mount produces, and the one worth naming: the directory is
			// there and carries nothing.
			name: "an empty directory names the marker it lacks",
			root: empty, wantErr: true, wantSays: hostRootMarkers[0],
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := newHostExec(tc.root).Validate()

			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSays)
		})
	}
}

// chroot is in /usr/sbin on the distributions this runs on, which is not on a non-login shell's
// PATH -- so resolving it by name reports a tool that is present as missing. The absolute path is
// pinned here because the failure it prevents looks exactly like a broken host.
func TestHostExec_CommandUsesAbsoluteChroot(t *testing.T) {
	argv := newHostExec("/host").Command("nvidia-smi", "-L")

	require.NotEmpty(t, argv)
	assert.Equal(t, "/usr/sbin/chroot", argv[0], "chroot is not on PATH; it must be addressed absolutely")
	assert.Equal(t, []string{"/usr/sbin/chroot", "/host", "nvidia-smi", "-L"}, argv)
}

// A failed command's error is carried verbatim into the row that reports it, so what it ends with is
// what an operator reads. A command that failed silently has no stderr to append, and appending it
// anyway left rows ending in "exit status 7: " -- which reads as output that went missing.
func TestRunCommand_ErrorCarriesStderrOnlyWhenThereIsSome(t *testing.T) {
	testCases := []struct {
		name   string
		script string
		want   string
	}{
		{
			name:   "a command that said why ends with what it said",
			script: "echo 'could not select device driver' >&2; exit 7",
			want:   "exit status 7: could not select device driver",
		},
		{
			name:   "a command that said nothing ends with the status",
			script: "exit 7",
			want:   "exit status 7",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCommand(context.Background(), "sh", "-c", tc.script)

			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
		})
	}
}

// The probe order is a preference, and --runtime overrides it -- including with a name the host
// does not carry, because the no-runtime path is the one that falls back to emitting the command
// and it has to stay exercisable.
func TestHostExec_ResolveRuntime(t *testing.T) {
	const (
		hasDocker  = "sh -c command -v docker"
		hasNerdctl = "sh -c command -v nerdctl"
		hasCtr     = "sh -c command -v ctr"
	)

	testCases := []struct {
		name            string
		present         map[string]string
		want            string
		wantName        string
		wantErr         bool
		wantSocket      string
		wantNamespace   string
		wantNerdctlGone bool
	}{
		{
			name:     "docker wins the probe",
			present:  map[string]string{hasDocker: "/usr/bin/docker", hasNerdctl: "/usr/bin/nerdctl"},
			wantName: "docker",
		},
		{
			// nerdctl is a containerd CLI too, so it is addressed exactly as ctr is: its own default
			// socket is not the one a k3s node carries, and its own default namespace belongs to
			// whichever component already owns it.
			name:          "nerdctl is taken when there is no docker, and is told where its daemon is",
			present:       map[string]string{hasNerdctl: "/usr/bin/nerdctl", hasCtr: "/usr/bin/ctr"},
			wantName:      "nerdctl",
			wantSocket:    "/run/k3s/containerd/containerd.sock",
			wantNamespace: containerdNamespace,
		},
		{
			// A k3s or RKE2 node carries its socket at neither default, so resolution has to find
			// it rather than assume one -- and it names the namespace, so a container this command
			// started is never looked for in the wrong place.
			name:            "ctr is taken last and is told where its daemon is",
			present:         map[string]string{hasCtr: "/usr/bin/ctr"},
			wantName:        "ctr",
			wantSocket:      "/run/k3s/containerd/containerd.sock",
			wantNamespace:   containerdNamespace,
			wantNerdctlGone: true,
		},
		{
			name:          "--runtime overrides the preference",
			present:       map[string]string{hasDocker: "/usr/bin/docker", hasNerdctl: "/usr/bin/nerdctl"},
			want:          "nerdctl",
			wantName:      "nerdctl",
			wantSocket:    "/run/k3s/containerd/containerd.sock",
			wantNamespace: containerdNamespace,
		},
		{
			// Present on the host and still refused: carrying the name is not what makes something
			// a runtime. Every argument built for a probe is docker's dialect, so an executable
			// that merely answers to the name would be handed `run --rm --label ...` and have
			// whatever it printed judged as evidence about this node's accelerators.
			name:    "--runtime naming a present executable that is not a runtime is refused",
			present: map[string]string{"sh -c command -v true": "/usr/bin/true", hasDocker: "/usr/bin/docker"},
			want:    "true",
			wantErr: true,
		},
		{
			// A ctr step is printed as a nerdctl command, so whether the host carries one decides
			// whether what is printed can be run. The auto-resolved ctr paths record it; named
			// explicitly it used to go unrecorded, and the one host where it matters was handed a
			// command naming an executable it does not have.
			name:            "--runtime=ctr on a host with no nerdctl records that nerdctl is absent",
			present:         map[string]string{"sh -c command -v ctr": "/usr/bin/ctr"},
			want:            "ctr",
			wantName:        "ctr",
			wantSocket:      "/run/k3s/containerd/containerd.sock",
			wantNamespace:   containerdNamespace,
			wantNerdctlGone: true,
		},
		{
			name:    "--runtime naming an absent runtime is refused, not silently probed past",
			present: map[string]string{hasDocker: "/usr/bin/docker"},
			want:    "podman",
			wantErr: true,
		},
		{
			name:    "a host with no runtime at all is a named outcome",
			present: map[string]string{},
			wantErr: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := scriptedHost(fakeHostRoot(t, "run/k3s/containerd/containerd.sock"), tc.present)

			rt, err := h.ResolveRuntime(context.Background(), tc.want)

			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, errNoHostRuntime,
					"no runtime is an answer with its own sentinel, not an anonymous failure")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantNerdctlGone, rt.NerdctlAbsent,
				"whether the printed nerdctl command can be run here")
			assert.Equal(t, tc.wantName, rt.Name)
			assert.Equal(t, tc.wantSocket, rt.Socket, "socket")
			assert.Equal(t, tc.wantNamespace, rt.Namespace, "namespace")
		})
	}
}

// The mismatch is determined when the runtime is resolved, not when a pull fails, because the
// failure it replaces is a DNS timeout against a loopback address that names neither the cause nor
// the fix. Unknown is its own answer: a host root mounted without procfs cannot be compared, and
// reporting a mismatch nobody established would be a wrong answer of the same kind.
func TestHostExec_NetworkNamespace(t *testing.T) {
	t.Run("a host root with no procfs leaves it unknown, and unknown is not a mismatch", func(t *testing.T) {
		h, _ := scriptedHost(fakeHostRoot(t), map[string]string{
			"sh -c command -v docker": "/usr/bin/docker",
		})

		shared, known := h.networkNamespaceShared()
		assert.False(t, known, "nothing to compare against")
		assert.False(t, shared)

		rt, err := h.ResolveRuntime(context.Background(), "")
		require.NoError(t, err)
		assert.Empty(t, rt.NetworkWarning,
			"a namespace nobody could compare must not be reported as one that differs")
	})

	t.Run("a namespace that differs is named at resolution, not at the pull that would fail", func(t *testing.T) {
		if _, err := os.Readlink("/proc/self/ns/net"); err != nil {
			t.Skip("no network namespace link on this platform")
		}

		root := fakeHostRoot(t)
		require.NoError(t, os.MkdirAll(filepath.Join(root, "proc/1/ns"), 0o755))
		require.NoError(t, os.Symlink("net:[4026531999]", filepath.Join(root, "proc/1/ns/net")))

		h, _ := scriptedHost(root, map[string]string{"sh -c command -v docker": "/usr/bin/docker"})

		rt, err := h.ResolveRuntime(context.Background(), "")

		require.NoError(t, err, "a mismatch is a warning, not a refusal: every step that does not pull still works")
		assert.Contains(t, rt.NetworkWarning, "host networking",
			"the warning names the fix, which the DNS timeout it replaces does not")
	})

	t.Run("our own namespace matches itself", func(t *testing.T) {
		ours, err := os.Readlink("/proc/self/ns/net")
		if err != nil {
			t.Skip("no network namespace link on this platform")
		}

		root := fakeHostRoot(t)
		require.NoError(t, os.MkdirAll(filepath.Join(root, "proc/1/ns"), 0o755))
		require.NoError(t, os.Symlink(ours, filepath.Join(root, "proc/1/ns/net")))

		shared, known := newHostExec(root).networkNamespaceShared()

		assert.True(t, known)
		assert.True(t, shared)
	})
}

// The cross-check is the one thing detect cannot do for an operator: from inside a container with
// no device mounts, "this machine has none" and "this machine has eight you cannot reach" are the
// same sentence. The remedy is offered in one direction only, and only when it is the remedy.
func TestCrossCheckHost(t *testing.T) {
	const nvidiaCmd = "nvidia-smi -L"

	twoGPUs := "GPU 0: NVIDIA A100 (UUID: GPU-aaa)\nGPU 1: NVIDIA A100 (UUID: GPU-bbb)"

	testCases := []struct {
		name              string
		manufacturer      string
		detected          int
		detectedState     device.PreflightState
		detectedReason    string
		answers           map[string]string
		wantNoView        bool
		wantAccelerators  int
		wantMounts        bool
		wantReasonSays    string
		wantState         device.PreflightState
		wantDetectionSays string
	}{
		{
			name:              "the host sees what this container could not, and the mounts are named",
			manufacturer:      nodefeature.ManufacturerNVIDIA,
			answers:           map[string]string{"sh -c command -v nvidia-smi": "/usr/bin/nvidia-smi", nvidiaCmd: twoGPUs},
			wantAccelerators:  2,
			wantMounts:        true,
			wantState:         device.PreflightStateUnavailable,
			wantDetectionSays: "reports 2 accelerator(s) where this container detected 0",
		},
		{
			// Both agree, so there is nothing to remedy -- offering mounts here would send an
			// operator to fix a container that is already correct.
			name:             "the host and this container agree, so no mounts are named",
			manufacturer:     nodefeature.ManufacturerNVIDIA,
			detected:         2,
			answers:          map[string]string{"sh -c command -v nvidia-smi": "/usr/bin/nvidia-smi", nvidiaCmd: twoGPUs},
			wantAccelerators: 2,
		},
		{
			// The one direction that is not a bring-up mistake: a host whose CLI is not installed
			// says nothing about a container that detected hardware perfectly well.
			name:           "the host sees fewer than this container, which is not a missing mount",
			manufacturer:   nodefeature.ManufacturerNVIDIA,
			detected:       2,
			answers:        map[string]string{},
			wantReasonSays: "carries no nvidia-smi",
		},
		{
			// Partial visibility is the same bring-up mistake as total, and the count being
			// non-zero is what used to hide it: a container that reached one of two cards was
			// reported ok, and the scheduling chain would then offer an allocation for a card no
			// workload on this node can be given.
			name:              "the host sees more than this container, which is the same missing mount",
			manufacturer:      nodefeature.ManufacturerNVIDIA,
			detected:          1,
			answers:           map[string]string{"sh -c command -v nvidia-smi": "/usr/bin/nvidia-smi", nvidiaCmd: twoGPUs},
			wantAccelerators:  2,
			wantMounts:        true,
			wantState:         device.PreflightStateUnavailable,
			wantDetectionSays: "reports 2 accelerator(s) where this container detected 1",
		},
		{
			// A detect pass that could not answer at all names zero accelerators for a reason of
			// its own, so the host seeing more is not evidence about mounts. Overwriting the reason
			// sends a reader to add mounts when the driver read is the thing that failed.
			name:              "a detection that already failed keeps the reason it failed for",
			manufacturer:      nodefeature.ManufacturerNVIDIA,
			detectedState:     device.PreflightStateUnavailable,
			detectedReason:    detectionUnmeasured,
			answers:           map[string]string{"sh -c command -v nvidia-smi": "/usr/bin/nvidia-smi", nvidiaCmd: twoGPUs},
			wantAccelerators:  2,
			wantMounts:        true,
			wantState:         device.PreflightStateUnavailable,
			wantDetectionSays: "could not measure this manufacturer",
		},
		{
			name:         "a manufacturer whose CLI output shape is unestablished gets no view at all",
			manufacturer: nodefeature.ManufacturerMThreads,
			wantNoView:   true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := scriptedHost(fakeHostRoot(t), tc.answers)
			detection := device.PreflightDetection{
				Accelerators: tc.detected, State: tc.detectedState, Reason: tc.detectedReason,
			}

			crossCheckHost(context.Background(), h, tc.manufacturer, &detection)

			if tc.wantNoView {
				assert.Nil(t, detection.Host, "a guessed count is worse than no cross-check")
				return
			}
			require.NotNil(t, detection.Host)
			assert.Contains(t, detection.Host.Command, "/usr/sbin/chroot",
				"the command is reported so a reader can run it themselves")
			assert.Equal(t, tc.wantAccelerators, detection.Host.Accelerators, "accelerators")

			if tc.wantReasonSays != "" {
				assert.Contains(t, detection.Host.Reason, tc.wantReasonSays)
			}
			// The exit code is what automation reads, so a cross-check that found hardware this
			// container cannot reach has to say it on the detection and not only in the host view.
			assert.Equal(t, tc.wantState, detection.State, "the state the cross-check left behind")
			if tc.wantDetectionSays != "" {
				assert.Contains(t, detection.Reason, tc.wantDetectionSays)
			}
			if !tc.wantMounts {
				assert.Empty(t, detection.Host.MissingMounts, "mounts are named only where they are the remedy")
				return
			}
			assert.NotEmpty(t, detection.Host.MissingMounts)
		})
	}
}

// The reported command promises to be runnable as printed, and the host root inside it is an
// operator's own path rather than a constant of this package. Joined on spaces alone, a root
// carrying one prints a chroot command that enters somewhere else -- while the pass itself
// succeeded, because execution goes through argv and never through a shell. The container steps
// already render through shellQuoteJoin; this is the same field with the same promise.
func TestCrossCheckHost_CommandIsRunnableAsPrinted(t *testing.T) {
	testCases := []struct {
		name        string
		rootDir     string
		wantCommand string
	}{
		{
			// The overwhelmingly common shape, pinned so the quoting cannot start rewriting it.
			name:        "an ordinary root is printed verbatim",
			rootDir:     "host",
			wantCommand: "nvidia-smi -L",
		},
		{
			name:        "a root carrying a space is quoted, not split",
			rootDir:     "host root",
			wantCommand: "nvidia-smi -L",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), tc.rootDir)
			for _, marker := range hostRootMarkers {
				require.NoError(t, os.MkdirAll(filepath.Join(root, marker), 0o755))
			}
			h, _ := scriptedHost(root, nil)

			detection := device.PreflightDetection{}
			crossCheckHost(context.Background(), h, nodefeature.ManufacturerNVIDIA, &detection)
			require.NotNil(t, detection.Host)

			assert.Equal(t, chrootPath+" "+shellQuote(root)+" "+tc.wantCommand, detection.Host.Command)
			// The property the quoting exists for: a reader's shell splits the printed line into the
			// same argv this pass executed, root included.
			assert.Equal(t, h.Command("nvidia-smi", "-L"), splitShellWords(t, detection.Host.Command),
				"what a shell would parse must be the argv that ran")
		})
	}
}

// splitShellWords parses a printed command the way a POSIX shell would: whitespace separates words
// except inside single quotes, where '\” closes, escapes and reopens. It is the reader this test
// asserts against, so "runnable as printed" is checked rather than assumed from the quoting.
func splitShellWords(t *testing.T, line string) []string {
	t.Helper()

	var (
		words   []string
		cur     strings.Builder
		inQuote bool
		started bool
	)
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\'':
			inQuote = !inQuote
			started = true
		case c == '\\' && !inQuote && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
			started = true
		case c == ' ' && !inQuote:
			if started {
				words = append(words, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	require.False(t, inQuote, "the printed command left a quote open")
	if started {
		words = append(words, cur.String())
	}
	return words
}

// Counting bare lines would count the header rules these tools draw, so what marks an accelerator
// is per CLI. A wrong match counts zero, and a zero reads as "the host sees nothing either" -- the
// one answer that sends an operator to debug the wrong layer.
func TestCountMatchingLines(t *testing.T) {
	testCases := []struct {
		name  string
		out   string
		match string
		want  int
	}{
		{
			name:  "nvidia-smi lists one accelerator per line",
			out:   "GPU 0: NVIDIA A100 (UUID: GPU-aaa)\nGPU 1: NVIDIA A100 (UUID: GPU-bbb)",
			match: hostVendorCLIs[nodefeature.ManufacturerNVIDIA].match, want: 2,
		},
		{
			// Copied from docs/operation/nvidia-mig.md. A partition is not a second card, and
			// counting it as one makes a single MIG-enabled GPU look like a host/container
			// discrepancy on a node where nothing is wrong. Only the physical line's UUID is
			// prefixed GPU-; a partition's is prefixed MIG-.
			name: "a mig partition is not a second accelerator",
			out: "GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-950792bf-a01c-3f1a-e122-3473e67f54b2)\n" +
				"  MIG 3g.40gb     Device  0: (UUID: MIG-b3061c09-2a4c-5026-a575-79f86a5bb12c)",
			match: hostVendorCLIs[nodefeature.ManufacturerNVIDIA].match, want: 1,
		},
		{
			name:  "a header rule is not an accelerator",
			out:   "+--------------------+\n| NPU ID  : 0        |\n+--------------------+\n| NPU ID  : 1        |",
			match: "NPU ID", want: 2,
		},
		{name: "an empty answer counts nothing", out: "", match: "UUID:"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, countMatchingLines(tc.out, tc.match))
		})
	}
}

// The remedy has to be the remedy. Measured on hardware: a run with /dev bind-mounted whole still
// found no accelerator -- the vendor libraries were what was missing -- and the row nonetheless told
// its reader to mount two device nodes that were already there. A list that names what is not broken
// costs its reader the one entry that is.
func TestAbsentHere(t *testing.T) {
	present := filepath.Join(t.TempDir(), "present")
	require.NoError(t, os.WriteFile(present, nil, 0o600))
	absent := filepath.Join(t.TempDir(), "absent")

	testCases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "a path this container has is dropped",
			in:   []string{present, absent},
			want: []string{absent},
		},
		{
			// Not a path, so nothing here can establish whether it is in force -- and the one thing
			// worse than naming a remedy already applied is dropping the one that was not.
			name: "prose is always kept",
			in:   []string{present, "the NVIDIA container runtime, or the toolkit libraries it would have injected"},
			want: []string{"the NVIDIA container runtime, or the toolkit libraries it would have injected"},
		},
		{
			name: "a list with nothing left over answers with nothing",
			in:   []string{present},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, absentHere(tc.in))
		})
	}
}

// Mounted and still unusable is the one shape the list above cannot say, and it is the shape that
// leaves a reader with nowhere to go. Measured on a host carrying two ROCm versions: /opt/rocm was
// mounted, /opt/rocm/lib inside it linked to a directory that was not, every ROCm library failed to
// load, and the only remedy offered was to mount /opt/rocm -- which the operator had already done.
//
// So this answers from what is here rather than from what a manufacturer needs, and says nothing at
// all unless it found the one thing a mount cannot fix by being repeated.
func TestUnresolvableLibDir(t *testing.T) {
	root := t.TempDir()

	resolves := filepath.Join(root, "resolves")
	require.NoError(t, os.Mkdir(resolves, 0o755))

	linked := filepath.Join(root, "linked")
	require.NoError(t, os.Symlink(resolves, linked))

	target := filepath.Join(root, "not-mounted")
	dangling := filepath.Join(root, "dangling")
	require.NoError(t, os.Symlink(target, dangling))

	notALink := filepath.Join(root, "a-file")
	require.NoError(t, os.WriteFile(notALink, nil, 0o600))

	testCases := []struct {
		name string
		dir  string
		want string
	}{
		{
			// A manufacturer with no library directory established asks nothing of this.
			name: "no directory to check",
			dir:  "",
		},
		{
			name: "a directory that is here",
			dir:  resolves,
		},
		{
			// The ordinary single-version shape: the link is resolved by the host before the mount.
			name: "a link that resolves",
			dir:  linked,
		},
		{
			// Absent is the other list's answer, not this one's -- saying both would name the same
			// gap twice and read as two problems.
			name: "a path that is not here at all",
			dir:  filepath.Join(root, "absent"),
		},
		{
			name: "something here that is not a link and not a directory",
			dir:  notALink,
		},
		{
			name: "a link that does not resolve",
			dir:  dangling,
			want: dangling + " is mounted but does not resolve: it links to " + target +
				", which is not in this container -- mount " + dangling +
				" itself, which resolves the link on the host",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, unresolvableLibDir(tc.dir))
		})
	}
}

// The remedy has to be the remedy. A manufacturer's list says what its detect pass needs, and a
// reader takes it as what to go and mount -- so an entry this container already has sends them to
// fix what is not broken, and costs them the one entry that was. Measured on hardware: a run with
// /dev bind-mounted whole still found no accelerator, because the vendor libraries were what was
// missing, and the row named the two device nodes that were already there.
//
// The list under test carries one of each, so the filter running and the filter not running give
// different answers on any machine.
func TestCrossCheckHost_NamesOnlyWhatThisContainerLacks(t *testing.T) {
	present := t.TempDir()
	absent := filepath.Join(present, "not-here")
	const manufacturer = "test-manufacturer"
	const prose = "the vendor container runtime, or the libraries it would have injected"

	original, had := hostVendorCLIs[manufacturer]
	hostVendorCLIs[manufacturer] = hostVendorCLI{
		name: "vendor-smi", args: []string{"-L"}, match: "UUID:",
		mounts: []string{present, absent, prose},
	}
	t.Cleanup(func() {
		if had {
			hostVendorCLIs[manufacturer] = original
			return
		}
		delete(hostVendorCLIs, manufacturer)
	})

	root := fakeHostRoot(t)
	host, _ := scriptedHost(root, map[string]string{
		"sh -c command -v vendor-smi": "/usr/bin/vendor-smi",
		"vendor-smi -L":               "GPU 0 (UUID: GPU-abc)\n",
	})

	detection := &device.PreflightDetection{}
	crossCheckHost(context.Background(), host, manufacturer, detection)

	require.NotNil(t, detection.Host)
	assert.Equal(t, 1, detection.Host.Accelerators)
	assert.Equal(t, []string{absent, prose}, detection.Host.MissingMounts,
		"a path this container already has was named as the remedy")
}

// What starts a container on a node in production is whatever the kubelet's CRI endpoint names, and
// reproducing production is the whole point of the container step. A node carrying both docker and
// containerd but a kubelet talking to containerd would otherwise be probed docker-first, and every
// container answer would describe a path no workload on that node ever takes.
// The value is read out of a file nobody here wrote, so every shape a real one takes has to be
// survivable. A key with nothing after it is the one that matters most: it used to index the first
// field of a string with no fields, which panics and takes the whole pass down before it can report
// anything at all.
func TestValueAfter(t *testing.T) {
	const key = "containerRuntimeEndpoint:"

	testCases := []struct {
		name, body string
		want       string
		wantFound  bool
	}{
		{
			name: "a plain value, with its scheme stripped",
			body: "containerRuntimeEndpoint: unix:///run/containerd/containerd.sock\n",
			want: "/run/containerd/containerd.sock", wantFound: true,
		},
		{
			name: "the key with nothing after it is absent, not a panic",
			body: "kind: KubeletConfiguration\ncontainerRuntimeEndpoint:",
			want: "", wantFound: false,
		},
		{
			name: "the key with only whitespace after it is absent too",
			body: "containerRuntimeEndpoint:   \nkind: KubeletConfiguration\n",
			want: "", wantFound: false,
		},
		{
			// A commented key is not a setting, and a substring search cannot tell the difference.
			name: "a commented key is skipped in favour of the live one",
			body: "# containerRuntimeEndpoint: unix:///run/stale.sock\ncontainerRuntimeEndpoint: unix:///run/live.sock\n",
			want: "/run/live.sock", wantFound: true,
		},
		{
			name: "a commented key alone is absent",
			body: "# containerRuntimeEndpoint: unix:///run/stale.sock\n",
			want: "", wantFound: false,
		},
		{
			// The kubelet applies a repeated setting last-wins, so reading the first is reading a
			// value it has already been told to ignore.
			name: "the last of several settings wins",
			body: "containerRuntimeEndpoint: unix:///run/first.sock\ncontainerRuntimeEndpoint: unix:///run/second.sock\n",
			want: "/run/second.sock", wantFound: true,
		},
		{
			name: "the key is absent entirely",
			body: "kind: KubeletConfiguration\naddress: 0.0.0.0\n",
			want: "", wantFound: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := valueAfter(tc.body, key)
			assert.Equal(t, tc.wantFound, found)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The kubeadm form puts every flag on one line, so "last wins" has to hold within a line as well as
// across them.
func TestValueAfter_KubeadmFlagsForm(t *testing.T) {
	const key = "--container-runtime-endpoint="

	got, found := valueAfter(
		`KUBELET_KUBEADM_ARGS="--container-runtime-endpoint=unix:///run/first.sock `+
			`--pod-infra-container-image=x --container-runtime-endpoint=unix:///run/second.sock"`, key)

	require.True(t, found)
	assert.Equal(t, "/run/second.sock", got, "the kubelet would have applied the later flag")
}

// Two distribution trees on one machine are two configurations, and only one of them belongs to the
// kubelet that is running. Picking the lexicographically later one would drive preflight against a
// socket that node's workloads never touch, and it would do it silently.
func TestHostExec_ResolveRuntime_RefusesToGuessBetweenDistributions(t *testing.T) {
	root := fakeHostRoot(t)
	for tree, sock := range map[string]string{
		"k3s":  "unix:///run/k3s/containerd/containerd.sock",
		"rke2": "unix:///run/k3s/containerd/rke2.sock",
	} {
		path := filepath.Join(root, "var/lib/rancher", tree, "agent/etc/kubelet.conf.d/00-defaults.conf")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("containerRuntimeEndpoint: "+sock+"\n"), 0o644))
	}
	host, _ := scriptedHost(root, map[string]string{"sh -c command -v nerdctl": "/usr/bin/nerdctl"})

	_, err := host.ResolveRuntime(context.Background(), "")

	require.Error(t, err, "two trees disagreed and one of them was picked anyway")
	assert.ErrorIs(t, err, errNoHostRuntime, "the affected steps fall back to being emitted")
	assert.Contains(t, err.Error(), "rke2.sock", "the reader is not told which answers were in conflict")
	assert.Contains(t, err.Error(), "containerd.sock")
}

// A kubelet configuration that cannot be read is not a node with no kubelet. These patterns name the
// kubelet's own paths, so a match that cannot be read may be the one that decides — and skipping it
// falls through to the probe order, which picks by what is installed rather than by what this node's
// kubelet talks to. On a node carrying both docker and a containerd-talking kubelet, that measures a
// runtime no workload here uses, silently.
func TestHostExec_ResolveRuntime_AnUnreadableKubeletConfigIsNotSilentlySkipped(t *testing.T) {
	root := fakeHostRoot(t)
	// A directory where the kubelet's own config file belongs: os.ReadFile fails on it, and the
	// failure does not depend on which user runs this test.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "var/lib/kubelet/config.yaml"), 0o755))
	// docker is present and would win the probe order, which is the wrong answer to fall through to.
	host, _ := scriptedHost(root, map[string]string{"sh -c command -v docker": "/usr/bin/docker"})

	_, err := host.ResolveRuntime(context.Background(), "")

	require.Error(t, err, "an unreadable kubelet configuration fell through to probing docker")
	assert.ErrorIs(t, err, errNoHostRuntime, "the affected steps fall back to being emitted")
	assert.Contains(t, err.Error(), "could not be read")
	assert.Contains(t, err.Error(), "config.yaml", "the reader is not told which file it was")
}

// Two files in one drop-in directory are one configuration, applied in name order — that is not a
// conflict, and treating it as one would refuse a node that is perfectly well configured.
func TestHostExec_ResolveRuntime_OneTreeWithTwoDropInsIsNotAConflict(t *testing.T) {
	root := fakeHostRoot(t)
	dir := filepath.Join(root, "var/lib/rancher/k3s/agent/etc/kubelet.conf.d")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for name, sock := range map[string]string{
		"00-defaults.conf": "unix:///run/k3s/containerd/first.sock",
		"99-override.conf": "unix:///run/k3s/containerd/second.sock",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name),
			[]byte("containerRuntimeEndpoint: "+sock+"\n"), 0o644))
	}
	host, _ := scriptedHost(root, map[string]string{"sh -c command -v nerdctl": "/usr/bin/nerdctl"})

	rt, err := host.ResolveRuntime(context.Background(), "")

	require.NoError(t, err)
	assert.Equal(t, "/run/k3s/containerd/second.sock", rt.Socket, "the later drop-in overrides the earlier")
}

func TestHostExec_ResolveRuntime_FollowsTheKubelet(t *testing.T) {
	testCases := []struct {
		name        string
		kubeletFile string
		kubeletBody string
		// extra carries further kubelet files, keyed by path, for the cases where one file is not
		// the whole configuration.
		extra       map[string]string
		has         []string
		wantName    string
		wantSocket  string
		wantErrSays string
	}{
		{
			name:        "a kubeadm node's flag file decides it",
			kubeletFile: "var/lib/kubelet/kubeadm-flags.env",
			kubeletBody: `KUBELET_KUBEADM_ARGS="--container-runtime-endpoint=unix:///run/containerd/containerd.sock --pod-infra-container-image=x"`,
			has:         []string{"docker", "nerdctl"},
			wantName:    "nerdctl", wantSocket: "/run/containerd/containerd.sock",
		},
		{
			name:        "a file-configured kubelet decides it, socket and all",
			kubeletFile: "var/lib/kubelet/config.yaml",
			kubeletBody: "apiVersion: kubelet.config.k8s.io/v1beta1\ncontainerRuntimeEndpoint: unix:///run/containerd/containerd.sock\n",
			has:         []string{"docker", "nerdctl"},
			wantName:    "nerdctl", wantSocket: "/run/containerd/containerd.sock",
		},
		{
			// Measured on a k3s node: neither file above exists, and the endpoint is in a kubelet
			// drop-in under the distribution's own tree. Reading only the two would have this node
			// probed docker-first while every workload on it goes through k3s' containerd.
			name:        "a drop-in under the distribution's tree decides it",
			kubeletFile: "var/lib/rancher/k3s/agent/etc/kubelet.conf.d/00-k3s-defaults.conf",
			kubeletBody: "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\ncontainerRuntimeEndpoint: unix:///run/k3s/containerd/containerd.sock\n",
			has:         []string{"docker", "nerdctl"},
			wantName:    "nerdctl", wantSocket: "/run/k3s/containerd/containerd.sock",
		},
		{
			// A drop-in directory is applied in name order, so the later file is the node's answer.
			// Taking the first would report a socket the kubelet has already been told to ignore.
			name:        "the last drop-in to name the endpoint wins",
			kubeletFile: "var/lib/rancher/k3s/agent/etc/kubelet.conf.d/00-k3s-defaults.conf",
			kubeletBody: "containerRuntimeEndpoint: unix:///run/k3s/containerd/containerd.sock\n",
			extra: map[string]string{
				"var/lib/rancher/k3s/agent/etc/kubelet.conf.d/99-operator.conf": "containerRuntimeEndpoint: unix:///run/other/containerd.sock\n",
			},
			has:      []string{"docker", "nerdctl"},
			wantName: "nerdctl", wantSocket: "/run/other/containerd.sock",
		},
		{
			// A machine that has hosted more than one distribution can carry both, and only one of
			// them is what its kubelet reads. The standard path is taken first because a kubelet
			// that reads it is reading it whatever else is on disk, while a distribution drop-in
			// only means something to the distribution that wrote it.
			name:        "the standard path is taken over a distribution drop-in when both exist",
			kubeletFile: "var/lib/kubelet/config.yaml",
			kubeletBody: "containerRuntimeEndpoint: unix:///run/containerd/containerd.sock\n",
			extra: map[string]string{
				"var/lib/rancher/k3s/agent/etc/kubelet.conf.d/00-k3s-defaults.conf": "containerRuntimeEndpoint: unix:///run/k3s/containerd/containerd.sock\n",
			},
			has:      []string{"docker", "nerdctl"},
			wantName: "nerdctl", wantSocket: "/run/containerd/containerd.sock",
		},
		{
			// The socket the kubelet named wins over the ones that merely exist: a node with two
			// has exactly one its kubelet uses, and the other belongs to something else.
			name:        "the named socket beats a probe of the usual paths",
			kubeletFile: "var/lib/kubelet/config.yaml",
			kubeletBody: "containerRuntimeEndpoint: unix:///run/k3s/containerd/containerd.sock\n",
			has:         []string{"nerdctl"},
			wantName:    "nerdctl", wantSocket: "/run/k3s/containerd/containerd.sock",
		},
		{
			name:        "a dockershim endpoint resolves to docker, which finds its own daemon",
			kubeletFile: "var/lib/kubelet/kubeadm-flags.env",
			kubeletBody: `KUBELET_KUBEADM_ARGS="--container-runtime-endpoint=unix:///var/run/dockershim.sock"`,
			has:         []string{"docker", "nerdctl"},
			wantName:    "docker",
		},
		{
			// The bare machine this command is designed for, before a cluster exists: no kubelet,
			// so the probe order is the honest answer.
			name:     "a host with no kubelet falls back to the probe order",
			has:      []string{"nerdctl", "ctr"},
			wantName: "nerdctl",
		},
		{
			// Saying which CLI is missing and what it was for beats reporting no runtime at all.
			name:        "a kubelet whose CLI is absent names both",
			kubeletFile: "var/lib/kubelet/config.yaml",
			kubeletBody: "containerRuntimeEndpoint: unix:///run/containerd/containerd.sock\n",
			has:         []string{"docker"},
			wantErrSays: "the kubelet is configured against /run/containerd/containerd.sock",
		},
		{
			// A containerd endpoint with no nerdctl still resolves, to ctr, because ctr carries the
			// kubelet's own socket into the command every container step then emits. Failing here
			// threw that socket away and the printed command named a daemon this node does not use.
			name:        "a containerd kubelet with only ctr resolves to ctr, keeping the socket",
			kubeletFile: "var/lib/kubelet/config.yaml",
			kubeletBody: "containerRuntimeEndpoint: unix:///run/k3s/containerd/containerd.sock\n",
			has:         []string{"ctr"},
			wantName:    "ctr", wantSocket: "/run/k3s/containerd/containerd.sock",
		},
		{
			// A CRI socket is not a containerd socket. Pointing nerdctl at CRI-O's would emit or run
			// a command that cannot connect to that daemon at all, so the runtime is named
			// unsupported instead -- which is what F9 asks for.
			name:        "a cri-o kubelet is named unsupported rather than driven with nerdctl",
			kubeletFile: "var/lib/kubelet/config.yaml",
			kubeletBody: "containerRuntimeEndpoint: unix:///var/run/crio/crio.sock\n",
			has:         []string{"docker", "nerdctl", "ctr"},
			wantErrSays: "carries no client for",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := fakeHostRoot(t)
			files := map[string]string{}
			if tc.kubeletFile != "" {
				files[tc.kubeletFile] = tc.kubeletBody
			}
			maps.Copy(files, tc.extra)
			for rel, body := range files {
				path := filepath.Join(root, rel)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
			}

			answers := map[string]string{}
			for _, name := range tc.has {
				answers["sh -c command -v "+name] = "/usr/bin/" + name
			}
			host, _ := scriptedHost(root, answers)

			rt, err := host.ResolveRuntime(context.Background(), "")

			if tc.wantErrSays != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrSays)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantName, rt.Name)
			assert.Equal(t, tc.wantSocket, rt.Socket)
		})
	}
}
