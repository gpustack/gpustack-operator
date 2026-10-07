package worker

import (
	"context"
	"encoding/json"
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
		tp    string
		cards string
		valid bool
	}{
		{"legacy", "", "1", true},
		{"explicit one", `,"tensorParallelSize":1`, "1", true},
		{"two cards", `,"tensorParallelSize":2`, "2", true},
		{"missing second card", `,"tensorParallelSize":2`, "1", false},
		{"extra card", `,"tensorParallelSize":2`, "3", false},
		{"fractional", `,"tensorParallelSize":2`, "1.5", false},
		{"zero", `,"tensorParallelSize":0`, "1", false},
		{"negative", `,"tensorParallelSize":-1`, "1", false},
		{"unsupported three", `,"tensorParallelSize":3`, "3", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := elasticMD()
			role := &md.Spec.Roles[0]
			require.NoError(t, json.Unmarshal([]byte(`{"width":2,"headInstanceType":"cpu-head"`+tc.tp+`}`), role.ElasticEP))
			role.Resources = &workercore.ModelDeploymentRoleResources{Accelerator: resource.NewQuantity(1, resource.DecimalSI)}
			q := resource.MustParse(tc.cards)
			role.Resources.Accelerator = &q
			errs := validateModelDeploymentElasticShape(md, modelDeploymentElasticRoles(md))
			assert.Equal(t, tc.valid, len(errs) == 0, "%v", errs)
		})
	}
}

func TestElasticTensorParallelIdentity(t *testing.T) {
	cases := []struct {
		name, oldTP, newTP string
		valid              bool
	}{
		{"legacy to explicit one", "", `,"tensorParallelSize":1`, true},
		{"one to two", "", `,"tensorParallelSize":2`, false},
		{"two to one", `,"tensorParallelSize":2`, `,"tensorParallelSize":1`, false},
		{"two removed", `,"tensorParallelSize":2`, "", false},
		{"two unchanged scale up", `,"tensorParallelSize":2`, `,"tensorParallelSize":2`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old, md := elasticMD(), elasticMD()
			require.NoError(t, json.Unmarshal([]byte(`{"width":2,"headInstanceType":"cpu-head"`+tc.oldTP+`}`), old.Spec.Roles[0].ElasticEP))
			require.NoError(t, json.Unmarshal([]byte(`{"width":4,"headInstanceType":"cpu-head"`+tc.newTP+`}`), md.Spec.Roles[0].ElasticEP))
			assert.Equal(t, tc.valid, len(validateModelDeploymentElasticIdentity(md, old)) == 0)
		})
	}
}

func TestElasticTensorParallelDefault(t *testing.T) {
	cases := []struct {
		name, tp, explicit string
		want               int64
	}{
		{"legacy", "", "", 1},
		{"two", `,"tensorParallelSize":2`, "", 2},
		{"preserve explicit", `,"tensorParallelSize":2`, "1", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := elasticMD()
			md.Spec.Roles[0].InstanceType = "h20-8x"
			require.NoError(t, json.Unmarshal([]byte(`{"width":2,"headInstanceType":"cpu-head"`+tc.tp+`}`), md.Spec.Roles[0].ElasticEP))
			md.Spec.Roles[0].Resources = nil
			if tc.explicit != "" {
				q := resource.MustParse(tc.explicit)
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Accelerator: &q}
			}
			w := newModelDeploymentWebhookWith([]ctrlcli.Object{acceleratableInstanceType("h20-8x", true)})
			require.NoError(t, w.Default(context.Background(), md))
			require.NotNil(t, md.Spec.Roles[0].Resources)
			assert.Equal(t, tc.want, md.Spec.Roles[0].Resources.Accelerator.Value())
		})
	}
}
