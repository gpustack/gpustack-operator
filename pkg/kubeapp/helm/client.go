package helm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	helmaction "helm.sh/helm/v3/pkg/action"
	helmchartutil "helm.sh/helm/v3/pkg/chartutil"
	helmregistry "helm.sh/helm/v3/pkg/registry"
	helmrelease "helm.sh/helm/v3/pkg/release"
	helmdriver "helm.sh/helm/v3/pkg/storage/driver"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"
	apireg "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubeconfig"
	"gpustack.ai/gpustack/pkg/kubediscovery"
)

type (
	// Client is a Helm client.
	Client struct {
		// getter is the Kubernetes REST client getter.
		getter genericclioptions.RESTClientGetter
		// defaultNamespace is the name of default Kubernetes Namespace, which to place the release info.
		defaultNamespace string
		// Timeout is the timeout of the action.
		timeout time.Duration
	}

	// ClientOption is a function to set the configuration of the Client.
	ClientOption func(*Client)
)

// WithDefaultNamespace returns a ClientOption to set the name of default Kubernetes Namespace,
// which to place the release info.
func WithDefaultNamespace(namespace string) func(*Client) {
	return func(c *Client) {
		c.defaultNamespace = namespace
	}
}

// WithTimeout returns a ClientOption to set the timeout of the action.
func WithTimeout(timeout time.Duration) func(*Client) {
	return func(c *Client) {
		c.timeout = timeout
	}
}

// NewClient creates a new Helm configuration from a Kubernetes rest.Config.
//
// The given defaultNamespace is used to place the release info.
func NewClient(restCfg rest.Config, opts ...ClientOption) (*Client, error) {
	restCfg.ContentType = runtime.ContentTypeJSON
	c := &Client{
		getter:           kubeconfig.ConvertRestConfigToRestClientGetter(&restCfg),
		defaultNamespace: core.NamespaceDefault,
		timeout:          30 * time.Minute,
	}
	for i := range opts {
		opts[i](c)
	}

	return c, nil
}

// DefaultNamespace returns the name of default Kubernetes Namespace,
// which to place the release info.
func (c *Client) DefaultNamespace() string {
	return c.defaultNamespace
}

// KubeRestClientGetter returns the Kubernetes REST client getter.
func (c *Client) KubeRestClientGetter() genericclioptions.RESTClientGetter {
	return c.getter
}

// KubeClientSet returns the Kubernetes clientset.
func (c *Client) KubeClientSet() kubernetes.Interface {
	conf, _ := c.getter.ToRESTConfig()
	return kubernetes.NewForConfigOrDie(conf)
}

// KubeVersion returns the Kubernetes version information. Minor is normalized
// to its leading digits, dropping any non-numeric suffix (e.g. "31+") that some
// distributions report, so it is safe to parse or compare numerically.
func (c *Client) KubeVersion(ctx context.Context) kubediscovery.Version {
	kv, err := kubediscovery.GetVersion(ctx, c.KubeClientSet().Discovery())
	if err != nil {
		return kubediscovery.Version{}
	}
	v := *kv
	v.Minor = strings.TrimRightFunc(v.Minor, func(r rune) bool { return r < '0' || r > '9' })
	return v
}

// Install installs the given chart, and returns the values.
//
// If the release has been found, it will be done.
func (c *Client) Install(ctx context.Context, chart *Chart, overrideNamespace ...string) (helmchartutil.Values, error) {
	next := func(r *helmrelease.Release) NextStepType {
		return c.nextStep(ctx, r, chart)
	}
	return c.InstallWith(ctx, chart, next, overrideNamespace...)
}

// nextStep decides what to do with an already-found release of the given chart.
func (c *Client) nextStep(ctx context.Context, r *helmrelease.Release, chart *Chart) NextStepType {
	switch r.Info.Status {
	case helmrelease.StatusDeployed, helmrelease.StatusSuperseded:
		if r.Chart.Metadata.Version != "" {
			lv := r.Chart.Metadata.Version
			rv := chart.Version
			if lv != rv {
				return NextStepUpgrade
			}
		}
		if rv := r.Config; rv != nil {
			cv, err := chart.Values.GetValues(ctx)
			if err != nil {
				return NextStepDone
			}
			if !reflect.DeepEqual(rv, cv) {
				return NextStepUpgrade
			}
		}
		return NextStepDone
	case helmrelease.StatusPendingInstall, helmrelease.StatusPendingUpgrade, helmrelease.StatusPendingRollback:
		// A pending record is either an operation still in flight or what a process killed
		// mid-flight left behind, and repairing the first would corrupt what it is doing.
		// ExclusiveAccess is what tells the two apart; without it the only evidence is age,
		// and a record younger than the action's own timeout may still belong to a peer.
		if !chart.ExclusiveAccess && time.Since(r.Info.LastDeployed.Time) <= c.timeout {
			return NextStepRequeue
		}

		// An abandoned pending-install is the one pending state Helm cannot act on at all:
		// its own name check refuses every later install while the record stands, and no
		// action removes it. So the record goes, and the release installs afresh.
		if r.Info.Status == helmrelease.StatusPendingInstall {
			return _NextStepDiscard
		}

		// An abandoned upgrade/rollback still has its last deployed revision intact — roll
		// back to it to clear the pending lock, then the loop upgrades. Others keep the
		// reinstall behavior.
		if chart.RepairViaUpgradeOnly {
			return NextStepRollback
		}

		return _NextStepInstall
	default:
		// A bad-status release (e.g. failed) is normally reinstalled, but that
		// uninstall can deadlock charts whose finalized CRs pin Helm-managed CRDs.
		// RepairViaUpgradeOnly repairs it with an upgrade instead — see the field doc.
		if chart.RepairViaUpgradeOnly {
			return NextStepUpgrade
		}
		return NextStepReinstall
	}
}

// NextStepType is the type of the next step.
type NextStepType uint8

const (
	NextStepDone NextStepType = iota
	NextStepRequeue
	NextStepUpgrade
	NextStepRollback
	NextStepReinstall
	_NextStepInstall
	_NextStepDiscard
)

// NextStepConditionFunc is a function to determine what to do next by the given release.
type NextStepConditionFunc func(release *helmrelease.Release) (next NextStepType)

// InstallWith installs the given chart with the given condition function.
//
// The condition function is used to determine what to do next by the given release,
// which only calls when the release has been found.
func (c *Client) InstallWith(
	ctx context.Context,
	chart *Chart,
	next NextStepConditionFunc,
	overrideNamespace ...string,
) (helmchartutil.Values, error) {
	// Validate.
	if err := chart.Validate(); err != nil {
		return nil, fmt.Errorf("validate chart: %w", err)
	}
	if next == nil {
		return nil, errors.New("next is required")
	}

	// Create config.
	namespace := c.defaultNamespace
	if len(overrideNamespace) > 0 {
		namespace = overrideNamespace[0]
	}
	{
		// Ensure the namespace exists, otherwise Helm will fail to create the release.
		_, err := kubeclientset.Create(
			ctx,
			c.KubeClientSet().CoreV1().Namespaces(),
			&core.Namespace{
				ObjectMeta: meta.ObjectMeta{
					Name: namespace,
				},
			},
		)
		if err != nil {
			return nil, fmt.Errorf("create namespace %s: %w", namespace, err)
		}
	}

	config, err := c.createConfig(namespace)
	if err != nil {
		return nil, fmt.Errorf("create helm config: %w", err)
	}

	logger := klog.Background().WithName(chart.Name).WithValues("release", chart.Release)

	// Get release.
	g := helmaction.NewGet(config)
	r, err := g.Run(chart.Release)
	if err != nil && !errors.Is(err, helmdriver.ErrReleaseNotFound) {
		return nil, fmt.Errorf("helm get: release %s: %w", chart.Release, err)
	}

	// Next.
	for {
		n := _NextStepInstall
		if r != nil {
			n = next(r)
		} else if isApiServiceReady(ctx, c, chart.SkippedInstallationIfApiServiceReady) {
			return nil, nil
		}

		switch n {
		case NextStepRequeue:
			// Requeue.
			logger.Info("requeueing")
			time.Sleep(10 * time.Second)
			r, err = g.Run(chart.Release)
			if err != nil && !errors.Is(err, helmdriver.ErrReleaseNotFound) {
				return nil, fmt.Errorf("helm get: release %s: %w", chart.Release, err)
			}
		case _NextStepDiscard:
			// Drop the abandoned revision record and re-evaluate. Helm has no action for
			// this: an install refuses the name while the record stands, a rollback has no
			// deployed revision to return to, and an uninstall would tear down whatever the
			// dead process did manage to create. Only this revision goes, so a release that
			// carries earlier ones converges on the newest of those instead of installing.
			logger.Info("discarding the abandoned release revision")
			if _, err := config.Releases.Delete(chart.Release, r.Version); err != nil {
				return nil, fmt.Errorf("helm delete revision: release %s: %w", chart.Release, err)
			}
			r, err = g.Run(chart.Release)
			if err != nil && !errors.Is(err, helmdriver.ErrReleaseNotFound) {
				return nil, fmt.Errorf("helm get: release %s: %w", chart.Release, err)
			}
		case NextStepRollback:
			// Clear a wedged pending release by rolling back to its last deployed revision,
			// then re-evaluate (the loop then upgrades it). This only removes the pending
			// lock; the upgrade that follows does the real reconciliation.
			logger.Info("rolling back wedged release")
			rb := helmaction.NewRollback(config)
			rb.Timeout = c.timeout
			if err := rb.Run(chart.Release); err != nil {
				return nil, fmt.Errorf("helm rollback: release %s: %w", chart.Release, err)
			}
			r, err = g.Run(chart.Release)
			if err != nil && !errors.Is(err, helmdriver.ErrReleaseNotFound) {
				return nil, fmt.Errorf("helm get: release %s: %w", chart.Release, err)
			}
		case NextStepUpgrade:
			// Upgrade.
			logger.Info("upgrading")
			u := c.newUpgrade(config, chart)
			ch, err := chart.Load(ctx, config)
			if err != nil {
				return nil, fmt.Errorf("helm upgrade: load chart: %w", err)
			}
			vs, err := chart.Values.GetValues(ctx)
			if err != nil {
				return nil, fmt.Errorf("helm upgrade: get values: %w", err)
			}
			r, err = u.RunWithContext(ctx, chart.Release, ch, vs)
			if err != nil {
				return nil, fmt.Errorf("helm upgrade: release %s: %w", chart.Release, err)
			}
			logger.Infof("upgraded: %s", r.Info.Status.String())
		case NextStepReinstall:
			// Uninstall.
			logger.Info("uninstalling")
			ui := helmaction.NewUninstall(config)
			ui.Timeout = c.timeout
			ui.IgnoreNotFound = true
			ui.KeepHistory = false
			ui.Wait = true
			ui.DeletionPropagation = string(meta.DeletePropagationForeground)
			r, err := ui.Run(chart.Release)
			if err != nil && errors.Is(err, helmdriver.ErrReleaseNotFound) {
				return nil, fmt.Errorf("helm uninstall: release %s: %w", chart.Release, err)
			}
			logger.Infof("uninstalled: %s", r.Info)
			fallthrough
		case _NextStepInstall:
			// Install.
			logger.Info("installing")
			i := c.newInstall(config, chart, namespace, r)
			ch, err := chart.Load(ctx, config)
			if err != nil {
				return nil, fmt.Errorf("helm install: load chart: %w", err)
			}
			vs, err := chart.Values.GetValues(ctx)
			if err != nil {
				return nil, fmt.Errorf("helm install: get values: %w", err)
			}
			r, err = i.RunWithContext(ctx, ch, vs)
			if err != nil {
				return nil, fmt.Errorf("helm install: release %s: %w", chart.Release, err)
			}
			logger.Infof("installed: %s", r.Info.Status.String())
		default:
			return helmchartutil.MergeValues(r.Chart, r.Config)
		}
	}
}

// newInstall builds the install action for a release, given whatever release record was
// found for it — nil when there is none.
//
// A first install is not atomic. Atomic implies Wait, so an atomic install blocks until
// every workload the chart deploys is Ready; a process killed inside that wait — the
// container's startup probe gives up long before the Helm timeout — strands a
// pending-install record that no later attempt can get past. Nothing is serving yet on a
// first install, so there is no revision for a rollback to return to and no reason to
// block: it applies and returns, and a release left failed is repaired by the upgrade path
// on the next pass. Installing over a release that does exist stays atomic.
func (c *Client) newInstall(
	config *helmaction.Configuration,
	chart *Chart,
	namespace string,
	existing *helmrelease.Release,
) *helmaction.Install {
	i := helmaction.NewInstall(config)
	i.Timeout = c.timeout
	i.ReleaseName = chart.Release
	i.Namespace = namespace
	i.Atomic = existing != nil
	chart.configureInstall(i)

	return i
}

// newUpgrade builds the upgrade action for a release.
//
// It is always atomic: an upgrade acts on a live release, so a failed one must be rolled
// back to the revision that was serving before it.
func (c *Client) newUpgrade(config *helmaction.Configuration, chart *Chart) *helmaction.Upgrade {
	u := helmaction.NewUpgrade(config)
	u.Timeout = c.timeout
	u.Atomic = true
	u.Recreate = true
	u.Force = true
	chart.configureUpgrade(u)

	return u
}

// createConfig creates a new Helm action configuration.
func (c *Client) createConfig(namespace string) (*helmaction.Configuration, error) {
	logger := klog.Background().WithName("helm")

	// Initialize.
	//
	// NB(thxCode): borrowed from helm.sh/helm/cmd/helm/helm.go
	var config helmaction.Configuration
	cd := "secret"
	cdl := func(format string, v ...any) {
		logger.Infof(format, v...)
	}
	err := config.Init(c.getter, namespace, cd, cdl)
	if err != nil {
		return nil, err
	}

	// Refill the registry client.
	//
	// NB(thxCode): borrowed from helm.sh/helm/cmd/helm/root.go
	ropts := []helmregistry.ClientOption{
		helmregistry.ClientOptDebug(true),
		helmregistry.ClientOptEnableCache(true),
		helmregistry.ClientOptWriter(logger),
	}
	config.RegistryClient, err = helmregistry.NewClient(ropts...)
	if err != nil {
		return nil, fmt.Errorf("create registry client: %w", err)
	}

	return &config, nil
}

func isApiServiceReady(ctx context.Context, c *Client, apiSvcName string) bool {
	if apiSvcName == "" {
		return false
	}

	svcCli := c.KubeClientSet().ApiregistrationV1().APIServices()

	svc, err := svcCli.Get(ctx, apiSvcName,
		meta.GetOptions{
			ResourceVersion: "0",
		})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return false
		}
		return false
	}

	ready := false
	for i := range svc.Status.Conditions {
		if svc.Status.Conditions[i].Type != apireg.Available {
			continue
		}
		ready = svc.Status.Conditions[i].Status == apireg.ConditionTrue
		break
	}
	return ready
}
