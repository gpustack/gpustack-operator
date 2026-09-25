package worker

import (
	"context"
	"slices"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// instancePersistentVolumePlacement is the escape Setting as an Instance's reconcile reads it. It
// is a variable so a test can choose it without a Settings store.
var instancePersistentVolumePlacement = func(ctx context.Context) bool {
	return settings.InstancePersistentVolumePlacement.ShouldValueBool(ctx)
}

// resolveInstancePersistentVolumes reads where every persistent claim the Instance mounts, the
// workspace's and each additional volume's, lets its Pod run. It returns the bound PVs' required
// node affinities, and why the Pod cannot be built yet, or "" once it can.
//
// A claim is placed by the rules a claim model volume follows, in placeModelArtifactClaim: a bound
// claim adds its PV's affinity, a claim provisioned on the first Pod's node adds nothing, and any
// other unbound claim holds the Pod back. The affinities are added when the Pod is created and are
// never compared afterwards, so a claim that binds after its Pod exists is honored from the next
// Pod on.
//
// With the escape Setting off it reads nothing, and the Pod is built from the claim names alone.
func (r *InstanceReconciler) resolveInstancePersistentVolumes(
	ctx context.Context, inst *workercore.Instance,
) ([]*core.NodeSelector, string, error) {
	if !instancePersistentVolumePlacement(ctx) {
		return nil, "", nil
	}

	// The same claims the Pod render mounts: the workspace only when it is not ephemeral, an
	// additional volume only when it names no model. A claim mounted twice is read once, so its
	// affinity is not crossed with itself.
	var claims []string
	if inst.Spec.Volume.Ephemeral == nil && inst.Spec.Volume.Persistent != nil {
		claims = append(claims, inst.Spec.Volume.Persistent.Name)
	}
	for i := range inst.Spec.AdditionalVolumes {
		av := &inst.Spec.AdditionalVolumes[i]
		if av.Model == nil && av.Persistent != nil && !slices.Contains(claims, av.Persistent.Name) {
			claims = append(claims, av.Persistent.Name)
		}
	}

	var affinities []*core.NodeSelector
	for _, claim := range claims {
		// One Pod mounts it, so its access modes never hold the Pod back.
		w := &modelArtifactWeights{}
		if err := placeModelArtifactClaim(ctx, r.Client, inst.Namespace, claim, 1, w); err != nil {
			return nil, "", err
		}
		if w.Blocked {
			return nil, w.Message, nil
		}
		if w.Affinity != nil {
			affinities = append(affinities, w.Affinity)
		}
	}

	return affinities, "", nil
}
