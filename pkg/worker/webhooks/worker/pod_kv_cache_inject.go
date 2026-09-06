package worker

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"

	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

const (
	// KVCacheInjectedAnnotationKey records what this webhook did, as JSON so a jsonpath query can
	// select one field. It is written by the webhook and never read from a submitted object: a
	// user-supplied value would be a forged record of a decision the user did not make.
	KVCacheInjectedAnnotationKey = "kvcache." + systemname.LabelPrefix + "injected"

	// observabilityMetricsEnv and observabilityBandwidthEnv are turned on when the container has not
	// spoken about them. They change no result, which is why a user-set value is left alone rather
	// than refused: rejecting a Pod over a metrics toggle would be a refusal with nothing behind it.
	observabilityMetricsEnv   = "MC_TE_METRIC"
	observabilityBandwidthEnv = "MC_STORE_CLIENT_METRIC_BANDWIDTH"
)

// ownedArgs are the flags this webhook writes. A container already carrying one has a KV cache
// configured by someone else, and merging would produce "two --kv-transfer-config, which one wins",
// which is undiagnosable from outside.
var ownedArgs = []string{"--kv-transfer-config", "--hicache-storage-backend"}

// ownedEnv is the variable this webhook writes that SELECTS the mechanism, as opposed to the ones that
// carry values inside it. Only this one refuses; the value variables yield, per
// deviceplugin.ContainerEnvDeclared.
//
// SGLANG_HICACHE_MOONCAKE_CONFIG_PATH is deliberately absent: this webhook stopped writing it when
// SGLang moved to the environment vehicle, and a user who sets it has configured SGLang from a file of
// their own, which is correct precedence rather than a collision.
var ownedEnv = []string{"MOONCAKE_CONFIG_PATH"}

// injectionRecord is the stamp: what this webhook decided, on the object it decided about.
//
// It is JSON so a jsonpath query can select one field, and it exists because the decisions below leave
// no other trace. A Pod given a tenant and one given none look alike from outside, and the cost of the
// difference - two reuse domains sharing an eviction pool - moves no metric at all.
type injectionRecord struct {
	// Binding is the KVCachePoolBinding this Pod NAMED and this webhook resolved, in the Pod's own
	// namespace. It provisioned the domain and the capacity; it authorized nothing, and the API
	// contract says so explicitly - a workload that knows another domain's name reaches it, Binding
	// or no Binding (see "What a Binding does not do" in docs/kv-cache/pool.md, and #168).
	//
	// It is NOT necessarily the Binding the writes are ACCOUNTED to, and the difference is visible in
	// this project's own e2e: when no tenant reaches the store, the client writes under the master's
	// own "default" name, so usage lands on whichever Binding registered THAT domain. Case 53 asserts
	// the split directly - usage rises on the default-domain Binding while this one stays at zero.
	// Reading this field as the accounting object sends an operator to the wrong status.
	Binding string `json:"binding"`

	// Engine and EngineVersion are what was configured and the version the isolation answer was
	// measured at, so the record says why rather than only what.
	Engine        string `json:"engine"`
	EngineVersion string `json:"engineVersion"`

	// Vehicle is "file" or "environment". A Pod on the environment vehicle whose cache is cold is a
	// Pod to check for a config path of its own, which is the one silent outcome this design accepts.
	Vehicle string `json:"vehicle"`

	// Domain is the reuse identity the Binding declared.
	Domain string `json:"domain,omitempty"`

	// TenantInjected is whether this webhook wrote a tenant identity into the container, and it
	// records an ACTION rather than an outcome. That distinction is the whole design of this field.
	//
	// Whether the tenant takes effect depends on the engine BUILD, which admission cannot see: it
	// never inspects the image, and the EngineVersion below is the release the facts table was measured
	// at rather than a reading of the container. A build older than that would be handed a variable it
	// never reads, and would then share a cache while a stamp claiming isolation said otherwise.
	// Over-claiming in that direction is the failure
	// this field exists to avoid, so it says what we did - which is certain - and never what
	// resulted, which is not.
	//
	// Reading it, TRUE: a tenant was supplied by this webhook. A build that honors it isolates, and
	// one too old to read the variable silently does not. On the client side the failure is loud
	// instead - a Mooncake too old to accept the argument raises rather than dropping it, so the Pod
	// stops rather than losing isolation quietly.
	//
	// Reading it, FALSE: this webhook wrote no tenant, and that has TWO causes which need different
	// Bindings. Both must be checked, because acting on the first one alone registers a name the Pod
	// never sends:
	//   - Nothing rendered one - the engine forwards no tenant on the path we configure, or the
	//     Binding declared no domain. Writes land on the store's own default tenant, so the pool
	//     needs a Binding registering the literal name "default" or nothing can be written at all.
	//   - The container already declared the tenant variable itself, and the precedence rule in
	//     injectPod left it alone. Writes land on THAT name, not on "default", and the pool needs a
	//     Binding registering it. Read the container's own environment to find which.
	// Domain above says what the Binding declared, which in the second case is not what the
	// container sends - so it does not separate them either.
	TenantInjected bool `json:"tenantInjected"`
}

// injectPod applies a rendered Result to one container of the Pod, and stamps what it did.
//
// It is the only function here that mutates, and it mutates last: every refusal has already run, so a
// Pod reaching this point is either fully injected or untouched. A half-injected Pod - the volume added
// and the env missing, say - would start and behave in a way no field explains.
func (r *PodKVCacheWebhook) injectPod(pod *core.Pod, res *resolution, out *inject.Result) error {
	ctr, err := targetContainer(pod)
	if err != nil {
		return err
	}
	if err = checkOwnedKeys(pod, ctr, out); err != nil {
		return err
	}
	if err = checkLaunchArgs(ctr); err != nil {
		return err
	}

	// tenantApplied starts from what the renderer produced and is narrowed by what actually lands.
	// The precedence rule below can drop any rendered variable, and for the environment vehicle the
	// tenant is one of them - so a stamp built from the renderer's answer alone would claim a tenant
	// while the container ran under the workload's own. The record has to describe the container.
	tenantApplied := out.TenantInjected
	for i := range out.Env {
		// An injection never overrules a variable the workload declared for itself. This is the
		// repository's existing rule and its existing helper; a second precedence invented here would
		// mean two answers to one question.
		if !deviceplugin.ContainerEnvDeclared(ctr, out.Env[i].Name) {
			ctr.Env = append(ctr.Env, out.Env[i])
			continue
		}
		if out.TenantEnvName != "" && out.Env[i].Name == out.TenantEnvName {
			tenantApplied = false
		}
	}
	for _, name := range []string{observabilityMetricsEnv, observabilityBandwidthEnv} {
		if !deviceplugin.ContainerEnvDeclared(ctr, name) {
			ctr.Env = append(ctr.Env, core.EnvVar{Name: name, Value: "1"})
		}
	}

	ctr.Args = append(ctr.Args, out.Args...)
	ctr.VolumeMounts = append(ctr.VolumeMounts, out.VolumeMounts...)
	pod.Spec.Volumes = append(pod.Spec.Volumes, out.Volumes...)

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	for k, v := range out.PodAnnotations {
		pod.Annotations[k] = v
	}

	vehicle := "environment"
	if len(out.Volumes) > 0 {
		vehicle = "file"
	}
	record, err := json.Marshal(injectionRecord{
		Binding:       pod.Annotations[KVCacheBindingAnnotationKey],
		Engine:        string(res.Input.Engine),
		EngineVersion: res.Isolation.EngineVersion,
		Vehicle:       vehicle,
		Domain:        res.Isolation.Domain,
		// From what was applied, not from what was rendered. The two differ exactly when the
		// workload declared the tenant variable itself and the precedence rule above left it alone.
		TenantInjected: tenantApplied,
	})
	if err != nil {
		// UNREACHABLE, and left in rather than dropped because dropping it would mean ignoring an
		// error return. injectionRecord is six strings and a bool, and encoding/json fails only on
		// values it cannot represent - channels, functions, cyclic references, NaN and infinities -
		// none of which this struct can hold. The branch is therefore not covered by any test, and
		// writing one would mean adding a field that can fail to a record that has no use for it.
		// Do not panic here either: this runs on an admission path, where a panic takes the webhook
		// down for every Pod rather than failing one.
		return fmt.Errorf("marshal the injection record: %w", err)
	}
	pod.Annotations[KVCacheInjectedAnnotationKey] = string(record)

	return nil
}

// targetContainer picks the container to inject into, and refuses rather than guessing.
//
// With one container it is that one. With several the caller must name it, because picking the first
// is exactly how an injection lands in a sidecar while the workload runs in the main container - the
// symptom is "artifacts present, feature absent", which is undiagnosable from outside the Pod.
//
// Init containers are never candidates and naming one is refused. An init container finishes before the
// workload starts, so configuring it caches nothing.
func targetContainer(pod *core.Pod) (*core.Container, error) {
	named := pod.Annotations[KVCacheContainerAnnotationKey]

	if named == "" {
		switch len(pod.Spec.Containers) {
		case 1:
			return &pod.Spec.Containers[0], nil
		case 0:
			return nil, fmt.Errorf("the Pod has no containers to inject into")
		default:
			return nil, fmt.Errorf("the Pod has %d containers (%s), so annotation %q must name the "+
				"one running the engine; this webhook never picks the first",
				len(pod.Spec.Containers), strings.Join(containerNames(pod.Spec.Containers), ", "),
				KVCacheContainerAnnotationKey)
		}
	}

	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == named {
			return &pod.Spec.Containers[i], nil
		}
	}
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == named {
			return nil, fmt.Errorf("annotation %q names %q, which is an init container: it finishes "+
				"before the workload starts, so configuring it would cache nothing. Name one of: %s",
				KVCacheContainerAnnotationKey, named,
				strings.Join(containerNames(pod.Spec.Containers), ", "))
		}
	}
	return nil, fmt.Errorf("annotation %q names %q, which is not a container of this Pod. Name one "+
		"of: %s", KVCacheContainerAnnotationKey, named,
		strings.Join(containerNames(pod.Spec.Containers), ", "))
}

// checkOwnedKeys refuses a container that already carries a key selecting a KV cache mechanism, or a
// volume or mount occupying a name or path this webhook owns.
//
// It refuses rather than merging because every one of these produces an ambiguity nothing reports: two
// connector flags on one command line, or two mounts at one path. Taking over is an explicit opt-out -
// set the inject label to "false", or leave it off.
func checkOwnedKeys(pod *core.Pod, ctr *core.Container, out *inject.Result) error {
	// Only what THIS render will write can collide. The owned lists span every engine, so scanning
	// them unfiltered refused Pods over keys the render was never going to add: an SGLang container
	// setting MOONCAKE_CONFIG_PATH - SGLang selects its file with SGLANG_HICACHE_MOONCAKE_CONFIG_PATH,
	// so that variable means nothing to it - or a vLLM one passing SGLang's
	// --hicache-storage-backend. Neither is a second source for one setting, which is the whole
	// premise of refusing here.
	for _, name := range ownedEnv {
		if !slices.ContainsFunc(out.Env, func(e core.EnvVar) bool { return e.Name == name }) {
			continue
		}
		if deviceplugin.ContainerEnvDeclared(ctr, name) {
			return fmt.Errorf("container %q already sets %s, so it already has a KV cache configured; "+
				"this webhook refuses to merge rather than leave two sources for one setting",
				ctr.Name, name)
		}
	}
	for _, flag := range ownedArgs {
		if !slices.Contains(out.Args, flag) {
			continue
		}
		// command is scanned as well as args, because a user may put the flag in either.
		if hasFlag(ctr.Args, flag) || hasFlag(ctr.Command, flag) {
			return fmt.Errorf("container %q already passes %s, so it already has a KV cache "+
				"configured; two of them on one command line is undiagnosable", ctr.Name, flag)
		}
	}

	// The Pod's own volume list is checked as well as the container's mounts. A volume of this name
	// can exist on the Pod without the target container mounting it, and appending a second one of
	// the same name is refused by the API server with an error that names neither this webhook nor
	// the manifest line that collided.
	for i := range out.Volumes {
		for j := range pod.Spec.Volumes {
			if pod.Spec.Volumes[j].Name == out.Volumes[i].Name {
				return fmt.Errorf("the Pod already declares a volume named %q, which this webhook owns",
					out.Volumes[i].Name)
			}
		}
	}

	for i := range out.VolumeMounts {
		for j := range ctr.VolumeMounts {
			switch {
			case ctr.VolumeMounts[j].Name == out.VolumeMounts[i].Name:
				return fmt.Errorf("container %q already mounts a volume named %q, which this webhook "+
					"owns", ctr.Name, out.VolumeMounts[i].Name)
			case ctr.VolumeMounts[j].MountPath == out.VolumeMounts[i].MountPath:
				return fmt.Errorf("container %q already mounts something at %q, which this webhook "+
					"owns", ctr.Name, out.VolumeMounts[i].MountPath)
			}
		}
	}
	return nil
}

// checkLaunchArgs refuses a container that declares neither command nor args.
//
// Appending to an empty args does not append: Kubernetes reads args alone as the whole command line and
// discards the image's CMD entirely, so the engine would lose its own launch arguments. This webhook
// cannot see the image and cannot restore them.
//
// WHY A COMMAND WITH NO ARGS IS NOT THE SAME CASE, written here because review has raised it three
// times and a reply on a thread does not survive the thread being resolved. The claim each time is
// that `command` set with `args` absent still runs the image's CMD, so appending to args silently
// replaces it. Kubernetes resolves the four combinations like this:
//
//	command unset, args unset  -> image ENTRYPOINT + image CMD
//	command unset, args set    -> image ENTRYPOINT + args        (CMD dropped)
//	command SET,   args unset  -> command alone                  (ENTRYPOINT and CMD BOTH dropped)
//	command set,   args set    -> command + args
//
// So on the third row there is no CMD left to replace: setting command already discarded it, before
// this webhook appends anything. The container's author dropped it, not the injection. That row is
// also why the refusal below fires on neither-declared rather than on args-empty - a container with
// only a command is complete, and appending to its args is the ordinary case this webhook is for.
func checkLaunchArgs(ctr *core.Container) error {
	if err := checkShellWrapper(ctr); err != nil {
		return err
	}
	if len(ctr.Command) > 0 || len(ctr.Args) > 0 {
		return nil
	}
	return fmt.Errorf("container %q declares neither command nor args, so it runs the image's own "+
		"ENTRYPOINT and CMD. Both engines need a flag on the command line, and setting args while "+
		"command is unset makes Kubernetes discard the image's CMD - copy the image's launch "+
		"arguments into args, leaving command unset. Do not move them into command: that would "+
		"override the ENTRYPOINT too, which on these images initializes the vendor runtime",
		ctr.Name)
}

// shellNames are the interpreters for which "-c" means "the next argument is the whole script".
var shellNames = []string{"sh", "bash", "dash", "ash", "zsh", "ksh"}

// checkShellWrapper refuses a container whose launch cannot be SHOWN to carry an appended argument
// through to the engine. It fails closed: "cannot tell" is a refusal with a message, not an
// admission.
//
// Three shapes are refused, and the message says which, because the reader's next move differs:
//
//	a shell's -c            the appended flag becomes $0 and $1 and never reaches the engine
//	a command in one token  `env -S "sh -c ..."` - there is nothing on the command line to test
//	a script by path        whether it forwards "$@" is inside the image, not on the command line
//
// WHAT THIS DOES NOT DO is decide whether the launcher enumeration below is complete, and no
// reading of it should be taken as evidence either way. A launcher this parser has never heard of
// still resolves to itself and is still admitted, exactly as before, because an unrecognized
// launcher is indistinguishable from an ordinary program by construction. That gap is the one
// tracked as #169; failing closed narrows the OTHER two, which are detectable.
//
// After `sh -c <script>`, everything further is a POSITIONAL PARAMETER: the flag becomes $0 and its
// value $1, and the shell never passes either to what it runs. Measured, not assumed -
// `sh -c 'echo "$0 [$*]"' --kv-config VAL` prints `--kv-config [VAL]`. The Pod then starts, the stamp
// says it was injected, the projected file is mounted, and the connector is simply never enabled: a
// silent outcome whose visible symptom is a cache that does not hit.
//
// The test is on the CONCATENATED argv, and that is the whole correctness of it. Kubernetes runs
// command followed by args, so these two spellings produce identical processes:
//
//	command: ["/bin/sh", "-c"]  args: ["<script>"]
//	command: ["/bin/sh"]        args: ["-c", "<script>"]
//
// A check written against `command` alone catches the first and misses the second. Neither field is
// the object that decides the behavior; their concatenation is.
//
// Both conditions are needed, and the first is what keeps the refusal narrow. `-c` is an ordinary
// option for ordinary programs - `command: ["myapp", "-c", "config.yaml"]` is common and has no
// problem here. Requiring argv[0] to be a shell narrows the refusal to the case where the behavior
// is guaranteed by POSIX rather than inferred from an absence of counter-examples.
func checkShellWrapper(ctr *core.Container) error {
	argv, insideOneString := shellLaunchArgv(slices.Concat(ctr.Command, ctr.Args))
	if insideOneString {
		return fmt.Errorf("container %q hides its command line inside a single argument, so this "+
			"webhook cannot tell what would run or whether appending to args would reach it. "+
			"Launch the engine directly - put its executable in command and its arguments in args - "+
			"or add the connector flag inside that argument yourself", ctr.Name)
	}
	if len(argv) == 0 {
		return nil
	}
	program := path.Base(argv[0])
	if !slices.Contains(shellNames, program) {
		// A LAUNCHER CAN BE AN ARBITRARY SCRIPT, AND ITS TRANSPARENCY LIVES IN ITS BODY. Whether
		// appending reaches the engine depends on whether the script forwards "$@" - which is the
		// contents of a file inside the image, not a token on the command line. No enumeration
		// closes that: it is not a launcher this parser has not heard of, it is a launcher whose
		// behavior is not on the command line at all. Measured on the images this repository names:
		// lmsysorg/sglang runs `/opt/nvidia/nvidia_entrypoint.sh`, and admitting it was never
		// justified, only lucky.
		//
		// So this refuses rather than guesses, which is the whole point of failing closed: it turns
		// "cannot tell" from a silent admission into a message. The cost is a script that DOES
		// forward "$@" and is now refused, and the remedy the message names is the same one the
		// shell case gives.
		if strings.HasSuffix(program, ".sh") {
			return fmt.Errorf("container %q runs the script %q, and whether anything this webhook "+
				"appends to args reaches the engine depends on whether that script forwards its "+
				"arguments - which admission cannot read, because it is inside the image. Launch "+
				"the engine directly - put its executable in command and its arguments in args - "+
				"or add the connector flag inside the script yourself", ctr.Name, program)
		}

		return nil
	}
	// A shell stops reading options at its first operand: `sh /app/run.sh -c config` passes -c to the
	// SCRIPT as $1, and that launch is perfectly appendable. An earlier revision scanned the whole
	// argv and refused those, turning an ordinary command line into a startup failure. So this walks
	// only the option prefix and stops exactly where the shell would.
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			// Options ended without -c; whatever follows is the command file.
			return nil
		case len(arg) < 2 || (arg[0] != '-' && arg[0] != '+'):
			// The command file itself. "-" alone is an operand too, not an option bundle.
			return nil
		case isShellCommandFlag(arg):
			// Fall through to the refusal. Checked BEFORE the operand rules below, because a bundle can
			// be both - `-co` is command mode whose -o still takes an operand.
		case slices.Contains(shellOperandLong, arg):
			// A long option whose operand is the next token, which is a startup FILE rather than the
			// command file: `bash --rcfile /tmp/x -c '<script>'` still reads -c. Skipping it is what
			// keeps the scan going; stopping there would admit that launch.
			i++
			continue
		case arg[len(arg)-1] == 'o' || arg[len(arg)-1] == 'O':
			// -o and bash's -O take an option NAME as their operand, and so do bundles ending in them
			// (-euo pipefail). That operand is never the command file, so the scan must skip it rather
			// than stop there.
			i++
			continue
		default:
			continue
		}
		return fmt.Errorf("container %q runs %q through a shell's %q, and anything this webhook "+
			"appends to args would become the shell's positional parameters ($0, $1) rather than "+
			"reaching the engine. The Pod would start, record itself as injected, and never enable "+
			"the connector. Launch the engine directly - put its executable in command and its "+
			"arguments in args - or add the connector flag to the script yourself",
			ctr.Name, program, arg)
	}
	return nil
}

// launcherGrammar is the only part of a launcher's option syntax that matters here: which of its
// options take the FOLLOWING token as their operand. Everything else is a flag or the command.
type launcherGrammar struct {
	// operandShort holds the option letters whose operand is a separate token, spelled as they
	// appear after a single dash.
	operandShort string
	// operandLong holds the long options that accept a separated operand. The --opt=value form
	// needs no entry, since the value travels inside the token.
	operandLong []string
	// positionalOperands is how many operands of the launcher's OWN sit between its options and the
	// command, as `timeout DURATION COMMAND` has one. Zero means the first non-option token is the
	// command, which is the shape of every other entry here.
	//
	// A launcher with a positional operand used to be unresolvable for that exact reason. It is
	// resolvable once the count is known, and `gosu USER` / `chroot DIR` are each a count of one -
	// but they are NOT listed below: their gap is an open review thread awaiting a human decision on
	// whether to parse them or to narrow the documented promise, and quietly closing it here would
	// answer that question by editing rather than by deciding.
	positionalOperands int
}

// shellLaunchers exec whatever follows their own arguments, so a shell behind one still ends up as
// the process that reads -c. Their grammar is options-then-command, which is what makes them
// resolvable; a launcher taking a positional operand first is not (see shellLaunchArgv).
//
// This map, its per-launcher option letters, and shellOperandLong below are three ENUMERATIONS, and
// none of them is closed: which programs are launchers, which of their options consume a token, and
// which of a shell's do. Four rounds of review each found one more. An entry missing here resolves
// to the wrong program and ADMITS the container, so the failure is silent. Tracked as #169, which
// records the unbounded denominator rather than any single gap.
//
// Each entry is copied from that program's own option list rather than generalised, because a wrong
// entry hides the shell in EITHER direction: assume too few operands and `env -C /tmp sh -c ...`
// reports /tmp as the program, assume too many and `env -i sh -c ...` reports -c.
var shellLaunchers = map[string]launcherGrammar{
	// GNU coreutils env(1): -u NAME, -C DIR, -S STRING, -a ARG, and the long forms of all four, which
	// getopt_long also accepts separated. Its three --*-signal options take an OPTIONAL operand
	// (`[=SIG]`), so their separated form consumes nothing and they are correctly absent here.
	"env": {
		operandShort: "uCSa",
		operandLong:  []string{"--unset", "--chdir", "--split-string", "--argv0"},
	},
	// tini(1) parses with plain getopt, so only -p SIGNAL and -e EXIT_CODE reach past themselves.
	"tini": {operandShort: "pe"},
	// dumb-init(1): -r/--rewrite takes a signal remapping.
	"dumb-init": {operandShort: "r", operandLong: []string{"--rewrite"}},
	// Multi-call binaries. Their first operand names the applet to run, so `busybox sh -c ...` runs a
	// shell this check has to see. That operand looks positional like gosu's user or chroot's
	// directory, but it IS the program rather than an argument to one, which is exactly what makes
	// these resolvable and those two not. Neither consumes a following token for any option.
	"busybox": {},
	"toybox":  {},
	// timeout(1) runs `timeout [OPTION] DURATION COMMAND [ARG]...`, so one operand of its own sits
	// between its options and the command. Without the count, `timeout 30 sh -c 'vllm ...'` resolved
	// to `timeout` - not a shell - and was ADMITTED while the appended flag became the shell's $0.
	// It is a common wrapper in container command lines, and unlike gosu/chroot it was not on the
	// documented list of shapes this parser knowingly lets through.
	"timeout": {
		operandShort:       "sk",
		operandLong:        []string{"--signal", "--kill-after"},
		positionalOperands: 1,
	},
}

// shellOperandLong lists the shell long options whose operand is the NEXT token. Among the shells
// named above, bash's --rcfile and its synonym --init-file are the whole set; every other long
// option those shells take is a flag. bash parses these itself rather than with getopt_long and does
// not accept an --opt=value form, so there is no inline spelling to strip - and were one written
// anyway, it falls through as an unrecognized flag and the scan still reaches the -c behind it.
var shellOperandLong = []string{"--rcfile", "--init-file"}

// shellLaunchArgv resolves transparent launcher prefixes, so the check below sees the program that
// actually runs rather than the first word on the command line.
//
// This exists because testing argv[0] has now been walked around three times: -c moved from command
// into args, a -c that belonged to the script rather than the shell, and `env sh -c`. The third one
// is not a fourth special case to add - it says the first word is simply not always the program. So
// the prefix is RESOLVED rather than enumerated against.
//
// SCOPE, stated because the gap is real. Two shapes are not resolved, and both are admitted and
// injected - the same outcome as before this function existed, not a new one:
//   - A launcher whose own first positional operand is an ARGUMENT rather than the program: `gosu
//     USER sh -c`, `chroot DIR sh -c`. Nothing here distinguishes that operand from a command.
//     (A multi-call binary's applet name is not this case - see busybox below.)
//   - `env -S "sh -c ..."`, where the whole command line is one string this parser cannot look
//     inside. -S is stepped over as an ordinary operand, which leaves no command to test.
//
// insideOneString reports that the command line, if there is one, sits inside a single token this
// parser cannot look into - `env -S "sh -c ..."` being the shape that motivates it. The argv is nil
// in that case, which is indistinguishable from "nothing to run" without this second return, and
// those two deserve opposite answers: nothing to run is admitted, a command hidden in a string is
// refused.
func shellLaunchArgv(argv []string) (resolved []string, insideOneString bool) {
	for len(argv) > 0 {
		grammar, ok := shellLaunchers[path.Base(argv[0])]
		if !ok {
			return argv, false
		}

		var swallowed bool
		argv, swallowed = stripLauncherOperands(argv[1:], grammar)
		if swallowed {
			return nil, true
		}
	}

	return argv, false
}

// stripLauncherOperands drops one launcher's own arguments - NAME=value assignments, its flags, and
// the operands its options consume - and returns the argv starting at the command it will exec.
//
// swallowed reports that an operand-taking option consumed the last token there was, so any command
// line is inside that token rather than after it. It is returned rather than folded into an empty
// argv because an argv that simply ENDED - `tini --` with the command supplied separately - is
// appendable, and one whose command was swallowed is not.
//
// TREATING EVERY EMPTY RESULT AS "CANNOT TELL" IS THE CONSERVATIVE DEFAULT AND IT IS WRONG ON THE
// COMMONEST SHAPE THERE IS. `tini --` resolves to an empty argv and is perfectly appendable: the
// appended arguments ARE the command tini execs. Measured over the runner image families this
// operator synthesizes, 56 of 60 ship exactly that entrypoint - so folding the two cases together
// would not cost the one refusal failing closed is meant to cost, it would refuse almost everything
// and make the check unusable. The distinction is cheap and local; the default that looks safe is
// not.
func stripLauncherOperands(argv []string, grammar launcherGrammar) (resolved []string, swallowed bool) {
	// Operands of the launcher's own that sit before the command, consumed as they are met. A
	// launcher declaring none behaves exactly as before: the first non-option token is the command.
	positional := grammar.positionalOperands
	for len(argv) > 0 {
		arg := argv[0]
		takesNext := false
		switch {
		case arg == "--":
			return argv[1:], false
		case strings.HasPrefix(arg, "--"):
			name, _, inline := strings.Cut(arg, "=")
			takesNext = !inline && slices.Contains(grammar.operandLong, name)
		case arg != "" && arg[0] == '-':
			// A lone "-" carries no letters, so it reaches past nothing; env(1) reads it as -i.
			takesNext = bundleTakesNextToken(arg[1:], grammar.operandShort)
		case strings.Contains(arg, "="):
			// A NAME=value assignment, which env(1) accepts before the command.
		case positional > 0:
			// An operand belonging to the launcher, not the command: timeout's DURATION. Consumed
			// before the default arm so the token after it is still tested as a program.
			positional--
		default:
			return argv, false
		}
		if takesNext {
			// Two tokens left means this option's operand IS the last one, so a command line, if the
			// author wrote one, is inside it. One token left is the same situation with the operand
			// missing. Both are the swallowed case; anything longer leaves a program to test.
			if len(argv) <= 2 {
				return nil, true
			}
			argv = argv[2:]

			continue
		}
		argv = argv[1:]
	}

	return argv, false
}

// bundleTakesNextToken reports whether a short-option bundle reaches past itself for an operand.
//
// An operand-taking letter consumes whatever remains of the bundle when there is any - `-uNAME`
// unsets NAME - so only a bundle ENDING in such a letter takes the token that follows. That is the
// same rule the shell scan applies to -o, and getting it wrong either way hides a shell.
func bundleTakesNextToken(bundle, operandLetters string) bool {
	for i, letter := range bundle {
		if strings.ContainsRune(operandLetters, letter) {
			return i == len(bundle)-1
		}
	}
	return false
}

// isShellCommandFlag reports whether a shell option bundle contains "c", covering -c as well as the
// combined forms a wrapper may use, such as -lc or -euc. A long option is never one of these.
func isShellCommandFlag(arg string) bool {
	if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") || len(arg) < 2 {
		return false
	}
	return strings.ContainsRune(arg[1:], 'c')
}

// hasFlag reports whether a flag appears in an argument list, including in its --flag=value form.
func hasFlag(args []string, flag string) bool {
	return slices.ContainsFunc(args, func(arg string) bool {
		return arg == flag || strings.HasPrefix(arg, flag+"=")
	})
}

// containerNames lists container names for a refusal a reader can act on.
func containerNames(ctrs []core.Container) []string {
	names := make([]string, 0, len(ctrs))
	for i := range ctrs {
		names = append(names, ctrs[i].Name)
	}
	return names
}
