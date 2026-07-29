package helm

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	helmaction "helm.sh/helm/v3/pkg/action"
)

// Test_Chart_configureActions pins the chart-declared options that reach the Helm
// actions: TakeOwnership must reach both the install and the upgrade action and
// default to false, while SkippedCRDsInstallation must drive the install action's
// SkipCRDs alone — never IncludeCRDs, which would put the crds/ files into the
// manifest and make the ownership check reject the CRDs Helm's own CRD phase just
// created without ownership metadata.
func Test_Chart_configureActions(t *testing.T) {
	cases := []struct {
		name              string
		chart             Chart
		wantTakeOwnership bool
		wantSkipCRDs      bool
	}{
		{
			name:              "zero chart takes no ownership",
			chart:             Chart{},
			wantTakeOwnership: false,
			wantSkipCRDs:      false,
		},
		{
			name:              "take ownership reaches both actions",
			chart:             Chart{TakeOwnership: true},
			wantTakeOwnership: true,
			wantSkipCRDs:      false,
		},
		{
			name:              "skipped CRDs installation skips the CRD phase only",
			chart:             Chart{SkippedCRDsInstallation: true},
			wantTakeOwnership: false,
			wantSkipCRDs:      true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			config := &helmaction.Configuration{}
			i := helmaction.NewInstall(config)
			u := helmaction.NewUpgrade(config)

			c.chart.configureInstall(i)
			c.chart.configureUpgrade(u)

			assert.Equal(t, c.wantTakeOwnership, i.TakeOwnership, "install action take ownership")
			assert.Equal(t, c.wantTakeOwnership, u.TakeOwnership, "upgrade action take ownership")
			assert.Equal(t, c.wantSkipCRDs, i.SkipCRDs, "install action skip CRDs")
			assert.False(t, i.IncludeCRDs, "install action must never render crds/ into the manifest")
		})
	}
}

// Test_Chart_Validate pins the required chart fields, in particular that a source is
// required but either a local path or a download URL satisfies it.
func Test_Chart_Validate(t *testing.T) {
	cases := []struct {
		name    string
		chart   Chart
		wantErr string
	}{
		{
			name:    "without name",
			chart:   Chart{Release: "gpustack-kueue", Path: "kueue.tgz"},
			wantErr: "name is required",
		},
		{
			name:    "without release",
			chart:   Chart{Name: "kueue", Path: "kueue.tgz"},
			wantErr: "release name is required",
		},
		{
			name:    "without path and download URL",
			chart:   Chart{Name: "kueue", Release: "gpustack-kueue"},
			wantErr: "path or download URL is required",
		},
		{
			name:  "with path",
			chart: Chart{Name: "kueue", Release: "gpustack-kueue", Path: "kueue.tgz"},
		},
		{
			name:  "with download URL",
			chart: Chart{Name: "kueue", Release: "gpustack-kueue", DownloadURL: "https://example.com/kueue.tgz"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.chart.Validate()
			if c.wantErr != "" {
				assert.EqualError(t, err, c.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// Test_Chart_GetValues pins the values resolution of every ChartValues implementation:
// no values at all, static values passed through as-is, and a template rendered with
// the sprig function set, the toYaml helper and the caller's extra functions.
func Test_Chart_GetValues(t *testing.T) {
	cases := []struct {
		name    string
		chart   Chart
		want    map[string]any
		wantErr bool
	}{
		{
			name:  "without values",
			chart: Chart{},
			want:  nil,
		},
		{
			name:  "with static values",
			chart: Chart{Values: StaticValues{"fullnameOverride": "gpustack-kueue"}},
			want:  map[string]any{"fullnameOverride": "gpustack-kueue"},
		},
		{
			name: "with template values",
			chart: Chart{Values: TemplateValues{
				Application: "kueue",
				Template: `
image:
  repository: {{ .registry }}/kueue
  tag: {{ upper .tag }}
labels:
  {{- toYaml .labels | nindent 2 }}
namespace: {{ prefixed "system" }}
`,
				ExtendFuncMap: template.FuncMap{
					"prefixed": func(s string) string { return "gpustack-" + s },
				},
				Context: map[string]any{
					"registry": "example.com",
					"tag":      "v0.18.4",
					"labels":   map[string]any{"owner": "gpustack"},
				},
			}},
			want: map[string]any{
				"image": map[string]any{
					"repository": "example.com/kueue",
					"tag":        "V0.18.4",
				},
				"labels":    map[string]any{"owner": "gpustack"},
				"namespace": "gpustack-system",
			},
		},
		{
			name:    "with malformed template values",
			chart:   Chart{Values: TemplateValues{Application: "kueue", Template: "{{ .registry"}},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.chart.GetValues(t.Context())
			if c.wantErr {
				assert.Error(t, err)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// Test_Chart_Load pins the local source resolution: a missing or empty path with no
// download URL to fall back on is an error, and an existing local chart is loaded.
// The download path is not covered, it needs a remote repository.
func Test_Chart_Load(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.tgz")

	cases := []struct {
		name     string
		chart    Chart
		wantName string
		wantErr  string
	}{
		{
			name:    "without path and download URL",
			chart:   Chart{},
			wantErr: "chart path  is not existed and download URL is not provided",
		},
		{
			name:    "with missing path and without download URL",
			chart:   Chart{Path: missing},
			wantErr: fmt.Sprintf("chart path %s is not existed and download URL is not provided", missing),
		},
		{
			name:     "with local path",
			chart:    Chart{Path: newLocalChart(t, "kueue")},
			wantName: "kueue",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.chart.Load(t.Context(), nil)
			if c.wantErr != "" {
				assert.EqualError(t, err, c.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.wantName, got.Metadata.Name)
		})
	}
}

// newLocalChart writes a minimal loadable chart with the given name and returns its path.
func newLocalChart(t *testing.T, name string) string {
	t.Helper()

	dir := t.TempDir()
	metadata := fmt.Sprintf("apiVersion: v2\nname: %s\nversion: 0.1.0\n", name)
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(metadata), 0o600); err != nil {
		t.Fatalf("write chart metadata: %v", err)
	}

	return dir
}
