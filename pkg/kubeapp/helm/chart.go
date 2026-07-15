package helm

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"text/template"

	sprig "github.com/go-task/slim-sprig/v3"
	helmaction "helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	klog "k8s.io/klog/v2"
	"sigs.k8s.io/yaml"

	"gpustack.ai/gpustack/pkg/utils/bytex"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

type (
	Chart struct {
		// Name is the name of the chart.
		Name string
		// Version is the version of the chart, a SemVer 2 conformant string.
		Version string
		// Release is the name of the release.
		Release string
		// Path is the path of the chart tar ball.
		Path string
		// DownloadURL is the URL to download the chart tar ball.
		// If the Path is not existed, the chart will be downloaded from this URL.
		DownloadURL string
		// Values is the values to be passed to the chart.
		Values ChartValues
		// SkippedCRDsInstallation defines whether to skip the installation of CRDs of this chart.
		//
		// Sometimes, the cluster has already installed the CRDs of the same API,
		// this function is a chance to disable the installation of CRDs on this fresh installation.
		SkippedCRDsInstallation bool
		// SkippedInstallationIfApiServiceReady is the APIService name,
		// if specified, the installation will be skipped if an APIService with this name already exists in the cluster.
		//
		// Sometimes, the cluster has already installed a same chart but not the same release,
		// this function is a chance to check whether to continue installation on this fresh installation,
		// or just skip.
		SkippedInstallationIfApiServiceReady string
		// RepairViaUpgradeOnly repairs a bad-status release with a Helm upgrade instead
		// of the destructive uninstall+install path.
		//
		// A chart whose CRDs are Helm-managed templates and whose custom resources carry
		// controller finalizers (e.g. Kueue) can deadlock on the uninstall path: the
		// uninstall removes the controller while its finalizers still pin the CRs, so the
		// CRDs get stuck Terminating and the reinstall can never recreate them. An upgrade
		// keeps the controller alive to clear the finalizers and recreates any missing
		// resources, so the release converges without stranding.
		RepairViaUpgradeOnly bool
	}

	ChartValues interface {
		// GetValues returns the values for chart installation.
		GetValues(ctx context.Context) (map[string]any, error)
	}
)

// Validate validates the chart.
func (ch Chart) Validate() error {
	if ch.Name == "" {
		return fmt.Errorf("name is required")
	}
	if ch.Release == "" {
		return fmt.Errorf("release name is required")
	}
	if ch.Path == "" && ch.DownloadURL == "" {
		return fmt.Errorf("path or download URL is required")
	}
	return nil
}

// Load loads the chart from local path or remote URL.
func (ch Chart) Load(_ context.Context, cfg *helmaction.Configuration) (*helmchart.Chart, error) {
	f := ch.Path
	if f != "" && osx.IsEmptyFile(f) {
		f = ""
	}

	if f == "" {
		if ch.DownloadURL == "" {
			return nil, fmt.Errorf("chart path %s is not existed and download URL is not provided", ch.Path)
		}
		_, fn := filepath.Split(ch.DownloadURL)
		f = filepath.Join(filepath.Dir(ch.Path), fn)
		if osx.IsEmptyFile(f) {
			p := helmaction.NewPullWithOpts(helmaction.WithConfig(cfg))
			p.Settings = cli.New()
			p.Version = ch.Version
			p.DestDir = filepath.Dir(ch.Path)
			pr, err := p.Run(ch.DownloadURL)
			if err != nil {
				return nil, fmt.Errorf("pull chart from %s: %s: %w", ch.DownloadURL, pr, err)
			}
		}
	}

	return helmloader.Load(f)
}

// GetValues returns the values for chart installation.
func (ch Chart) GetValues(ctx context.Context) (map[string]any, error) {
	if ch.Values == nil {
		return nil, nil
	}
	return ch.Values.GetValues(ctx)
}

type StaticValues map[string]any

func (cv StaticValues) GetValues(ctx context.Context) (map[string]any, error) {
	return cv, nil
}

type TemplateValues struct {
	Application   string
	Template      string
	ExtendFuncMap template.FuncMap
	Context       map[string]any
}

func (cv TemplateValues) GetValues(ctx context.Context) (map[string]any, error) {
	tmpl, err := template.New("helm-values-template").
		Funcs(templateFuncMap(cv.ExtendFuncMap)).
		Parse(cv.Template)
	if err != nil {
		return nil, err
	}

	buf := bytex.GetBuffer()
	defer bytex.Put(buf)

	if err = tmpl.Execute(buf, cv.Context); err != nil {
		return nil, err
	}

	if klog.V(3).Enabled() {
		klog.Infof("rendered application values %s:\n%s", cv.Application, buf.String())
	}

	vs := map[string]any{}
	err = yaml.Unmarshal(buf.Bytes(), &vs)
	return vs, err
}

func templateFuncMap(extend template.FuncMap) template.FuncMap {
	fm := sprig.TxtFuncMap()
	fm["toYaml"] = toYAML
	if len(extend) > 0 {
		for k, v := range extend {
			fm[k] = v
		}
	}

	return fm
}

// toYAML borrows from helm.sh/helm/pkg/engine/engine.go.
func toYAML(v any) string {
	data, err := yaml.Marshal(v)
	if err != nil {
		// Swallow errors inside a template.
		return ""
	}

	return strings.TrimSuffix(string(data), "\n")
}
