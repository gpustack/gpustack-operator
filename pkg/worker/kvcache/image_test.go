package kvcache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// TestEffectivePullPolicy pins the rule against the one it mirrors.
//
// Every row below is what k8s.io/kubernetes/pkg/apis/core/v1.SetDefaults_Container would have
// written for the same image, read from its source: ParseImageName reports ":latest" only when a
// reference carries NEITHER a tag nor a digest, and the defaulter asks whether that tag is "latest".
// The last two rows are the ones a looser parser gets wrong — a reference carrying both a tag and a
// digest keeps its tag, so ":latest@sha256:…" is Always and ":v1@sha256:…" is not.
func TestEffectivePullPolicy(t *testing.T) {
	const digest = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"

	cases := []struct {
		name     string
		declared core.PullPolicy
		image    string
		want     core.PullPolicy
	}{
		{"a pinned tag", "", "example.com/mooncake:v0.3.13", core.PullIfNotPresent},
		{"an explicit latest", "", "example.com/mooncake:latest", core.PullAlways},
		{"no tag at all reads as latest", "", "example.com/mooncake", core.PullAlways},
		{"a bare name reads as latest", "", "mooncake", core.PullAlways},
		{"a digest carries no tag", "", "example.com/mooncake" + digest, core.PullIfNotPresent},
		{"a tag beside a digest still counts", "", "example.com/mooncake:latest" + digest, core.PullAlways},
		{"and a pinned one beside a digest does not", "", "example.com/mooncake:v1" + digest, core.PullIfNotPresent},
		{"an unparseable reference has no tag", "", "NOT AN IMAGE", core.PullIfNotPresent},

		// A declared policy wins over every row above, including the one that would disagree.
		{"declared beats the tag", core.PullNever, "example.com/mooncake:latest", core.PullNever},
		{"declared beats the default", core.PullAlways, "example.com/mooncake:v1", core.PullAlways},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kvcb := &workercore.KVCacheBackend{
				Spec: workercore.KVCacheBackendSpec{ImagePullPolicy: c.declared},
			}

			assert.Equal(t, c.want, EffectivePullPolicy(kvcb, c.image))
		})
	}
}

// TestEffectivePullPolicy_IsNeverEmpty is the property the aligner depends on. An empty policy is
// filled in by the API server, and an aligner comparing against that default either rolls the
// workload on every pass or skips the comparison and leaves a stale value standing. Neither is
// reachable while this returns a value for every image.
func TestEffectivePullPolicy_IsNeverEmpty(t *testing.T) {
	for _, image := range []string{"", " ", "mooncake", "mooncake:v1", "NOT AN IMAGE", "://"} {
		kvcb := new(workercore.KVCacheBackend)

		assert.NotEmpty(t, EffectivePullPolicy(kvcb, image),
			"image %q must still resolve to a policy", image)
	}
}
