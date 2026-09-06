package worker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// admit runs the whole webhook over a Pod and returns it mutated in place.
func admit(t *testing.T, pod *core.Pod, objs ...ctrlcli.Object) error {
	t.Helper()

	if len(objs) == 0 {
		objs = kvCacheFixture()
	}
	return newPodKVCacheWebhook(objs...).Default(context.Background(), pod)
}

// stampOf decodes the injection record the webhook wrote.
func stampOf(t *testing.T, pod *core.Pod) injectionRecord {
	t.Helper()

	raw, ok := pod.Annotations[KVCacheInjectedAnnotationKey]
	require.True(t, ok, "an injected Pod records what was done to it")

	var record injectionRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &record),
		"the stamp is JSON so a jsonpath query can select one field")
	return record
}

// containerEnv returns a container's environment as a name-to-value map.
func containerEnv(ctr *core.Container) map[string]string {
	out := make(map[string]string, len(ctr.Env))
	for i := range ctr.Env {
		out[ctr.Env[i].Name] = ctr.Env[i].Value
	}
	return out
}

// TestPodKVCacheInject_VLLMCarriesTheFileVehicle asserts the admitted object, not the webhook's own
// decisions: the container's final env, args and mounts, and the Pod's volume and annotations.
func TestPodKVCacheInject_VLLMCarriesTheFileVehicle(t *testing.T) {
	pod := kvCachePod()
	require.NoError(t, admit(t, pod))

	ctr := &pod.Spec.Containers[0]
	env := containerEnv(ctr)
	assert.Equal(t, "/etc/gpustack/kvcache/mooncake.json", env["MOONCAKE_CONFIG_PATH"])
	// The flag and its value are checked separately, and the document semantically: comparing the
	// whole slice pinned a JSON key order that JSON does not define, which is what kept the renderer
	// hand-building the string instead of marshaling a type.
	require.Len(t, ctr.Args, 3)
	assert.Equal(t, []string{"serve", "--kv-transfer-config"}, ctr.Args[:2],
		"the injection is appended, so the workload's own arguments come first and stay intact")
	assert.JSONEq(t, `{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_both"}`, ctr.Args[2])

	require.Len(t, pod.Spec.Volumes, 1)
	require.Len(t, ctr.VolumeMounts, 1)
	assert.True(t, ctr.VolumeMounts[0].ReadOnly,
		"nothing in the container writes it, and a writable projection would let a process edit the "+
			"record of what was injected")

	assert.Contains(t, pod.Annotations, inject.ClientConfigAnnotationKey,
		"the downwardAPI projection reads the file back out of this annotation")
}

// TestPodKVCacheInject_SGLangCarriesTheEnvironmentVehicle is the counterpart, and its negative half
// carries as much weight as its positive one.
func TestPodKVCacheInject_SGLangCarriesTheEnvironmentVehicle(t *testing.T) {
	pod := kvCachePod()
	pod.Annotations[KVCacheEngineAnnotationKey] = "sglang"
	require.NoError(t, admit(t, pod))

	ctr := &pod.Spec.Containers[0]
	assert.Empty(t, pod.Spec.Volumes, "the environment is the whole vehicle")
	assert.Empty(t, ctr.VolumeMounts)
	assert.NotContains(t, pod.Annotations, inject.ClientConfigAnnotationKey,
		"nothing is projected, so nothing carries a projection's annotation")

	env := containerEnv(ctr)
	assert.Equal(t, "mc-leader.gpustack-system.svc:50051", env["MOONCAKE_MASTER"])
	assert.NotContains(t, env, "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH",
		"setting it would select the file branch and void the whole injection")

	// Asserted on the ADMITTED Pod rather than on the stamp, deliberately. The stamp is the
	// renderer's own report of what it did; this is the container that will run. A mutation stopping
	// the emission while leaving the report intact turned out to be possible, and only an assertion
	// at this level catches that class.
	assert.Equal(t, "team-a-chat", env["MOONCAKE_TENANT_ID"],
		"this engine reads a tenant, so the Binding's reuse domain is what it is told to write under")
}

// TestPodKVCacheInject_StampRecordsWhatWasDecided pins every field, and the isolation one is why the
// stamp exists: F4a injects rather than refusing, so nothing else on the Pod says the declared domain
// is not being enforced, and the cost of that - one domain evicting another's blocks - moves no metric.
func TestPodKVCacheInject_StampRecordsWhatWasDecided(t *testing.T) {
	pod := kvCachePod()
	require.NoError(t, admit(t, pod))

	record := stampOf(t, pod)
	assert.Equal(t, "chat", record.Binding)
	assert.Equal(t, "vllm", record.Engine)
	assert.Equal(t, "v0.25.1", record.EngineVersion)
	assert.Equal(t, "file", record.Vehicle)
	assert.Equal(t, "team-a-chat", record.Domain)
	assert.False(t, record.TenantInjected,
		"vLLM's config class has no tenant key, so nothing was written for it to read")
}

// TestPodKVCacheInject_StampTenantFollowsTheEngine is the paired control for the field above.
//
// The assertion there observes vLLM, which injects nothing, so on its own it passes just as well
// against a field hard-coded to false. What discriminates is that the two accepted engines answer
// DIFFERENTLY: SGLang reads a tenant variable and is given one, vLLM has no tenant key in its config
// class and is given none. No substituted verdict is needed for that - the engines themselves are the
// two sides.
//
// The field records what was injected and never whether isolation resulted, so there is deliberately
// no assertion here about the latter: whether the engine build honors the variable is not knowable
// at admission, and a test claiming otherwise would be pinning an over-claim.
func TestPodKVCacheInject_StampTenantFollowsTheEngine(t *testing.T) {
	testCases := []struct {
		engine string
		want   bool
	}{
		// vllm-ascend is absent because it cannot arrive here: the annotation refuses it, and the
		// operator derives it further in. Its stamp is covered where it is reachable, in the
		// inject package's own table.
		{engine: "vllm", want: false},
		{engine: "sglang", want: true},
	}
	require.NotEqual(t, testCases[0].want, testCases[len(testCases)-1].want,
		"the table must contain both answers, or it cannot tell a rendered value from a constant")

	for _, tc := range testCases {
		t.Run(tc.engine, func(t *testing.T) {
			pod := kvCachePod()
			pod.Annotations[KVCacheEngineAnnotationKey] = tc.engine
			require.NoError(t, admit(t, pod))
			assert.Equal(t, tc.want, stampOf(t, pod).TenantInjected,
				"the stamp reports what this engine's renderer emitted")
		})
	}
}

// TestPodKVCacheInject_RefusesAShellWrapper covers both spellings of the same process, because they
// are the same process and a check written against either field alone catches only one of them.
//
// Kubernetes runs command followed by args, so ["/bin/sh","-c"]+["script"] and ["/bin/sh"]+["-c",
// "script"] produce an identical argv. The first revision of this refusal tested command's last
// element and would have passed the second spelling - which is the one this suite's own fixtures use,
// so the defect would have been left in place while looking fixed.
func TestPodKVCacheInject_RefusesAShellWrapper(t *testing.T) {
	testCases := []struct {
		name          string
		command, args []string
		refuse        bool
		// wantMsg is the phrase that identifies WHICH reason the refusal gave, and asserting it per
		// row is what keeps the three apart. They lead to different next moves - rewrite the shell
		// invocation, unpack the single argument, or look inside the script - so a test that only
		// checked "some error" would let any one of them answer for another.
		wantMsg string
	}{
		{name: "-c in command", command: []string{"/bin/sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "-c in args", command: []string{"/bin/sh"}, args: []string{"-c", "vllm serve"}, refuse: true},
		{name: "bash -lc bundle", command: []string{"/bin/bash", "-lc"}, args: []string{"vllm serve"}, refuse: true},
		{name: "bare basename", command: []string{"sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		// -c belongs to the program, not to a shell. Refusing these is the mistake this check must not make.
		{name: "not a shell", command: []string{"myapp", "-c", "config.yaml"}, args: []string{"run"}},
		{name: "engine directly", command: []string{"vllm"}, args: []string{"serve", "--model", "x"}},
		{name: "long option", command: []string{"/bin/sh", "--config"}, args: []string{"x"}},
		// A shell stops reading options at its first operand, so a -c AFTER the script file belongs to
		// the script: `sh /app/run.sh -c config` sets the script's $1, and appending to args is safe.
		// An earlier revision scanned the whole argv and refused these, which is a false refusal.
		{name: "-c after the script operand", command: []string{"/bin/sh", "/app/run.sh"}, args: []string{"-c", "config"}},
		{name: "options ended by --", command: []string{"/bin/sh", "--", "run.sh"}, args: []string{"-c", "x"}},
		// -o and bash's -O take an option NAME as their operand, so the scan must not mistake it for
		// the script file and stop there - the -c that follows is still the shell's. A bundle ending
		// in o does the same, which is why the rule is on the last character rather than on "-o".
		{name: "-o operand is not the script", command: []string{"/bin/sh", "-o", "pipefail", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "bash -O operand is not the script", command: []string{"/bin/bash", "-O", "extglob", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "bundle ending in o", command: []string{"/bin/bash", "-euo", "pipefail", "-c"}, args: []string{"vllm serve"}, refuse: true},
		// A launcher prefix does not make the shell stop being the process that reads -c. Testing
		// argv[0] alone was walked around three times; the third was this shape, so the prefix is now
		// resolved rather than special-cased.
		{name: "env wrapper", command: []string{"/usr/bin/env", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "env with assignments", command: []string{"env", "FOO=1", "BAR=2", "bash", "-lc"}, args: []string{"vllm serve"}, refuse: true},
		{name: "env -u takes a name", command: []string{"env", "-u", "SHLVL", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "tini then a shell", command: []string{"/sbin/tini", "--", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		// Which launcher options take an operand is per-launcher, and a wrong answer hides the shell
		// in either direction: stepping over too little makes the operand look like the program,
		// stepping over too much makes the program look like an operand. Both shapes below were
		// admitted by a revision that treated every option as operand-free except env -u.
		{name: "env -C takes a directory", command: []string{"env", "-C", "/tmp", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "env --chdir separated", command: []string{"env", "--chdir", "/tmp", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "env --chdir inline keeps its operand", command: []string{"env", "--chdir=/tmp", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "env -i reaches past nothing", command: []string{"env", "-i", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "bundle ending in an operand letter", command: []string{"env", "-iu", "SHLVL", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "operand inlined into the bundle", command: []string{"env", "-uSHLVL", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "tini -p takes a signal", command: []string{"/sbin/tini", "-p", "SIGTERM", "--", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "dumb-init -r takes a remapping", command: []string{"dumb-init", "-r", "15:2", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		// timeout(1) puts an operand of its OWN before the command - the duration - which no grammar
		// of options can describe. Without a count of them `timeout 30 sh -c ...` resolved to
		// `timeout`, was ADMITTED, and the appended flag became the shell's $0.
		{name: "timeout duration then a shell", command: []string{"timeout", "30", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "timeout -s takes a signal first", command: []string{"timeout", "-s", "KILL", "30", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "timeout --kill-after separated", command: []string{"timeout", "--kill-after", "5s", "30", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		// The control that gives the three above their teeth. The duration is stepped over and the
		// token after it is still tested as a program: a count that stepped over one token too many
		// would read "serve" as the program here, and refusing this would be a false refusal.
		{name: "timeout then the engine directly", command: []string{"timeout", "30", "vllm"}, args: []string{"serve"}},
		// tini has no -C, so its table must not borrow env's. Borrowing would make -C swallow "sh",
		// leaving "-c" as the apparent program and the shell unseen - this case fails if it does.
		{name: "an operand letter is per launcher", command: []string{"/sbin/tini", "-C", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		// Resolving the prefix must not turn a direct launch into a refusal.
		{name: "env then the engine", command: []string{"/usr/bin/env", "vllm"}, args: []string{"serve", "--model", "x"}},
		{name: "env then a script", command: []string{"env", "sh", "/app/run.sh"}, args: []string{"-c", "config"}},
		{name: "env -C then the engine", command: []string{"env", "-C", "/work", "vllm"}, args: []string{"serve", "--model", "x"}},
		// env -a/--argv0 sets the command's argv[0]; its operand is not the command.
		{name: "env -a takes an argv0", command: []string{"env", "-a", "custom", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "env --argv0 separated", command: []string{"env", "--argv0", "custom", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		// A shell's OWN long options can take an operand too, and it is a startup file rather than the
		// command file - so the scan skips it and keeps going, instead of stopping as if it had found
		// the script. Refusing to skip admits the launch.
		{name: "bash --rcfile takes a file", command: []string{"/bin/bash", "--rcfile", "/tmp/x", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "bash --init-file takes a file", command: []string{"/bin/bash", "--init-file", "/tmp/x", "-c"}, args: []string{"vllm serve"}, refuse: true},
		// An ordinary long flag still must not swallow the token after it: --norc is operand-free, so
		// what follows is the script, and the -c after THAT belongs to the script.
		{name: "operand-free long flag", command: []string{"/bin/bash", "--norc", "/app/run.sh"}, args: []string{"-c", "config"}},
		// A multi-call binary names its applet in the first operand, so the shell is one token further
		// along than argv[0]. Neither "busybox" nor "toybox" is a shell name, so an unresolved prefix
		// here admits the launch outright.
		{name: "busybox shell applet", command: []string{"busybox", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "busybox by path", command: []string{"/bin/busybox", "ash", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "toybox shell applet", command: []string{"toybox", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		{name: "launcher then multi-call", command: []string{"env", "-i", "busybox", "sh", "-c"}, args: []string{"vllm serve"}, refuse: true},
		// A non-shell applet is an ordinary launch and must stay admitted.
		{name: "busybox non-shell applet", command: []string{"busybox", "vllm"}, args: []string{"serve", "--model", "x"}},
		// A trailing -c has no script in the SUBMITTED container, and that is exactly why it is refused:
		// this webhook appends to args, so its own flag becomes the shell's command string and runs as
		// a command. An earlier revision admitted this, reasoning that "the shell fails on its own
		// terms" - it does not, it runs the wrong thing, and the injection is what put it there.
		{name: "trailing -c", command: []string{"/bin/sh", "-c"}, refuse: true},

		// FAILING CLOSED: the two shapes this check used to admit because it could not read them.
		// Neither is a launcher missing from the enumeration - both are launches whose behavior is
		// not on the command line at all, which is why no entry would fix them.
		//
		// A script by path. Measured on lmsysorg/sglang, whose entrypoint is
		// /opt/nvidia/nvidia_entrypoint.sh: whether an appended flag reaches the engine depends on
		// whether that file forwards "$@", and admission cannot open the file.
		{
			name: "a script by path", command: []string{"/opt/nvidia/nvidia_entrypoint.sh"},
			args: []string{"vllm", "serve"}, refuse: true, wantMsg: "forwards its arguments",
		},
		{
			name:    "a script by path behind a launcher it knows",
			command: []string{"tini", "--", "/app/entrypoint.sh"}, args: []string{"vllm", "serve"},
			refuse: true, wantMsg: "forwards its arguments",
		},
		// The whole command line inside one token. env -S splits the string itself, so there is
		// nothing on the argv to test and the -S operand is the last token there is.
		{
			name: "env -S hides the command line", command: []string{"env", "-S", "sh -c 'vllm serve'"},
			refuse: true, wantMsg: "inside a single argument",
		},
		{
			name:    "env --split-string hides it too",
			command: []string{"env", "--split-string", "sh -c 'vllm serve'"},
			refuse:  true, wantMsg: "inside a single argument",
		},

		// AND THE SHAPES FAILING CLOSED MUST NOT TOUCH. A launcher whose options simply ended leaves
		// the appended args AS the command, which is the appendable case: `tini --` plus args runs
		// exactly those args. Refusing it would be the false refusal that makes fail-closed
		// unusable, and it is the shape 56 of the 60 runner-image families ship.
		{name: "tini with the command in args", command: []string{"tini", "--"}, args: []string{"vllm", "serve"}},
		{name: "tini and nothing else", command: []string{"tini", "--"}},
		// An operand-taking option with real tokens after it still resolves.
		{name: "env -u then a program", command: []string{"env", "-u", "HOME", "vllm"}, args: []string{"serve"}},
		// A program that merely has a script-looking argument is not a script launch.
		{name: "a program taking a script argument", command: []string{"vllm", "serve", "--x", "a.sh"}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := kvCachePod()
			pod.Spec.Containers[0].Command = tc.command
			pod.Spec.Containers[0].Args = tc.args

			err := admit(t, pod)
			if !tc.refuse {
				require.NoError(t, err, "this launch shape reaches the engine, so it must be admitted")
				return
			}
			require.Error(t, err)
			want := tc.wantMsg
			if want == "" {
				want = "positional parameters"
			}
			assert.Contains(t, err.Error(), want,
				"the message has to say WHY appending cannot work, or the fix is not obvious")
		})
	}
}

// TestPodKVCacheInject_StampFollowsWhatLandedNotWhatWasRendered covers the third appearance of one
// shape: a field reporting an action, computed from something that is not the action.
//
// First it was computed from the input condition rather than the emission, so a renderer that stopped
// emitting still reported true. That was fixed inside the renderer. Then the caller turned out to
// have its own veto - this repository's rule that an injection never overrules a variable the
// workload declared - and for the environment vehicle the tenant is exactly such a variable. The
// renderer's answer was honest about what it produced and wrong about what the container got.
//
// So the assertion is on the admitted Pod: the container keeps the workload's own value, and the
// stamp says no tenant was injected.
func TestPodKVCacheInject_StampFollowsWhatLandedNotWhatWasRendered(t *testing.T) {
	pod := kvCachePod()
	pod.Annotations[KVCacheEngineAnnotationKey] = "sglang"
	pod.Spec.Containers[0].Env = []core.EnvVar{{Name: "MOONCAKE_TENANT_ID", Value: "mine"}}
	require.NoError(t, admit(t, pod), "a declared variable is precedence, never a refusal")

	assert.Equal(t, "mine", containerEnv(&pod.Spec.Containers[0])["MOONCAKE_TENANT_ID"],
		"the workload's own value stands")
	assert.False(t, stampOf(t, pod).TenantInjected,
		"the record describes the container, so it cannot claim a tenant this webhook did not apply")
}

// TestPodKVCacheInject_StampVehicleFollowsTheEngine. The vehicle is on the stamp because it turns the
// one silent outcome this design accepts into a one-line check: a Pod on the environment vehicle whose
// cache is cold is a Pod to check for a config path of its own.
func TestPodKVCacheInject_StampVehicleFollowsTheEngine(t *testing.T) {
	testCases := []struct {
		engine, want string
	}{
		{engine: "vllm", want: "file"},
		{engine: "sglang", want: "environment"},
	}

	for _, tc := range testCases {
		t.Run(tc.engine, func(t *testing.T) {
			pod := kvCachePod()
			pod.Annotations[KVCacheEngineAnnotationKey] = tc.engine
			require.NoError(t, admit(t, pod))

			assert.Equal(t, tc.want, stampOf(t, pod).Vehicle)
		})
	}
}

// TestPodKVCacheInject_AscendIsNotAnEngineAUserNames asserts the refusal AT THE SURFACE THE VALUE
// ARRIVES ON, which is the annotation and not the constant block.
//
// `ModelDeployment.spec.engine` had already ruled that vllm_ascend is an accelerator backend rather
// than an engine, while this annotation went on accepting it: one concept, two published value sets.
// Aligning them only means anything if the refusal lands where a user can see it, so this asserts
// the admission error and that it names the value to use instead -- a bare rejection would send the
// reader looking for a third annotation to carry the distinction, which is the dead end that
// alignment exists to close.
func TestPodKVCacheInject_AscendIsNotAnEngineAUserNames(t *testing.T) {
	pod := kvCachePod()
	pod.Annotations[KVCacheEngineAnnotationKey] = "vllm-ascend"

	err := admit(t, pod)
	require.Error(t, err, "the annotation must refuse a value the other API surface already closed")
	assert.Contains(t, err.Error(), `"vllm"`,
		"and it must name the engine to set instead, since the operator picks the Ascend "+
			"connector from the pool's accelerator on its own")
}

// TestPodKVCacheInject_ObservabilityDefaultsOnAndYieldToTheUser. These two change no result, which is
// why a user-set value is left alone rather than refused: rejecting a Pod over a metrics toggle would
// be a refusal with nothing behind it.
func TestPodKVCacheInject_ObservabilityDefaultsOnAndYieldToTheUser(t *testing.T) {
	t.Run("absent, so defaulted on", func(t *testing.T) {
		pod := kvCachePod()
		require.NoError(t, admit(t, pod))

		env := containerEnv(&pod.Spec.Containers[0])
		assert.Equal(t, "1", env["MC_TE_METRIC"])
		assert.Equal(t, "1", env["MC_STORE_CLIENT_METRIC_BANDWIDTH"])
	})

	t.Run("user set, so left alone", func(t *testing.T) {
		pod := kvCachePod()
		pod.Spec.Containers[0].Env = []core.EnvVar{{Name: "MC_TE_METRIC", Value: "0"}}
		require.NoError(t, admit(t, pod), "a metrics toggle is never a reason to refuse")

		assert.Equal(t, "0", containerEnv(&pod.Spec.Containers[0])["MC_TE_METRIC"])
	})
}

// TestPodKVCacheInject_ValueVariableYieldsToTheWorkload is the F6 distinction between the keys that
// select the mechanism and the ones that carry a value inside it. A user overriding one value keeps the
// rest of the injection, which is what makes the rule useful rather than all-or-nothing.
func TestPodKVCacheInject_ValueVariableYieldsToTheWorkload(t *testing.T) {
	pod := kvCachePod()
	pod.Annotations[KVCacheEngineAnnotationKey] = "sglang"
	pod.Spec.Containers[0].Env = []core.EnvVar{{Name: "MOONCAKE_PROTOCOL", Value: "rdma"}}

	require.NoError(t, admit(t, pod), "a value variable is not a mechanism key, so it does not refuse")

	env := containerEnv(&pod.Spec.Containers[0])
	assert.Equal(t, "rdma", env["MOONCAKE_PROTOCOL"], "the workload's own declaration is authoritative")
	assert.Equal(t, "mc-leader.gpustack-system.svc:50051", env["MOONCAKE_MASTER"],
		"the rest of the injection still lands")
}

// TestPodKVCacheInject_ContainerSelection. Never the first of several: the grounding is this
// repository's own experience, where an injection landed in a sidecar while the workload ran in the
// main container, and the symptom was "artifacts present, feature absent".
func TestPodKVCacheInject_ContainerSelection(t *testing.T) {
	sidecar := core.Container{Name: "logs", Args: []string{"tail"}}

	t.Run("one container needs no annotation", func(t *testing.T) {
		pod := kvCachePod()
		require.NoError(t, admit(t, pod))
		assert.Contains(t, containerEnv(&pod.Spec.Containers[0]), "MOONCAKE_CONFIG_PATH")
	})

	t.Run("several and none named is refused", func(t *testing.T) {
		pod := kvCachePod()
		pod.Spec.Containers = append(pod.Spec.Containers, sidecar)

		err := admit(t, pod)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "never picks the first")
		assert.Contains(t, err.Error(), "server", "the message lists the candidates")
	})

	t.Run("the named one, and only it", func(t *testing.T) {
		pod := kvCachePod()
		pod.Spec.Containers = append(pod.Spec.Containers, sidecar)
		pod.Annotations[KVCacheContainerAnnotationKey] = "logs"

		require.NoError(t, admit(t, pod))
		assert.NotContains(t, containerEnv(&pod.Spec.Containers[0]), "MOONCAKE_CONFIG_PATH",
			"the container that was not named is untouched")
		assert.Contains(t, containerEnv(&pod.Spec.Containers[1]), "MOONCAKE_CONFIG_PATH")
	})

	t.Run("a name that is not a container", func(t *testing.T) {
		pod := kvCachePod()
		pod.Annotations[KVCacheContainerAnnotationKey] = "nope"

		err := admit(t, pod)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a container of this Pod")
		assert.Contains(t, err.Error(), "server", "the message lists the candidates")
	})

	t.Run("an init container is never a candidate", func(t *testing.T) {
		pod := kvCachePod()
		pod.Spec.InitContainers = []core.Container{{Name: "setup"}}
		pod.Annotations[KVCacheContainerAnnotationKey] = "setup"

		err := admit(t, pod)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "init container")
		assert.Contains(t, err.Error(), "cache nothing")
	})
}

// TestPodKVCacheInject_ConflictRefusals covers the keys whose presence makes the injection ambiguous.
// Every one of them would otherwise produce two sources for one setting, with nothing reporting which
// won.
func TestPodKVCacheInject_ConflictRefusals(t *testing.T) {
	testCases := []struct {
		name    string
		engine  string
		mutate  func(ctr *core.Container, pod *core.Pod)
		wantMsg string
	}{
		{
			name: "the config path variable is already set",
			mutate: func(ctr *core.Container, _ *core.Pod) {
				ctr.Env = []core.EnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/x"}}
			},
			wantMsg: "MOONCAKE_CONFIG_PATH",
		},
		{
			name:    "the connector flag is already in args",
			mutate:  func(ctr *core.Container, _ *core.Pod) { ctr.Args = append(ctr.Args, "--kv-transfer-config", "{}") },
			wantMsg: "--kv-transfer-config",
		},
		{
			name:    "the connector flag is in command instead",
			mutate:  func(ctr *core.Container, _ *core.Pod) { ctr.Command = []string{"python", "--kv-transfer-config={}"} },
			wantMsg: "--kv-transfer-config",
		},
		{
			name:   "the sglang backend flag is already set",
			engine: "sglang",
			mutate: func(ctr *core.Container, _ *core.Pod) {
				ctr.Args = append(ctr.Args, "--hicache-storage-backend", "mooncake")
			},
			wantMsg: "--hicache-storage-backend",
		},
		{
			name: "the mount path is already taken",
			mutate: func(ctr *core.Container, _ *core.Pod) {
				ctr.VolumeMounts = []core.VolumeMount{{Name: "other", MountPath: "/etc/gpustack/kvcache"}}
			},
			wantMsg: "/etc/gpustack/kvcache",
		},
		{
			name: "a mount already has our volume name, at another path",
			mutate: func(ctr *core.Container, _ *core.Pod) {
				ctr.VolumeMounts = []core.VolumeMount{{Name: "gpustack-kvcache-config", MountPath: "/elsewhere"}}
			},
			wantMsg: "already mounts a volume named",
		},
		{
			name: "a Pod volume already has our name",
			mutate: func(_ *core.Container, pod *core.Pod) {
				pod.Spec.Volumes = []core.Volume{{Name: "gpustack-kvcache-config"}}
			},
			wantMsg: "gpustack-kvcache-config",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := kvCachePod()
			if tc.engine != "" {
				pod.Annotations[KVCacheEngineAnnotationKey] = tc.engine
			}
			tc.mutate(&pod.Spec.Containers[0], pod)

			err := admit(t, pod)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// TestPodKVCacheInject_UserTenantIDIsNotAConflict. This webhook does not write the key, and refusing a
// Pod over one it does not write would block the single workaround available to somebody running a
// patched engine that does forward a tenant.
func TestPodKVCacheInject_UserTenantIDIsNotAConflict(t *testing.T) {
	pod := kvCachePod()
	pod.Spec.Containers[0].Env = []core.EnvVar{{Name: "MOONCAKE_TENANT_ID", Value: "team-a-chat"}}

	require.NoError(t, admit(t, pod))
	assert.Equal(t, "team-a-chat", containerEnv(&pod.Spec.Containers[0])["MOONCAKE_TENANT_ID"],
		"left exactly as the author wrote it")
}

// TestPodKVCacheInject_SGLangConfigPathIsNotAConflict. The webhook stopped writing this key with the
// per-engine vehicle, and a user who sets it has configured SGLang from a file of their own - correct
// precedence rather than a collision. The injection yields to it silently, which is recorded as the one
// accepted silent outcome in the design.
func TestPodKVCacheInject_SGLangConfigPathIsNotAConflict(t *testing.T) {
	pod := kvCachePod()
	pod.Annotations[KVCacheEngineAnnotationKey] = "sglang"
	pod.Spec.Containers[0].Env = []core.EnvVar{
		{Name: "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH", Value: "/mine.json"},
	}

	require.NoError(t, admit(t, pod))
	assert.Equal(t, "/mine.json",
		containerEnv(&pod.Spec.Containers[0])["SGLANG_HICACHE_MOONCAKE_CONFIG_PATH"])
}

// TestPodKVCacheInject_NoCommandNoArgsIsRefused. Appending to an empty args does not append: Kubernetes
// reads args alone as the whole command line and discards the image's CMD, so the engine would lose its
// own launch arguments.
func TestPodKVCacheInject_NoCommandNoArgsIsRefused(t *testing.T) {
	pod := kvCachePod()
	pod.Spec.Containers[0].Args = nil
	pod.Spec.Containers[0].Command = nil

	err := admit(t, pod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "discard the image's CMD")
	assert.Contains(t, err.Error(), "copy the image's launch arguments into args",
		"the message names which field to fill, because command is the wrong one")
	assert.Contains(t, err.Error(), "ENTRYPOINT",
		"and says why command is wrong: it overrides the vendor runtime's entrypoint too")
}

// TestPodKVCacheInject_AnnotationVocabulary. A typo is the case this exists for: an ignored
// "kvcache.gpustack.ai/bindng" would leave the Pod with no Binding at all, and the symptom is a
// container that starts fine and caches nothing.
func TestPodKVCacheInject_AnnotationVocabulary(t *testing.T) {
	testCases := []struct {
		name, key, value, wantMsg string
	}{
		{
			name:    "a typo in an accepted key",
			key:     "kvcache.gpustack.ai/bindng",
			value:   "chat",
			wantMsg: "not one this webhook accepts",
		},
		{
			name:    "the client config, which the webhook writes",
			key:     inject.ClientConfigAnnotationKey,
			value:   "{}",
			wantMsg: "written by this webhook",
		},
		{
			name:    "the stamp, which the webhook writes",
			key:     KVCacheInjectedAnnotationKey,
			value:   "{}",
			wantMsg: "written by this webhook",
		},
		{
			name:    "the domain, which has its own reason",
			key:     KVCacheDomainAnnotationKey,
			value:   "team-b",
			wantMsg: "comes from the KVCachePoolBinding",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := kvCachePod()
			pod.Annotations[tc.key] = tc.value

			err := admit(t, pod)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// TestPodKVCacheInject_UnrelatedAnnotationsAreLeftAlone is the control for the case above. A vocabulary
// check that refused every unfamiliar key would reject a Pod carrying an unrelated tool's annotation,
// which is most Pods.
func TestPodKVCacheInject_UnrelatedAnnotationsAreLeftAlone(t *testing.T) {
	pod := kvCachePod()
	pod.Annotations["prometheus.io/scrape"] = "true"
	pod.Annotations["kubectl.kubernetes.io/default-container"] = "server"

	require.NoError(t, admit(t, pod))
	assert.Equal(t, "true", pod.Annotations["prometheus.io/scrape"])
}

// TestPodKVCacheInject_RefusalLeavesThePodUntouched. Every refusal runs before the first mutation, so a
// refused Pod is byte-identical to the one submitted. A half-injected Pod would be worse than a refused
// one: it would start, and no field on it would explain how it behaved.
func TestPodKVCacheInject_RefusalLeavesThePodUntouched(t *testing.T) {
	pod := kvCachePod()
	pod.Spec.Containers = append(pod.Spec.Containers, core.Container{Name: "logs", Args: []string{"tail"}})
	before := pod.DeepCopy()

	require.Error(t, admit(t, pod))
	assert.Equal(t, before, pod)
}

// TestPodKVCacheInject_TouchesOnlyTheAllowedPaths diffs the whole object rather than inspecting the
// webhook's own decision. Anything outside this list is a mutation nobody asked for, and resources in
// particular belongs to the other Pod webhook.
func TestPodKVCacheInject_TouchesOnlyTheAllowedPaths(t *testing.T) {
	pod := kvCachePod()
	before := pod.DeepCopy()
	require.NoError(t, admit(t, pod))

	assert.Equal(t, before.Spec.Containers[0].Resources, pod.Spec.Containers[0].Resources,
		"resources belong to the accelerator webhook; this one never touches them")
	assert.Equal(t, before.Labels, pod.Labels)
	assert.Equal(t, before.OwnerReferences, pod.OwnerReferences)
	assert.Equal(t, before.Spec.NodeSelector, pod.Spec.NodeSelector)
	assert.Equal(t, before.Spec.Containers[0].Image, pod.Spec.Containers[0].Image)
	assert.Equal(t, before.Spec.Containers[0].Command, pod.Spec.Containers[0].Command)

	// Everything the webhook is allowed to change, and nothing else.
	pod.Spec.Containers[0].Env = before.Spec.Containers[0].Env
	pod.Spec.Containers[0].Args = before.Spec.Containers[0].Args
	pod.Spec.Containers[0].VolumeMounts = before.Spec.Containers[0].VolumeMounts
	pod.Spec.Volumes = before.Spec.Volumes
	delete(pod.Annotations, inject.ClientConfigAnnotationKey)
	delete(pod.Annotations, KVCacheInjectedAnnotationKey)
	assert.Equal(t, before, pod, "no other field changed")
}
