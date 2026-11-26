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
	// fragments are files written under the configuration directory before the read, keyed by their
	// path relative to it; config is the root file itself.
	cases := []struct {
		name        string
		config      string
		fragments   map[string]string
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
		{
			// containerd merges every file the imports key names into the configuration, so a fact kept
			// in a drop-in fragment is read from there rather than missed at the root.
			name:   "a relative import contributes a fact the root lacks",
			config: "version = 2\nimports = [\"conf.d/cdi.toml\"]\n",
			fragments: map[string]string{
				"conf.d/cdi.toml": "[plugins.\"io.containerd.grpc.v1.cri\"]\n  enable_cdi = true\n",
			},
			wantKnown: true,
			wantCDI:   true,
		},
		{
			// A glob import takes every match in glob order, and a later file overrides an earlier one.
			name:   "a glob import takes the fragment listed last",
			config: "version = 3\nimports = [\"conf.d/*.toml\"]\n",
			fragments: map[string]string{
				"conf.d/10-runc.toml": "[plugins.'io.containerd.cri.v1.runtime'.containerd]\n" +
					"  default_runtime_name = \"runc\"\n",
				"conf.d/20-nvidia.toml": "[plugins.'io.containerd.cri.v1.runtime'.containerd]\n" +
					"  default_runtime_name = \"nvidia\"\n",
			},
			wantKnown:   true,
			wantCDI:     true,
			wantHandler: "nvidia",
			wantVendor:  true,
		},
		{
			// A fragment's own imports are followed, relative to the fragment, and merged after it.
			name:   "an import of an import overrides the importing fragment",
			config: "version = 3\nimports = [\"first.toml\"]\n",
			fragments: map[string]string{
				"first.toml": "imports = [\"second.toml\"]\n\n[plugins.'io.containerd.cri.v1.runtime'.containerd]\n" +
					"  default_runtime_name = \"runc\"\n",
				"second.toml": "[plugins.'io.containerd.cri.v1.runtime'.containerd]\n" +
					"  default_runtime_name = \"nvidia\"\n",
			},
			wantKnown:   true,
			wantCDI:     true,
			wantHandler: "nvidia",
			wantVendor:  true,
		},
		{
			// Each file is read once, so two fragments importing each other terminate, and both are read.
			name:   "circular imports terminate and are both read",
			config: "version = 3\nimports = [\"a.toml\"]\n",
			fragments: map[string]string{
				"a.toml": "imports = [\"b.toml\"]\n\n[plugins.'io.containerd.cri.v1.runtime']\n  enable_cdi = false\n",
				"b.toml": "imports = [\"a.toml\"]\n\n[plugins.'io.containerd.cri.v1.runtime'.containerd]\n" +
					"  default_runtime_name = \"nvidia\"\n",
			},
			wantKnown:   true,
			wantCDI:     false,
			wantHandler: "nvidia",
			wantVendor:  true,
		},
		{
			// containerd 1.x kept each plugin section as a whole tree and replaced it when a later file
			// named it, so a key the fragment does not repeat is gone: CDI falls back to the default off.
			name:   "configuration version 2 replaces a plugin section a fragment names",
			config: "version = 2\nimports = [\"handler.toml\"]\n\n[plugins.\"io.containerd.grpc.v1.cri\"]\n  enable_cdi = true\n",
			fragments: map[string]string{
				"handler.toml": "[plugins.\"io.containerd.grpc.v1.cri\".containerd]\n" +
					"  default_runtime_name = \"nvidia\"\n",
			},
			wantKnown:   true,
			wantCDI:     false,
			wantHandler: "nvidia",
			wantVendor:  true,
		},
		{
			// From version 3 the sections are merged key by key: the fragment's key overrides and the
			// handler it omits survives.
			name: "configuration version 3 merges a plugin section key by key",
			config: "version = 3\nimports = [\"cdi.toml\"]\n\n" +
				"[plugins.'io.containerd.cri.v1.runtime'.containerd]\n  default_runtime_name = \"nvidia\"\n",
			fragments: map[string]string{
				"cdi.toml": "[plugins.'io.containerd.cri.v1.runtime']\n  enable_cdi = false\n",
			},
			wantKnown:   true,
			wantCDI:     false,
			wantHandler: "nvidia",
			wantVendor:  true,
		},
		{
			// containerd refuses to start on an import it cannot read, so the configuration is not one
			// this reader may claim to know either.
			name:      "an import naming a missing file is unknown",
			config:    "version = 3\nimports = [\"missing.toml\"]\n",
			wantKnown: false,
		},
		{
			// A glob matching nothing is the node with no drop-in yet: not an error in containerd either.
			name:        "a glob import matching nothing keeps the root facts",
			config:      "version = 2\nimports = [\"conf.d/*.toml\"]\n\n[plugins.\"io.containerd.grpc.v1.cri\".containerd]\n  default_runtime_name = \"runc\"\n",
			wantKnown:   true,
			wantCDI:     false,
			wantHandler: "runc",
		},
		{
			// Only the top-level key is followed, so the same name inside a table is an ordinary setting.
			name: "an imports key inside a table is ignored",
			config: "version = 2\n\n[plugins.\"io.containerd.grpc.v1.cri\".containerd]\n  default_runtime_name = \"runc\"\n" +
				"  imports = [\"ignored.toml\"]\n",
			fragments: map[string]string{
				"ignored.toml": "[plugins.\"io.containerd.grpc.v1.cri\"]\n  enable_cdi = true\n",
			},
			wantKnown:   true,
			wantCDI:     false,
			wantHandler: "runc",
		},
		{
			// The version merges like any other key, and it is the merged one that settles the defaults:
			// a version 2 root with a version 3 fragment reads as version 3, where CDI is always on.
			name:   "a fragment's version overrides the root's",
			config: "version = 2\nimports = [\"version.toml\"]\n",
			fragments: map[string]string{
				"version.toml": "version = 3\n",
			},
			wantKnown: true,
			wantCDI:   true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := redirectContainerdConfigDir(t)
			if c.config != "" {
				writeTestFile(t, filepath.Join(dir, containerdConfigFile), c.config)
			}
			for rel, content := range c.fragments {
				writeTestFile(t, filepath.Join(dir, rel), content)
			}

			// The path is named whether or not it was read: an unreadable configuration is exactly when
			// an operator needs to be told which file the node looked at.
			want := ContainerEngineFacts{
				Known:                  c.wantKnown,
				Path:                   filepath.Join(dir, containerdConfigFile),
				ResolvesCDI:            c.wantCDI,
				DefaultHandler:         c.wantHandler,
				DefaultHandlerIsVendor: c.wantVendor,
			}
			assert.Equal(t, want, ReadContainerEngineFacts())
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

// TestLoadContainerdConfig_ImportErrorsNameTheImportingFile pins that a failure inside an import
// says which file declared the import: an operator with several fragments has nothing else to go
// on, and the TOML decoder's own error names only a line.
func TestLoadContainerdConfig_ImportErrorsNameTheImportingFile(t *testing.T) {
	cases := []struct {
		name     string
		fragment string
	}{
		{name: "a malformed fragment", fragment: "version = = 3\n"},
		{name: "a fragment with a bad glob", fragment: "imports = [\"*[.toml\"]\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestFile(t, filepath.Join(dir, "frag.toml"), c.fragment)
			writeTestFile(t, filepath.Join(dir, containerdConfigFile), "version = 3\nimports = [\"frag.toml\"]\n")

			_, err := loadContainerdConfig(filepath.Join(dir, containerdConfigFile))
			if !assert.Error(t, err) {
				return
			}
			assert.Contains(t, err.Error(), "frag.toml", "the error names the importing file")
		})
	}
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
