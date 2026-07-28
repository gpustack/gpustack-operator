package helm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
			name:    "pending upgrade with RepairViaUpgradeOnly rolls back",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true},
			release: release(helmrelease.StatusPendingUpgrade, "0.18.2", nil, time.Now()),
			timeout: time.Hour,
			want:    NextStepRollback,
		},
		{
			name:    "pending rollback with RepairViaUpgradeOnly rolls back",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true},
			release: release(helmrelease.StatusPendingRollback, "0.18.2", nil, time.Now()),
			timeout: time.Hour,
			want:    NextStepRollback,
		},
		{
			name:    "pending install with RepairViaUpgradeOnly still requeues",
			chart:   &Chart{Version: "0.18.2", RepairViaUpgradeOnly: true},
			release: release(helmrelease.StatusPendingInstall, "0.18.2", nil, time.Now()),
			timeout: time.Hour,
			want:    NextStepRequeue,
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
