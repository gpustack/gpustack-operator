package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

// ModelDeploymentScalerPriorResolutionMode identifies how a scaler resolved
// capacity priors across its roles.
type ModelDeploymentScalerPriorResolutionMode string

const (
	// ModelDeploymentScalerPriorModeAggregated resolves each role independently from
	// mode: aggregated profiles (or explicit manual capacities).
	ModelDeploymentScalerPriorModeAggregated ModelDeploymentScalerPriorResolutionMode = "Aggregated"
	// ModelDeploymentScalerPriorModeDisaggregated resolves coupled PD/EPD roles as a
	// fixed-ratio whole unit from a mode: disaggregated profile.
	ModelDeploymentScalerPriorModeDisaggregated ModelDeploymentScalerPriorResolutionMode = "Disaggregated"
	// ModelDeploymentScalerPriorModeAggregatedFallback estimates fixed-ratio
	// whole-unit capacity from mode: aggregated profiles when mode: disaggregated is absent.
	ModelDeploymentScalerPriorModeAggregatedFallback ModelDeploymentScalerPriorResolutionMode = "AggregatedFallback"
)

// ModelDeploymentScalerWholeUnitPrior captures fixed-ratio whole-unit scaling
// capacity derived from disaggregated or aggregated fallback priors.
type ModelDeploymentScalerWholeUnitPrior struct {
	UnitVector                map[string]int32
	PrefillRole               string
	DecodeRole                string
	EncodeRole                string
	PrefillGPUsPerReplica     int32
	DecodeGPUsPerReplica      int32
	UnitGPUs                  int32
	MinUnits                  int32
	MaxUnits                  int32
	PerGPUTokensPerSecond     float64
	UnitTargetTokensPerSecond float64
	JointCompatibilityDigest  string
}

// ModelDeploymentScalerCapacityPriors contains independent effective algorithms and their evidence.
type ModelDeploymentScalerCapacityPriors struct {
	Algorithms       map[string]workercore.ModelDeploymentScalerAlgorithmSpec
	Status           []workercore.ModelDeploymentScalerResolvedCapacityPriorStatus
	Mode             ModelDeploymentScalerPriorResolutionMode
	WholeUnit        *ModelDeploymentScalerWholeUnitPrior
	ConditionMessage string
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
	result := &ModelDeploymentScalerCapacityPriors{
		Algorithms: make(map[string]workercore.ModelDeploymentScalerAlgorithmSpec),
		Mode:       ModelDeploymentScalerPriorModeAggregated,
	}
	// THE QUANTILE THE SCALER IS HELD TO IS RESOLVED ONCE, because the
	// resolution and the online sampling have to answer the same question.
	quantile, quantileErr := ModelDeploymentScalerLatencyQuantile(&scaler.Spec.Algorithm)
	if quantileErr != nil {
		return nil, modelDeploymentScalerPriorRefusal("", "%v", quantileErr)
	}
	if !modelDeploymentScalerPriorRequested(scaler) {
		for _, role := range target.Spec.Roles {
			result.Algorithms[role.Name] = *scaler.Spec.Algorithm.DeepCopy()
		}
		return result, nil
	}

	needsCatalog := false
	for _, role := range target.Spec.Roles {
		kind := ModelDeploymentEffectiveRoleKind(&role)
		switch kind {
		case workercore.ModelDeploymentRoleKindServer, workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode, workercore.ModelDeploymentRoleKindEncode:
		default:
			return nil, modelDeploymentScalerPriorRefusal(role.Name, "role kind %q is not supported by Catalog", kind)
		}
		effective := scaler.Spec.Algorithm.DeepCopy()
		for _, field := range modelDeploymentScalerCapacityFields(effective, kind) {
			if *field.value == nil {
				needsCatalog = true
				continue
			}
			if (*field.value).Sign() <= 0 || !modelDeploymentScalerFinite((*field.value).AsApproximateFloat64()) {
				return nil, modelDeploymentScalerPriorRefusal(role.Name, "explicit capacity field %s must be finite and positive", field.name)
			}
		}
	}
	if !needsCatalog {
		return modelDeploymentScalerResolveExplicitPriors(scaler, target, result)
	}

	catalog, err := modelDeploymentScalerLoadCatalog(ctx, reader, scaler)
	if err != nil {
		return nil, err
	}
	if ModelDeploymentDeclaresBothHalves(target) {
		return modelDeploymentScalerResolveDisaggregatedPriors(ctx, reader, scaler, target, catalog, quantile)
	}
	return modelDeploymentScalerResolveAggregatedLoop(ctx, reader, scaler, target, catalog, quantile)
}

func modelDeploymentScalerResolveExplicitPriors(
	scaler *workercore.ModelDeploymentScaler, target *workercore.ModelDeployment, result *ModelDeploymentScalerCapacityPriors,
) (*ModelDeploymentScalerCapacityPriors, error) {
	for _, role := range target.Spec.Roles {
		effective := scaler.Spec.Algorithm.DeepCopy()
		fields := modelDeploymentScalerCapacityFields(effective, ModelDeploymentEffectiveRoleKind(&role))
		status := workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: role.Name}
		for _, field := range fields {
			status.Fields = append(status.Fields, workercore.ModelDeploymentScalerResolvedCapacityFieldStatus{
				Field:                    field.name,
				Source:                   workercore.ModelDeploymentScalerCapacityFieldSourceExplicit,
				EffectiveTokensPerSecond: (*field.value).DeepCopy(),
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

func modelDeploymentScalerResolveAggregatedLoop(
	ctx context.Context, reader ctrlcli.Reader, scaler *workercore.ModelDeploymentScaler,
	target *workercore.ModelDeployment, catalog *priors.Catalog, quantile float64,
) (*ModelDeploymentScalerCapacityPriors, error) {
	result := &ModelDeploymentScalerCapacityPriors{
		Algorithms: make(map[string]workercore.ModelDeploymentScalerAlgorithmSpec, len(target.Spec.Roles)),
		Mode:       ModelDeploymentScalerPriorModeAggregated,
	}
	for _, role := range target.Spec.Roles {
		effective, status, err := modelDeploymentScalerResolveSingleRole(ctx, reader, scaler, target, &role, catalog, quantile)
		if err != nil {
			return nil, err
		}
		result.Algorithms[role.Name] = *effective
		result.Status = append(result.Status, status)
	}
	return result, nil
}

func modelDeploymentScalerResolveSingleRole(
	ctx context.Context, reader ctrlcli.Reader, scaler *workercore.ModelDeploymentScaler,
	target *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, catalog *priors.Catalog, quantile float64,
) (*workercore.ModelDeploymentScalerAlgorithmSpec, workercore.ModelDeploymentScalerResolvedCapacityPriorStatus, error) {
	effective := scaler.Spec.Algorithm.DeepCopy()
	kind := ModelDeploymentEffectiveRoleKind(role)
	if kind == workercore.ModelDeploymentRoleKindEncode {
		if err := modelDeploymentScalerValidateEncodeRole(ctx, reader, target, role); err != nil {
			return nil, workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{}, err
		}
		status := workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: role.Name}
		fingerprint, err := modelDeploymentScalerPriorFingerprint(target, role, effective, status)
		if err != nil {
			return nil, status, err
		}
		status.CompatibilityFingerprint = fingerprint
		return effective, status, nil
	}
	if kind == workercore.ModelDeploymentRoleKindPrefill || kind == workercore.ModelDeploymentRoleKindDecode {
		return nil, workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: role.Name}, modelDeploymentScalerPriorRefusal(
			role.Name, "role kind %q requires both Prefill and Decode roles for catalog prior resolution", kind,
		)
	}
	fields := modelDeploymentScalerCapacityFields(effective, kind)
	missing := false
	for _, field := range fields {
		if *field.value == nil {
			missing = true
		}
	}
	status := workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: role.Name}
	var resolution priors.Resolution
	if missing {
		var err error
		resolution, status, err = modelDeploymentScalerResolveRolePrior(ctx, reader, scaler, target, role, effective, catalog, quantile)
		if err != nil {
			return nil, status, err
		}
	}
	for _, field := range fields {
		source := workercore.ModelDeploymentScalerCapacityFieldSourceExplicit
		if *field.value == nil {
			quantity, err := modelDeploymentScalerPriorQuantity(resolution.EffectiveTokensPerSecond)
			if err != nil {
				return nil, status, modelDeploymentScalerPriorRefusal(role.Name, "effective capacity cannot be represented: %v", err)
			}
			*field.value = quantity
			source = workercore.ModelDeploymentScalerCapacityFieldSourceCatalog
		}
		status.Fields = append(status.Fields, workercore.ModelDeploymentScalerResolvedCapacityFieldStatus{
			Field: field.name, Source: source, EffectiveTokensPerSecond: (*field.value).DeepCopy(),
		})
	}
	fingerprint, err := modelDeploymentScalerPriorFingerprint(target, role, effective, status)
	if err != nil {
		return nil, status, err
	}
	status.CompatibilityFingerprint = fingerprint
	return effective, status, nil
}

const maxCachedConfigMapCatalogs = 32

var (
	configMapCatalogCacheMu sync.RWMutex
	configMapCatalogCache   = make(map[[32]byte]*priors.Catalog, maxCachedConfigMapCatalogs)
)

func parseCachedConfigMapCatalog(data []byte) (*priors.Catalog, error) {
	sum := sha256.Sum256(data)
	configMapCatalogCacheMu.RLock()
	cached, ok := configMapCatalogCache[sum]
	configMapCatalogCacheMu.RUnlock()
	if ok {
		return cached, nil
	}
	catalog, err := priors.ParseCatalog(data)
	if err != nil {
		return nil, err
	}
	configMapCatalogCacheMu.Lock()
	if len(configMapCatalogCache) >= maxCachedConfigMapCatalogs {
		for key := range configMapCatalogCache {
			delete(configMapCatalogCache, key)
			if len(configMapCatalogCache) < maxCachedConfigMapCatalogs {
				break
			}
		}
	}
	configMapCatalogCache[sum] = catalog
	configMapCatalogCacheMu.Unlock()
	return catalog, nil
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
	catalog, err := parseCachedConfigMapCatalog(catalogBytes)
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
	declaration, err := modelDeploymentScalerPriorDeclarationOf(ctx, reader, target, role)
	if err != nil {
		return priors.Resolution{}, workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: role.Name}, err
	}
	return modelDeploymentScalerResolveProfilePrior(
		scaler, target, role, effective, catalog, declaration, quantile, false,
	)
}

func modelDeploymentScalerApplyRequestConstraints(
	request *priors.Request, scaler *workercore.ModelDeploymentScaler, effective *workercore.ModelDeploymentScalerAlgorithmSpec,
	roleName string, quantile float64,
) error {
	token := effective.TokenThroughput
	if effective.Type == workercore.ModelDeploymentScalerAlgorithmTypePredictive {
		token = &effective.Predictive.TokenThroughput
	}
	if token != nil && token.TTFTTargetMilliseconds != nil {
		request.Constraints.TTFTTargetMilliseconds = token.TTFTTargetMilliseconds.AsApproximateFloat64()
	}
	if token != nil && token.ITLTargetMilliseconds != nil {
		request.Constraints.ITLTargetMilliseconds = token.ITLTargetMilliseconds.AsApproximateFloat64()
	}
	// THE RESOLUTION IS HELD TO THE QUANTILE THE SCALER ASKED FOR, and the
	// resolver answers it with the measured quantile it could prove.
	request.Constraints.LatencyQuantile = quantile
	if headroom := scaler.Spec.Algorithm.CapacityPrior.Headroom; headroom != nil {
		value := headroom.AsApproximateFloat64()
		if !modelDeploymentScalerFinite(value) || value <= 0 || value > 1 {
			return modelDeploymentScalerPriorRefusal(roleName, "headroom must be in (0,1]")
		}
		request.Constraints.Headroom = value
	}
	return nil
}

func modelDeploymentScalerPopulateStatusFromResolution(
	roleName, catalogDigest string, resolution priors.Resolution, quantile float64,
	seedTokensPerSecond float64, gpusPerReplica, measuredReplicas int,
) (workercore.ModelDeploymentScalerResolvedCapacityPriorStatus, error) {
	status := workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: roleName}
	basis := resolution.Basis
	status.SourceID, status.ProfileDigest = basis.SourceID, basis.ProfileDigest
	status.Metric, status.Quantile, status.WorkloadID = basis.Metric, basis.Quantile, basis.WorkloadID
	status.Denominator, status.CatalogDigest, status.SourceRevision = basis.Denominator, catalogDigest, basis.SourceRevision
	status.WorkloadRevision = resolution.WorkloadRevision
	if gpusPerReplica <= 0 || measuredReplicas <= 0 || gpusPerReplica > math.MaxInt32 || measuredReplicas > math.MaxInt32 {
		return workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: roleName}, modelDeploymentScalerPriorRefusal(
			roleName, "measured replica denominator exceeds the supported status range",
		)
	}
	status.GPUsPerReplica, status.MeasuredReplicas = int32(gpusPerReplica), int32(measuredReplicas)
	var err error
	status.Headroom, err = modelDeploymentScalerPriorQuantity(basis.Headroom)
	if err != nil {
		return workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: roleName}, modelDeploymentScalerPriorRefusal(
			roleName, "headroom cannot be represented: %v", err,
		)
	}
	status.SeedTokensPerSecond, err = modelDeploymentScalerPriorQuantity(seedTokensPerSecond)
	if err != nil {
		return workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: roleName}, modelDeploymentScalerPriorRefusal(
			roleName, "seed capacity cannot be represented: %v", err,
		)
	}
	for _, value := range []string{status.SourceID, status.SourceRevision, status.WorkloadID, status.WorkloadRevision} {
		if utf8.RuneCountInString(value) > 253 {
			return workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: roleName}, modelDeploymentScalerPriorRefusal(
				roleName, "catalog provenance identifiers must not exceed 253 characters",
			)
		}
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
	return status, nil
}

func modelDeploymentScalerResolveProfilePrior(
	scaler *workercore.ModelDeploymentScaler, target *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole, effective *workercore.ModelDeploymentScalerAlgorithmSpec, catalog *priors.Catalog,
	declaration modelDeploymentScalerPriorDeclaration, quantile float64, serverRouterFallback bool,
) (priors.Resolution, workercore.ModelDeploymentScalerResolvedCapacityPriorStatus, error) {
	status := workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{Name: role.Name}
	shape := declaration.aggregatedShape()
	profile, refusal := catalog.Select(shape)
	if refusal != nil {
		refusal.Role = role.Name
		return priors.Resolution{}, status, refusal
	}
	buildRequest := modelDeploymentScalerPriorRequest
	if serverRouterFallback {
		buildRequest = modelDeploymentScalerPriorRequestForServerFallback
	}
	request, err := buildRequest(target, role, &profile, declaration)
	if err != nil {
		return priors.Resolution{}, status, err
	}
	if err := modelDeploymentScalerApplyRequestConstraints(&request, scaler, effective, role.Name, quantile); err != nil {
		return priors.Resolution{}, status, err
	}
	resolution, err := catalog.Resolve(request)
	if err != nil {
		var refusal *priors.RefusalError
		if errors.As(err, &refusal) {
			refusal.Role = role.Name
		}
		return priors.Resolution{}, status, err
	}
	status, err = modelDeploymentScalerPopulateStatusFromResolution(
		role.Name, catalog.Digest, resolution, quantile,
		resolution.SeedTokensPerSecond, resolution.GPUsPerReplica, resolution.MeasuredReplicas,
	)
	if err != nil {
		return priors.Resolution{}, status, err
	}
	return resolution, status, nil
}

type modelDeploymentScalerDisaggRoleSet struct {
	encode  *workercore.ModelDeploymentRole
	prefill *workercore.ModelDeploymentRole
	decode  *workercore.ModelDeploymentRole
}

func modelDeploymentScalerDisaggregatedRoles(target *workercore.ModelDeployment) (modelDeploymentScalerDisaggRoleSet, error) {
	var set modelDeploymentScalerDisaggRoleSet
	for i := range target.Spec.Roles {
		role := &target.Spec.Roles[i]
		switch ModelDeploymentEffectiveRoleKind(role) {
		case workercore.ModelDeploymentRoleKindEncode:
			if set.encode != nil {
				return set, modelDeploymentScalerPriorRefusal(role.Name, "multiple Encode roles are not supported by disaggregated capacity priors")
			}
			set.encode = role
		case workercore.ModelDeploymentRoleKindPrefill:
			if set.prefill != nil {
				return set, modelDeploymentScalerPriorRefusal(role.Name, "multiple Prefill roles are not supported by disaggregated capacity priors")
			}
			set.prefill = role
		case workercore.ModelDeploymentRoleKindDecode:
			if set.decode != nil {
				return set, modelDeploymentScalerPriorRefusal(role.Name, "multiple Decode roles are not supported by disaggregated capacity priors")
			}
			set.decode = role
		default:
			return set, modelDeploymentScalerPriorRefusal(
				role.Name,
				"mixed or unsupported role kind %q cannot resolve disaggregated capacity priors",
				ModelDeploymentEffectiveRoleKind(role),
			)
		}
	}
	if set.prefill == nil || set.decode == nil {
		return set, modelDeploymentScalerPriorRefusal("", "disaggregated deployment must declare both Prefill and Decode roles")
	}
	return set, nil
}

func modelDeploymentScalerValidateEncodeRole(
	ctx context.Context, reader ctrlcli.Reader, target *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) (err error) {
	defer func() {
		err = modelDeploymentScalerRedactPriorRefusal(err, role, nil)
	}()
	if len(role.Command) != 0 || role.ReplicaSize > 1 || role.ElasticEP != nil {
		return modelDeploymentScalerPriorRefusal(role.Name, "Catalog requires a managed single-serving-Pod replica")
	}
	manufacturer := ""
	if role.AcceleratorType != "" {
		acc, _, accErr := modelDeploymentScalerPriorHardwareIdentity(ctx, reader, role)
		if accErr != nil {
			return accErr
		}
		manufacturer = acc.Status.Detail.Manufacturer
	}
	if gateErr := modelDeploymentScalerOperatorEnvGateWithManufacturer(target.Spec.Engine.Name, target, role, manufacturer); gateErr != nil {
		return modelDeploymentScalerPriorRefusal(role.Name, "%v", gateErr)
	}
	return nil
}

func modelDeploymentScalerResolveDisaggregatedPriors(
	ctx context.Context, reader ctrlcli.Reader, scaler *workercore.ModelDeploymentScaler,
	target *workercore.ModelDeployment, catalog *priors.Catalog, quantile float64,
) (*ModelDeploymentScalerCapacityPriors, error) {
	roles, err := modelDeploymentScalerDisaggregatedRoles(target)
	if err != nil {
		return nil, err
	}
	if roles.encode != nil {
		if err := modelDeploymentScalerValidateEncodeRole(ctx, reader, target, roles.encode); err != nil {
			return nil, err
		}
	}
	if err := modelDeploymentScalerValidateWholeUnitPrerequisites(scaler, roles); err != nil {
		return nil, err
	}
	unitVector, minUnits, maxUnits, err := solveFixedWholeUnitVector(scaler, target)
	if err != nil {
		return nil, err
	}
	prefillDecl, err := modelDeploymentScalerPriorDeclarationOf(ctx, reader, target, roles.prefill)
	if err != nil {
		return nil, err
	}
	decodeDecl, err := modelDeploymentScalerPriorDeclarationOf(ctx, reader, target, roles.decode)
	if err != nil {
		return nil, err
	}

	uP, uD := int64(unitVector[roles.prefill.Name]), int64(unitVector[roles.decode.Name])
	pdScale := gcdInt64(uP, uD)
	if pdScale <= 0 {
		return nil, modelDeploymentScalerPriorRefusal("", "whole-unit PD stage ratio must be positive")
	}
	reducedPrefill, reducedDecode := uP/pdScale, uD/pdScale
	if reducedPrefill > math.MaxInt32 || reducedDecode > math.MaxInt32 {
		return nil, modelDeploymentScalerPriorRefusal("", "reduced disaggregated replica count exceeds supported int32 bounds")
	}
	rP, rD := int(reducedPrefill), int(reducedDecode)

	sharedQuant, quantConflict := modelDeploymentScalerDisaggregatedQuantization(
		prefillDecl.identity.Model.Quantization, decodeDecl.identity.Model.Quantization,
	)
	if !quantConflict {
		shape := priors.Shape{
			Mode:         priors.ModeDisaggregated,
			Model:        priors.CanonicalModelName(prefillDecl.identity.Model.Name),
			Quantization: sharedQuant,
			Engine:       prefillDecl.engineFamily,
			Prefill:      prefillDecl.stageShape(rP),
			Decode:       decodeDecl.stageShape(rD),
		}
		profile, selectErr := catalog.Select(shape)
		if selectErr != nil {
			if !priors.IsProfileNotFound(selectErr) {
				return nil, selectErr
			}
		} else {
			return modelDeploymentScalerResolveDisaggregatedWholeUnit(
				scaler, target, roles, &profile, prefillDecl, decodeDecl, catalog, quantile,
				unitVector, minUnits, maxUnits, pdScale, rP, rD,
			)
		}
	}

	return modelDeploymentScalerResolveAggregatedFallback(
		scaler, target, roles, prefillDecl, decodeDecl, catalog, quantile,
		unitVector, minUnits, maxUnits,
	)
}

func modelDeploymentScalerValidateWholeUnitPrerequisites(
	scaler *workercore.ModelDeploymentScaler, roles modelDeploymentScalerDisaggRoleSet,
) error {
	for _, role := range []*workercore.ModelDeploymentRole{roles.prefill, roles.decode} {
		probeSpec := scaler.Spec.Algorithm.DeepCopy()
		for _, field := range modelDeploymentScalerCapacityFields(probeSpec, ModelDeploymentEffectiveRoleKind(role)) {
			if *field.value != nil {
				return modelDeploymentScalerPriorRefusal(
					role.Name, "disaggregated whole-unit prior resolution does not permit partial explicit capacity field %s", field.name,
				)
			}
		}
	}
	tokenSpec := scaler.Spec.Algorithm.TokenThroughput
	if scaler.Spec.Algorithm.Type == workercore.ModelDeploymentScalerAlgorithmTypePredictive && scaler.Spec.Algorithm.Predictive != nil {
		tokenSpec = &scaler.Spec.Algorithm.Predictive.TokenThroughput
	}
	if tokenSpec == nil || tokenSpec.TTFTTargetMilliseconds == nil || tokenSpec.TTFTTargetMilliseconds.AsApproximateFloat64() <= 0 ||
		!modelDeploymentScalerFinite(tokenSpec.TTFTTargetMilliseconds.AsApproximateFloat64()) {
		return modelDeploymentScalerPriorRefusal("", "disaggregated whole-unit prior resolution requires positive TTFT latency target")
	}
	if tokenSpec.ITLTargetMilliseconds == nil || tokenSpec.ITLTargetMilliseconds.AsApproximateFloat64() <= 0 ||
		!modelDeploymentScalerFinite(tokenSpec.ITLTargetMilliseconds.AsApproximateFloat64()) {
		return modelDeploymentScalerPriorRefusal("", "disaggregated whole-unit prior resolution requires positive ITL latency target")
	}
	return nil
}

func modelDeploymentScalerDisaggregatedQuantization(prefillQuant, decodeQuant string) (string, bool) {
	canonP := priors.CanonicalQuantization(prefillQuant)
	canonD := priors.CanonicalQuantization(decodeQuant)
	if canonP != "" && canonD != "" {
		if canonP != canonD {
			return "", true
		}
		return canonP, false
	}
	rawP := strings.ToLower(strings.TrimSpace(prefillQuant))
	rawD := strings.ToLower(strings.TrimSpace(decodeQuant))
	if rawP != "" && rawD != "" && rawP != rawD {
		return "", true
	}
	if canonP != "" {
		return canonP, false
	}
	return canonD, false
}

func modelDeploymentScalerResolveDisaggregatedWholeUnit(
	scaler *workercore.ModelDeploymentScaler, target *workercore.ModelDeployment,
	roles modelDeploymentScalerDisaggRoleSet, profile *priors.Profile,
	prefillDecl, decodeDecl modelDeploymentScalerPriorDeclaration,
	catalog *priors.Catalog, quantile float64,
	unitVector map[string]int32, minUnits, maxUnits int32,
	pdScale int64, rP, rD int,
) (*ModelDeploymentScalerCapacityPriors, error) {
	request, err := modelDeploymentScalerDisaggregatedPriorRequest(
		target, roles.prefill, roles.decode, profile, prefillDecl, decodeDecl, rP, rD,
	)
	if err != nil {
		return nil, err
	}
	prefillEffective := scaler.Spec.Algorithm.DeepCopy()
	if err := modelDeploymentScalerApplyRequestConstraints(&request, scaler, prefillEffective, "", quantile); err != nil {
		return nil, err
	}
	resolution, err := catalog.Resolve(request)
	if err != nil {
		return nil, modelDeploymentScalerRedactDisaggregatedRefusal(err, roles.prefill, roles.decode, profile)
	}
	if profile.Prefill == nil || profile.Decode == nil ||
		profile.Prefill.Topology.GPUsPerReplica <= 0 || profile.Decode.Topology.GPUsPerReplica <= 0 {
		return nil, modelDeploymentScalerPriorRefusal("", "resolved disaggregated profile must carry positive stage GPUsPerReplica")
	}

	gP, gD := int64(profile.Prefill.Topology.GPUsPerReplica), int64(profile.Decode.Topology.GPUsPerReplica)
	uP, uD := int64(unitVector[roles.prefill.Name]), int64(unitVector[roles.decode.Name])
	unitGPUs := uP*gP + uD*gD
	if unitGPUs <= 0 || unitGPUs > math.MaxInt32 {
		return nil, modelDeploymentScalerPriorRefusal("", "whole-unit GPU count exceeds supported int32 bounds")
	}

	cUnit := float64(pdScale) * resolution.EffectiveTokensPerSecond
	seedTokensPerSecond := float64(pdScale) * resolution.SeedTokensPerSecond
	cGPU := cUnit / float64(unitGPUs)

	prefillStatus, err := modelDeploymentScalerPopulateStatusFromResolution(
		roles.prefill.Name, catalog.Digest, resolution, quantile,
		seedTokensPerSecond, profile.Prefill.Topology.GPUsPerReplica, profile.Prefill.Replicas,
	)
	if err != nil {
		return nil, err
	}
	decodeStatus, err := modelDeploymentScalerPopulateStatusFromResolution(
		roles.decode.Name, catalog.Digest, resolution, quantile,
		seedTokensPerSecond, profile.Decode.Topology.GPUsPerReplica, profile.Decode.Replicas,
	)
	if err != nil {
		return nil, err
	}

	return modelDeploymentScalerBuildWholeUnitResult(
		scaler, target, roles, ModelDeploymentScalerPriorModeDisaggregated,
		unitVector, minUnits, maxUnits, gP, gD, unitGPUs, cGPU, cUnit,
		prefillStatus, decodeStatus,
		"all required capacities are resolved via catalog disaggregated whole-unit priors",
	)
}

func modelDeploymentScalerResolveAggregatedFallback(
	scaler *workercore.ModelDeploymentScaler, target *workercore.ModelDeployment,
	roles modelDeploymentScalerDisaggRoleSet,
	prefillDecl, decodeDecl modelDeploymentScalerPriorDeclaration,
	catalog *priors.Catalog, quantile float64,
	unitVector map[string]int32, minUnits, maxUnits int32,
) (*ModelDeploymentScalerCapacityPriors, error) {
	prefillEffective := scaler.Spec.Algorithm.DeepCopy()
	prefillRes, prefillStatus, err := modelDeploymentScalerResolveProfilePrior(
		scaler, target, roles.prefill, prefillEffective, catalog, prefillDecl, quantile, true,
	)
	if err != nil {
		return nil, err
	}
	decodeEffective := scaler.Spec.Algorithm.DeepCopy()
	decodeRes, decodeStatus, err := modelDeploymentScalerResolveProfilePrior(
		scaler, target, roles.decode, decodeEffective, catalog, decodeDecl, quantile, true,
	)
	if err != nil {
		return nil, err
	}
	if prefillRes.GPUsPerReplica <= 0 || decodeRes.GPUsPerReplica <= 0 {
		return nil, modelDeploymentScalerPriorRefusal("", "resolved aggregated profiles must carry positive GPUsPerReplica")
	}

	gP, gD := int64(prefillRes.GPUsPerReplica), int64(decodeRes.GPUsPerReplica)
	uP, uD := int64(unitVector[roles.prefill.Name]), int64(unitVector[roles.decode.Name])
	unitGPUs := uP*gP + uD*gD
	if unitGPUs <= 0 || unitGPUs > math.MaxInt32 {
		return nil, modelDeploymentScalerPriorRefusal("", "whole-unit GPU count exceeds supported int32 bounds")
	}

	cGPU := math.Min(
		prefillRes.EffectiveTokensPerSecond/float64(gP),
		decodeRes.EffectiveTokensPerSecond/float64(gD),
	)
	cUnit := math.Max(1, math.Floor(float64(unitGPUs)*cGPU))

	return modelDeploymentScalerBuildWholeUnitResult(
		scaler, target, roles, ModelDeploymentScalerPriorModeAggregatedFallback,
		unitVector, minUnits, maxUnits, gP, gD, unitGPUs, cGPU, cUnit,
		prefillStatus, decodeStatus,
		"all required capacities are resolved via whole-unit mode: aggregated fallback "+
			"(initial estimate assumes GPU-time-balanced stage sizing across the fixed unit ratio)",
	)
}

func modelDeploymentScalerBuildWholeUnitResult(
	scaler *workercore.ModelDeploymentScaler, target *workercore.ModelDeployment,
	roles modelDeploymentScalerDisaggRoleSet, mode ModelDeploymentScalerPriorResolutionMode,
	unitVector map[string]int32, minUnits, maxUnits int32,
	gP, gD, unitGPUs int64, cGPU, cUnit float64,
	prefillStatus, decodeStatus workercore.ModelDeploymentScalerResolvedCapacityPriorStatus,
	conditionMessage string,
) (*ModelDeploymentScalerCapacityPriors, error) {
	cUnitQuantity, err := modelDeploymentScalerPriorQuantity(cUnit)
	if err != nil {
		return nil, modelDeploymentScalerPriorRefusal("", "whole-unit capacity cannot be represented: %v", err)
	}

	unitFieldNames := []string{"tokenThroughput.unitTargetTokensPerSecond"}
	if scaler.Spec.Algorithm.Type == workercore.ModelDeploymentScalerAlgorithmTypePredictive {
		unitFieldNames = []string{
			"predictive.unitTokenCapacityPerReplica",
			"predictive.tokenThroughput.unitTargetTokensPerSecond",
		}
	}
	for _, fieldName := range unitFieldNames {
		entry := workercore.ModelDeploymentScalerResolvedCapacityFieldStatus{
			Field:                    fieldName,
			Source:                   workercore.ModelDeploymentScalerCapacityFieldSourceCatalog,
			EffectiveTokensPerSecond: cUnitQuantity.DeepCopy(),
		}
		prefillStatus.Fields = append(prefillStatus.Fields, entry)
		decodeStatus.Fields = append(decodeStatus.Fields, entry)
	}

	prefillEffective := scaler.Spec.Algorithm.DeepCopy()
	for _, field := range modelDeploymentScalerCapacityFields(prefillEffective, workercore.ModelDeploymentRoleKindPrefill) {
		if *field.value == nil {
			q := cUnitQuantity.DeepCopy()
			*field.value = &q
		}
	}
	decodeEffective := scaler.Spec.Algorithm.DeepCopy()
	for _, field := range modelDeploymentScalerCapacityFields(decodeEffective, workercore.ModelDeploymentRoleKindDecode) {
		if *field.value == nil {
			q := cUnitQuantity.DeepCopy()
			*field.value = &q
		}
	}

	wholeUnit := &ModelDeploymentScalerWholeUnitPrior{
		UnitVector:                unitVector,
		PrefillRole:               roles.prefill.Name,
		DecodeRole:                roles.decode.Name,
		PrefillGPUsPerReplica:     int32(gP),
		DecodeGPUsPerReplica:      int32(gD),
		UnitGPUs:                  int32(unitGPUs),
		MinUnits:                  minUnits,
		MaxUnits:                  maxUnits,
		PerGPUTokensPerSecond:     cGPU,
		UnitTargetTokensPerSecond: cUnit,
	}
	if roles.encode != nil {
		wholeUnit.EncodeRole = roles.encode.Name
	}

	jointDigest, err := modelDeploymentScalerWholeUnitFingerprint(
		mode, scaler, target, unitVector, minUnits, maxUnits, cUnit, prefillStatus, decodeStatus,
	)
	if err != nil {
		return nil, err
	}
	wholeUnit.JointCompatibilityDigest = jointDigest
	prefillStatus.CompatibilityFingerprint = jointDigest
	decodeStatus.CompatibilityFingerprint = jointDigest

	result := &ModelDeploymentScalerCapacityPriors{
		Algorithms: map[string]workercore.ModelDeploymentScalerAlgorithmSpec{
			roles.prefill.Name: *prefillEffective,
			roles.decode.Name:  *decodeEffective,
		},
		Status:           []workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{prefillStatus, decodeStatus},
		Mode:             mode,
		WholeUnit:        wholeUnit,
		ConditionMessage: conditionMessage,
	}
	if roles.encode != nil {
		result.Algorithms[roles.encode.Name] = *scaler.Spec.Algorithm.DeepCopy()
	}
	return result, nil
}

func gcdInt64(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func modelDeploymentScalerWholeUnitFingerprint(
	mode ModelDeploymentScalerPriorResolutionMode,
	scaler *workercore.ModelDeploymentScaler, target *workercore.ModelDeployment,
	unitVector map[string]int32, minUnits, maxUnits int32, unitTokens float64,
	prefillStatus, decodeStatus workercore.ModelDeploymentScalerResolvedCapacityPriorStatus,
) (string, error) {
	canonicalRoles := make([]workercore.ModelDeploymentRole, 0, len(target.Spec.Roles))
	for i := range target.Spec.Roles {
		roleCopy := target.Spec.Roles[i].DeepCopy()
		roleCopy.Replicas = 0
		canonicalRoles = append(canonicalRoles, *roleCopy)
	}
	annotations := make(map[string]string)
	for _, key := range []string{
		workercore.ModelDeploymentScalerModelIDAnnotationKey,
		workercore.ModelDeploymentScalerHardwareIDAnnotationKey,
		workercore.ModelDeploymentScalerPrecisionAnnotationKey,
		workercore.ModelDeploymentScalerPrefillHardwareIDAnnotationKey,
		workercore.ModelDeploymentScalerDecodeHardwareIDAnnotationKey,
	} {
		annotations[key] = target.Annotations[key]
	}
	return priors.Digest(struct {
		Mode        ModelDeploymentScalerPriorResolutionMode
		TargetUID   string
		UnitVector  map[string]int32
		MinUnits    int32
		MaxUnits    int32
		UnitTokens  float64
		Roles       []workercore.ModelDeploymentRole
		Model       workercore.ModelDeploymentModel
		Engine      workercore.ModelDeploymentEngine
		Router      *workercore.ModelDeploymentRouter
		KVCache     *workercore.ModelDeploymentKVCache
		KVTransfer  *workercore.ModelDeploymentKVTransfer
		Annotations map[string]string
		Algorithm   *workercore.ModelDeploymentScalerAlgorithmSpec
		Ratios      []workercore.ModelDeploymentScalerRoleRatio
		RoleBounds  []workercore.ModelDeploymentScalerRoleBounds
		Prefill     workercore.ModelDeploymentScalerResolvedCapacityPriorStatus
		Decode      workercore.ModelDeploymentScalerResolvedCapacityPriorStatus
	}{
		mode,
		string(target.UID), unitVector, minUnits, maxUnits, unitTokens,
		canonicalRoles, target.Spec.Model, target.Spec.Engine, target.Spec.Router,
		target.Spec.KVCache, target.Spec.KVTransfer, annotations,
		scaler.Spec.Algorithm.DeepCopy(), scaler.Spec.Ratios, scaler.Spec.Roles,
		prefillStatus, decodeStatus,
	})
}

func quantityToPositiveRat(q *resource.Quantity) (*big.Rat, error) {
	if q == nil {
		return nil, fmt.Errorf("missing ratio quantity")
	}
	rat, ok := new(big.Rat).SetString(q.AsDec().String())
	if !ok || rat.Sign() <= 0 {
		return nil, fmt.Errorf("ratio %q must be a positive rational number", q.String())
	}
	return rat, nil
}

func solveFixedWholeUnitVector(
	scaler *workercore.ModelDeploymentScaler, target *workercore.ModelDeployment,
) (map[string]int32, int32, int32, error) {
	roleNames := make(map[string]struct{}, len(target.Spec.Roles))
	orderedNames := make([]string, 0, len(target.Spec.Roles))
	for i := range target.Spec.Roles {
		name := target.Spec.Roles[i].Name
		if _, dup := roleNames[name]; dup {
			return nil, 0, 0, modelDeploymentScalerPriorRefusal(name, "duplicate role name in target deployment")
		}
		roleNames[name] = struct{}{}
		orderedNames = append(orderedNames, name)
	}
	if len(orderedNames) < 2 {
		return nil, 0, 0, modelDeploymentScalerPriorRefusal("", "fixed whole-unit vector requires at least two roles")
	}
	if len(scaler.Spec.Ratios) == 0 {
		return nil, 0, 0, modelDeploymentScalerPriorRefusal("", "Tier 2 whole-unit Server fallback requires fixed ratios covering all target roles")
	}

	bounds := make(map[string]workercore.ModelDeploymentScalerRoleBounds, len(scaler.Spec.Roles))
	for i := range scaler.Spec.Roles {
		b := scaler.Spec.Roles[i]
		bounds[b.Name] = b
	}
	for _, name := range orderedNames {
		b, ok := bounds[name]
		if !ok {
			return nil, 0, 0, modelDeploymentScalerPriorRefusal(name, "missing scaler role bounds for target role")
		}
		minR := max(b.MinReplicas, 1)
		if b.MaxReplicas < minR {
			return nil, 0, 0, modelDeploymentScalerPriorRefusal(name, "maxReplicas %d is less than minReplicas %d", b.MaxReplicas, minR)
		}
	}

	type ratioEdge struct {
		to     string
		factor *big.Rat
	}
	adj := make(map[string][]ratioEdge, len(orderedNames))
	for _, ratio := range scaler.Spec.Ratios {
		if _, ok := roleNames[ratio.NumeratorRole]; !ok {
			return nil, 0, 0, modelDeploymentScalerPriorRefusal(ratio.NumeratorRole, "ratio references unknown role")
		}
		if _, ok := roleNames[ratio.DenominatorRole]; !ok {
			return nil, 0, 0, modelDeploymentScalerPriorRefusal(ratio.DenominatorRole, "ratio references unknown role")
		}
		if ratio.Min == nil || ratio.Max == nil {
			return nil, 0, 0, modelDeploymentScalerPriorRefusal(
				"", "fixed-ratio constraints require min == max on %s/%s",
				ratio.NumeratorRole, ratio.DenominatorRole,
			)
		}
		minRat, err := quantityToPositiveRat(ratio.Min)
		if err != nil {
			return nil, 0, 0, modelDeploymentScalerPriorRefusal("", "%v", err)
		}
		maxRat, err := quantityToPositiveRat(ratio.Max)
		if err != nil {
			return nil, 0, 0, modelDeploymentScalerPriorRefusal("", "%v", err)
		}
		if minRat.Cmp(maxRat) != 0 {
			return nil, 0, 0, modelDeploymentScalerPriorRefusal(
				"", "Tier 2 whole-unit Server fallback requires fixed ratios (min == max) on %s/%s",
				ratio.NumeratorRole, ratio.DenominatorRole,
			)
		}
		if ratio.Target != nil {
			targetRat, err := quantityToPositiveRat(ratio.Target)
			if err != nil {
				return nil, 0, 0, modelDeploymentScalerPriorRefusal("", "%v", err)
			}
			if targetRat.Cmp(minRat) != 0 {
				return nil, 0, 0, modelDeploymentScalerPriorRefusal(
					"", "ratio target conflicts with fixed min == max on %s/%s",
					ratio.NumeratorRole, ratio.DenominatorRole,
				)
			}
		}
		invRat := new(big.Rat).Inv(minRat)
		adj[ratio.DenominatorRole] = append(adj[ratio.DenominatorRole], ratioEdge{to: ratio.NumeratorRole, factor: minRat})
		adj[ratio.NumeratorRole] = append(adj[ratio.NumeratorRole], ratioEdge{to: ratio.DenominatorRole, factor: invRat})
	}

	vals := map[string]*big.Rat{orderedNames[0]: big.NewRat(1, 1)}
	queue := []string{orderedNames[0]}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, edge := range adj[cur] {
			nextVal := new(big.Rat).Mul(vals[cur], edge.factor)
			if existing, seen := vals[edge.to]; seen {
				if existing.Cmp(nextVal) != 0 {
					return nil, 0, 0, modelDeploymentScalerPriorRefusal("", "fixed-ratio constraints contain an inconsistent cycle")
				}
				continue
			}
			vals[edge.to] = nextVal
			queue = append(queue, edge.to)
		}
	}
	if len(vals) != len(orderedNames) {
		return nil, 0, 0, modelDeploymentScalerPriorRefusal("", "fixed-ratio constraints must connect all target roles into one component")
	}

	sort.Strings(orderedNames)
	lcm := big.NewInt(1)
	gcdTmp := new(big.Int)
	for _, name := range orderedNames {
		d := vals[name].Denom()
		gcdTmp.GCD(nil, nil, lcm, d)
		lcm.Quo(lcm, gcdTmp)
		lcm.Mul(lcm, d)
	}

	scaled := make(map[string]*big.Int, len(orderedNames))
	var overallGCD *big.Int
	for _, name := range orderedNames {
		val := vals[name]
		term := new(big.Int).Quo(lcm, val.Denom())
		term.Mul(term, val.Num())
		scaled[name] = term
		if overallGCD == nil {
			overallGCD = new(big.Int).Set(term)
		} else {
			overallGCD.GCD(nil, nil, overallGCD, term)
		}
	}

	unit := make(map[string]int32, len(orderedNames))
	maxInt32 := big.NewInt(math.MaxInt32)
	for _, name := range orderedNames {
		reduced := new(big.Int).Quo(scaled[name], overallGCD)
		if reduced.Sign() <= 0 || reduced.Cmp(maxInt32) > 0 {
			return nil, 0, 0, modelDeploymentScalerPriorRefusal(name, "canonical unit vector exceeds supported int32 bounds")
		}
		unit[name] = int32(reduced.Int64())
	}

	kMin := int64(1)
	kMax := int64(math.MaxInt32)
	for _, name := range orderedNames {
		b := bounds[name]
		minR := int64(max(b.MinReplicas, 1))
		maxR := int64(b.MaxReplicas)
		u := int64(unit[name])
		ceilK := (minR + u - 1) / u
		floorK := maxR / u
		if ceilK > kMin {
			kMin = ceilK
		}
		if floorK < kMax {
			kMax = floorK
		}
	}
	if kMin > kMax || kMax <= 0 {
		return nil, 0, 0, modelDeploymentScalerPriorRefusal("", "no positive integer unit multiplier satisfies role replica bounds")
	}
	return unit, int32(kMin), int32(kMax), nil
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
