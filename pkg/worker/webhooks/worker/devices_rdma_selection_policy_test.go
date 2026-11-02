package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/webhook"
)

func TestValidateDevicesRDMASelectionPolicy(t *testing.T) {
	valid := func() *workercore.DevicesRDMASelectionPolicy {
		return &workercore.DevicesRDMASelectionPolicy{
			Spec: workercore.DevicesRDMASelectionPolicySpec{
				NodeSelector: &metav1.LabelSelector{},
			},
		}
	}

	tests := []struct {
		name  string
		edit  func(*workercore.DevicesRDMASelectionPolicy)
		valid bool
	}{
		{name: "empty selector matches every node", edit: func(*workercore.DevicesRDMASelectionPolicy) {}, valid: true},
		{name: "match labels selector", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"example.com/rdma-group": "compute-a"}}
		}, valid: true},
		{name: "match expressions selector", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.NodeSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "example.com/rdma-group",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"compute-a", "compute-b"},
			}}}
		}, valid: true},
		{name: "missing selector", edit: func(p *workercore.DevicesRDMASelectionPolicy) { p.Spec.NodeSelector = nil }},
		{name: "invalid selector operator", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.NodeSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "example.com/rdma-group", Operator: metav1.LabelSelectorOpIn,
			}}}
		}},
		{name: "invalid selector label key", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"a b": "c"}}
		}},
		{name: "include with names", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"mlx5_4", "mlx5_5"}
		}, valid: true},
		{name: "exclude list", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Exclude = []string{"mlx5_5"}
		}, valid: true},
		{name: "include and exclude together are mutually exclusive", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"mlx5_4"}
			p.Spec.Exclude = []string{"mlx5_4"}
		}},
		{name: "include and exclude of disjoint names are still mutually exclusive", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"mlx5_4"}
			p.Spec.Exclude = []string{"mlx5_5"}
		}},
		{name: "duplicate include names", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"mlx5_4", "mlx5_4"}
		}},
		{name: "duplicate exclude names", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Exclude = []string{"mlx5_4", "mlx5_4"}
		}},
		{name: "empty include name", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"mlx5_4", ""}
		}},
		{name: "blank include name", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"  "}
		}},
		{name: "include name with surrounding whitespace", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{" mlx5_4"}
		}},
		{name: "include name with inner whitespace", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"mlx5 4"}
		}},
		{name: "include name with glob wildcard", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"mlx5_*"}
		}},
		{name: "include name with question mark", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"mlx5_?"}
		}},
		{name: "include name with exact prefix syntax", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"=mlx5_4"}
		}},
		{name: "include name with exclusion prefix syntax", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"^mlx5_4"}
		}},
		{name: "include name with port suffix", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"mlx5_4:1"}
		}},
		{name: "exclude name with wildcard", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Exclude = []string{"mlx5_*"}
		}},
		{name: "exclude name with port suffix", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Exclude = []string{"mlx5_4:1"}
		}},
		{name: "underscore and digit names are valid", edit: func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"mlx5_1", "mlx5_10", "hns1", "siw_0"}
		}, valid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := valid()
			tt.edit(policy)
			errs := validateDevicesRDMASelectionPolicy(policy)
			if tt.valid {
				assert.Empty(t, errs, "expected a valid policy: %v", errs.ToAggregate())
				return
			}
			assert.NotEmpty(t, errs, "expected the policy to be rejected")
			for _, err := range errs {
				assert.NotEmpty(t, err.Field, "every error carries a field path")
			}
		})
	}
}

func TestDevicesRDMASelectionPolicyWebhookValidationBoundary(t *testing.T) {
	hook := &DevicesRDMASelectionPolicyWebhook{}

	obj, err := hook.SetupWebhook(context.TODO(), webhook.SetupOptions{})
	require.NoError(t, err)
	require.NotNil(t, obj)

	policy := &workercore.DevicesRDMASelectionPolicy{
		Spec: workercore.DevicesRDMASelectionPolicySpec{
			NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"example.com/rdma-group": "compute-a"}},
			Include:      []string{"mlx5_4"},
		},
	}

	warnings, err := hook.ValidateCreate(context.TODO(), policy)
	assert.Empty(t, warnings)
	assert.NoError(t, err)

	warnings, err = hook.ValidateUpdate(context.TODO(), policy.DeepCopy(), policy)
	assert.Empty(t, warnings)
	assert.NoError(t, err)

	warnings, err = hook.ValidateDelete(context.TODO(), policy)
	assert.Empty(t, warnings)
	assert.NoError(t, err)

	policy.Spec.NodeSelector = nil
	_, err = hook.ValidateCreate(context.TODO(), policy)
	assert.Error(t, err, "a missing selector stays invalid at admission")
}

// TestValidateRDMAHCANamesRejectWhitespaceAnywhere pins that any whitespace character
// anywhere in an HCA name is rejected, including control and Unicode spaces that a
// trimmed-prefix or ASCII-only check lets through, on both the include and exclude lists.
func TestValidateRDMAHCANamesRejectWhitespaceAnywhere(t *testing.T) {
	tests := []struct {
		name    string
		include []string
		exclude []string
		valid   bool
	}{
		{name: "include name with inner carriage return", include: []string{"mlx5\r4"}},
		{name: "exclude name with inner carriage return", exclude: []string{"mlx5\r4"}},
		{name: "include name with inner non-breaking space", include: []string{"mlx5\u00a04"}},
		{name: "exclude name with inner non-breaking space", exclude: []string{"mlx5\u00a04"}},
		{name: "include name with inner vertical tab", include: []string{"mlx5\v4"}},
		{name: "include name with inner next line", include: []string{"mlx5\u00854"}},
		{name: "exclude name with inner ideographic space", exclude: []string{"mlx5\u30004"}},
		{name: "exclude name with leading non-breaking space", exclude: []string{"\u00a0mlx5_4"}},
		{name: "bare include names stay valid", include: []string{"mlx5_4", "mlx5_10"}, valid: true},
		{name: "bare exclude names stay valid", exclude: []string{"ibp1"}, valid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := &workercore.DevicesRDMASelectionPolicy{
				Spec: workercore.DevicesRDMASelectionPolicySpec{
					NodeSelector: &metav1.LabelSelector{},
				},
			}
			if tt.include != nil {
				policy.Spec.Include = tt.include
			}
			policy.Spec.Exclude = tt.exclude

			errs := validateDevicesRDMASelectionPolicy(policy)
			if tt.valid {
				assert.Empty(t, errs, "expected a valid policy: %v", errs.ToAggregate())
				return
			}
			assert.NotEmpty(t, errs, "expected whitespace inside the name to be rejected")
			for _, err := range errs {
				assert.NotEmpty(t, err.Field, "every error carries a field path")
			}
		})
	}
}
