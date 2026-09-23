package worker

import (
	"context"
	"net/url"
	"strings"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// TopologySourceWebhook validates the cluster-wide topology inventory boundary.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="topologysources",scope="Cluster"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type TopologySourceWebhook struct{}

func (*TopologySourceWebhook) SetupWebhook(_ context.Context, _ webhook.SetupOptions) (runtime.Object, error) {
	return &workercore.TopologySource{}, nil
}

var _ ctrladmission.Validator[runtime.Object] = (*TopologySourceWebhook)(nil)

func (*TopologySourceWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	source := obj.(*workercore.TopologySource)
	if errs := validateTopologySource(source); len(errs) > 0 {
		return nil, kerrors.NewInvalid(source.GroupVersionKind().GroupKind(), source.Name, errs)
	}
	return nil, nil
}

func (*TopologySourceWebhook) ValidateUpdate(
	_ context.Context, _, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	source := newObj.(*workercore.TopologySource)
	if errs := validateTopologySource(source); len(errs) > 0 {
		return nil, kerrors.NewInvalid(source.GroupVersionKind().GroupKind(), source.Name, errs)
	}
	return nil, nil
}

func (*TopologySourceWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

func validateTopologySource(source *workercore.TopologySource) field.ErrorList {
	specPath := field.NewPath("spec")
	errs := validateTopologyLevels(source.Spec.Levels, specPath.Child("levels"))
	errs = append(errs, validateTopologyAdditionalWritePrefix(source.Spec.AdditionalWritePrefix, specPath.Child("additionalWritePrefix"))...)
	if _, err := meta.LabelSelectorAsSelector(&source.Spec.NodeSelector); err != nil {
		errs = append(errs, field.Invalid(specPath.Child("nodeSelector"), source.Spec.NodeSelector, err.Error()))
	}

	arms := 0
	if source.Spec.NodeLabels != nil {
		arms++
	}
	if source.Spec.ConfigMap != nil {
		arms++
	}
	if source.Spec.Webhook != nil {
		arms++
	}
	if arms != 1 {
		errs = append(errs, field.Invalid(specPath, source.Spec, "exactly one of nodeLabels, configMap, or webhook is required"))
	}
	if source.Spec.ConfigMap != nil {
		errs = append(errs, validateTopologyConfigMap(source.Spec.ConfigMap, specPath.Child("configMap"))...)
	}
	if source.Spec.Webhook != nil {
		errs = append(errs, validateTopologyWebhook(source.Spec.Webhook, specPath.Child("webhook"))...)
	}
	if source.Spec.NodeLabels != nil && source.Spec.AdditionalWritePrefix != "" {
		errs = append(errs, field.Forbidden(specPath.Child("additionalWritePrefix"), "nodeLabels is read-only"))
	}
	return errs
}

func validateTopologyAdditionalWritePrefix(prefix string, path *field.Path) field.ErrorList {
	if prefix == "" {
		return nil
	}
	if !strings.HasSuffix(prefix, "/") || strings.Count(prefix, "/") != 1 {
		return field.ErrorList{field.Invalid(path, prefix, "must be one DNS prefix followed by a slash")}
	}
	domain := strings.TrimSuffix(prefix, "/")
	if messages := validation.IsDNS1123Subdomain(domain); len(messages) > 0 {
		return field.ErrorList{field.Invalid(path, prefix, "must use a DNS prefix: "+strings.Join(messages, "; "))}
	}
	protectedPrefixes := []string{
		"kubernetes.io", "k8s.io", "topology.gpustack.ai", "feature.gpustack.ai",
		"fabric.topograph.run", "accelerator.topograph.run",
	}
	for _, protected := range protectedPrefixes {
		if domain == protected || strings.HasSuffix(domain, "."+protected) {
			return field.ErrorList{field.Forbidden(path, "is reserved by Kubernetes, GPUStack, or Topograph")}
		}
	}
	return nil
}

func validateTopologyLevels(levels []string, path *field.Path) field.ErrorList {
	const maxTopologyLevels = 16 // must match +k8s:validation:maxItems in api/worker/v1alpha1/topology_source.go
	if len(levels) == 0 {
		return field.ErrorList{field.Required(path, "at least one level is required")}
	}
	if len(levels) > maxTopologyLevels {
		return field.ErrorList{field.TooMany(path, len(levels), maxTopologyLevels)}
	}
	seen := make(map[string]struct{}, len(levels))
	var errs field.ErrorList
	for i, level := range levels {
		itemPath := path.Index(i)
		if msgs := validation.IsQualifiedName(level); len(msgs) > 0 {
			errs = append(errs, field.Invalid(itemPath, level, "is not a label key: "+strings.Join(msgs, "; ")))
		}
		if level == "kubernetes.io/hostname" {
			errs = append(errs, field.Forbidden(itemPath, "kubernetes.io/hostname is implicit"))
		}
		if _, exists := seen[level]; exists {
			errs = append(errs, field.Duplicate(itemPath, level))
		}
		seen[level] = struct{}{}
	}
	return errs
}

func validateTopologyConfigMap(configMap *workercore.TopologySourceConfigMap, path *field.Path) field.ErrorList {
	errs := validateTopologyObjectReference(configMap.ConfigMapRef, path.Child("configMapRef"))
	if strings.TrimSpace(configMap.Key) == "" {
		errs = append(errs, field.Required(path.Child("key"), "must name the snapshot key"))
	}
	return append(errs, validatePositiveDuration(configMap.MaxStaleness.Duration, path.Child("maxStaleness"))...)
}

func validateTopologyWebhook(webhook *workercore.TopologySourceWebhook, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	parsed, err := url.Parse(webhook.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		errs = append(errs, field.Invalid(path.Child("url"), webhook.URL, "must be an absolute HTTPS URL without user information or a fragment"))
	}
	errs = append(errs, validatePositiveDuration(webhook.PollInterval.Duration, path.Child("pollInterval"))...)
	errs = append(errs, validatePositiveDuration(webhook.Timeout.Duration, path.Child("timeout"))...)
	errs = append(errs, validatePositiveDuration(webhook.MaxStaleness.Duration, path.Child("maxStaleness"))...)
	if webhook.Timeout.Duration > webhook.PollInterval.Duration && webhook.PollInterval.Duration > 0 {
		errs = append(errs, field.Invalid(path.Child("timeout"), webhook.Timeout.Duration, "must not exceed pollInterval"))
	}
	if webhook.CABundleConfigMapRef != nil {
		errs = append(errs, validateTopologyObjectReference(*webhook.CABundleConfigMapRef, path.Child("caBundleConfigMapRef"))...)
	}
	credentials := 0
	if webhook.BearerTokenSecretRef != nil {
		credentials++
		errs = append(errs, validateTopologyObjectReference(*webhook.BearerTokenSecretRef, path.Child("bearerTokenSecretRef"))...)
	}
	if webhook.TLSClientCertificateSecretRef != nil {
		credentials++
		errs = append(errs, validateTopologyObjectReference(*webhook.TLSClientCertificateSecretRef, path.Child("tlsClientCertificateSecretRef"))...)
	}
	if credentials != 1 {
		errs = append(errs, field.Invalid(path, webhook, "exactly one of bearerTokenSecretRef or tlsClientCertificateSecretRef is required"))
	}
	return errs
}

func validateTopologyObjectReference(ref workercore.TopologySourceObjectReference, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if ref.Namespace != kuberess.SystemNamespaceName {
		errs = append(errs, field.Forbidden(path.Child("namespace"), "references must be in the worker namespace"))
	}
	if msgs := validation.IsDNS1123Subdomain(ref.Name); len(msgs) > 0 {
		errs = append(errs, field.Invalid(path.Child("name"), ref.Name, strings.Join(msgs, "; ")))
	}
	return errs
}

func validatePositiveDuration(value time.Duration, path *field.Path) field.ErrorList {
	if value <= 0 {
		return field.ErrorList{field.Invalid(path, value.String(), "must be positive")}
	}
	return nil
}
