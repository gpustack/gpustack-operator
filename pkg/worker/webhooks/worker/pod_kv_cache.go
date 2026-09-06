package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// The vocabulary a Pod uses to ask for a KV cache. The trigger is a LABEL because an objectSelector
// can only match labels, and it carries a fixed value so it never meets the 63-character ceiling a
// label value has; everything configurable is an ANNOTATION, where a name of any length is legal.
const (
	// KVCacheInjectLabelKey opts a Pod into injection. It is the mutating webhook's objectSelector,
	// so a Pod without it never reaches this handler at all.
	KVCacheInjectLabelKey = "kvcache." + systemname.LabelPrefix + "inject"

	// KVCacheInjectLabelValue is the only value that opts in. "false" is the explicit opt-out, and
	// so is dropping the label.
	KVCacheInjectLabelValue = "true"

	// KVCacheBindingAnnotationKey names the KVCachePoolBinding, in the Pod's own namespace, that
	// provisions this Pod's grant on a pool - capacity and a reuse domain, not access. There is no
	// cross-namespace form.
	KVCacheBindingAnnotationKey = "kvcache." + systemname.LabelPrefix + "binding"

	// KVCacheEngineAnnotationKey names the inference engine to configure. It is required and never
	// guessed from an image: engines take different flags, and a wrong guess yields a container that
	// starts normally and caches nothing.
	KVCacheEngineAnnotationKey = "kvcache." + systemname.LabelPrefix + "engine"

	// KVCacheRoleAnnotationKey declares a prefill/decode role. Unset is legal and means the caller
	// has no such split.
	KVCacheRoleAnnotationKey = "kvcache." + systemname.LabelPrefix + "role"

	// KVCacheContainerAnnotationKey names which container to inject into. It is required only when
	// the Pod has more than one, because picking the first is how an injection lands in a sidecar
	// while the workload runs elsewhere.
	KVCacheContainerAnnotationKey = "kvcache." + systemname.LabelPrefix + "container"

	// KVCacheDomainAnnotationKey is a REFUSED key, not an unrecognized one. The reuse domain comes
	// from the Binding and only from the Binding, so a Pod that names one here is rejected rather
	// than silently ignored - a manifest written against an escape hatch that does not exist should
	// fail where its author can see it.
	KVCacheDomainAnnotationKey = "kvcache." + systemname.LabelPrefix + "domain"
)

// PodKVCacheWebhook injects the client configuration an inference engine needs to use a KV cache
// pool, into any Pod that opts in with the KVCacheInjectLabelKey label. Mutating on CREATE: it
// appends env, args, a volume and a mount to one named container and stamps the Pod with what it
// did. Every check that half makes is a reason to refuse the mutation itself, so it reports its
// refusals from Default rather than from a validating handler.
//
// Validating on UPDATE, and that half answers a different question. The mutating half decides once,
// and the keys carrying its decision stay editable on the Pod for as long as it lives. The
// validating half freezes those keys - the opt-in label, the injection record, and the client
// configuration the container projects - so that what this webhook wrote about a Pod stays true for
// the Pod's life rather than only at the instant it was admitted. The same three keys are refused as
// INPUTS at CREATE, by checkAnnotationVocabulary and by the resolution path; this is that contract
// held past admission.
//
// WHAT THE UPDATE PATH DOES NOT HOLD, each gap reachable rather than hypothetical:
//
//   - Its failurePolicy is Ignore, against Fail on the mutating half, so every edit it would refuse
//     is admitted while the webhook is unreachable. That direction is deliberate: this guard keeps a
//     record honest, and it must never be the reason a live workload cannot be updated or finished.
//     Under Fail an unreachable webhook would block finalizer removal on every opted-in Pod, and
//     this feature's own teardown waits on exactly that - a Binding's finalizer holds while
//     status.usedBy is non-empty, which cannot empty while its Pods cannot finish deleting. One
//     missed refusal costs one wrong record; a wedged deletion costs the pool.
//   - Its objectSelector is the opt-in label, so a Pod that never carried it is never seen, and
//     forged values for the two annotations can be written onto such a Pod. They are inert there -
//     no volume projects the configuration and no container reads it - but an operator who read the
//     record would be reading a fabrication.
//   - It freezes metadata only. The volume, mount, env and args the mutating half writes live in the
//     Pod spec, which this path does not read or compare.
//
// It is a second Pod webhook pair rather than a branch inside PodWebhook, and the two are
// independent by construction: PodWebhook triggers on the Kueue queue-name label and rewrites
// container resources, this one triggers on its own label and never touches resources. A Pod may
// carry both labels, neither, or one.
//
// reinvocationPolicy is Never. This webhook refuses a Pod whose target container already carries a
// key it owns, so a second pass over its own output would refuse the Pod it had just configured.
//
// nolint: lll
// +k8s:webhook-gen:mutating:group="",version="v1",resource="pods",scope="Namespaced"
// +k8s:webhook-gen:mutating:operations=["CREATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",reinvocationPolicy="Never",timeoutSeconds=10
// +k8s:webhook-gen:mutating:objectSelector={"matchExpressions":[{"key":"kvcache.gpustack.ai/inject","operator":"In","values":["true"]}]}
// +k8s:webhook-gen:mutating:namePrefix="gpustack-worker-kvcache"
// +k8s:webhook-gen:validating:group="",version="v1",resource="pods",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["UPDATE"],failurePolicy="Ignore",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:validating:objectSelector={"matchExpressions":[{"key":"kvcache.gpustack.ai/inject","operator":"In","values":["true"]}]}
// +k8s:webhook-gen:validating:namePrefix="gpustack-worker-kvcache"
type PodKVCacheWebhook struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (r *PodKVCacheWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return &core.Pod{}, nil
}

var (
	_ ctrladmission.Defaulter[runtime.Object] = (*PodKVCacheWebhook)(nil)
	_ ctrladmission.Validator[runtime.Object] = (*PodKVCacheWebhook)(nil)
)

func (r *PodKVCacheWebhook) Default(ctx context.Context, obj runtime.Object) error {
	pod, ok := obj.(*core.Pod)
	if !ok {
		return fmt.Errorf("expected a Pod, got %T", obj)
	}

	// Injection happens on CREATE and once. What keeps its decision from being edited afterwards is
	// ValidateUpdate below, not anything here.
	//
	// The objectSelector already filters to Pods carrying the opt-in label at its accepted value, so
	// this is defense in depth rather than the primary guard. It is here because widening that
	// selector is a one-line edit in a generated file, and the spec names that exact reversion as a
	// risk: without this check, a widened selector would send every Pod in the cluster through
	// resolution, and a webhook that fails closed would then stop the cluster. With it, the blast
	// radius of that mistake is a no-op.
	if pod.Labels[KVCacheInjectLabelKey] != KVCacheInjectLabelValue {
		return nil
	}

	res, err := r.resolve(ctx, pod)
	if err != nil {
		return err
	}

	out, err := inject.Render(res.Input)
	if err != nil {
		return err
	}

	// Every refusal has run by now, so the Pod is either fully injected or untouched. A half-injected
	// Pod - the volume added and the variable missing - would start and behave in a way no field on it
	// explains.
	return r.injectPod(pod, res, out)
}

// ValidateCreate is not registered: CREATE is the mutating half's, which refuses from Default because
// each of its checks is a reason not to mutate.
func (r *PodKVCacheWebhook) ValidateCreate(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	return nil, nil
}

func (r *PodKVCacheWebhook) ValidateUpdate(
	_ context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	oldPod, ok := oldObj.(*core.Pod)
	if !ok {
		return nil, fmt.Errorf("expected a Pod, got %T", oldObj)
	}
	newPod, ok := newObj.(*core.Pod)
	if !ok {
		return nil, fmt.Errorf("expected a Pod, got %T", newObj)
	}

	if errs := validatePodKVCacheOwnedMetadata(oldPod, newPod); len(errs) > 0 {
		return nil, kerrors.NewInvalid(
			core.SchemeGroupVersion.WithKind("Pod").GroupKind(), newPod.Name, errs)
	}

	return nil, nil
}

// ValidateDelete is not registered. Nothing about a KV cache decision needs re-checking as the Pod
// goes: the grant is the Binding's and outlives the Pod, and refusing a delete here is the one thing
// that could leave a workload unable to finish.
func (r *PodKVCacheWebhook) ValidateDelete(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	return nil, nil
}

// podKVCacheOwnedAnnotations are the two keys this webhook WRITES, with what editing one costs. They
// are frozen for the same reason a submitted value is refused at CREATE - they record what was
// decided rather than carry an input - and the two reasons differ, so each carries its own.
var podKVCacheOwnedAnnotations = []struct {
	key    string
	reason string
}{
	{
		key: KVCacheInjectedAnnotationKey,
		reason: "this annotation is written by this webhook and may not be edited: it is the record " +
			"of what admission decided, nothing re-derives it, and an edited value is a record of a " +
			"decision nobody made. Change the kvcache annotations where the Pod is defined and let " +
			"the Pod be replaced",
	},
	{
		key: inject.ClientConfigAnnotationKey,
		reason: "this annotation is written by this webhook and may not be edited: the container " +
			"reads it as a downwardAPI projection at " + inject.ConfigFilePath + ", so editing it " +
			"swaps the master address, transport and segment sizes under a running container with " +
			"nothing recording that they moved. It is rendered from the Binding this Pod names and " +
			"the pool behind it, so change those and let the Pod be replaced",
	},
}

// validatePodKVCacheOwnedMetadata holds the three keys admission decided for the Pod's whole life.
//
// It compares presence as well as value, so removing a key is refused on the same footing as changing
// it. Reading the value alone would let a delete pass as an edit to the empty string.
//
// Every other metadata edit is left alone, and that is the requirement rather than a side effect: an
// opted-in Pod is a live workload whose labels, other annotations and finalizers belong to whatever
// is managing it.
func validatePodKVCacheOwnedMetadata(oldPod, newPod *core.Pod) field.ErrorList {
	oldOptedIn := oldPod.Labels[KVCacheInjectLabelKey] == KVCacheInjectLabelValue
	newOptedIn := newPod.Labels[KVCacheInjectLabelKey] == KVCacheInjectLabelValue

	// The objectSelector matches when EITHER object carries the label, so a transition in either
	// direction arrives here and a Pod outside the feature does not. Neither side opting in is
	// therefore only reachable through a widened selector, where the answer is the mutating half's.
	if !oldOptedIn && !newOptedIn {
		return nil
	}

	var errs field.ErrorList

	labelPath := field.NewPath("metadata", "labels").Key(KVCacheInjectLabelKey)
	switch {
	case !oldOptedIn && newOptedIn:
		errs = append(errs, field.Forbidden(labelPath,
			"the opt-in label is immutable: injection runs at CREATE and only there, so adding this "+
				"label to an existing Pod gives it the label that says it uses a cache and none of "+
				"the configuration - no volume, no mount, no environment. Add the label where the "+
				"Pod is defined and let the Pod be replaced"))
	case oldOptedIn && !newOptedIn:
		errs = append(errs, field.Forbidden(labelPath,
			"the opt-in label is immutable: nothing un-injects a Pod, so the volume, mount, "+
				"environment and arguments this webhook wrote are still on this one, and dropping "+
				"the label would leave the container configured for a cache while the Pod no longer "+
				"says it is. Remove the label where the Pod is defined and let the Pod be replaced"))
	}

	annotationsPath := field.NewPath("metadata", "annotations")
	for _, owned := range podKVCacheOwnedAnnotations {
		oldValue, hadOld := oldPod.Annotations[owned.key]
		newValue, hasNew := newPod.Annotations[owned.key]
		if hadOld == hasNew && oldValue == newValue {
			continue
		}
		errs = append(errs, field.Forbidden(annotationsPath.Key(owned.key), owned.reason))
	}

	return errs
}
