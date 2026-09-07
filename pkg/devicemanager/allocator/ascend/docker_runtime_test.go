package ascend

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
)

// installInfoV730 is the file as ascend-docker-runtime v7.3.0 wrote it, captured verbatim from the
// 910B2 lab host bms-910b2-001-001 (aarch64, driver 25.5.1). It is what the parser meets on a real
// pre-A5 node, trailing empty value and all.
const installInfoV730 = `version=v7.3.0
arch=aarch64
os=linux
path=/usr/local/Ascend/Ascend-Docker-Runtime/Ascend-Docker-Runtime
build=Ascend-docker-runtime_7.3.0-aarch64
a500=n
a500a2=n
a200=n
a200isoc=n
a200ia2=n
install-scene=docker
config-file-path=
`

// installInfoV2610 is what a MindCluster 26.x installer writes: the same keys plus the build
// metadata and the injection mode that line added (build/scripts/run_main.sh, save_install_args).
const installInfoV2610 = `version=v26.1.0
arch=aarch64
os=linux
gitCommit=1db90bca3
gitBranch=master
goVersion=go1.22.7
path=/usr/local/Ascend/Ascend-Docker-Runtime/Ascend-Docker-Runtime
build=Ascend-docker-runtime_26.1.0-aarch64
a500=n
a500a2=n
a200=n
a200isoc=n
a200ia2=n
install-scene=docker
config-file-path=
injection-mode=legacy
`

// writeInstallInfo puts content where a test can point the reader at it, and returns that path. An
// empty content means the file is never created, which is the node that has no vendor runtime.
func writeInstallInfo(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "ascend_docker_runtime_install.info")
	if content == "" {
		return path
	}
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestReadDockerRuntimeVersion(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{name: "the lab host's own file", content: installInfoV730, want: "v7.3.0"},
		{name: "a 26.x file", content: installInfoV2610, want: "v26.1.0"},
		// The runtime is simply not installed, which is as much a failed precondition as an old one.
		{name: "no file at all", wantErr: true},
		{name: "no version entry", content: "arch=aarch64\ninstall-scene=docker\n", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readDockerRuntimeVersion(writeInstallInfo(t, c.content))
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// The one question this check answers: would an A5 allocation on this node reach an accelerator?
func TestCheckDockerRuntime(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    device.PreflightState
	}{
		{name: "the lab host's v7.3.0 predates A5", content: installInfoV730, want: device.PreflightStateUnavailable},
		{name: "26.1.0 serves A5", content: installInfoV2610, want: device.PreflightStateOK},
		// The floor itself, and the release after it: the boundary is 26.0.0 and everything above.
		{name: "26.0.0 is the floor", content: "version=v26.0.0\n", want: device.PreflightStateOK},
		{name: "26.0.0.beta.1 is above it", content: "version=v26.0.0.beta.1\n", want: device.PreflightStateOK},
		{name: "a later major serves A5 too", content: "version=v27.0.0\n", want: device.PreflightStateOK},
		// The vendor's pre-26 line carries non-numeric minors, which must not be mistaken for an
		// unreadable version: the major alone answers the question.
		{name: "7.0.RC1 predates A5", content: "version=v7.0.RC1\n", want: device.PreflightStateUnavailable},
		// An unestablished precondition is never reported as a pass.
		{name: "no file at all", want: device.PreflightStateUnavailable},
		{name: "a version naming no release", content: "version=unknown\n", want: device.PreflightStateUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkDockerRuntime(writeInstallInfo(t, c.content))

			assert.Equal(t, c.want, got.State)
			assert.Equal(t, dockerRuntimeCapability, got.Capability)
			// Reason is empty exactly when the state is ok, and the detail is what was read.
			if c.want == device.PreflightStateOK {
				assert.Empty(t, got.Reason)
				assert.NotEmpty(t, got.Detail)
			} else {
				assert.NotEmpty(t, got.Reason)
			}
		})
	}
}

// The runtime version is a precondition for A5 and for nothing else.
//
// A node carrying both generations is the discriminating case, and the only one: on a node of a
// single family, "the row is emitted where an A5 is present" and "the row is emitted on an A5 group"
// are indistinguishable, so a fixture of one family would pass with the family test removed
// entirely.
func TestPreflightAccelerator_DockerRuntimeRowIsA5Only(t *testing.T) {
	p := &preflighter{
		logger:      klog.Background(),
		share:       &fakeShareDriver{enabled: map[[2]int32]bool{}},
		installInfo: writeInstallInfo(t, installInfoV2610),
	}
	devs := ascendDevicesFixture()
	a5 := *devs.Spec.Groups[0].DeepCopy()
	a5.ID, a5.Family = "950", family950
	a5.Accelerators = []workercore.Accelerator{{ID: "a5-0", Index: 0, PhysicalIndexes: []uint32{7, 3, 0}}}
	devs.Spec.Groups = append(devs.Spec.Groups, a5)

	grp := p.PreflightAccelerator(devs.Spec.Groups)

	// Each row names the accelerator AND the mode it was filed under, so a reader filtering the
	// report by either finds the node-wide fact rather than missing it.
	var carried []string
	for _, check := range grp.Checks {
		if check.Capability != dockerRuntimeCapability {
			continue
		}
		assert.Equal(t, device.PreflightStateOK, check.State)
		carried = append(carried, check.Accelerator+"/"+check.Mode)
	}
	sort.Strings(carried)
	assert.Equal(t, []string{
		"a5-0/exclusive", "a5-0/shared", "a5-0/sliced", "a5-0/visibility",
	}, carried, "the runtime row is filed on the A5 accelerator alone, once per mode it is a "+
		"precondition for")
}
