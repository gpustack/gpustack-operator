package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func newInstanceType(cpu, ram, localStorage string) *workercore.InstanceType {
	return &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "gpustack-generic-linux-amd64"},
		Spec: workercore.InstanceTypeSpec{
			UnitResources: workercore.InstanceTypeUnitResources{CPU: cpu, RAM: ram},
			LocalStorage:  localStorage,
		},
	}
}

// TestInstanceTypeWebhook_ValidateUnitSpec pins the all-or-nothing unit-spec rule: an
// unset unit spec is accepted (derived pools leave it empty), but a set one must have
// all three fields well-formed.
func TestInstanceTypeWebhook_ValidateUnitSpec(t *testing.T) {
	cases := []struct {
		name                   string
		cpu, ram, localStorage string
		wantErr                bool
	}{
		{"unset is accepted (derived)", "", "", "", false},
		{"all three valid", "12", "48Gi", "100Gi", false},
		{"partial: only cpu", "12", "", "", true},
		{"partial: missing localStorage", "12", "48Gi", "", true},
		{"cpu with unit suffix", "12Gi", "48Gi", "100Gi", true},
		{"cpu fractional", "0.5", "48Gi", "100Gi", true},
		{"cpu zero", "0", "48Gi", "100Gi", true},
		{"ram without Gi suffix", "12", "48", "100Gi", true},
		{"ram lowercase gi", "12", "48gi", "100Gi", true},
		{"localStorage zero", "12", "48Gi", "0Gi", true},
	}

	wh := &InstanceTypeWebhook{}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := wh.ValidateCreate(context.Background(), newInstanceType(c.cpu, c.ram, c.localStorage))
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestInstanceTypeWebhook_ValidateUpdate pins that update is validated the same way.
func TestInstanceTypeWebhook_ValidateUpdate(t *testing.T) {
	wh := &InstanceTypeWebhook{}
	_, err := wh.ValidateUpdate(context.Background(),
		newInstanceType("12", "48Gi", "100Gi"), newInstanceType("12", "48", "100Gi"))
	assert.Error(t, err, "an invalid unit spec is rejected on update")
}
