package deviceplugin

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// redirectContainerdConfigDir points the engine-configuration setting at a temporary directory and
// returns it. Nothing is created in it: an absent configuration is a case in its own right.
func redirectContainerdConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(ContainerdConfigDirEnv, dir)

	return dir
}

func TestReadContainerEngineFacts(t *testing.T) {
	cases := []struct {
		name        string
		config      string
		wantKnown   bool
		wantCDI     bool
		wantHandler string
		wantVendor  bool
	}{
		{
			name:      "no configuration is unknown, not disabled",
			config:    "",
			wantKnown: false,
		},
		{
			// containerd 2.x reads configuration version 3 and removed the switch: CDI is always on.
			name: "configuration version 3 resolves CDI with no switch present",
			config: "version = 3\n\n[plugins.'io.containerd.cri.v1.runtime']\n" +
				"  enable_selinux = false\n",
			wantKnown: true,
			wantCDI:   true,
		},
		{
			name:      "configuration version 2 defaults it off",
			config:    "version = 2\n\n[plugins.\"io.containerd.grpc.v1.cri\"]\n  sandbox_image = \"pause\"\n",
			wantKnown: true,
			wantCDI:   false,
		},
		{
			name: "an explicit switch is honoured wherever the version puts it",
			config: "version = 2\n\n[plugins.\"io.containerd.grpc.v1.cri\"]\n" +
				"  enable_cdi = true\n",
			wantKnown: true,
			wantCDI:   true,
		},
		{
			// An explicit false wins over the version's default, which is the case a node that turned CDI
			// off deliberately presents.
			name: "an explicit false wins over the version default",
			config: "version = 3\n\n[plugins.'io.containerd.cri.v1.runtime']\n" +
				"  enable_cdi = false\n",
			wantKnown: true,
			wantCDI:   false,
		},
		{
			name: "the default runtime handler is read, and a vendor one recognized",
			config: "version = 3\n\n[plugins.'io.containerd.cri.v1.runtime'.containerd]\n" +
				"  default_runtime_name = \"nvidia\"\n",
			wantKnown:   true,
			wantCDI:     true,
			wantHandler: "nvidia",
			wantVendor:  true,
		},
		{
			name: "a generic default runtime handler is not a vendor one",
			config: "version = 3\n\n[plugins.'io.containerd.cri.v1.runtime'.containerd]\n" +
				"  default_runtime_name = \"runc\"\n",
			wantKnown:   true,
			wantCDI:     true,
			wantHandler: "runc",
		},
		{
			name:      "a malformed configuration is unknown",
			config:    "version = = 3\n",
			wantKnown: false,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := redirectContainerdConfigDir(t)
			if c.config != "" {
				writeTestFile(t, filepath.Join(dir, containerdConfigFile), c.config)
			}

			got := ReadContainerEngineFacts()
			assert.Equal(t, c.wantKnown, got.Known)
			assert.Equal(t, c.wantCDI, got.ResolvesCDI)
			assert.Equal(t, c.wantHandler, got.DefaultHandler)
			assert.Equal(t, c.wantVendor, got.DefaultHandlerIsVendor)
			// Named whether or not it was read: an unreadable configuration is exactly when an
			// operator needs to be told which file the node looked at.
			assert.Equal(t, filepath.Join(dir, containerdConfigFile), got.Path)
		})
	}
}

// The setting names the directory the file is looked for in, so a distribution keeping it outside
// /etc/containerd is served by pointing the setting at it — no path this code knows about.
func TestReadContainerEngineFactsFollowsTheConfiguredDirectory(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(root, "var/lib/rancher/k3s/agent/etc/containerd")
	writeTestFile(t, filepath.Join(elsewhere, containerdConfigFile),
		"version = 3\n\n[plugins.'io.containerd.cri.v1.runtime'.containerd]\n  default_runtime_name = \"runc\"\n")

	t.Setenv(ContainerdConfigDirEnv, elsewhere)
	got := ReadContainerEngineFacts()
	assert.True(t, got.Known)
	assert.True(t, got.ResolvesCDI)
	assert.Equal(t, "runc", got.DefaultHandler)

	// And a directory holding no configuration reads as unknown rather than as a disabled engine.
	t.Setenv(ContainerdConfigDirEnv, root)
	assert.False(t, ReadContainerEngineFacts().Known)
}

func TestIsVendorRuntimeHandler(t *testing.T) {
	for name, want := range map[string]bool{
		"":       false,
		"runc":   false,
		"crun":   false,
		"nvidia": true,
		"amd":    true,
	} {
		assert.Equal(t, want, isVendorRuntimeHandler(name), name)
	}
}
