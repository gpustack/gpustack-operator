package worker

import (
	"context"
	"fmt"
	"sort"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/webhook"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

// ModelPrefetchWebhook validates a v1alpha1.ModelPrefetch.
//
// It is validating only, and every question it asks is one the schema cannot see: that the artifact
// and the grant exist, that the placement is not ambiguous, that the target set lies inside stores
// the grant actually covers, that the projection fits the budget, and that pinning rides a grant
// that allows it.
//
// An artifact that exists but has not resolved yet IS ADMITTED, exactly as a ModelDeployment over an
// unresolved artifact is: the prefetch waits in status until the weights have an identity, so a
// GitOps tool need not order the two objects. The budget projection runs on what is knowable — an
// unresolved artifact carries no size yet — and the accounting controller reports the drift the
// projection could not see.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="modelprefetches",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type ModelPrefetchWebhook struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (r *ModelPrefetchWebhook) SetupWebhook(
	_ context.Context, opts webhook.SetupOptions,
) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return &workercore.ModelPrefetch{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*ModelPrefetchWebhook)(nil)
	_ webhook.ReceiveDeletionUpdate           = (*ModelPrefetchWebhook)(nil)
)

// ReceiveDeletionUpdate is a marker with no behavior: a prefetch being deleted changes nothing this
// webhook judges, and the controller releases the pins.
func (r *ModelPrefetchWebhook) ReceiveDeletionUpdate() {}

func (r *ModelPrefetchWebhook) ValidateCreate(
	ctx context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	pf := obj.(*workercore.ModelPrefetch)

	if errs := r.validateModelPrefetch(ctx, pf); len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("ModelPrefetch"), pf.Name, errs)
	}

	return nil, nil
}

func (r *ModelPrefetchWebhook) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	pf := newObj.(*workercore.ModelPrefetch)

	// The spec is mutable on purpose: a placement edit retargets the delivery, a retention edit
	// changes what keeping means. Every rule is re-asked, because a retarget is a new claim on the
	// budget and the stores.
	if errs := r.validateModelPrefetch(ctx, pf); len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("ModelPrefetch"), pf.Name, errs)
	}

	return nil, nil
}

func (r *ModelPrefetchWebhook) ValidateDelete(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	return nil, nil
}

// validateModelPrefetch refuses the shapes that cannot work, in the order a reader would diagnose
// them: what the prefetch names, where it points, what the grant allows, and what the budget holds.
func (r *ModelPrefetchWebhook) validateModelPrefetch(ctx context.Context, pf *workercore.ModelPrefetch) field.ErrorList {
	errs := validateModelPrefetchPlacement(pf)
	if len(errs) > 0 {
		return errs
	}

	artifact := new(workercore.ModelArtifact)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: pf.Namespace, Name: pf.Spec.ArtifactRef.Name}, artifact); err != nil {
		if kerrors.IsNotFound(err) {
			return field.ErrorList{field.NotFound(field.NewPath("spec", "artifactRef", "name"),
				pf.Spec.ArtifactRef.Name)}
		}

		return field.ErrorList{field.InternalError(field.NewPath("spec", "artifactRef", "name"),
			fmt.Errorf("get model artifact %q: %w", pf.Spec.ArtifactRef.Name, err))}
	}
	if artifact.Spec.Source.Image != nil {
		return field.ErrorList{field.Forbidden(field.NewPath("spec", "artifactRef"),
			"an image artifact delivers on demand through kubelet and never enters the node cache: "+
				"there is nothing to warm, no budget to count against, and no pinning to grant")}
	}

	binding := new(workercore.ModelStoreBinding)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: pf.Namespace, Name: pf.Spec.BindingRef.Name}, binding); err != nil {
		if kerrors.IsNotFound(err) {
			return field.ErrorList{field.NotFound(field.NewPath("spec", "bindingRef", "name"),
				pf.Spec.BindingRef.Name)}
		}

		return field.ErrorList{field.InternalError(field.NewPath("spec", "bindingRef", "name"),
			fmt.Errorf("get model store binding %q: %w", pf.Spec.BindingRef.Name, err))}
	}

	if pf.Spec.Retention.Pinned && !modelStoreBindingAllowsPinned(binding) {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "retention", "pinned"),
			pf.Spec.Retention.Pinned,
			"pinning is an admin-granted capability and this namespace's grant does not allow it; "+
				"ask for allowPinned on the grant, or warm without pinning")}
	}

	nodes := new(core.NodeList)
	if err := r.APIReader.List(ctx, nodes); err != nil {
		return field.ErrorList{field.InternalError(field.NewPath("spec", "placement"), err)}
	}
	deps := new(workercore.ModelDeploymentList)
	if err := r.APIReader.List(ctx, deps, ctrlcli.InNamespace(pf.Namespace)); err != nil {
		return field.ErrorList{field.InternalError(field.NewPath("spec", "placement"), err)}
	}
	targets, err := workerctrl.PrefetchTargetNodes(pf, nodes.Items, deps.Items)
	if err != nil {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "placement"), "", err.Error())}
	}

	if err := r.validateModelPrefetchGranted(ctx, binding, nodes.Items, targets); err != nil {
		return err
	}

	if int32(len(targets)) < pf.Spec.MinReady {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "minReady"), pf.Spec.MinReady,
			fmt.Sprintf("asks for more ready nodes than the placement resolves to (%d); "+
				"a bar above the target set would hold the prefetch unavailable forever", len(targets)))}
	}

	return r.validateModelPrefetchBudget(ctx, pf, binding, artifact, len(targets))
}

// validateModelPrefetchGranted refuses a target set no granted store covers: the grant says which
// pools this namespace warms into, and a prefetch over nodes outside every named store is warming
// into a pool it was never granted — with the node's own policy governed by whoever wins that pool.
func (r *ModelPrefetchWebhook) validateModelPrefetchGranted(
	ctx context.Context, binding *workercore.ModelStoreBinding, nodes []core.Node, targets []string,
) field.ErrorList {
	var granted []workercore.ModelStore
	for _, ref := range binding.Spec.StoreRefs {
		store := new(workercore.ModelStore)
		if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: ref.Name}, store); err != nil {
			if kerrors.IsNotFound(err) {
				continue // the Binding's own webhook refuses the ghost; here it simply covers nothing
			}

			return field.ErrorList{field.InternalError(field.NewPath("spec", "placement"),
				fmt.Errorf("get model store %q: %w", ref.Name, err))}
		}
		granted = append(granted, *store)
	}

	uncovered := map[string]bool{}
	for _, node := range targets {
		nd := nodeByName(nodes, node)
		if nd == nil {
			continue
		}
		covered := false
		for i := range granted {
			sel, err := meta.LabelSelectorAsSelector(&granted[i].Spec.NodeSelector)
			if err != nil {
				continue
			}
			if sel.Matches(labels.Set(nd.Labels)) {
				covered = true
				break
			}
		}
		if !covered {
			uncovered[node] = true
		}
	}
	if len(uncovered) == 0 {
		return nil
	}

	names := make([]string, 0, len(uncovered))
	for name := range uncovered {
		names = append(names, name)
	}
	sort.Strings(names)

	return field.ErrorList{field.Invalid(field.NewPath("spec"), names, fmt.Sprintf(
		"the target set leaves the grant: none of the stores %s names covers the node(s) %s, so "+
			"warming there was never granted. Widen storeRefs on %s, or point the placement at "+
			"nodes the grant covers", binding.Name, names, binding.Name))}
}

// validateModelPrefetchBudget projects the namespace's warm footprint onto the grant and refuses
// what does not fit.
//
// The projection is the accounting's shape at admission time: per DISTINCT digest, the artifact's
// resolved size times the widest node count any same-namespace prefetch asks of it. An artifact
// that has not resolved yet contributes no size — its prefetch is admitted against the budget's
// remaining room and the accounting controller reports the drift the projection could not see. The
// quota compares quantities, so the refusal states both figures.
func (r *ModelPrefetchWebhook) validateModelPrefetchBudget(
	ctx context.Context, pf *workercore.ModelPrefetch, binding *workercore.ModelStoreBinding,
	artifact *workercore.ModelArtifact, targetCount int,
) field.ErrorList {
	pfs := new(workercore.ModelPrefetchList)
	if err := r.APIReader.List(ctx, pfs, ctrlcli.InNamespace(pf.Namespace)); err != nil {
		return field.ErrorList{field.InternalError(field.NewPath("spec"), err)}
	}

	// sizes: digest -> resolved size, from each referenced artifact that has resolved. asked:
	// digest -> the widest node count asked of it, this prefetch included.
	sizes := map[string]int64{}
	asked := map[string]int64{}
	for i := range pfs.Items {
		other := &pfs.Items[i]
		otherArtifact := artifact
		if other.Name != pf.Name {
			otherArtifact = new(workercore.ModelArtifact)
			if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: other.Namespace, Name: other.Spec.ArtifactRef.Name}, otherArtifact); err != nil {
				if !kerrors.IsNotFound(err) {
					// A transient read error on an enforcement gate is not a pass: admission is
					// retried, rather than granted against a projection that skipped a sibling.
					return field.ErrorList{field.InternalError(field.NewPath("spec"),
						fmt.Errorf("get model artifact %q for budget projection: %w", other.Spec.ArtifactRef.Name, err))}
				}
				continue
			}
		}
		resolved := otherArtifact.Status.Resolved
		if resolved == nil || resolved.ManifestDigest == "" {
			continue
		}
		sizes[resolved.ManifestDigest] = resolved.SizeBytes

		// A sibling is projected at its own facts only: a prefetch whose targets are not counted
		// yet contributes nothing here rather than this one's node count.
		count := int64(0)
		if other.Name == pf.Name {
			count = int64(targetCount)
		} else if other.Status.DesiredNodes > 0 {
			count = int64(other.Status.DesiredNodes)
		}
		if count > asked[resolved.ManifestDigest] {
			asked[resolved.ManifestDigest] = count
		}
	}

	// The object under admission is not in the list yet: its own claim joins last, at the target
	// count the placement resolves to right now.
	if resolved := artifact.Status.Resolved; resolved != nil && resolved.ManifestDigest != "" {
		if _, ok := sizes[resolved.ManifestDigest]; !ok {
			sizes[resolved.ManifestDigest] = resolved.SizeBytes
		}
		if int64(targetCount) > asked[resolved.ManifestDigest] {
			asked[resolved.ManifestDigest] = int64(targetCount)
		}
	}

	var projected int64
	for digest, count := range asked {
		projected += sizes[digest] * count
	}

	quota := binding.Spec.Quota.Bytes.Value()
	if projected <= quota {
		return nil
	}

	return field.ErrorList{field.Invalid(field.NewPath("spec"), fmt.Sprintf("%d bytes over %d node(s) per digest",
		projected, targetCount), fmt.Sprintf("the namespace's warm footprint would reach %d bytes, past "+
		"the grant's %s in %s: warm fewer nodes, or ask for a larger budget",
		projected, binding.Spec.Quota.Bytes.String(), binding.Name))}
}

// validateModelPrefetchPlacement refuses what the schema cannot see: both placement kinds set. The
// shared error is the controller's, so admission and delivery name the same mistake the same way.
func validateModelPrefetchPlacement(pf *workercore.ModelPrefetch) field.ErrorList {
	placement := pf.Spec.Placement
	if placement != nil && len(placement.InstanceTypes) > 0 && placement.NodeSelector != nil {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "placement"), "",
			workerctrl.ErrPrefetchPlacementAmbiguous.Error())}
	}

	return nil
}

func nodeByName(nodes []core.Node, name string) *core.Node {
	for i := range nodes {
		if nodes[i].Name == name {
			return &nodes[i]
		}
	}

	return nil
}
