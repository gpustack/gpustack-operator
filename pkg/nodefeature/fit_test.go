package nodefeature

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestFitLabelKeys(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "sliced",
			got:  FitSlicedMaxFreeUnitsLabelKey("nvidia-tesla-t4"),
			want: "sliced-max-free-units.fit.gpustack.ai/nvidia-tesla-t4",
		},
		{
			name: "shared",
			got:  FitSharedFreeCardsLabelKey("nvidia-tesla-t4"),
			want: "shared-free-cards.fit.gpustack.ai/nvidia-tesla-t4",
		},
		{
			name: "the longest accelerator key still makes a valid label key",
			got:  FitSlicedMaxFreeUnitsLabelKey(strings.Repeat("a", 63)),
			want: "sliced-max-free-units.fit.gpustack.ai/" + strings.Repeat("a", 63),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.got)
			assert.Empty(t, validation.IsQualifiedName(c.got))
		})
	}
}

func TestIsFitLabelKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{key: "sliced-max-free-units.fit.gpustack.ai/nvidia-tesla-t4", want: true},
		{key: "shared-free-cards.fit.gpustack.ai/nvidia-tesla-t4", want: true},
		{key: "sliced-max-free-units.fit.gpustack.ai/", want: false},
		{key: "fit.gpustack.ai/nvidia-tesla-t4", want: false},
		{key: "other.fit.gpustack.ai/nvidia-tesla-t4", want: false},
		{key: "sliced-max-free-units.fit.gpustack.ai.evil/nvidia-tesla-t4", want: false},
		{key: "acceleratable.feature.gpustack.ai/nvidia-tesla-t4.count", want: false},
		{key: "nvidia-tesla-t4", want: false},
		{key: "sliced-max-free-units.fit.gpustack.ai/Bad Group", want: false},
		{key: "shared-free-cards.fit.gpustack.ai/a/b", want: false},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			assert.Equal(t, c.want, IsFitLabelKey(c.key))
		})
	}
}

func TestEqualIgnoringFitLabels(t *testing.T) {
	sliced := FitSlicedMaxFreeUnitsLabelKey("nvidia-tesla-t4")
	shared := FitSharedFreeCardsLabelKey("nvidia-tesla-t4")

	cases := []struct {
		name string
		a, b map[string]string
		want bool
	}{
		{
			name: "no change",
			a:    map[string]string{"zone": "a", sliced: "1600000"},
			b:    map[string]string{"zone": "a", sliced: "1600000"},
			want: true,
		},
		{
			name: "only a fit value changes",
			a:    map[string]string{"zone": "a", sliced: "1600000"},
			b:    map[string]string{"zone": "a", sliced: "640000"},
			want: true,
		},
		{
			name: "only a fit key appears",
			a:    map[string]string{"zone": "a"},
			b:    map[string]string{"zone": "a", shared: "4"},
			want: true,
		},
		{
			name: "another label changes",
			a:    map[string]string{"zone": "a"},
			b:    map[string]string{"zone": "b"},
			want: false,
		},
		{
			name: "another label appears beside a fit change",
			a:    map[string]string{sliced: "1600000"},
			b:    map[string]string{sliced: "640000", "zone": "b"},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, EqualIgnoringFitLabels(c.a, c.b))
		})
	}
}

func TestFilterFitLabels(t *testing.T) {
	sliced := FitSlicedMaxFreeUnitsLabelKey("nvidia-tesla-t4")
	assert.Equal(t,
		map[string]string{sliced: "640000"},
		FilterFitLabels(map[string]string{sliced: "640000", "zone": "a", "fit.gpustack.ai/x": "1"}))
	assert.Empty(t, FilterFitLabels(nil))
}
