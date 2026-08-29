package preflight

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// mustShellSplit undoes shellQuoteJoin, so a test can assert what a printed command actually
// contains rather than pattern-matching its text. It understands exactly the one quoting scheme
// shellQuote produces -- bare tokens, and single-quoted tokens with an embedded quote escaped by
// closing, backslash-quoting and reopening the quote -- which is all this package ever emits.
func mustShellSplit(t *testing.T, line string) []string {
	t.Helper()

	var argv []string
	i := 0
	for i < len(line) {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		if i >= len(line) {
			break
		}
		if line[i] != '\'' {
			j := i
			for j < len(line) && line[j] != ' ' {
				j++
			}
			argv = append(argv, line[i:j])
			i = j
			continue
		}

		var tok strings.Builder
		i++ // opening quote
		for i < len(line) {
			if line[i] == '\'' {
				if strings.HasPrefix(line[i:], `'\''`) {
					tok.WriteByte('\'')
					i += 4
					continue
				}
				i++ // closing quote
				break
			}
			tok.WriteByte(line[i])
			i++
		}
		argv = append(argv, tok.String())
	}
	return argv
}

// forceIfNot forces the emit for the cases that are not testing a refusal, so that the refusal
// cases reach the branch that declines on its own and their reason is theirs rather than a dry run's.
func forceIfNot(refusing bool) string {
	if refusing {
		return ""
	}
	return dryRunReason
}

func testSpec() containerRunSpec {
	return containerRunSpec{
		Image: "vendor/probe:cann",
		Injection: &deviceplugin.ContainerAllocateResponse{
			Envs: map[string]string{"ASCEND_VISIBLE_DEVICES": "0"},
			Mounts: []*deviceplugin.Mount{
				{ContainerPath: "/usr/local/Ascend/driver", HostPath: "/usr/local/Ascend/driver", ReadOnly: true},
			},
			Devices: []*deviceplugin.DeviceSpec{
				{ContainerPath: "/dev/davinci0", HostPath: "/dev/davinci0", Permissions: "rw"},
			},
			Annotations: map[string]string{"gpustack.ai/preflight": "true"},
		},
		Args: []string{"npu-smi", "info"},
	}
}

// The headline test: emitOrRun's run branch and its emit branch are built from the same
// buildContainerRunArgv call, so the argv the host actually executes and the command line a caller
// prints for it can never name two different invocations.
func TestEmitOrRun_EmitAndActAgree(t *testing.T) {
	root := fakeHostRoot(t)
	spec := testSpec()
	rt := &hostRuntime{
		Name:      "nerdctl",
		Socket:    "/run/k3s/containerd/containerd.sock",
		Namespace: containerdNamespace,
	}

	expectedArgv := buildContainerRunArgv(rt.Name, rt.Socket, rt.Namespace, spec)
	answers := map[string]string{strings.Join(expectedArgv, " "): "npu-smi output"}
	host, asked := scriptedHost(root, answers)

	ran, err := emitOrRun(context.Background(), host, rt, "", "", spec)
	require.NoError(t, err)
	assert.False(t, ran.Emitted)
	assert.Equal(t, "npu-smi output", string(ran.Output))
	require.Len(t, *asked, 1, "the run branch executes exactly one command")

	emitted, err := emitOrRun(context.Background(), host, rt, "", dryRunReason, spec)
	require.NoError(t, err)
	assert.True(t, emitted.Emitted)
	require.Len(t, *asked, 1, "the emit branch must never run anything")

	// The printed command, decoded, is the exact argv that was executed -- chroot and all.
	printedArgv := mustShellSplit(t, emitted.Command)
	assert.Equal(t, append([]string{chrootPath, root}, expectedArgv...), printedArgv)

	// And the run branch's own recorded argv (host-side, chroot already stripped by the fake) is
	// the same construction, one layer in.
	assert.Equal(t, strings.Join(expectedArgv, " "), (*asked)[0])
}

// What is printed has to be complete and runnable as printed, because the named consumer is a
// script rather than a person: every element the container needs -- image, env, mounts, devices,
// annotations and the assertion command -- must be present and in the right shape once decoded.
func TestEmitOrRun_PrintedIsComplete(t *testing.T) {
	root := fakeHostRoot(t)
	spec := testSpec()
	rt := &hostRuntime{Name: "docker"}
	host, _ := scriptedHost(root, nil)

	result, err := emitOrRun(context.Background(), host, rt, "", dryRunReason, spec)
	require.NoError(t, err)

	argv := mustShellSplit(t, result.Command)
	full := strings.Join(argv, " ")

	assert.Contains(t, full, "vendor/probe:cann", "the image")
	assert.Contains(t, full, "ASCEND_VISIBLE_DEVICES=0", "the env")
	assert.Contains(t, full, "/usr/local/Ascend/driver:/usr/local/Ascend/driver:ro", "the mount")
	assert.Contains(t, full, "/dev/davinci0:/dev/davinci0:rw", "the device")
	assert.Contains(t, full, "gpustack.ai/preflight=true", "the annotation")
	assert.Equal(t, []string{"npu-smi", "info"}, argv[len(argv)-2:], "the assertion command")
}

// A host with no usable runtime is told why in the words the resolution used, not in a sentence
// composed here. Measured on a k3s node: the row read "no container runtime was found on the host:
// probed docker, nerdctl, ctr" on a machine carrying both docker and ctr -- the reader is sent to
// install a runtime that is already there, while the real answer (the kubelet uses k3s' containerd
// and nothing here drives it) goes unsaid.
func TestEmitOrRun_NoRuntimeUsesTheResolutionsOwnWords(t *testing.T) {
	host, _ := scriptedHost(fakeHostRoot(t), nil)
	const resolved = "the kubelet is configured against /run/k3s/containerd/containerd.sock, " +
		"and this host carries no nerdctl to drive it with"

	result, err := emitOrRun(context.Background(), host, nil, resolved, "", testSpec())
	require.NoError(t, err)

	assert.True(t, result.Emitted)
	assert.Contains(t, result.Reason, resolved)
	assert.NotContains(t, result.Reason, "probed docker, nerdctl, ctr",
		"a host that carries docker and ctr was told it carries neither")
	assert.Contains(t, result.Reason, "assumes docker",
		"the dialect the printed command is written in still has to be named")
}

// With no reason to hand -- nothing resolved and nothing to say why -- the probe order is still the
// honest fallback, because that is what was tried.
func TestEmitOrRun_NoRuntimeAndNoReasonNamesTheProbeOrder(t *testing.T) {
	host, _ := scriptedHost(fakeHostRoot(t), nil)

	result, err := emitOrRun(context.Background(), host, nil, "", "", testSpec())
	require.NoError(t, err)

	assert.Contains(t, result.Reason, "probed docker, nerdctl, ctr")
}

// A dry run that is also a fallback has to say both. Measured on a k3s node with no nerdctl: the
// runtime resolved to nothing, the printed command fell back to docker's dialect, and the row said
// only "this is a dry run" -- so a reader would take a command written for a runtime this node's
// kubelet does not use, and run it believing it reproduced production.
func TestEmitOrRun_DryRunNamesTheFallbackToo(t *testing.T) {
	testCases := []struct {
		name     string
		rt       *hostRuntime
		saysAlso string
	}{
		{
			name: "a dry run on a host with a usable runtime says only that",
			rt:   &hostRuntime{Name: "docker"},
		},
		{
			name:     "a dry run on a host with no runtime says so as well",
			rt:       nil,
			saysAlso: "no container runtime was found on the host",
		},
		{
			name:     "a dry run whose runtime cannot start a probe says so as well",
			rt:       &hostRuntime{Name: "ctr", Socket: "/run/k3s/containerd/containerd.sock", Namespace: containerdNamespace},
			saysAlso: "ctr",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			host, _ := scriptedHost(fakeHostRoot(t), nil)

			result, err := emitOrRun(context.Background(), host, tc.rt, "", dryRunReason, testSpec())
			require.NoError(t, err)

			assert.True(t, result.Emitted)
			assert.Contains(t, result.Reason, "this is a dry run")
			if tc.saysAlso == "" {
				assert.Equal(t, "this is a dry run", result.Reason,
					"nothing else was wrong, so nothing else should be claimed")
				return
			}
			assert.Contains(t, result.Reason, tc.saysAlso,
				"the dry run hid the reason the step would have been emitted anyway")
		})
	}
}

// An env value carrying a space and a quote must not split the printed command when it is pasted
// into a shell, and it must decode back to exactly what was set.
func TestEmitOrRun_ShellQuoting(t *testing.T) {
	root := fakeHostRoot(t)
	spec := containerRunSpec{
		Image: "vendor/probe:cann",
		Injection: &deviceplugin.ContainerAllocateResponse{
			Envs: map[string]string{"NOTE": `hello, "world" it's fine`},
		},
	}
	rt := &hostRuntime{Name: "docker"}
	host, _ := scriptedHost(root, nil)

	result, err := emitOrRun(context.Background(), host, rt, "", dryRunReason, spec)
	require.NoError(t, err)

	argv := mustShellSplit(t, result.Command)
	var found bool
	for _, a := range argv {
		if a == `NOTE=hello, "world" it's fine` {
			found = true
		}
	}
	assert.True(t, found, "the env value round-trips through the quoted command intact: %q", result.Command)
}

// The three fallbacks are answers, not failures: no runtime resolved on the host, a resolved runtime
// that cannot start a probe, and no usable host root. All three emit rather than run, and none
// returns an error a caller would read as the step having failed.
func TestEmitOrRun_Fallbacks(t *testing.T) {
	spec := testSpec()

	t.Run("no runtime resolved", func(t *testing.T) {
		root := fakeHostRoot(t)
		host, asked := scriptedHost(root, nil)

		result, err := emitOrRun(context.Background(), host, nil, "", "", spec)

		require.NoError(t, err, "a fallback is an answer, never an error a caller would fail on")
		assert.True(t, result.Emitted)
		assert.Contains(t, result.Reason, "no container runtime")
		assert.Empty(t, *asked, "nothing is run when there is no runtime to run it with")
		assert.NotEmpty(t, result.Command, "the fallback still prints a command")
	})

	t.Run("no usable host root", func(t *testing.T) {
		empty := t.TempDir() // carries none of hostRootMarkers, so Validate fails
		host, asked := scriptedHost(empty, nil)
		rt := &hostRuntime{Name: "docker"}

		result, err := emitOrRun(context.Background(), host, rt, "", "", spec)

		require.NoError(t, err)
		assert.True(t, result.Emitted)
		assert.NotEmpty(t, result.Reason)
		assert.Empty(t, *asked, "nothing is run against a host root that failed validation")
	})

	// ctr resolves as a containerd CLI, but `ctr run` has no flag that passes a device node, so the
	// only way to reach an accelerator through it is --privileged -- which grants every device on the
	// host and would report an isolation the injection never established. The step is emitted, and
	// what is printed uses nerdctl against the very socket and namespace ctr resolved, so it is
	// runnable as printed rather than being a command nobody could take.
	t.Run("ctr cannot pass a device node", func(t *testing.T) {
		root := fakeHostRoot(t)
		host, asked := scriptedHost(root, nil)
		rt := &hostRuntime{
			Name:      "ctr",
			Socket:    "/run/k3s/containerd/containerd.sock",
			Namespace: containerdNamespace,
		}

		result, err := emitOrRun(context.Background(), host, rt, "", "", spec)

		require.NoError(t, err, "ctr's inability is a fallback, not a failure of the node")
		assert.True(t, result.Emitted)
		assert.Contains(t, result.Reason, "ctr")
		assert.Empty(t, *asked, "nothing is run through a runtime that cannot take the step")

		argv := mustShellSplit(t, result.Command)
		assert.Equal(t, "nerdctl", argv[2], "the printed command names a runtime that can take it")
		assert.NotContains(t, argv, "ctr")
		assert.Contains(t, result.Command, "--address /run/k3s/containerd/containerd.sock",
			"the socket ctr resolved is carried over, so the command reaches the same containerd")
		assert.Contains(t, result.Command, "--namespace "+containerdNamespace)
	})
}

// A containerd CLI is pointed at the socket and namespace that were resolved rather than left to its
// own defaults, because on a k3s or RKE2 node neither default is the one the node carries.
func TestEmitOrRun_CarriesResolvedContainerdAddress(t *testing.T) {
	root := fakeHostRoot(t)
	host, _ := scriptedHost(root, nil)

	t.Run("nerdctl is addressed explicitly", func(t *testing.T) {
		rt := &hostRuntime{
			Name:      "nerdctl",
			Socket:    "/run/k3s/containerd/containerd.sock",
			Namespace: containerdNamespace,
		}

		result, err := emitOrRun(context.Background(), host, rt, "", dryRunReason, testSpec())

		require.NoError(t, err)
		argv := mustShellSplit(t, result.Command)
		// chroot, host root, then the runtime and its global flags, before the subcommand.
		assert.Equal(t,
			[]string{
				"nerdctl", "--address", "/run/k3s/containerd/containerd.sock",
				"--namespace", containerdNamespace, "run", "--rm",
			},
			argv[2:9])
	})

	t.Run("docker is left to find its own daemon", func(t *testing.T) {
		result, err := emitOrRun(context.Background(), host, &hostRuntime{Name: "docker"}, "", dryRunReason, testSpec())

		require.NoError(t, err)
		argv := mustShellSplit(t, result.Command)
		assert.Equal(t, []string{"docker", "run", "--rm"}, argv[2:5],
			"docker has a daemon of its own and takes neither flag")
	})
}

// The vendor runtime and the label are the container's own, not the host CLI's, so they sit with the
// subcommand rather than with the CLI's global flags -- and both are omitted entirely where there is
// none, because `--runtime ""` is not the same command as no --runtime at all.
func TestEmitOrRun_CarriesTheVendorRuntimeAndLabel(t *testing.T) {
	root := fakeHostRoot(t)
	host, _ := scriptedHost(root, nil)
	rt := &hostRuntime{Name: "docker"}

	t.Run("both are placed after the subcommand", func(t *testing.T) {
		spec := testSpec()
		spec.Runtime, spec.Label = "ascend", "gpustack.ai/preflight=true"

		result, err := emitOrRun(context.Background(), host, rt, "", dryRunReason, spec)

		require.NoError(t, err)
		argv := mustShellSplit(t, result.Command)
		assert.Equal(t,
			[]string{"docker", "run", "--rm", "--runtime", "ascend", "--label", "gpustack.ai/preflight=true"},
			argv[2:9])
	})

	t.Run("a manufacturer needing neither gets neither", func(t *testing.T) {
		result, err := emitOrRun(context.Background(), host, rt, "", dryRunReason, testSpec())

		require.NoError(t, err)
		argv := mustShellSplit(t, result.Command)
		assert.NotContains(t, argv, "--runtime")
		assert.NotContains(t, argv, "--label")
	})
}

// hostRuntime.NetworkWarning is carried into the printed command as a leading comment, so a command
// that will die on DNS says so next to itself rather than after someone has already run it.
func TestEmitOrRun_CarriesNetworkWarning(t *testing.T) {
	root := fakeHostRoot(t)
	host, _ := scriptedHost(root, nil)
	rt := &hostRuntime{Name: "docker", NetworkWarning: networkWarning}

	result, err := emitOrRun(context.Background(), host, rt, "", dryRunReason, testSpec())

	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(result.Command, "# "+networkWarning),
		"the warning is named next to the command it would break")
}

func TestShellQuote(t *testing.T) {
	testCases := []struct {
		name string
		in   string
		want string
	}{
		{name: "a bare value needs no quoting", in: "docker", want: "docker"},
		{name: "a path-shaped value needs no quoting", in: "/dev/davinci0:/dev/davinci0:rw", want: "/dev/davinci0:/dev/davinci0:rw"},
		{name: "a space forces quoting", in: "a b", want: `'a b'`},
		{name: "an embedded single quote is escaped", in: `it's`, want: `'it'\''s'`},
		{name: "an empty string is quoted so it is not lost", in: "", want: "''"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shellQuote(tc.in))
		})
	}
}

// A vendor runtime is not one flag. `--runtime` on docker names an entry in the daemon's own
// configuration; on nerdctl it names an OCI shim, and measured on hardware `nerdctl run --runtime
// nvidia` dies with "invalid runtime name nvidia". A command that cannot run as printed is the one
// thing emit must never produce, so a CLI with no known door for a manufacturer emits instead.
func TestEmitOrRun_TranslatesTheVendorRuntimePerCLI(t *testing.T) {
	root := fakeHostRoot(t)
	host, asked := scriptedHost(root, nil)

	testCases := []struct {
		name          string
		cli           string
		vendorRuntime string
		wantFlags     []string
		wantEmitted   bool
		wantReason    string
		wantCLI       string
		nerdctlAbsent bool
	}{
		{
			name: "docker names the daemon's own runtime entry",
			cli:  "docker", vendorRuntime: "nvidia",
			wantFlags: []string{"--runtime", "nvidia"},
		},
		{
			name: "docker does the same for ascend",
			cli:  "docker", vendorRuntime: "ascend",
			wantFlags: []string{"--runtime", "ascend"},
		},
		{
			// Measured: nerdctl's door to the same hook is --gpus, and --runtime nvidia is fatal.
			name: "nerdctl reaches the nvidia hook through --gpus",
			cli:  "nerdctl", vendorRuntime: "nvidia",
			wantFlags: []string{"--gpus", "all"},
		},
		{
			// No door is known for this pair, so the step is emitted rather than printed with a
			// flag that would fail -- and the reason says the printed command is written for
			// docker, so it has to actually be a docker command. Printing nerdctl here left the
			// emitted line missing the very flag it was emitted for the want of.
			name: "nerdctl has no door to the ascend runtime, so the step is emitted as docker",
			cli:  "nerdctl", vendorRuntime: "ascend",
			wantEmitted: true, wantReason: "no way to hand a container to the ascend runtime",
			wantFlags: []string{"--runtime", "ascend"},
			wantCLI:   "docker",
		},
		{
			name: "a manufacturer needing no vendor runtime gets no flag at all",
			cli:  "nerdctl", vendorRuntime: "",
		},
		{
			// ctr resolves on a containerd host carrying no nerdctl, and falls back to nerdctl,
			// which drives the same daemon -- so the socket and namespace ctr resolved ride along
			// and the printed command runs against that containerd. Measured on an AMD host.
			name: "ctr falls back to nerdctl, keeping the containerd it resolved",
			cli:  "ctr", vendorRuntime: "",
			wantEmitted: true, wantReason: "no flag that passes a device node",
			wantCLI: "nerdctl",
		},
		{
			// ctr is resolved only where nerdctl was looked for and missing, so the command written
			// for nerdctl cannot run here as printed. Claiming otherwise sends an operator to run a
			// command whose first word this host does not have.
			name: "a ctr resolved for want of nerdctl says the command needs one installed",
			cli:  "ctr", vendorRuntime: "", nerdctlAbsent: true,
			wantEmitted: true, wantReason: "which this host does not carry",
			wantCLI: "nerdctl",
		},
		{
			name: "ctr keeps that containerd where nerdctl has a door to the vendor runtime",
			cli:  "ctr", vendorRuntime: "nvidia",
			wantEmitted: true, wantReason: "no flag that passes a device node",
			wantFlags: []string{"--gpus", "all"},
			wantCLI:   "nerdctl",
		},
		{
			// Measured on an Ascend host: ctr returned before the vendor-runtime clause ran at all,
			// so the printed nerdctl command carried no --runtime ascend -- the same defect the
			// nerdctl case above settles, reached by the other road. ctr's fallback has to clear
			// the same bar, and for this pair that means moving on to docker. The reason names ctr
			// because ctr is what was resolved; nerdctl was only ever a fallback that did not hold.
			name: "ctr whose nerdctl has no door to ascend moves the fallback on to docker",
			cli:  "ctr", vendorRuntime: "ascend",
			wantEmitted: true, wantReason: "the resolved container runtime is ctr",
			wantFlags: []string{"--runtime", "ascend"},
			wantCLI:   "docker",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec()
			spec.Runtime = tc.vendorRuntime

			// A containerd CLI is resolved against a socket and a namespace, which is what makes
			// the fallback's dialect switch observable: they belong to the CLI that was resolved,
			// not to the one the command ends up printed in.
			rt := &hostRuntime{Name: tc.cli, NerdctlAbsent: tc.nerdctlAbsent}
			if slices.Contains(containerdRuntimes, tc.cli) {
				rt.Socket, rt.Namespace = "/run/k3s/containerd/containerd.sock", containerdNamespace
			}

			// force is off for the refusal case, so the run branch is the one that declines: on a dry
			// run every step is emitted anyway and the reason would lead with that.
			result, err := emitOrRun(context.Background(), host, rt, "", forceIfNot(tc.wantEmitted), spec)

			require.NoError(t, err)
			assert.Empty(t, *asked, "no step is ever taken in this test")
			assert.True(t, result.Emitted)
			if tc.wantEmitted {
				assert.Contains(t, result.Reason, tc.wantReason)
			}

			argv := mustShellSplit(t, result.Command)
			if tc.wantCLI != "" {
				// A fallback that names a different CLI has to print that CLI's command: the whole
				// reason it fell back is that the resolved one cannot pass the vendor runtime.
				assert.Contains(t, argv, tc.wantCLI, "the emitted command is in the dialect it claims")
				if slices.Contains(containerdRuntimes, tc.wantCLI) {
					// A fallback within containerd is still that containerd: the socket the host
					// resolved is what makes the printed command runnable against the same daemon.
					assert.Contains(t, argv, "--address", "a containerd CLI keeps the socket resolved for it")
					assert.Contains(t, argv, "--namespace", "and the namespace it works in")
				} else {
					assert.NotContains(t, argv, "--address",
						"containerd addressing means nothing to a CLI that is not a containerd CLI")
					assert.NotContains(t, argv, "--namespace", "and neither does its namespace")
				}
			}
			if len(tc.wantFlags) == 0 {
				assert.NotContains(t, argv, "--runtime")
				assert.NotContains(t, argv, "--gpus")
				return
			}
			assert.Contains(t, strings.Join(argv, " "), strings.Join(tc.wantFlags, " "))
		})
	}
}

// A container that was created and then died is an observation about this node; a runtime that
// refused to create one is a limit of the environment probing it. Only the first belongs in a row.
//
// The marker cannot separate them on its own, which is what this locks. The injected preload library
// loads into every process in the container, the shell included, so a shim aborting as it is loaded
// leaves exit 139 and no output whatsoever -- measured on hardware with a library whose constructor
// calls abort. Read as "could not be started", a broken injected runtime is reported as a simulated
// pass on a node where slicing does not work.
//
// The refusal statuses below are measured too, and the two runtimes do not agree: docker keeps 125,
// nerdctl keeps 1, and both pass a container process's own status through unchanged.
func TestContainerRan(t *testing.T) {
	exitWith := func(t *testing.T, code int) error {
		t.Helper()
		err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
		require.Error(t, err, "a command exiting %d reported no error", code)
		return err
	}

	testCases := []struct {
		name    string
		runtime string
		out     string
		code    int
		want    bool
	}{
		{
			// Paired with the runtime's own refusal status, so the marker is the only thing that
			// can carry this: a container that printed it ran, whatever the status says afterwards.
			name:    "output carrying the marker outweighs the runtime's refusal status",
			runtime: "docker",
			out:     "prefix\n" + mapsBegin + "\nrest",
			code:    125,
			want:    true,
		},
		{
			// The defect this closes: a shim that aborts as the shell loads it, so the script never
			// reaches its first line. Measured as exit 139 with no output at all.
			name:    "a signal death with no marker is a container that ran",
			runtime: "docker",
			code:    139, // 128 + SIGSEGV, and what an aborting constructor actually produced
			want:    true,
		},
		{
			// docker keeps this for an image it could not pull and a flag it would not take.
			name:    "docker's own refusal is not a container that ran",
			runtime: "docker",
			code:    125,
			want:    false,
		},
		{
			// nerdctl uses 1 where docker uses 125, so the same status means opposite things.
			name:    "nerdctl's own refusal is not a container that ran",
			runtime: "nerdctl",
			code:    1,
			want:    false,
		},
		{
			// The same status under docker is a container process, not a refusal.
			name:    "an exit of 1 under docker is a container that ran",
			runtime: "docker",
			code:    1,
			want:    true,
		},
		{
			// docker answers 127 both for a device path it would not take and for a command the
			// image does not carry, so this one is deliberately read as the latter.
			name:    "an ambiguous 127 under docker is read as a container that ran",
			runtime: "docker",
			code:    127,
			want:    true,
		},
		{
			name:    "a container process's ordinary exit is a container that ran",
			runtime: "nerdctl",
			code:    3,
			want:    true,
		},
		{
			// Nothing is claimed about a runtime whose statuses were never established, so the
			// marker stays the only evidence.
			name:    "an unestablished runtime falls back to the marker alone",
			runtime: "ctr",
			code:    139,
			want:    false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, containerRan(tc.runtime, []byte(tc.out), exitWith(t, tc.code)))
		})
	}

	t.Run("an error carrying no exit status is not a container that ran", func(t *testing.T) {
		// The command could not be launched on the host at all, which is the environment rather
		// than the node under test.
		assert.False(t, containerRan("docker", nil, errors.New("fork/exec: no such file or directory")))
	})
}
