package kernelkey

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	// EngineVLLM is the canonical lowercase engine segment for vLLM deployments.
	EngineVLLM = "vllm"
	// EngineSGLang is the canonical lowercase engine segment for SGLang deployments.
	EngineSGLang = "sglang"
)

var (
	// gitDescribeSuffixPattern strips git commit suffixes such as -12-g1a2b3c4,
	// -g1a2b3c4, .g1a2b3c4, -git1a2b3c4, or -dirty without touching pre-release tags
	// like rc1, a1, b2, dev0, or -rc.1.
	gitDescribeSuffixPattern = regexp.MustCompile(`(?:-\d+)?[-.](?:g|git)[0-9a-fA-F]{6,}(?:[-.].*)?$`)
	dirtySuffixPattern       = regexp.MustCompile(`[-.]dirty$`)

	// normalizedVersionPattern accepts major.minor(.patch(.micro)) followed by optional
	// PEP 440 or SemVer pre-release/post-release tags, without slashes or whitespace.
	normalizedVersionPattern = regexp.MustCompile(
		`^(\d+)\.(\d+)(?:\.(\d+)(?:\.(\d+))?)?(?:(?:\.?[ab]|(?:\.?|-)(?:rc|alpha|beta|post|dev))\d*(?:\.\d+)*)?$`,
	)
	bareMajorMinorPattern = regexp.MustCompile(`^(\d+)\.(\d+)$`)
)

// NormalizeEngine converts an inference engine name (such as "vLLM" or "SGLang")
// to its canonical lowercase LocalKernelKey engine segment.
func NormalizeEngine(engine string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case EngineVLLM:
		return EngineVLLM, nil
	case EngineSGLang:
		return EngineSGLang, nil
	case "":
		return "", fmt.Errorf("engine must not be empty")
	default:
		return "", fmt.Errorf("unsupported engine %q: must be vllm or sglang", engine)
	}
}

// NormalizeEngineVersion strips build metadata (+cu128, +rocm6.3, +g...) and
// git commit suffixes while preserving major.minor.patch and pre-release tags.
// Two-part versions such as "0.34" are padded to "0.34.0".
func NormalizeEngineVersion(version string) (string, error) {
	v := strings.TrimSpace(version)
	if v == "" {
		return "", fmt.Errorf("engineVersion must not be empty")
	}
	if strings.ContainsAny(v, " \t\r\n/") {
		return "", fmt.Errorf("engineVersion %q contains invalid characters", version)
	}

	if len(v) > 1 && (v[0] == 'v' || v[0] == 'V') && v[1] >= '0' && v[1] <= '9' {
		v = v[1:]
	}

	if plus := strings.IndexByte(v, '+'); plus >= 0 {
		v = v[:plus]
	}
	v = gitDescribeSuffixPattern.ReplaceAllString(v, "")
	v = dirtySuffixPattern.ReplaceAllString(v, "")

	if bareMajorMinorPattern.MatchString(v) {
		v += ".0"
	}

	if !normalizedVersionPattern.MatchString(v) {
		return "", fmt.Errorf("invalid engineVersion %q", version)
	}

	return v, nil
}
