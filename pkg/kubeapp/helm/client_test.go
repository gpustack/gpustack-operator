package helm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	helmaction "helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmrelease "helm.sh/helm/v3/pkg/release"
	helmtime "helm.sh/helm/v3/pkg/time"
)

// Test_Client_nextStep pins the release-state decision table, in particular the
// RepairViaUpgradeOnly branch: a bad-status release is upgraded (not uninstalled)
// when the flag is set, and the pre-existing behavior is preserved when it is not.
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
			name:    "an abandoned pending upgrade with RepairViaUpgradeOnly rolls back",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true, ExclusiveAccess: true},
			release: release(helmrelease.StatusPendingUpgrade, "0.18.2", nil, time.Now()),
			timeout: time.Hour,
			want:    NextStepRollback,
		},
		{
			name:    "a pending upgrade past the timeout rolls back without exclusive access",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true},
			release: release(helmrelease.StatusPendingUpgrade, "0.18.2", nil, time.Now().Add(-2*time.Hour)),
			timeout: time.Hour,
			want:    NextStepRollback,
		},
		{
			name:    "an abandoned pending rollback with RepairViaUpgradeOnly rolls back",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true, ExclusiveAccess: true},
			release: release(helmrelease.StatusPendingRollback, "0.18.2", nil, time.Now()),
			timeout: time.Hour,
			want:    NextStepRollback,
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
