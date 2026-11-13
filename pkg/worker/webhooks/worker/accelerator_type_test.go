package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func newAcceleratorType() *workercore.AcceleratorType {
	return &workercore.AcceleratorType{
		ObjectMeta: meta.ObjectMeta{Name: "gpustack--nvidia-a10g-linux-amd64"},
		Spec: workercore.AcceleratorTypeSpec{
			AcceleratorGroup: "nvidia-a10g",
			GeneralGroup:     "generic",
			OS:               "linux",
			Arch:             "amd64",
		},
	}
}

func TestAcceleratorTypeWebhook_ValidateCreateRequired(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(at *workercore.AcceleratorType)
		wantErr bool
	}{
		{"valid spec is accepted", func(at *workercore.AcceleratorType) {}, false},
		{"missing acceleratorGroup", func(at *workercore.AcceleratorType) { at.Spec.AcceleratorGroup = "" }, true},
		{"missing os", func(at *workercore.AcceleratorType) { at.Spec.OS = "" }, true},
		{"missing arch", func(at *workercore.AcceleratorType) { at.Spec.Arch = "" }, true},
	}

	wh := &AcceleratorTypeWebhook{}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			at := newAcceleratorType()
			c.mutate(at)
			_, err := wh.ValidateCreate(context.Background(), at)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestAcceleratorTypeWebhook_ValidateUpdateImmutable(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(at *workercore.AcceleratorType)
		wantErr bool
	}{
		{"displayName change is allowed", func(at *workercore.AcceleratorType) { at.Spec.DisplayName = "Renamed" }, false},
		{"description change is allowed", func(at *workercore.AcceleratorType) { at.Spec.Description = "note" }, false},
		{"inactive toggle is allowed", func(at *workercore.AcceleratorType) { at.Spec.Inactive = true }, false},
		{"os change is rejected", func(at *workercore.AcceleratorType) { at.Spec.OS = "windows" }, true},
		{"arch change is rejected", func(at *workercore.AcceleratorType) { at.Spec.Arch = "arm64" }, true},
		{"generalGroup change is rejected", func(at *workercore.AcceleratorType) { at.Spec.GeneralGroup = "amd-epyc-7763" }, true},
		{"acceleratorGroup change is rejected", func(at *workercore.AcceleratorType) { at.Spec.AcceleratorGroup = "nvidia-t4" }, true},
	}

	wh := &AcceleratorTypeWebhook{}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			base := newAcceleratorType()
			updated := base.DeepCopy()
			c.mutate(updated)
			_, err := wh.ValidateUpdate(context.Background(), base, updated)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestAcceleratorTypeWebhook_DefaultStampsScheduleLabels(t *testing.T) {
	wh := &AcceleratorTypeWebhook{}

	t.Run("derived type gets schedule labels and canonical queue entrance", func(t *testing.T) {
		at := &workercore.AcceleratorType{
			ObjectMeta: meta.ObjectMeta{Name: "gpustack--nvidia-a10g-linux-amd64"},
			Spec:       workercore.AcceleratorTypeSpec{AcceleratorGroup: "nvidia-a10g", OS: "linux", Arch: "amd64"},
		}
		require.NoError(t, wh.Default(context.Background(), at))
		assert.Equal(t, nodefeature.GeneralGroupGeneric, at.Spec.GeneralGroup)
		assert.Equal(t, "true", at.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-a10g"])
		assert.Equal(t, "true", at.Labels[nodefeature.NodeAcceleratableLabelKey])
		assert.Equal(t, "linux", at.Labels[core.LabelOSStable])
		assert.Equal(t, "amd64", at.Labels[core.LabelArchStable])
		assert.Equal(t, nodefeature.FormatLocalQueueName("gpustack--nvidia-a10g-linux-amd64"), at.Labels[QueueEntranceLabelKey])
	})

	t.Run("custom-named type stamps the canonical pool queue entrance", func(t *testing.T) {
		at := &workercore.AcceleratorType{
			ObjectMeta: meta.ObjectMeta{Name: "my-custom-a10g"},
			Spec:       workercore.AcceleratorTypeSpec{AcceleratorGroup: "nvidia-a10g", OS: "linux", Arch: "amd64"},
		}
		require.NoError(t, wh.Default(context.Background(), at))
		assert.Equal(t, nodefeature.FormatLocalQueueName("gpustack--nvidia-a10g-linux-amd64"), at.Labels[QueueEntranceLabelKey])
	})

	t.Run("missing acceleratorGroup leaves labels untouched for validation to reject", func(t *testing.T) {
		at := &workercore.AcceleratorType{
			ObjectMeta: meta.ObjectMeta{Name: "missing-group"},
			Spec:       workercore.AcceleratorTypeSpec{OS: "linux", Arch: "amd64"},
		}
		require.NoError(t, wh.Default(context.Background(), at))
		assert.Equal(t, nodefeature.GeneralGroupGeneric, at.Spec.GeneralGroup)
		assert.Empty(t, at.Labels)
	})

	t.Run("prunes a stale feature key on a group change", func(t *testing.T) {
		at := &workercore.AcceleratorType{
			ObjectMeta: meta.ObjectMeta{
				Name: "repoint",
				Labels: map[string]string{
					nodefeature.GeneralFeatureLabelPrefix + "amd-epyc-7763":   "true",
					nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-t4": "true",
				},
			},
			Spec: workercore.AcceleratorTypeSpec{AcceleratorGroup: "nvidia-a10g", OS: "linux", Arch: "amd64"},
		}
		require.NoError(t, wh.Default(context.Background(), at))
		assert.Equal(t, "true", at.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-a10g"])
		assert.NotContains(t, at.Labels, nodefeature.GeneralFeatureLabelPrefix+"amd-epyc-7763")
		assert.NotContains(t, at.Labels, nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-t4")
	})
}
