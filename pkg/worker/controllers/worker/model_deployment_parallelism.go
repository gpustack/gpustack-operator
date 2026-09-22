package worker

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ModelDeploymentDeclaredParallelism is the parallel shape one role's author wrote on the
// books: the degree flags of the role's engine parsed from the role's own argument stream --
// its ExtraArgs, or its Command when that replaces the line, never both -- plus the one degree
// the engine accepts as a literal environment entry. A degree nothing declares stays at its
// engine's own default of one -- the operator composes no degree of its own, because the
// transfer document must agree with the engine, and only the author's numbers can be right.
type ModelDeploymentDeclaredParallelism struct {
	TensorParallel   int
	PipelineParallel int
	DataParallel     int
	// DataParallelLocal is vLLM's --data-parallel-size-local: the share of the DP width placed
	// on this member. Zero means undeclared -- the engine then places the full width here and
	// the size check multiplies. A declared share above zero IS the per-member data-parallel
	// width -- with no wiring flag it is exactly the ranks this member runs -- so the check
	// multiplies that instead. A DECLARED zero stores the same zero: it is the engine's own
	// sentinel for DP specified externally, the check reads it as undeclared, and the env
	// precedence still honors it.
	DataParallelLocal int
	// The degrees below have no key in today's transfer document; they are carried so the size
	// check and any future consumer read the full picture without a parser change.
	PrefillContextParallel int
	DecodeContextParallel  int
	// ExpertParallel is SGLang's degree. vLLM's same-named concept is a mode, so it lands in
	// Modes instead -- the two spellings are never merged into one field. The three after it
	// are SGLang's remaining degrees: the size check branches on their presence, a future
	// consumer on their values.
	ExpertParallel           int
	AttentionContextParallel int
	MoEDataParallel          int
	DWDPSize                 int
	// Modes records the mode flags seen, by canonical spelling (expert-parallel, DP
	// attention, prefill CP); consumers branch on them, as the size check does.
	Modes []string
	// Wiring records the wiring flags seen, by canonical spelling (--nnodes and its family,
	// the --data-parallel-* collective flags, --dist-init-addr): any of them means some of
	// the width may live off this member, so the size check stays silent.
	Wiring []string
}

// modelDeploymentParallelFlag is one table row: every accepted spelling of one flag, its kind
// (degree, mode or wiring) and its arity. A row is exactly one kind.
type modelDeploymentParallelFlag struct {
	// names carries every ENGINE-REGISTERED spelling in dash form, the canonical long spelling
	// first: it is what Modes, Wiring and error messages record. A registered spelling matches
	// an exact token and its "--flag=value" form; a single-letter one also matches the
	// concatenated form ("-n2"), argparse's short-option rule.
	names []string
	// abbr carries single-dash ABBREVIATIONS argparse resolves to this row: not registered
	// spellings but unique prefixes of one ("-t" of "-tp"), which argparse's single-dash
	// branch prefix-matches on every supported Python. They match a BARE token only: glued
	// to "=" the form forks by interpreter version -- 3.12 and up read "-t=2" as this row
	// while 3.10 and 3.11 refuse the token -- so a degree abbreviation carrying "=" is an
	// error at parse, and a mode one passes through because every version refuses it. Glued
	// to a bare value ("-t2") every version refuses the token as well, and that is an error
	// at parse too: a crash certain to happen at engine start is better refused at
	// admission, where the message can name the registered spelling. Each abbreviation was
	// verified unique against the engine's full single-dash option set
	// (today: -q -n -r -tp -pp -dcp -pcp -dp -dpn -dpr -dpl -dpa -dpp -dpb -dph -dpe -dpm -ep
	// -sc -dc -cc -ac); a future engine option that collides turns the form into the
	// engine's own ambiguous refusal, the loud direction.
	abbr []string
	// degree marks a flag whose integer value is validated and stored. A degree always takes
	// a value.
	degree bool
	// min is the lower bound a degree value is refused below: one everywhere but vLLM's
	// local data-parallel share, whose engine accepts zero as a sentinel.
	min int
	// mode marks a flag that takes no value and is recorded in Modes by presence.
	mode bool
	// wiring marks a flag recorded in Wiring by presence; takesValue says whether a value
	// token follows, which the scan must skip to keep the stream aligned.
	wiring     bool
	takesValue bool
}

// modelDeploymentParallelFlags is the recognition table, one per engine the API accepts. The
// rows are built from the engines' own parsers (vllm/engine/arg_utils.py,
// sglang/srt/arg_groups/fields/parallel.py); an engine with no entry parses to all ones
// without error, and recognizing a new flag is adding a row, not touching the parser.
var modelDeploymentParallelFlags = map[string][]modelDeploymentParallelFlag{
	workercore.ModelDeploymentEngineVLLM: {
		{names: []string{"--tensor-parallel-size", "-tp"}, abbr: []string{"-t"}, degree: true, min: 1},
		{names: []string{"--pipeline-parallel-size", "-pp"}, degree: true, min: 1},
		{names: []string{"--data-parallel-size", "-dp"}, degree: true, min: 1},
		{names: []string{"--data-parallel-size-local", "-dpl"}, degree: true, min: 0},
		{names: []string{"--prefill-context-parallel-size", "-pcp"}, abbr: []string{"-pc"}, degree: true, min: 1},
		{names: []string{"--decode-context-parallel-size", "-dcp"}, degree: true, min: 1},
		{names: []string{"--enable-expert-parallel", "-ep"}, abbr: []string{"-e"}, mode: true},
		{names: []string{"--nnodes", "-n"}, wiring: true, takesValue: true},
		{names: []string{"--node-rank", "-r"}, wiring: true, takesValue: true},
		{names: []string{"--master-addr"}, wiring: true, takesValue: true},
		{names: []string{"--master-port"}, wiring: true, takesValue: true},
		{names: []string{"--data-parallel-rank", "-dpn"}, wiring: true, takesValue: true},
		{names: []string{"--data-parallel-start-rank", "-dpr"}, wiring: true, takesValue: true},
		{names: []string{"--data-parallel-address", "-dpa"}, wiring: true, takesValue: true},
		{names: []string{"--data-parallel-rpc-port", "-dpp"}, wiring: true, takesValue: true},
		{names: []string{"--data-parallel-backend", "-dpb"}, wiring: true, takesValue: true},
		{names: []string{"--data-parallel-hybrid-lb", "-dph"}, wiring: true},
		{names: []string{"--data-parallel-external-lb", "-dpe"}, wiring: true},
		{names: []string{"--data-parallel-multi-port-external-lb", "-dpm"}, wiring: true},
	},
	workercore.ModelDeploymentEngineSGLang: {
		{names: []string{"--tp-size", "--tensor-parallel-size"}, degree: true, min: 1},
		{names: []string{"--pp-size", "--pipeline-parallel-size"}, degree: true, min: 1},
		{names: []string{"--dp-size", "--data-parallel-size"}, degree: true, min: 1},
		{names: []string{"--ep-size", "--expert-parallel-size", "--ep"}, degree: true, min: 1},
		{names: []string{"--dcp-size", "--decode-context-parallel-size"}, degree: true, min: 1},
		{names: []string{"--attn-cp-size", "--attention-context-parallel-size"}, degree: true, min: 1},
		{names: []string{"--moe-dp-size", "--moe-data-parallel-size"}, degree: true, min: 1},
		{names: []string{"--dwdp-size"}, degree: true, min: 1},
		{names: []string{"--enable-dp-attention"}, mode: true},
		{names: []string{"--enable-prefill-cp"}, mode: true},
		{names: []string{"--nnodes"}, wiring: true, takesValue: true},
		{names: []string{"--node-rank"}, wiring: true, takesValue: true},
		{names: []string{"--dist-init-addr", "--nccl-init-addr"}, wiring: true, takesValue: true},
	},
}

// ParseModelDeploymentDeclaredParallelism reads one role's books the way the role's engine
// would: both value forms, last-wins on a repeat, underscore spellings only where the engine's
// own parser rewrites them, a unique prefix resolving and an ambiguous one passing through for
// the engine itself to refuse, the single-dash abbreviations the engine itself resolves and a
// concatenated value on a single-letter registered spelling, a bare "--" ending recognition,
// and a negative number consumed as a value exactly as argparse consumes it. Values parse with
// the engine's own integer alphabet. It errors only on a KNOWN degree flag whose value is
// missing, non-integer, out of range or below its bound, and on a VLLM_DP_SIZE that is
// unreadable while being the effective DP source -- every other token is the engine's own
// business and passes through, which is what keeps an unknown flag from ever becoming a wrong
// answer here.
//
// The DP env precedence mirrors the engine (parallel.py: data_parallel_size above one, or a
// local size declared as zero, takes the CLI path and the variable is never read; otherwise
// the variable supplies the degree), so a doubly stated DP cannot put the relay and the engine
// on different numbers.
func ParseModelDeploymentDeclaredParallelism(
	engine string, extraArgs []string, env []workercore.ModelDeploymentEnvVar,
) (ModelDeploymentDeclaredParallelism, error) {
	declared := ModelDeploymentDeclaredParallelism{
		TensorParallel:           1,
		PipelineParallel:         1,
		DataParallel:             1,
		PrefillContextParallel:   1,
		DecodeContextParallel:    1,
		ExpertParallel:           1,
		AttentionContextParallel: 1,
		MoEDataParallel:          1,
		DWDPSize:                 1,
	}

	table := modelDeploymentParallelFlags[engine]

	localZeroDeclared := false

	for i := 0; i < len(extraArgs); i++ {
		token := extraArgs[i]
		if token == "--" {
			break
		}

		flag, value, hasValue, ok, err := matchModelDeploymentParallelFlag(engine, table, token)
		if err != nil {
			return declared, err
		}
		if !ok {
			continue
		}
		canonical := flag.names[0]

		switch {
		case flag.mode:
			// A mode flag takes no value; an explicit one is the engine's own refusal, so it
			// passes through unrecorded.
			if hasValue {
				continue
			}
			if !slices.Contains(declared.Modes, canonical) {
				declared.Modes = append(declared.Modes, canonical)
			}
		case flag.wiring:
			// A wiring flag that takes no value is the engine's own refusal when one is
			// attached; it passes through unrecorded.
			if hasValue && !flag.takesValue {
				continue
			}
			if !slices.Contains(declared.Wiring, canonical) {
				declared.Wiring = append(declared.Wiring, canonical)
			}
			if flag.takesValue && !hasValue && i+1 < len(extraArgs) &&
				modelDeploymentConsumableValue(extraArgs[i+1]) {
				i++
			}
		case flag.degree:
			if !hasValue {
				if i+1 >= len(extraArgs) || !modelDeploymentConsumableValue(extraArgs[i+1]) {
					return declared, fmt.Errorf("the %s declaration has no value", canonical)
				}
				i++
				value = extraArgs[i]
			}

			degree, err := modelDeploymentEngineInt(value)
			switch {
			case errors.Is(err, errModelDeploymentIntRange):
				return declared, fmt.Errorf("the %s value %q is out of range", canonical, value)
			case err != nil:
				return declared, fmt.Errorf("the %s value %q is not an integer", canonical, value)
			}
			if degree < flag.min {
				return declared, fmt.Errorf("the %s value %d is below %d", canonical, degree, flag.min)
			}

			switch canonical {
			case "--tensor-parallel-size", "--tp-size":
				declared.TensorParallel = degree
			case "--pipeline-parallel-size", "--pp-size":
				declared.PipelineParallel = degree
			case "--data-parallel-size", "--dp-size":
				declared.DataParallel = degree
			case "--data-parallel-size-local":
				declared.DataParallelLocal = degree
				localZeroDeclared = degree == 0
			case "--prefill-context-parallel-size":
				declared.PrefillContextParallel = degree
			case "--decode-context-parallel-size", "--dcp-size":
				declared.DecodeContextParallel = degree
			case "--ep-size":
				declared.ExpertParallel = degree
			case "--attn-cp-size":
				declared.AttentionContextParallel = degree
			case "--moe-dp-size":
				declared.MoEDataParallel = degree
			case "--dwdp-size":
				declared.DWDPSize = degree
			}
		}
	}

	if engine != workercore.ModelDeploymentEngineVLLM {
		return declared, nil
	}

	// The engine reads the variable only when the CLI did not drive data parallelism; mirror
	// that rather than rank the two sources ourselves.
	if declared.DataParallel > 1 || localZeroDeclared {
		return declared, nil
	}
	raw, ok := modelDeploymentEnvValue(env, "VLLM_DP_SIZE")
	if !ok {
		return declared, nil
	}
	degree, err := modelDeploymentEngineInt(raw)
	switch {
	case errors.Is(err, errModelDeploymentIntRange):
		return declared, fmt.Errorf("the VLLM_DP_SIZE value %q is out of range", raw)
	case err != nil:
		return declared, fmt.Errorf("the VLLM_DP_SIZE value %q is not an integer", raw)
	}
	if degree < 1 {
		return declared, fmt.Errorf("the VLLM_DP_SIZE value %d is below 1", degree)
	}
	declared.DataParallel = degree

	return declared, nil
}

// matchModelDeploymentParallelFlag resolves one token against the engine's table the way the
// engine's own parser would: vLLM rewrites underscores to dashes in long-flag keys before
// matching, an exact registered spelling wins outright in either value form, a bare
// single-dash abbreviation listed by the row matches next, and a long-flag key otherwise
// resolves by unique-prefix abbreviation -- with the count taken over SPELLINGS, because
// argparse calls a prefix matching two option strings ambiguous even when both belong to one
// action. A token glued to a single-letter registered spelling ("-n2") is that spelling with
// the rest of the token as its value. An ambiguous or unknown token reports no match and
// passes through for the engine to judge. Two tokens ERROR rather than pass through: a degree
// abbreviation carrying "=", which argparse splits for an abbreviated single-dash key only
// from Python 3.12 while 3.10 and 3.11 refuse the token -- the image's version is not knowable
// here, so the token is refused rather than guessed into a document the engine may not run --
// and an abbreviation carrying a glued value ("-t2", "-tp2"), which every supported argparse
// refuses at engine start, refused here instead so the message can name the registered
// spelling.
func matchModelDeploymentParallelFlag(
	engine string, table []modelDeploymentParallelFlag, token string,
) (modelDeploymentParallelFlag, string, bool, bool, error) {
	var zero modelDeploymentParallelFlag

	if !strings.HasPrefix(token, "-") {
		return zero, "", false, false, nil
	}

	key, value, hasValue := token, "", false
	if i := strings.IndexByte(token, '='); i >= 0 {
		key, value, hasValue = token[:i], token[i+1:], true
	}

	long := strings.HasPrefix(key, "--")
	if long && engine == workercore.ModelDeploymentEngineVLLM {
		key = strings.ReplaceAll(key, "_", "-")
	}

	for _, flag := range table {
		if slices.Contains(flag.names, key) {
			return flag, value, hasValue, true, nil
		}
	}

	for _, flag := range table {
		if !slices.Contains(flag.abbr, key) {
			continue
		}
		if hasValue && flag.degree {
			return zero, "", false, false, fmt.Errorf(
				"the %s abbreviation %q is accepted by argparse only from Python 3.12 "+
					"and refused before, so the engine's reading depends on the image; "+
					"use the registered %s",
				key, token, flag.names[0])
		}
		if !hasValue {
			return flag, "", false, true, nil
		}
		// A mode abbreviation carrying a value is the engine's own refusal on every
		// version; it passes through unrecorded.
	}

	if !long {
		if !hasValue && len(token) > 2 {
			for _, flag := range table {
				if slices.Contains(flag.names, token[:2]) {
					return flag, token[2:], true, true, nil
				}
			}
			for _, flag := range table {
				if slices.Contains(flag.abbr, token[:2]) {
					return zero, "", false, false, fmt.Errorf(
						"the %s abbreviation %q carries a glued value, which argparse "+
							"refuses on every supported Python version; use the registered %s",
						token[:2], token, flag.names[0])
				}
			}
		}
		return zero, "", false, false, nil
	}

	spellings := 0
	matched := -1
	for i, flag := range table {
		for _, name := range flag.names {
			if strings.HasPrefix(name, key) {
				spellings++
				matched = i
			}
		}
	}
	if spellings != 1 {
		return zero, "", false, false, nil
	}

	return table[matched], value, hasValue, true, nil
}

// modelDeploymentConsumableValue reports whether the token after a value-taking flag can be
// its value. argparse consumes anything that does not look like an option -- and a token that
// looks like a NEGATIVE NUMBER is not an option to a parser with no numeric option strings,
// which neither engine's table has, so "-2" is consumed here exactly as there.
func modelDeploymentConsumableValue(token string) bool {
	if token == "" || token[0] != '-' {
		return true
	}

	return modelDeploymentLooksLikeNegativeNumber(token)
}

// modelDeploymentLooksLikeNegativeNumber is argparse's own test: ^-\d+$ or ^-\d*\.\d+$.
func modelDeploymentLooksLikeNegativeNumber(token string) bool {
	if len(token) < 2 || token[0] != '-' {
		return false
	}

	rest := token[1:]
	if modelDeploymentAllDigits(rest) {
		return true
	}

	integer, fraction, found := strings.Cut(rest, ".")
	return found && (integer == "" || modelDeploymentAllDigits(integer)) &&
		modelDeploymentAllDigits(fraction)
}

func modelDeploymentAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}

var (
	// errModelDeploymentIntSyntax marks a degree value the engine's own int() conversion
	// would refuse as not an integer.
	errModelDeploymentIntSyntax = errors.New("not an integer")
	// errModelDeploymentIntRange marks a degree value beyond the host's int, which the
	// engine's arbitrary-precision int() would accept but no real deployment can mean.
	errModelDeploymentIntRange = errors.New("out of range")
)

// modelDeploymentEngineInt parses a degree value with the engine's own int() alphabet:
// surrounding whitespace is ignored, one leading sign is allowed, and single underscores may
// sit between digits -- " 3 " is 3, "1_0" is 10, "_3" and "3_" are not integers.
func modelDeploymentEngineInt(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	for i := 0; i < len(s); i++ {
		if s[i] != '_' {
			continue
		}
		if i == 0 || i == len(s)-1 ||
			s[i-1] < '0' || s[i-1] > '9' || s[i+1] < '0' || s[i+1] > '9' {
			return 0, errModelDeploymentIntSyntax
		}
	}

	n, err := strconv.Atoi(strings.ReplaceAll(s, "_", ""))
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return 0, errModelDeploymentIntRange
		}
		return 0, errModelDeploymentIntSyntax
	}

	return n, nil
}

// modelDeploymentEnvValue reads a literal environment entry last-wins, the order the container
// runtime applies to a repeated name.
func modelDeploymentEnvValue(env []workercore.ModelDeploymentEnvVar, name string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if env[i].Name == name {
			return env[i].Value, true
		}
	}

	return "", false
}
