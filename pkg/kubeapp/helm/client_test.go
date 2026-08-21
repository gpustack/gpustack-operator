package helm

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	helmaction "helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmchartutil "helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	helmrelease "helm.sh/helm/v3/pkg/release"
	helmstorage "helm.sh/helm/v3/pkg/storage"
	helmdriver "helm.sh/helm/v3/pkg/storage/driver"
	helmtime "helm.sh/helm/v3/pkg/time"
)

// Test_Client_nextStep pins the release-state decision table, in particular the
// RepairViaUpgradeOnly branch: a bad-status release is upgraded (not uninstalled)
// when the flag is set, an abandoned pending upgrade/rollback has its record
// discarded (not rolled back — a rollback would delete adopted objects), and the
// pre-existing behavior is preserved when the flag is not set.
func Test_Client_nextStep(t *testing.T) {
	release := func(status helmrelease.Status, chartVersion string, cfg map[string]any, lastDeployed time.Time) *helmrelease.Release {
		return &helmrelease.Release{
			Info:   &helmrelease.Info{Status: status, LastDeployed: helmtime.Time{Time: lastDeployed}},
			Chart:  &helmchart.Chart{Metadata: &helmchart.Metadata{Version: chartVersion}},
			Config: cfg,
		}
	}

	cases := []struct {
		name    string
		chart   *Chart
		release *helmrelease.Release
		timeout time.Duration
		want    NextStepType
	}{
		{
			name:    "deployed with version mismatch upgrades",
			chart:   &Chart{Version: "0.18.2"},
			release: release(helmrelease.StatusDeployed, "0.18.0", nil, time.Time{}),
			want:    NextStepUpgrade,
		},
		{
			name:    "deployed with same version and values is done",
			chart:   &Chart{Version: "0.18.2", Values: StaticValues{"a": "b"}},
			release: release(helmrelease.StatusDeployed, "0.18.2", map[string]any{"a": "b"}, time.Time{}),
			want:    NextStepDone,
		},
		{
			name:    "deployed with same version but changed values upgrades",
			chart:   &Chart{Version: "0.18.2", Values: StaticValues{"a": "c"}},
			release: release(helmrelease.StatusDeployed, "0.18.2", map[string]any{"a": "b"}, time.Time{}),
			want:    NextStepUpgrade,
		},
		{
			name:    "failed without RepairViaUpgradeOnly reinstalls",
			chart:   &Chart{Version: "0.18.2"},
			release: release(helmrelease.StatusFailed, "0.18.2", nil, time.Time{}),
			want:    NextStepReinstall,
		},
		{
			name:    "failed with RepairViaUpgradeOnly upgrades",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true},
			release: release(helmrelease.StatusFailed, "0.18.2", nil, time.Time{}),
			want:    NextStepUpgrade,
		},
		{
			name:    "unknown with RepairViaUpgradeOnly upgrades",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true},
			release: release(helmrelease.StatusUnknown, "0.18.2", nil, time.Time{}),
			want:    NextStepUpgrade,
		},
		{
			name:    "pending within timeout requeues",
			chart:   &Chart{Version: "0.18.2"},
			release: release(helmrelease.StatusPendingUpgrade, "0.18.2", nil, time.Now().Add(-time.Minute)),
			timeout: time.Hour,
			want:    NextStepRequeue,
		},
		{
			name:    "pending past timeout installs",
			chart:   &Chart{Version: "0.18.2"},
			release: release(helmrelease.StatusPendingUpgrade, "0.18.2", nil, time.Now().Add(-2*time.Hour)),
			timeout: time.Hour,
			want:    _NextStepInstall,
		},
		{
			name:    "a peer's pending upgrade is left alone, not rolled back",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true},
			release: release(helmrelease.StatusPendingUpgrade, "0.18.2", nil, time.Now()),
			timeout: time.Hour,
			want:    NextStepRequeue,
		},
		{
			name:    "an abandoned pending upgrade with RepairViaUpgradeOnly discards the record",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true, ExclusiveAccess: true},
			release: release(helmrelease.StatusPendingUpgrade, "0.18.2", nil, time.Now()),
			timeout: time.Hour,
			want:    _NextStepDiscard,
		},
		{
			name:    "a pending upgrade past the timeout discards the record without exclusive access",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true},
			release: release(helmrelease.StatusPendingUpgrade, "0.18.2", nil, time.Now().Add(-2*time.Hour)),
			timeout: time.Hour,
			want:    _NextStepDiscard,
		},
		{
			name:    "an abandoned pending rollback with RepairViaUpgradeOnly discards the record",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true, ExclusiveAccess: true},
			release: release(helmrelease.StatusPendingRollback, "0.18.2", nil, time.Now()),
			timeout: time.Hour,
			want:    _NextStepDiscard,
		},
		{
			name:    "a peer's pending install is left alone",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true},
			release: release(helmrelease.StatusPendingInstall, "0.18.2", nil, time.Now()),
			timeout: time.Hour,
			want:    NextStepRequeue,
		},
		{
			name:    "an abandoned pending install has its record discarded",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true, ExclusiveAccess: true},
			release: release(helmrelease.StatusPendingInstall, "0.18.2", nil, time.Now()),
			timeout: time.Hour,
			want:    _NextStepDiscard,
		},
		{
			name:    "a pending install past the timeout has its record discarded",
			chart:   &Chart{Version: "0.18.2"},
			release: release(helmrelease.StatusPendingInstall, "0.18.2", nil, time.Now().Add(-2*time.Hour)),
			timeout: time.Hour,
			want:    _NextStepDiscard,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cli := &Client{timeout: c.timeout}
			assert.Equal(t, c.want, cli.nextStep(t.Context(), c.release, c.chart))
		})
	}
}

// Test_Client_newInstall pins the atomicity of an install to whether a release record was
// found for it, which is what keeps a first install from wedging.
//
// Atomic implies Wait (helm.sh/helm/v3/pkg/action.(*Install).RunWithContext), so an atomic
// install of this chart blocks until every workload it deploys is Ready. A process killed
// inside that wait — the container's startup probe gives up long before the Helm timeout —
// strands a pending-install record that no later attempt can get past. There is nothing
// live to protect on a first install, so it applies and returns instead, and a release left
// failed is repaired by the upgrade path on the next boot.
func Test_Client_newInstall(t *testing.T) {
	cases := []struct {
		name       string
		existing   *helmrelease.Release
		wantAtomic bool
	}{
		{
			name:       "no release record is a permissive first install",
			existing:   nil,
			wantAtomic: false,
		},
		{
			name:       "an existing release is rolled back on failure",
			existing:   &helmrelease.Release{Info: &helmrelease.Info{Status: helmrelease.StatusFailed}},
			wantAtomic: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := &Client{timeout: time.Hour}
			chart := &Chart{Name: "kueue", Release: "gpustack-kueue", Path: "kueue.tgz"}

			i := cli.newInstall(&helmaction.Configuration{}, chart, "gpustack-system", c.existing)
			assert.Equal(t, c.wantAtomic, i.Atomic)
			assert.Equal(t, chart.Release, i.ReleaseName)
			assert.Equal(t, "gpustack-system", i.Namespace)
			assert.Equal(t, time.Hour, i.Timeout)
		})
	}
}

// Test_Client_newUpgrade holds the upgrade atomic: it acts on a live release, so a failure
// must roll back to the revision that was serving before it.
//
// It also holds the upgrade unforced. A forced upgrade replaces every object rather than
// patching it, and a replace is a read of the live object followed by a write of the whole
// rendered one. Two things follow, both measured against a cluster. The write carries the
// resourceVersion the read returned, so a write by anyone else inside that window fails the
// upgrade with "the object has been modified" — and with the upgrade atomic, a release that
// has never had a successful revision cannot roll back, so it stays failed and every retry
// re-enters the same operation. And the object written is the rendered manifest, so a field
// the chart does not render is dropped: the bundled Kueue runs its own cert controller and
// the chart renders no caBundle, so a forced upgrade strips the conversion-webhook CA off
// Kueue's CRDs, which is what makes that window contested on every pass.
func Test_Client_newUpgrade(t *testing.T) {
	cli := &Client{timeout: time.Hour}

	u := cli.newUpgrade(&helmaction.Configuration{}, &Chart{Name: "kueue", Release: "gpustack-kueue"})
	assert.True(t, u.Atomic)
	assert.False(t, u.Force)
	assert.Equal(t, time.Hour, u.Timeout)
}

// Test_Client_InstallWith pins the argument guards, which reject before the first
// cluster call. The convergence loop itself needs a cluster and is covered by e2e.
func Test_Client_InstallWith(t *testing.T) {
	cases := []struct {
		name    string
		chart   *Chart
		next    NextStepConditionFunc
		wantErr string
	}{
		{
			name:    "with invalid chart",
			chart:   &Chart{},
			next:    func(*helmrelease.Release) NextStepType { return NextStepDone },
			wantErr: "validate chart: name is required",
		},
		{
			name:    "without next",
			chart:   &Chart{Name: "kueue", Release: "gpustack-kueue", Path: "kueue.tgz"},
			wantErr: "next is required",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := &Client{}
			got, err := cli.InstallWith(t.Context(), c.chart, c.next)
			assert.EqualError(t, err, c.wantErr)
			assert.Nil(t, got)
		})
	}
}

// newConvergeTestBed builds the convergence loop's test rig: a one-manifest chart packaged on
// the fly, an in-memory Helm storage plus a printing kube client to act against, and a release
// seeded with a deployed revision 1 beneath an abandoned pending revision 2 — both already at
// the target chart version and values, so once the pending record is gone the release reads as
// converged, which is exactly the Done the forced upgrade must turn into one upgrade.
func newConvergeTestBed(t *testing.T) (*Client, *Chart, *helmaction.Configuration) {
	t.Helper()

	ch := &helmchart.Chart{
		Metadata: &helmchart.Metadata{Name: "test", Version: "0.1.1", APIVersion: helmchart.APIVersionV2},
		Templates: []*helmchart.File{
			{
				Name: "templates/configmap.yaml",
				Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n  namespace: {{ .Release.Namespace }}\n"),
			},
		},
	}
	chartPath, err := helmchartutil.Save(ch, t.TempDir())
	assert.NoError(t, err)

	config := &helmaction.Configuration{
		Releases:     helmstorage.Init(helmdriver.NewMemory()),
		KubeClient:   &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}},
		Capabilities: helmchartutil.DefaultCapabilities,
		Log:          func(format string, v ...any) { t.Logf(format, v...) },
	}

	old := time.Now().Add(-2 * time.Hour)
	seed := func(version int, status helmrelease.Status) *helmrelease.Release {
		return &helmrelease.Release{
			Name:      "test",
			Namespace: "default",
			Version:   version,
			Info:      &helmrelease.Info{Status: status, LastDeployed: helmtime.Time{Time: old}},
			Chart:     ch,
			Config:    map[string]any{"a": "b"},
		}
	}
	assert.NoError(t, config.Releases.Create(seed(1, helmrelease.StatusDeployed)))
	assert.NoError(t, config.Releases.Create(seed(2, helmrelease.StatusPendingUpgrade)))

	cli := &Client{timeout: time.Hour}
	chart := &Chart{
		Name:                 "test",
		Release:              "test",
		Version:              "0.1.1",
		Path:                 chartPath,
		Values:               StaticValues{"a": "b"},
		RepairViaUpgradeOnly: true,
	}
	return cli, chart, config
}

// Test_Client_converge_discardThenUpgradeOnce runs the convergence loop end to end against an
// in-memory Helm storage and a printing kube client, pinning the repair this file's decision
// table only half-covers (gpustack-operator#122): a release with a deployed revision beneath
// an abandoned pending upgrade must converge by discarding the pending record and firing
// exactly one upgrade — never a rollback, which would re-apply the old manifest over whatever
// the killed operation had already adopted.
func Test_Client_converge_discardThenUpgradeOnce(t *testing.T) {
	cli, chart, config := newConvergeTestBed(t)
	next := func(r *helmrelease.Release) NextStepType { return cli.nextStep(t.Context(), r, chart) }

	last, err := config.Releases.Last("test")
	assert.NoError(t, err)
	_, err = cli.converge(t.Context(), config, chart, next, "default", last)
	assert.NoError(t, err)

	// The pending record is gone and the forced upgrade fired: a new revision 2 stands
	// deployed over the superseded revision 1.
	history, err := config.Releases.History("test")
	assert.NoError(t, err)
	if assert.Len(t, history, 2) {
		assert.Equal(t, helmrelease.StatusSuperseded, history[0].Info.Status)
		assert.Equal(t, helmrelease.StatusDeployed, history[1].Info.Status)
		assert.Equal(t, 2, history[1].Version)
	}

	// Fired once: a second pass reads converged and adds no revision.
	last, err = config.Releases.Last("test")
	assert.NoError(t, err)
	_, err = cli.converge(t.Context(), config, chart, next, "default", last)
	assert.NoError(t, err)
	history, err = config.Releases.History("test")
	assert.NoError(t, err)
	assert.Len(t, history, 2)
}

// Test_Client_converge_requeueKeepsForcedUpgrade pins that a requeue between the discard and
// the converged read does not spend the forced upgrade: the flag is consumed by the upgrade it
// forces, not by an iteration that fired nothing.
func Test_Client_converge_requeueKeepsForcedUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("the requeue arm waits out a fixed 10s before re-reading the release")
	}

	cli, chart, config := newConvergeTestBed(t)
	var requeued bool
	next := func(r *helmrelease.Release) NextStepType {
		if r.Info.Status == helmrelease.StatusDeployed && !requeued {
			requeued = true
			return NextStepRequeue
		}
		return cli.nextStep(t.Context(), r, chart)
	}

	last, err := config.Releases.Last("test")
	assert.NoError(t, err)
	_, err = cli.converge(t.Context(), config, chart, next, "default", last)
	assert.NoError(t, err)

	// The forced upgrade survived the requeue and fired: a new revision 2 stands deployed.
	history, err := config.Releases.History("test")
	assert.NoError(t, err)
	if assert.Len(t, history, 2) {
		assert.Equal(t, helmrelease.StatusDeployed, history[1].Info.Status)
		assert.Equal(t, 2, history[1].Version)
	}
}
