package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestModelDeploymentBalanceRouting(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		kind     workercore.ModelDeploymentRoleKind
		balance  workercore.ModelDeploymentLoadBalance
		external bool
		reason   string
	}{
		{name: "normalized hybrid internal", args: []string{"--data-parallel-hybrid-lb", "--data-parallel-size=4", "--data-parallel-size-local=4"}, balance: workercore.ModelDeploymentLoadBalanceInternal},
		{name: "explicit single node internal", args: []string{"--data-parallel-size=4", "--data-parallel-start-rank=0", "--nnodes=1"}, balance: workercore.ModelDeploymentLoadBalanceInternal},
		{name: "unreadable node count", args: []string{"--data-parallel-start-rank=0", "--nnodes=2:3"}, balance: workercore.ModelDeploymentLoadBalanceUnknown, reason: "nnodes must be a readable positive integer"},
		{name: "missing node count", args: []string{"--data-parallel-start-rank=0", "--nnodes"}, balance: workercore.ModelDeploymentLoadBalanceUnknown, reason: "nnodes must be a readable positive integer"},
		{name: "nonpositive node count", args: []string{"--data-parallel-start-rank=0", "--nnodes=0"}, balance: workercore.ModelDeploymentLoadBalanceUnknown, reason: "nnodes must be a readable positive integer"},
		{name: "multiple nodes need local size", args: []string{"--data-parallel-start-rank=0", "--nnodes=2"}, balance: workercore.ModelDeploymentLoadBalanceUnknown, reason: "requires an explicit data-parallel-size-local"},
		{name: "internal", balance: workercore.ModelDeploymentLoadBalanceInternal},
		{name: "rank", args: []string{"--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"}, balance: workercore.ModelDeploymentLoadBalanceExternal, external: true},
		{name: "start rank alone", args: []string{"--data-parallel-start-rank=1"}, balance: workercore.ModelDeploymentLoadBalanceInternal},
		{name: "implicit external", args: []string{"--data-parallel-size=4", "--data-parallel-size-local=1", "--data-parallel-start-rank=1"}, balance: workercore.ModelDeploymentLoadBalanceExternal, external: true},
		{name: "implicit hybrid", args: []string{"--data-parallel-size=4", "--data-parallel-size-local=2", "--data-parallel-start-rank=2"}, balance: workercore.ModelDeploymentLoadBalanceHybrid, external: true},
		{name: "all local internal", args: []string{"--data-parallel-size=4", "--data-parallel-size-local=4", "--data-parallel-start-rank=0"}, balance: workercore.ModelDeploymentLoadBalanceInternal},
		{name: "headless internal", args: []string{"--data-parallel-size=4", "--data-parallel-size-local=1", "--data-parallel-start-rank=1", "--headless"}, balance: workercore.ModelDeploymentLoadBalanceInternal},
		{name: "hybrid", args: []string{"--data-parallel-hybrid-lb"}, balance: workercore.ModelDeploymentLoadBalanceHybrid, external: true},
		{name: "multi port", args: []string{"--data-parallel-multi-port-external-lb"}, balance: workercore.ModelDeploymentLoadBalanceMultiPort, external: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			role := &md.Spec.Roles[0]
			role.ExtraArgs = tc.args
			role.Kind = tc.kind
			status := ReadModelDeploymentRoleParallelism(md.Spec.Engine.Name, role)
			assert.Equal(t, tc.balance, status.LoadBalance)
			assert.Equal(t, tc.external, modelDeploymentRoleExternalDP(md, role))
			if tc.reason != "" {
				assert.Contains(t, status.Source.UnreadableReason, tc.reason)
			}
		})
	}
}
