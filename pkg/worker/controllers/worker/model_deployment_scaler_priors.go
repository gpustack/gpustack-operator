package worker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

// ModelDeploymentScalerCapacityPriors contains independent effective algorithms and their evidence.
type ModelDeploymentScalerCapacityPriors struct {
	Algorithms map[string]workercore.ModelDeploymentScalerAlgorithmSpec
	Status     []workercore.ModelDeploymentScalerResolvedCapacityPriorStatus
}

type modelDeploymentScalerCapacityField struct {
	name  string
	value **resource.Quantity
}

// ModelDeploymentScalerNeedsCapacityPrior reports whether an actual serving role lacks capacity.
// A missing target conservatively checks every serving kind. A scaler without a capacity prior
// states every capacity by hand and never needs one.
func ModelDeploymentScalerNeedsCapacityPrior(scaler *workercore.ModelDeploymentScaler, target *workercore.ModelDeployment) bool {
	if !modelDeploymentScalerPriorRequested(scaler) {
		return false
	}
	roles := []workercore.ModelDeploymentRole{
		{Kind: workercore.ModelDeploymentRoleKindServer},
		{Kind: workercore.ModelDeploymentRoleKindPrefill},
		{Kind: workercore.ModelDeploymentRoleKindDecode},
	}
	if target != nil {
		roles = target.Spec.Roles
	}
	for _, role := range roles {
		effective := scaler.Spec.Algorithm.DeepCopy()
		for _, field := range modelDeploymentScalerCapacityFields(effective, ModelDeploymentEffectiveRoleKind(&role)) {
			if *field.value == nil {
				return true
			}
		}
	}
	return false
}

// ResolveModelDeploymentScalerCapacityPriors fills only missing role capacities on deep copies.
// Catalog refusals are typed; API failures retain their original type for admission and retries.
func ResolveModelDeploymentScalerCapacityPriors(
	ctx context.Context, reader ctrlcli.Reader, scaler *workercore.ModelDeploymentScaler, target *workercore.ModelDeployment,
) (*ModelDeploymentScalerCapacityPriors, error) {
	result := &ModelDeploymentScalerCapacityPriors{Algorithms: make(map[string]workercore.ModelDeploymentScalerAlgorithmSpec)}
	// THE QUANTILE THE SCALER IS HELD TO IS RESOLVED ONCE, because the
	// resolution and the online sampling have to answer the same question.
	quantile, quantileErr := ModelDeploymentScalerLatencyQuantile(&scaler.Spec.Algorithm)
	if quantileErr != nil {
		return nil, modelDeploymentScalerPriorRefusal("", "%v", quantileErr)
	}
	var catalog *priors.Catalog
	for _, role := range target.Spec.Roles {
		effective := scaler.Spec.Algorithm.DeepCopy()
		if !modelDeploymentScalerPriorRequested(scaler) {
			result.Algorithms[role.Name] = *effective
			continue
		}
		fields := modelDeploymentScalerCapacityFields(effective, ModelDeploymentEffectiveRoleKind(&role))
		missing := false
		for _, field := range fields {
			if *field.value == nil {
				missing = true
				continue
			}
			if (*field.value).Sign() <= 0 || !modelDeploymentScalerFinite((*field.value).AsApproximateFloat64()) {
				return nil, modelDeploymentScalerPriorRefusal(role.Name, "explicit capacity field %s must be finite and positive", field.name)
			}
		}
		status := workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: role.Name}
		var resolution priors.Resolution
		if missing {
			if catalog == nil {
				var err error
				catalog, err = modelDeploymentScalerLoadCatalog(ctx, reader, scaler)
				if err != nil {
					return nil, err
				}
			}
			var err error
			resolution, status, err = modelDeploymentScalerResolveRolePrior(ctx, reader, scaler, target, &role, effective, catalog, quantile)
			if err != nil {
				return nil, err
			}
		}
		for _, field := range fields {
			source := workercore.ModelDeploymentScalerCapacityFieldSourceExplicit
			if *field.value == nil {
				quantity, err := modelDeploymentScalerPriorQuantity(resolution.EffectiveTokensPerSecond)
				if err != nil {
					return nil, modelDeploymentScalerPriorRefusal(role.Name, "effective capacity cannot be represented: %v", err)
				}
				*field.value = quantity
				source = workercore.ModelDeploymentScalerCapacityFieldSourceCatalog
			}
			status.Fields = append(status.Fields, workercore.ModelDeploymentScalerResolvedCapacityFieldStatus{
				Field: field.name, Source: source, EffectiveTokensPerSecond: (*field.value).DeepCopy(),
			})
		}
		fingerprint, err := modelDeploymentScalerPriorFingerprint(target, &role, effective, status)
		if err != nil {
			return nil, err
		}
		status.CompatibilityFingerprint = fingerprint
		result.Algorithms[role.Name] = *effective
		result.Status = append(result.Status, status)
	}
	return result, nil
}

func modelDeploymentScalerLoadCatalog(ctx context.Context, reader ctrlcli.Reader, scaler *workercore.ModelDeploymentScaler) (*priors.Catalog, error) {
	ref := scaler.Spec.Algorithm.CapacityPrior.CatalogRef
	if ref == nil {
		return priors.LoadEmbedded()
	}
	cm := new(core.ConfigMap)
	if err := reader.Get(ctx, ctrlcli.ObjectKey{Namespace: scaler.Namespace, Name: ref.Name}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, modelDeploymentScalerPriorRefusal("", "catalog ConfigMap %q does not exist", ref.Name)
		}
		return nil, err
	}
	key := ref.Key
	if key == "" {
		key = priors.DefaultCatalogConfigMapKey
	}
	catalogBytes, found, err := modelDeploymentScalerCatalogData(cm, key)
	if err != nil {
		return nil, err
	}
	if !found {
		// THE OTHER NAME IS TRIED, because a ConfigMap written before the
		// catalog changed its file name still holds the catalog the operator
		// shipped, and a reader that refuses it refuses the deployment too.
		for _, fallback := range []string{priors.DefaultCatalogConfigMapKey, priors.LegacyCatalogConfigMapKey} {
			if fallback == key {
				continue
			}
			catalogBytes, found, err = modelDeploymentScalerCatalogData(cm, fallback)
			if err != nil {
				return nil, err
			}
			if found {
				key = fallback
				break
			}
		}
	}
	if !found {
		return nil, modelDeploymentScalerPriorRefusal("", "catalog ConfigMap %q lacks key %q", ref.Name, key)
	}
	catalog, err := priors.ParseCatalog(catalogBytes)
	if err != nil {
		return nil, modelDeploymentScalerPriorRefusal("", "catalog ConfigMap %q is invalid: %.512s", ref.Name, err)
	}
	return catalog, nil
}

// modelDeploymentScalerCatalogData returns the bytes of one catalog key.
//
// IT REFUSES A KEY THAT EXISTS IN BOTH DATA AND BINARY DATA, because two
// copies of a catalog are not one catalog.
func modelDeploymentScalerCatalogData(cm *core.ConfigMap, key string) ([]byte, bool, error) {
	data, present := cm.Data[key]
	binary := cm.BinaryData[key]
	if present && binary != nil {
		return nil, false, modelDeploymentScalerPriorRefusal("", "catalog key %q exists in both data and binaryData", key)
	}
	if present {
		return []byte(data), true, nil
	}
	if binary != nil {
		return binary, true, nil
	}
	return nil, false, nil
}

func modelDeploymentScalerResolveRolePrior(
	ctx context.Context, reader ctrlcli.Reader, scaler *workercore.ModelDeploymentScaler, target *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole, effective *workercore.ModelDeploymentScalerAlgorithmSpec, catalog *priors.Catalog,
	quantile float64,
) (priors.Resolution, workercore.ModelDeploymentScalerResolvedCapacityPriorStatus, error) {
	status := workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: role.Name}
	// THE CATALOG IS SELECTED BY WHAT THE DEPLOYMENT STATES, because a profile
	// is one deployment shape and the deployment states the shape it runs.
	declaration, err := modelDeploymentScalerPriorDeclarationOf(ctx, reader, target, role)
	if err != nil {
		return priors.Resolution{}, status, err
	}
	shape := declaration.shape(modelDeploymentScalerRoleKind(ModelDeploymentEffectiveRoleKind(role)))
	profile, refusal := catalog.Select(shape)
	if refusal != nil {
		refusal.Role = role.Name
		return priors.Resolution{}, status, refusal
	}
	request, err := modelDeploymentScalerPriorRequest(target, role, &profile, declaration)
	if err != nil {
		return priors.Resolution{}, status, err
	}
	token := effective.TokenThroughput
	if effective.Type == workercore.ModelDeploymentScalerAlgorithmTypePredictive {
		token = &effective.Predictive.TokenThroughput
	}
	if token.TTFTTargetMilliseconds != nil {
		request.Constraints.TTFTTargetMilliseconds = token.TTFTTargetMilliseconds.AsApproximateFloat64()
	}
	if token.ITLTargetMilliseconds != nil {
		request.Constraints.ITLTargetMilliseconds = token.ITLTargetMilliseconds.AsApproximateFloat64()
	}
	// THE RESOLUTION IS HELD TO THE QUANTILE THE SCALER ASKED FOR, and the
	// resolver answers it with the measured quantile it could prove.
	request.Constraints.LatencyQuantile = quantile
	if headroom := scaler.Spec.Algorithm.CapacityPrior.Headroom; headroom != nil {
		value := headroom.AsApproximateFloat64()
		if !modelDeploymentScalerFinite(value) || value <= 0 || value > 1 {
			return priors.Resolution{}, status, modelDeploymentScalerPriorRefusal(role.Name, "headroom must be in (0,1]")
		}
		request.Constraints.Headroom = value
	}
	resolution, err := catalog.Resolve(request)
	if err != nil {
		var refusal *priors.RefusalError
		if errors.As(err, &refusal) {
			refusal.Role = role.Name
		}
		return priors.Resolution{}, status, err
	}
	basis := resolution.Basis
	status.SourceID, status.ProfileDigest = basis.SourceID, basis.ProfileDigest
	status.Metric, status.Quantile, status.WorkloadID = basis.Metric, basis.Quantile, basis.WorkloadID
	status.Denominator, status.CatalogDigest, status.SourceRevision = basis.Denominator, catalog.Digest, basis.SourceRevision
	status.WorkloadRevision = resolution.WorkloadRevision
	status.GPUsPerReplica, status.MeasuredReplicas = int32(resolution.GPUsPerReplica), int32(resolution.MeasuredReplicas)
	status.Headroom, err = modelDeploymentScalerPriorQuantity(basis.Headroom)
	if err != nil {
		return priors.Resolution{}, status, modelDeploymentScalerPriorRefusal(role.Name, "headroom cannot be represented: %v", err)
	}
	status.SeedTokensPerSecond, err = modelDeploymentScalerPriorQuantity(resolution.SeedTokensPerSecond)
	if err != nil {
		return priors.Resolution{}, status, modelDeploymentScalerPriorRefusal(role.Name, "seed capacity cannot be represented: %v", err)
	}
	for _, value := range []string{status.SourceID, status.SourceRevision, status.WorkloadID, status.WorkloadRevision} {
		if utf8.RuneCountInString(value) > 253 {
			return priors.Resolution{}, status, modelDeploymentScalerPriorRefusal(role.Name, "catalog provenance identifiers must not exceed 253 characters")
		}
	}
	if profile.MeasuredReplicas > math.MaxInt32 {
		return priors.Resolution{}, status, modelDeploymentScalerPriorRefusal(role.Name, "measured replica denominator exceeds the supported status range")
	}
	selectedQuantile := 0.0
	for _, bound := range basis.LatencyBounds {
		selectedQuantile = max(selectedQuantile, bound.Quantile)
		status.LatencyBounds = append(status.LatencyBounds, workercore.ModelDeploymentScalerResolvedLatencyBoundStatus{
			Metric: string(bound.Metric), Quantile: priors.FormatLatencyQuantile(bound.Quantile), Conservative: bound.Conservative,
		})
	}

	// THE MEASURED QUANTILE AND THE REQUESTED ONE ARE KEPT APART, because a
	// catalog that measured a higher quantile than the request needed is a
	// different answer from one that measured the requested quantile.
	status.Quantile = priors.FormatLatencyQuantile(selectedQuantile)
	status.RequestedQuantile = priors.FormatLatencyQuantile(quantile)
	return resolution, status, nil
}

func modelDeploymentScalerPriorFingerprint(
	target *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
	effective *workercore.ModelDeploymentScalerAlgorithmSpec, status workercore.ModelDeploymentScalerResolvedCapacityPriorStatus,
) (string, error) {
	canonicalRole := role.DeepCopy()
	canonicalRole.Replicas = 0
	annotations := make(map[string]string)
	for _, key := range []string{
		workercore.ModelDeploymentScalerModelIDAnnotationKey, workercore.ModelDeploymentScalerHardwareIDAnnotationKey,
		workercore.ModelDeploymentScalerPrecisionAnnotationKey, workercore.ModelDeploymentScalerPrefillHardwareIDAnnotationKey,
		workercore.ModelDeploymentScalerDecodeHardwareIDAnnotationKey,
	} {
		annotations[key] = target.Annotations[key]
	}
	// Sorting removes declaration order without changing the ordered launch arguments.
	canonicalAlgorithm := effective.DeepCopy()
	return priors.Digest(struct {
		TargetUID   string
		Role        *workercore.ModelDeploymentRole
		Model       workercore.ModelDeploymentModel
		Engine      workercore.ModelDeploymentEngine
		Router      *workercore.ModelDeploymentRouter
		KVCache     *workercore.ModelDeploymentKVCache
		KVTransfer  *workercore.ModelDeploymentKVTransfer
		Annotations map[string]string
		Algorithm   *workercore.ModelDeploymentScalerAlgorithmSpec
		Evidence    workercore.ModelDeploymentScalerResolvedCapacityPriorStatus
	}{
		string(target.UID), canonicalRole, target.Spec.Model, target.Spec.Engine, target.Spec.Router, target.Spec.KVCache,
		target.Spec.KVTransfer, annotations, canonicalAlgorithm, status,
	})
}

func modelDeploymentScalerFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// modelDeploymentScalerPriorRequested reports whether the scaler asked for a capacity prior.
//
// The presence of the block is the request, because the block holds no switch that could turn
// the resolution off. A scaler without the block states every capacity by hand.
func modelDeploymentScalerPriorRequested(scaler *workercore.ModelDeploymentScaler) bool {
	return scaler.Spec.Algorithm.CapacityPrior != nil
}

func modelDeploymentScalerCapacityFields(
	algorithm *workercore.ModelDeploymentScalerAlgorithmSpec, kind workercore.ModelDeploymentRoleKind,
) []modelDeploymentScalerCapacityField {
	if kind == workercore.ModelDeploymentRoleKindEncode || algorithm.Type == workercore.ModelDeploymentScalerAlgorithmTypeProportional {
		return nil
	}
	var fields []modelDeploymentScalerCapacityField
	var token *workercore.ModelDeploymentScalerTokenThroughputSpec
	prefix := "tokenThroughput."
	if algorithm.Type == workercore.ModelDeploymentScalerAlgorithmTypePredictive {
		if algorithm.Predictive == nil {
			algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{}
		}
		predictive := algorithm.Predictive
		switch kind {
		case workercore.ModelDeploymentRoleKindServer:
			fields = append(fields, modelDeploymentScalerCapacityField{"predictive.serverTokenCapacityPerReplica", &predictive.ServerTokenCapacityPerReplica})
		case workercore.ModelDeploymentRoleKindPrefill:
			fields = append(fields, modelDeploymentScalerCapacityField{
				"predictive.prefillTokenCapacityPerReplica", &predictive.PrefillTokenCapacityPerReplica,
			})
		case workercore.ModelDeploymentRoleKindDecode:
			fields = append(fields, modelDeploymentScalerCapacityField{"predictive.decodeTokenCapacityPerReplica", &predictive.DecodeTokenCapacityPerReplica})
		}
		token, prefix = &predictive.TokenThroughput, "predictive.tokenThroughput."
	} else {
		if algorithm.TokenThroughput == nil {
			algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{}
		}
		token = algorithm.TokenThroughput
	}
	switch kind {
	case workercore.ModelDeploymentRoleKindServer:
		fields = append(fields, modelDeploymentScalerCapacityField{prefix + "serverTargetTokensPerSecond", &token.ServerTargetTokensPerSecond})
	case workercore.ModelDeploymentRoleKindPrefill:
		fields = append(fields, modelDeploymentScalerCapacityField{prefix + "prefillTargetTokensPerSecond", &token.PrefillTargetTokensPerSecond})
	case workercore.ModelDeploymentRoleKindDecode:
		fields = append(fields, modelDeploymentScalerCapacityField{prefix + "decodeTargetTokensPerSecond", &token.DecodeTargetTokensPerSecond})
	}
	return fields
}

func modelDeploymentScalerPriorRefusal(role, format string, args ...any) error {
	return &priors.RefusalError{Reason: priors.RefusalCapacityPriorUnresolved, Message: fmt.Sprintf("role %q: "+format, append([]any{role}, args...)...)}
}

func modelDeploymentScalerPriorQuantity(value float64) (*resource.Quantity, error) {
	if !modelDeploymentScalerFinite(value) || value <= 0 {
		return nil, fmt.Errorf("capacity must be finite and positive")
	}
	quantity, err := resource.ParseQuantity(strconv.FormatFloat(value, 'g', -1, 64))
	if err != nil {
		return nil, err
	}
	represented := quantity.AsApproximateFloat64()
	if !modelDeploymentScalerFinite(represented) || represented <= 0 || math.Abs(represented-value)/value > 1e-12 {
		return nil, fmt.Errorf("capacity exceeds Quantity precision or range")
	}
	return &quantity, nil
}
