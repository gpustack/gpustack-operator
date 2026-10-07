package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestElasticTensorParallelShape(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		cards string
		valid bool
	}{
		{"legacy", nil, "1", true},
		{"explicit one", []string{"--tensor-parallel-size", "1"}, "1", true},
		{"two cards", []string{"--tensor-parallel-size", "2"}, "2", true},
		{"four cards", []string{"--tensor-parallel-size=4"}, "4", true},
		{"eight cards", []string{"-tp", "8"}, "8", true},
		{"three is not an operator limit", []string{"--tensor_parallel_size", "3"}, "3", true},
		{"last spelling wins", []string{"--tensor-parallel-size", "2", "-tp", "4"}, "4", true},
		{"missing second card", []string{"--tensor-parallel-size", "2"}, "1", false},
		{"extra card", []string{"--tensor-parallel-size", "2"}, "3", false},
		{"fractional cards", []string{"--tensor-parallel-size", "2"}, "1.5", false},
		{"zero", []string{"--tensor-parallel-size", "0"}, "1", false},
		{"negative", []string{"--tensor-parallel-size", "-1"}, "1", false},
		{"unreadable", []string{"-tp", "banana"}, "1", false},
		{"missing value", []string{"-tp"}, "1", false},
		{"explicit PP one", []string{"--tensor-parallel-size", "2", "-pp", "1"}, "2", true},
		{"PP two", []string{"--tensor-parallel-size", "2", "--pipeline-parallel-size", "2"}, "2", false},
		{"PCP two", []string{"-tp", "2", "-pcp", "2"}, "2", false},
		{"operator DP override", []string{"--data-parallel-size", "4"}, "1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := elasticMD()
			role := &md.Spec.Roles[0]
			role.ExtraArgs = tc.args
			q := resource.MustParse(tc.cards)
			role.Resources = &workercore.ModelDeploymentRoleResources{Accelerator: &q}
			errs := validateModelDeploymentElasticShape(md, modelDeploymentElasticRoles(md))
			assert.Equal(t, tc.valid, len(errs) == 0, "%v", errs)
		})
	}
}

func TestElasticTensorParallelIdentity(t *testing.T) {
	cases := []struct {
		name    string
		oldArgs []string
		newArgs []string
		valid   bool
	}{
		{"legacy to explicit one", nil, []string{"-tp", "1"}, true},
		{"one to two", nil, []string{"-tp", "2"}, false},
		{"two to one", []string{"-tp", "2"}, []string{"-tp", "1"}, false},
		{"two removed", []string{"-tp", "2"}, nil, false},
		{"unchanged TP scale up", []string{"-tp", "4"}, []string{"--tensor-parallel-size=4"}, true},
		{"other tuning changes", []string{"-tp", "8", "--max-model-len", "2048"}, []string{"-tp", "8", "--max-model-len", "4096"}, true},
		{"new TP unreadable", []string{"-tp", "2"}, []string{"-tp", "bad"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old, md := elasticMD(), elasticMD()
			old.Spec.Roles[0].ExtraArgs = tc.oldArgs
			md.Spec.Roles[0].ExtraArgs = tc.newArgs
			md.Spec.Roles[0].ElasticEP.Width = 4
			assert.Equal(t, tc.valid, len(validateModelDeploymentElasticIdentity(md, old)) == 0)
		})
	}
}

func TestElasticTensorParallelDefault(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		explicit string
		want     int64
		valid    bool
	}{
		{"legacy", nil, "", 1, true},
		{"two", []string{"-tp", "2"}, "", 2, true},
		{"four", []string{"--tensor-parallel-size=4"}, "", 4, true},
		{"eight", []string{"-tp", "8"}, "", 8, true},
		{"preserve explicit", []string{"-tp", "2"}, "1", 1, true},
		{"unreadable", []string{"-tp", "bad"}, "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := elasticMD()
			md.Spec.Roles[0].InstanceType = "h20-8x"
			md.Spec.Roles[0].ExtraArgs = tc.args
			md.Spec.Roles[0].Resources = nil
			if tc.explicit != "" {
				q := resource.MustParse(tc.explicit)
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Accelerator: &q}
			}
			w := newModelDeploymentWebhookWith([]ctrlcli.Object{acceleratableInstanceType("h20-8x", true)})
			err := w.Default(context.Background(), md)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, md.Spec.Roles[0].Resources)
			assert.Equal(t, tc.want, md.Spec.Roles[0].Resources.Accelerator.Value())
		})
	}
}
