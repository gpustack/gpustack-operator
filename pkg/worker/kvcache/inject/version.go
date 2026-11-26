package inject

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// runnerTagPattern matches gpustack/runner image tags of the form:
// {backend}{backend_version}[-{backend_variant}]-{service}{service_version}[-{suffix}].
// The [XY0-9.]+ class mirrors gpustack/runner to match both HostX.Y/hostX.Y host tags and
// numeric backend versions such as cuda12.4-vllm0.30.0.
var runnerTagPattern = regexp.MustCompile(
	`^(?:(?i:host)|cann|corex|cuda|dtk|hggc|maca|musa|rocm)[XY0-9.]+(?:-[A-Za-z0-9_]+)?-(vllm|sglang)([A-Za-z0-9_.]+)(?:-[A-Za-z0-9_.-]+)?$`,
)

// engineVersionPattern matches PEP 440 / SemVer numeric engine release strings:
// major.minor[.patch[.micro]][(a|b|rc|post|dev)N | .(a|b|rc|post|dev)N].
var engineVersionPattern = regexp.MustCompile(
	`^(\d+)\.(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:\.?(?:a|b|rc|post|dev)\d+)*$`,
)

type engineVersion struct {
	major int
	minor int
	patch int
}

// ParseRunnerEngineVersion extracts the inference engine version from a standard
// gpustack/runner container image reference when the tag's service matches engineName.
//
// It strips any @digest suffix, preserves registry ports before the final slash, and
// requires the repository basename to be "runner" and the tag's service segment to match
// the declared engineName case-insensitively ("vLLM", "vllm", "vllm-ascend" -> "vllm";
// "SGLang", "sglang" -> "sglang"). It returns "", false when the image is digest-only,
// non-runner, unparseable, or disagrees with engineName.
func ParseRunnerEngineVersion(image, engineName string) (string, bool) {
	expectedService, ok := runnerServiceForEngine(engineName)
	if !ok || image == "" || strings.ContainsAny(image, " \t\r\n") {
		return "", false
	}

	if at := strings.IndexByte(image, '@'); at >= 0 {
		image = image[:at]
	}

	lastSlash := strings.LastIndexByte(image, '/')
	if lastSlash == 0 {
		return "", false
	}

	repoAndTag := image[lastSlash+1:]
	colon := strings.IndexByte(repoAndTag, ':')
	if colon < 0 {
		return "", false
	}

	repo := repoAndTag[:colon]
	tag := repoAndTag[colon+1:]
	if repo != "runner" || tag == "" {
		return "", false
	}

	matches := runnerTagPattern.FindStringSubmatch(tag)
	if matches == nil {
		return "", false
	}

	service, serviceVersion := matches[1], matches[2]
	if service != expectedService {
		return "", false
	}
	if err := ValidateEngineVersion(serviceVersion); err != nil {
		return "", false
	}

	return serviceVersion, true
}

// ValidateEngineVersion verifies that version is a non-empty, parseable PEP 440 / SemVer
// numeric engine version (for example "0.34.0", "0.30.0rc1", "0.30.0.rc1", "0.5.15.post1").
func ValidateEngineVersion(version string) error {
	_, err := parseEngineVersion(version)
	return err
}

// CompareEngineVersion compares two engine versions by their (major, minor, patch) release
// tuple, treating a missing patch as 0. Pre-release and post-release builds on a release line
// (such as "0.30.0rc1", "0.30.0.rc1", "0.34.0rc0") compare equal to that release line's tuple.
// It returns -1 if a < b, 0 if a == b, and +1 if a > b.
func CompareEngineVersion(a, b string) (int, error) {
	va, err := parseEngineVersion(a)
	if err != nil {
		return 0, fmt.Errorf("invalid engine version %q: %w", a, err)
	}
	vb, err := parseEngineVersion(b)
	if err != nil {
		return 0, fmt.Errorf("invalid engine version %q: %w", b, err)
	}

	if c := cmp.Compare(va.major, vb.major); c != 0 {
		return c, nil
	}
	if c := cmp.Compare(va.minor, vb.minor); c != 0 {
		return c, nil
	}
	return cmp.Compare(va.patch, vb.patch), nil
}

// EngineVersionAtLeast reports whether version is at least minimum under CompareEngineVersion.
func EngineVersionAtLeast(version, minimum string) (bool, error) {
	c, err := CompareEngineVersion(version, minimum)
	if err != nil {
		return false, err
	}
	return c >= 0, nil
}

func runnerServiceForEngine(engineName string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(engineName)) {
	case "vllm", string(EngineVLLMAscend):
		return "vllm", true
	case "sglang":
		return "sglang", true
	default:
		return "", false
	}
}

func parseEngineVersion(version string) (engineVersion, error) {
	if version == "" {
		return engineVersion{}, fmt.Errorf("engine version must not be empty")
	}

	matches := engineVersionPattern.FindStringSubmatch(version)
	if matches == nil {
		return engineVersion{}, fmt.Errorf(
			"engine version %q must match major.minor[.patch][rcN|.rcN|.postN]", version,
		)
	}

	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return engineVersion{}, fmt.Errorf("invalid major version in %q: %w", version, err)
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return engineVersion{}, fmt.Errorf("invalid minor version in %q: %w", version, err)
	}

	var patch int
	if matches[3] != "" {
		patch, err = strconv.Atoi(matches[3])
		if err != nil {
			return engineVersion{}, fmt.Errorf("invalid patch version in %q: %w", version, err)
		}
	}
	if matches[4] != "" {
		if _, err := strconv.Atoi(matches[4]); err != nil {
			return engineVersion{}, fmt.Errorf("invalid micro version in %q: %w", version, err)
		}
	}

	return engineVersion{major: major, minor: minor, patch: patch}, nil
}
