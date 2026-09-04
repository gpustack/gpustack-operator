package ascend

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
)

// dockerRuntimeCapability names the vendor runtime in a preflight row, in the vendor's own word:
// "Ascend Docker Runtime" is what its installer calls the package.
const dockerRuntimeCapability = "ascend-docker-runtime"

// dockerRuntimeInstallInfo is the file that installer writes, and the only place the installed
// version can be read from: the wrapper's own `--version` execs the host's runc and prints runc's
// banner, so it reports nothing about the vendor release. The path is fixed by the installer
// (ascend-docker-runtime/build/scripts/run_main.sh, save_install_args).
//
// Reading it adds nothing to the DaemonSet: /usr/local/Ascend is already mounted read-only, because
// that is where libdcmi.so is loaded from.
const dockerRuntimeInstallInfo = "/usr/local/Ascend/Ascend-Docker-Runtime/ascend_docker_runtime_install.info"

// dockerRuntimeMajor950 is the first MindCluster major release whose runtime knows what an A5 is.
//
// Only the major is compared, because the boundary is a major-release one: A5 support landed in
// 26.0.0 -- the release that first recognizes the "Ascend950" chip name and mounts the UB devices --
// and the line before it ends at 7.3.x. The vendor's own tags run 5, 6, 7 then 26, so nothing sits
// between the two, and the positions after the major are not always numbers there ("v7.0.RC1").
const dockerRuntimeMajor950 = 26

// checkDockerRuntime establishes whether this node's ascend-docker-runtime can serve an A5
// allocation, by reading the version its installer recorded at path.
//
// It is a precondition rather than an observation, and the reason it is worth a row is that an older
// runtime refuses the allocation while naming a device node instead of its own version.
//
// GetDeviceTypeByChipName maps "Ascend950" to no type it knows before 26.0.0 -- that release added the
// A5 branch (ascend-docker-runtime/runtime/process/process.go:163) -- so getCommonManagerDevices falls
// back to the list every earlier generation used, which carries /dev/devmm_svm (process.go:104,
// 651-656). A5 has no such node: measured on the 950 lab host, /dev/devmm_svm and /dev/dvpp_cmdlist are
// absent while /dev/davinci_manager and /dev/hisi_hdc are present, which is why 26.0.0's A5 list is
// hisi_hdc alone. Adding a device that is not there fails the spec, and that error is returned all the
// way out (process.go:663, 706, 1008), so container creation fails.
//
// The cards themselves are not the gap: /dev/davinci<N> is built from the requested index with no
// chip-name lookup at all (addDevice, process.go:776), so an older runtime would have injected them.
// It never gets that far.
//
// A version that cannot be read or cannot be parsed is reported unavailable rather than given the
// benefit of the doubt -- an unestablished precondition reported as a pass is the outcome a
// preflight exists to rule out -- and the reason carries the raw value so an operator sees what was
// there.
//
// The row names no accelerator: the runtime is one node-level fact, and the caller attaches it to
// each accelerator it is a precondition for.
func checkDockerRuntime(path string) device.PreflightCheck {
	c := device.PreflightCheck{
		Capability: dockerRuntimeCapability,
		// Every A5 allocation carries the same injection whatever mode serves it, so the row is
		// filed under the baseline mode rather than repeated once per mode.
		Mode:  device.PreflightModeOf(workercore.DeviceAllocationModeExclusive),
		State: device.PreflightStateUnavailable,
		Depth: device.PreflightDepthDeclared,
	}

	version, err := readDockerRuntimeVersion(path)
	if err != nil {
		c.Reason = fmt.Sprintf("the installed ascend-docker-runtime version could not be read, so "+
			"whether this node serves A5 is unknown: %v", err)
		return c
	}

	major, err := mindClusterMajor(version)
	if err != nil {
		c.Reason = fmt.Sprintf("%s records an ascend-docker-runtime version this cannot read, so "+
			"which MindCluster release is installed is unknown: %v", path, err)
		return c
	}

	if major < dockerRuntimeMajor950 {
		c.Reason = fmt.Sprintf("ascend-docker-runtime %s predates MindCluster %d.0.0, where A5 "+
			"support landed: it maps the Ascend950 chip name to no device type it knows, so it falls "+
			"back to a manager-device list carrying /dev/devmm_svm, which A5 does not have -- an "+
			"allocation on this accelerator fails at container creation, naming that device rather "+
			"than this version", version, dockerRuntimeMajor950)
		return c
	}

	c.State = device.PreflightStateOK
	c.Detail = fmt.Sprintf("ascend-docker-runtime %s is MindCluster %d.0.0 or newer, so it resolves "+
		"ASCEND_VISIBLE_DEVICES to this node's A5 device nodes and mounts the UB fabric beside them",
		version, dockerRuntimeMajor950)
	return c
}

// readDockerRuntimeVersion returns the version the installer recorded, exactly as written --
// "v7.3.0" on the 910B lab host.
//
// The file is a flat key=value list whose values may be empty ("config-file-path="), so a line is
// split on its first separator rather than on the only one.
func readDockerRuntimeVersion(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(string(content), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found && strings.TrimSpace(key) == "version" {
			return strings.TrimSpace(value), nil
		}
	}

	return "", fmt.Errorf("%s carries no version entry", path)
}

// mindClusterMajor returns the major release number of a MindCluster version string such as
// "v7.3.0", "v7.0.RC1" or "v26.0.0.beta.1".
func mindClusterMajor(version string) (int, error) {
	major, _, _ := strings.Cut(strings.TrimPrefix(version, "v"), ".")

	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, fmt.Errorf("%q begins with no release number", version)
	}

	return n, nil
}
