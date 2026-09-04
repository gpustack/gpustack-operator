package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
// pool, into any Pod that opts in with the KVCacheInjectLabelKey label. Mutating only: it appends
// env, args, a volume and a mount to one named container and stamps the Pod with what it did. It
// validates nothing, because every check it makes is a reason to refuse the mutation itself.
//
// It is a second Pod mutating webhook rather than a branch inside PodWebhook, and the two are
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
type PodKVCacheWebhook struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (r *PodKVCacheWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return &core.Pod{}, nil
}

var _ ctrladmission.Defaulter[runtime.Object] = (*PodKVCacheWebhook)(nil)

func (r *PodKVCacheWebhook) Default(ctx context.Context, obj runtime.Object) error {
	pod, ok := obj.(*core.Pod)
	if !ok {
		return fmt.Errorf("expected a Pod, got %T", obj)
	}

	// CREATE only, and the annotations this webhook reads and writes stay mutable afterwards: nothing
	// revalidates the trigger label, the injection record, or the client-config the projection reads.
	// Filed as #167 rather than handled here - an UPDATE surface is its own admission path.
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
