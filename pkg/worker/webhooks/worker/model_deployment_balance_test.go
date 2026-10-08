package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestModelDeploymentManagedBalance(t *testing.T) {
	cases := []struct {
		name          string
		args, command []string
		kind          workercore.ModelDeploymentRoleKind
		want          string
	}{
		{name: "internal"},
		{name: "single node internal", args: []string{"--data-parallel-size=4", "--data-parallel-start-rank=0", "--nnodes=1"}},
		{name: "external rank", args: []string{"--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"}},
		{name: "multi port", args: []string{"-dpm"}, want: "MultiPort"},
		{name: "conflicting modes", args: []string{"-dpe", "-dph"}, want: "different shapes"},
		{name: "headless internal", args: []string{"--data-parallel-size=4", "--data-parallel-size-local=1", "--data-parallel-start-rank=1", "--headless"}},
		{name: "custom command", command: []string{"custom-engine"}, args: []string{"-dpm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := modelDeployment(workercore.ModelDeploymentEngineVLLM)
			role := &md.Spec.Roles[0]
			role.ExtraArgs = tc.args
			role.Command = tc.command
			role.Kind = tc.kind
			errs := validateModelDeploymentRoles(md)
			if tc.want == "" {
				assert.Empty(t, errs)
			} else if assert.NotEmpty(t, errs) {
				assert.Contains(t, errs.ToAggregate().Error(), tc.want)
			}
		})
	}
}
