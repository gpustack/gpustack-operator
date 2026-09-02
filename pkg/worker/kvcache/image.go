// This file holds what any backend's renderers have to know about an image beyond its name. Only the
// pull policy so far, and it is here rather than beside a renderer because every role of every
// backend must resolve it the same way: a backend declares one policy for everything it runs.
package kvcache

import (
	dockerref "github.com/distribution/reference"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// EffectivePullPolicy resolves the policy a rendered container carries.
//
// The policy is RESOLVED here rather than left empty for the API server to default, and that is the
// whole point of the function. A rendered empty policy cannot be converged: the server fills it in
// on write, so an aligner comparing an empty expectation against the stored default differs on every
// pass and rolls the workload forever — while an aligner that skips the comparison to avoid that can
// never correct a stale value. Two states got stuck that way: a policy set and then REMOVED kept the
// value it was set to, and a backend whose image moved between a pinned tag and :latest kept the
// default derived from the old one.
//
// With a concrete value rendered on every pass there is no default to fight and nothing to skip.
func EffectivePullPolicy(kvcb *workercore.KVCacheBackend, image string) core.PullPolicy {
	if declared := kvcb.Spec.ImagePullPolicy; declared != "" {
		return declared
	}
	if imageTag(image) == "latest" {
		return core.PullAlways
	}
	return core.PullIfNotPresent
}

// imageTag returns the tag the API server's own defaulting would read, so an unset policy resolves
// to exactly what leaving the field empty would have produced.
//
// It mirrors k8s.io/kubernetes/pkg/util/parsers.ParseImageName, through the same parser: a reference
// carrying neither a tag nor a digest is read as ":latest", one carrying only a digest has no tag at
// all, and one carrying both keeps its tag. The rule reading that tag is SetDefaults_Container's,
// and it is identical in every version this project builds against.
//
// A reference that does not parse yields no tag, which is not "latest" — the same answer the API
// server reaches, since it ignores the parse error and reads the empty tag it was handed. Admission
// refuses such an image anyway; this is what the renderer does if one ever gets past it.
func imageTag(image string) string {
	named, err := dockerref.ParseNormalizedNamed(image)
	if err != nil {
		return ""
	}

	var tag, digest string
	if tagged, ok := named.(dockerref.Tagged); ok {
		tag = tagged.Tag()
	}
	if digested, ok := named.(dockerref.Digested); ok {
		digest = digested.Digest().String()
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}

	return tag
}
