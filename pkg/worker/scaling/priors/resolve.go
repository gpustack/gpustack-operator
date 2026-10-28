package priors

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// DefaultHeadroom is the margin applied to a measured capacity when a request
// does not set one.
//
// IT IS AN ENGINEERING MARGIN, not a proof that the queue length or the
// replica count stays below a fixed multiple.
const DefaultHeadroom = 0.75

// Request is one capacity prior resolution request.
type Request struct {
	// Role is the serving role the capacity is for.
	Role RoleKind

	// Identity is the deployment identity the profile must match.
	Identity Identity

	// ServingConfig is the serving configuration the profile must match.
	ServingConfig ServingConfig

	// Topology is the shape of one scaling replica the profile must match.
	Topology Topology

	// Constraints are the latency targets and the headroom.
	Constraints Constraints
}

// Constraints are the requirements a resolved prior must satisfy.
type Constraints struct {
	// TTFTTargetMilliseconds is the time to first token target the role has
	// to keep under.
	TTFTTargetMilliseconds float64

	// ITLTargetMilliseconds is the inter-token latency target the role has
	// to keep under.
	ITLTargetMilliseconds float64

	// Headroom is the margin applied to a measured capacity.
	//
	// IT MUST BE IN (0,1]. IT DEFAULTS TO DefaultHeadroom WHEN IT IS NOT SET.
	Headroom float64

	// LatencyQuantile is the latency quantile the request is held to.
	//
	// IT IS OMITTED WHEN IT IS ZERO, and the resolver then requires
	// DefaultLatencyQuantile.
	//
	// IT MUST BE IN (0,1] WHEN IT IS SET, because a quantile outside that
	// range names no quantile.
	//
	// IT APPLIES TO EVERY REQUIRED LATENCY METRIC, because one request cannot
	// hold two metrics to two different quantiles.
	LatencyQuantile float64
}

// Resolution is a resolved capacity prior.
type Resolution struct {
	// Shape is the shape the value came from.
	Shape string

	// WorkloadRevision is the revision of the workload the measurement ran.
	WorkloadRevision string

	// GPUsPerReplica is how many GPUs one scaling replica of the measurement used.
	GPUsPerReplica int

	// MeasuredReplicas is how many replicas the measurement ran on.
	MeasuredReplicas int

	// SeedTokensPerSecond is the measured capacity of one replica.
	SeedTokensPerSecond float64

	// EffectiveTokensPerSecond is the value the capacity is used with.
	EffectiveTokensPerSecond float64

	// Basis records why the value was selected.
	Basis Basis
}

// Basis records why a capacity prior was selected.
//
// IT EXISTS SO A STATUS FIELD CAN SAY WHY A VALUE WAS CHOSEN OR REFUSED.
type Basis struct {
	// LatencyBounds describes the directly measured bounds on the selected seed point.
	LatencyBounds []LatencyBound

	// SourceID names the source the profile was imported from.
	SourceID string

	// SourceRevision is the pinned revision of that source.
	SourceRevision string

	// ProfileDigest is the digest of the profile the value came from.
	ProfileDigest string

	// Metric is the numerator the capacity was read from.
	Metric string

	// TokenCountConvention says which tokens the numerator counted.
	TokenCountConvention string

	// Quantile is the latency quantile the constraint was read from.
	//
	// IT IS THE QUANTILE THE SOURCE MEASURED and not the quantile the request
	// asked for. A source that measured a higher quantile bounds the requested
	// one from above.
	Quantile string

	// RequestedQuantile is the latency quantile the request was held to.
	//
	// IT IS THE QUANTILE THE CALLER ASKED FOR and not the quantile the source
	// measured, so a status field can say which was asked for and which was
	// measured.
	RequestedQuantile float64

	// Denominator says what the measured rate was counted over.
	Denominator string

	// WorkloadID is the workload envelope the measurement was taken under.
	WorkloadID string

	// Headroom is the margin that was applied.
	Headroom float64

	// Conservative says a higher quantile of the same metric was used as the
	// bound.
	Conservative bool

	// FeasiblePoints is how many measured load points satisfied the
	// constraints.
	FeasiblePoints int

	// Boundary says how the capacity boundary was established.
	Boundary string
}

// The ways a capacity boundary can be established.
const (
	// BoundaryHigherLoadFailure says a load point above the feasible boundary
	// failed the latency constraint.
	BoundaryHigherLoadFailure = "HigherLoadSLOFailure"

	// BoundaryExplicitSaturation says a feasible load point recorded an
	// explicit saturation measurement.
	BoundaryExplicitSaturation = "ExplicitSaturation"
)

// LatencyBound records the statistic used to check one latency metric on the selected seed point.
type LatencyBound struct {
	// Metric names the measured latency.
	Metric LatencyMetric
	// Quantile is the directly measured quantile used as the latency bound.
	Quantile float64
	// Conservative is true when a higher quantile bounds the required quantile.
	Conservative bool
}

// roleRequirement is what a role needs from a profile.
type roleRequirement struct {
	// metric is the throughput numerator the role divides its demand by.
	metric ThroughputMetric

	// convention is the token count convention the numerator must use.
	convention TokenCountConvention

	// latencies are the latency metrics the role has to keep under target.
	latencies []LatencyMetric
}

// anyMeasuredQuantile is the quantile the evidence check requires.
//
// IT EXISTS BECAUSE THE EVIDENCE CHECK MUST NOT DEPEND ON THE QUANTILE ONE
// REQUEST HAPPENS TO ASK FOR. A profile that measured a latency at any
// quantile carries the evidence, and the request that needs a higher quantile
// decides for itself whether the measurement reaches it.
const anyMeasuredQuantile = 0.0

// FormatLatencyQuantile renders a latency quantile so that reading it back
// gives the same number.
//
// IT KEEPS EVERY DIGIT THE VALUE CARRIES, because a status that writes
// 0.00001 as 0.0000 reports a quantile nobody asked for and a status that
// writes 0.97505 as 0.9750 reports another one.
func FormatLatencyQuantile(quantile float64) string {
	return strconv.FormatFloat(quantile, 'f', -1, 64)
}

// constraintOutcome is what a measured latency says about a target.
type constraintOutcome int

const (
	// constraintUndecided says the measurement cannot decide the target.
	//
	// IT IS THE OUTCOME OF A HIGHER QUANTILE THAT MISSED THE TARGET, because
	// a quantile above the requested one bounds it from above and cannot say
	// whether the requested quantile missed the target.
	constraintUndecided constraintOutcome = iota

	// constraintMet says the measurement proves the target is met.
	constraintMet

	// constraintMissed says the measurement proves the target is missed.
	//
	// IT REQUIRES THE REQUESTED QUANTILE ITSELF, because only the requested
	// quantile can prove that the requested quantile missed the target.
	constraintMissed
)

// requestedQuantile returns the latency quantile a request is held to.
//
// IT REFUSES A QUANTILE OUTSIDE (0,1], BECAUSE A QUANTILE OUTSIDE THAT RANGE
// NAMES NO QUANTILE, AND IT REFUSES A NAN OR AN INFINITY, BECAUSE NEITHER IS A
// QUANTILE.
func requestedQuantile(constraints Constraints) (float64, *RefusalError) {
	quantile := constraints.LatencyQuantile
	if quantile == 0 {
		return DefaultLatencyQuantile, nil
	}
	if math.IsNaN(quantile) || math.IsInf(quantile, 0) || quantile <= 0 || quantile > 1 {
		return 0, newRefusal(
			RefusalCapacityPriorUnresolved,
			"the latency quantile must be greater than zero and no greater than one",
			map[string]string{"latencyQuantile": fmt.Sprintf("%v", quantile)},
		)
	}
	return quantile, nil
}

// quantileIs says whether a measured quantile is the requested quantile.
//
// IT COMPARES THE TWO QUANTILES EXACTLY, because a quantile above the
// requested one is a different quantile however close it is. A measured
// quantile of 0.9500000005 is not the requested 0.95, and a source that
// measured it cannot prove that the requested quantile missed a target.
func quantileIs(measured, requested float64) bool {
	return measured == requested
}

// isConservative says whether a measured quantile bounds the requested one
// from above.
//
// IT IS FALSE FOR THE REQUESTED QUANTILE ITSELF, because a measurement at the
// requested quantile is the measurement and not a bound above it.
func isConservative(measured, requested float64) bool {
	return measured > requested
}

// worseOutcome returns the outcome that decides between two.
//
// IT RANKS A PROVEN FAILURE ABOVE AN UNDECIDED MEASUREMENT AND AN UNDECIDED
// MEASUREMENT ABOVE A PROVEN PASS, because one metric that provably missed the
// target decides the point and one metric that cannot decide does not overturn
// the others.
func worseOutcome(current, candidate constraintOutcome) constraintOutcome {
	switch {
	case current == constraintMissed:
		return current
	case candidate == constraintMissed:
		return candidate
	case current == constraintUndecided:
		return current
	default:
		return candidate
	}
}

// requirementFor returns what a role needs from a profile.
//
// IT IS EXHAUSTIVE ON PURPOSE, because a role the resolver does not know
// cannot be given a capacity.
func requirementFor(role RoleKind) (roleRequirement, bool) {
	switch role {
	case RoleKindServer:
		return roleRequirement{
			metric:     ThroughputMetricCacheMissedTotalTokensPerSecond,
			convention: TokenCountConventionCacheMissedInputPlusOutput,
			latencies:  []LatencyMetric{LatencyMetricTTFT, LatencyMetricITL},
		}, true
	case RoleKindPrefill:
		return roleRequirement{
			metric:     ThroughputMetricCacheMissedInputTokensPerSecond,
			convention: TokenCountConventionCacheMissedInput,
			latencies:  []LatencyMetric{LatencyMetricTTFT},
		}, true
	case RoleKindDecode:
		return roleRequirement{
			metric:     ThroughputMetricOutputTokensPerSecond,
			convention: TokenCountConventionOutput,
			latencies:  []LatencyMetric{LatencyMetricITL},
		}, true
	default:
		return roleRequirement{}, false
	}
}

// countsPromptTokens says whether the role's numerator counts prompt tokens.
//
// IT MATTERS FOR THE CACHE CONVENTION, because a numerator that counts prompt
// tokens only matches a measurement whose prompt tokens were cache misses.
func countsPromptTokens(role RoleKind) bool {
	return role == RoleKindServer || role == RoleKindPrefill
}

// loadAxisIncoherence returns the way a profile's load points disagree about
// what was varied, or the empty string when they agree.
//
// CONCURRENCY AND REQUEST RATE ARE NOT COMPARABLE NUMBERS, so the points of
// one profile must vary one load kind, one client model and one load unit,
// and the client model must be the arrival model the workload declares. A
// profile that mixes them has no load level a boundary could be read from.
func loadAxisIncoherence(profile Profile) string {
	if len(profile.Points) == 0 {
		return ""
	}
	first := profile.Points[0].Load
	for _, point := range profile.Points[1:] {
		load := point.Load
		if load.Kind != first.Kind {
			return fmt.Sprintf("the load points mix the %s and %s load kinds", first.Kind, load.Kind)
		}
		if load.ClientModel != first.ClientModel {
			return fmt.Sprintf("the load points mix the %s and %s client models", first.ClientModel, load.ClientModel)
		}
		if load.Unit != first.Unit {
			return fmt.Sprintf("the load points mix the %s and %s load units", first.Unit, load.Unit)
		}
	}
	if first.ClientModel != profile.Workload.ArrivalModel {
		return fmt.Sprintf(
			"the load points were offered with the %s client model while the workload declares %s",
			first.ClientModel, profile.Workload.ArrivalModel,
		)
	}
	return ""
}

// qualifiedPoints returns the eligible points that directly measure every
// latency statistic the role needs at the requested quantile or higher.
//
// IT IS THE ONE DEFINITION OF A POINT THAT CAN CARRY A CAPACITY, because
// CheckEligibility and Resolve have to agree on which points count toward
// load coverage, the feasible set and the boundary. A point that misses a
// statistic is a diagnostic: it stays in the profile's raw evidence but
// counts for nothing. A point that measures the same statistic twice with
// different values is reported as a conflict, which the caller refuses
// instead of filtering away.
//
// IT TAKES THE QUANTILE THE EVIDENCE HAS TO REACH. THE EVIDENCE CHECK PASSES
// anyMeasuredQuantile, because a profile that measured a latency at any
// quantile carries the evidence, and the resolution passes the quantile the
// request asked for.
func qualifiedPoints(
	requirement roleRequirement,
	eligible []pointEvaluation,
	requested float64,
) ([]pointEvaluation, bool) {
	qualified := make([]pointEvaluation, 0, len(eligible))
	conflict := false
	for _, evaluation := range eligible {
		counts := true
		for _, metric := range requirement.latencies {
			_, _, found, conflicting := boundStatistic(evaluation.point, metric, requested)
			if conflicting {
				conflict = true
				counts = false
				break
			}
			if !found {
				counts = false
			}
		}
		if counts {
			qualified = append(qualified, evaluation)
		}
	}
	return qualified, conflict
}

// Resolve returns the capacity prior for a request.
//
// IT REFUSES INSTEAD OF GUESSING, so a caller that cannot use the result
// learns which evidence was missing.
func (c *Catalog) Resolve(request Request) (Resolution, error) {
	if c == nil {
		return Resolution{}, newRefusal(
			RefusalCapacityPriorUnresolved,
			"no catalog is loaded",
			nil,
		)
	}

	headroom := request.Constraints.Headroom
	if headroom == 0 {
		headroom = DefaultHeadroom
	}
	if headroom <= 0 || headroom > 1 {
		return Resolution{}, newRefusal(
			RefusalCapacityPriorUnresolved,
			"headroom must be greater than zero and no greater than one",
			map[string]string{"headroom": fmt.Sprintf("%v", headroom)},
		)
	}

	quantile, refusal := requestedQuantile(request.Constraints)
	if refusal != nil {
		return Resolution{}, refusal
	}

	profile, refusal := c.selectProfile(request)
	if refusal != nil {
		return Resolution{}, refusal
	}

	if mismatch := profileMismatches(profile, request); mismatch != "" {
		return Resolution{}, newRefusal(
			RefusalCapacityPriorUnresolved,
			fmt.Sprintf("profile %s does not match the deployment: %s", profile.Shape(), mismatch),
			map[string]string{"shape": profile.Shape().String(), "mismatch": mismatch},
		)
	}

	resolution, refusal := resolveProfile(c, profile, request, headroom, quantile)
	if refusal != nil {
		return Resolution{}, refusal
	}
	return resolution, nil
}

// Select returns the profile a shape names.
//
// IT REFUSES WHEN NO PROFILE MATCHES AND WHEN SEVERAL PROFILES MATCH, because a
// deployment that matches two measurements has no evidence about which one
// applies.
func (c *Catalog) Select(shape Shape) (Profile, *RefusalError) {
	return c.selectProfile(Request{
		Role: shape.Role,
		Identity: Identity{
			Model:    ModelIdentity{Name: shape.Model, Quantization: shape.Quantization},
			Engine:   EngineIdentity{Image: shape.Engine},
			Hardware: HardwareIdentity{Accelerator: shape.Hardware},
		},
		Topology: shape.Topology,
	})
}

// selectProfile finds the one profile a request may use.
//
// IT MATCHES THE SIX-TUPLE, because the shape is what a profile is known by and
// a deployment states its shape before it states anything else about the
// catalog.
//
// IT REFUSES WHEN SEVERAL PROFILES MATCH, because a deployment that matches two
// measurements has no evidence about which one applies.
func (c *Catalog) selectProfile(request Request) (Profile, *RefusalError) {
	want := request.Shape()
	var matched []Profile
	for _, profile := range c.Profiles {
		if shapeMatches(want, profile.Shape()) {
			matched = append(matched, profile)
		}
	}

	switch len(matched) {
	case 0:
		return Profile{}, newRefusal(
			RefusalCapacityPriorUnresolved,
			fmt.Sprintf("no catalog profile matches the deployment shape %s", want),
			map[string]string{"role": string(request.Role), "shape": want.String()},
		)
	case 1:
		return matched[0], nil
	default:
		shapes := make([]string, 0, len(matched))
		for _, profile := range matched {
			shapes = append(shapes, profile.Shape().String())
		}
		sort.Strings(shapes)
		return Profile{}, newRefusal(
			RefusalCapacityPriorUnresolved,
			"several catalog profiles match the deployment shape",
			map[string]string{"shapes": strings.Join(shapes, ",")},
		)
	}
}

// profileMismatches returns the fields a profile and a request disagree on.
//
// IT IS EMPTY WHEN THEY AGREE.
func profileMismatches(profile Profile, request Request) string {
	mismatched := make([]string, 0, 12)
	mismatched = append(mismatched, identityMismatch(profile.Identity, request.Identity)...)
	mismatched = append(mismatched, servingConfigMismatch(profile.ServingConfig, request.ServingConfig)...)
	mismatched = append(mismatched, topologyMismatch(profile.Topology.Normalized(), request.Topology.Normalized())...)
	return formatMismatch(mismatched)
}

// CheckEligibility reports why a profile cannot carry a capacity.
//
// IT DOES NOT DEPEND ON THE LATENCY TARGETS, because those belong to a
// deployment and not to a measurement. A GENERATOR USES IT TO KEEP A CATALOG
// FREE OF PROFILES THAT COULD NEVER RESOLVE.
//
// IT ALSO DOES NOT DEPEND ON THE LATENCY QUANTILE, because the quantile
// belongs to a request and not to a measurement. A PROFILE THAT MEASURED A
// LATENCY AT A QUANTILE BELOW THE DEFAULT STILL CARRIES THE EVIDENCE, AND
// A REQUEST THAT NEEDS A HIGHER QUANTILE DECIDES FOR ITSELF WHETHER THE
// MEASUREMENT REACHES IT.
//
// IT REPORTS EVERY GAP IT FOUND, because a profile that misses one piece of
// evidence usually misses more than one and the caller needs the whole list
// to know what to measure next.
func CheckEligibility(profile Profile) *RefusalError {
	requirement, known := requirementFor(profile.Role)
	if !known {
		return newRefusal(
			RefusalCapacityPriorUnresolved,
			fmt.Sprintf("the %s role needs no capacity prior", profile.Role),
			map[string]string{"role": string(profile.Role), "shape": profile.Shape().String()},
		)
	}

	detail := map[string]string{"shape": profile.Shape().String()}
	var findings []string

	// THE ENVIRONMENT IS NOT REQUIRED TO BE COMPLETE, because a public source
	// rarely captures the whole container environment and the environment a
	// source did not observe says nothing about the variables it did not see.
	// THE DEPLOYMENT IS STILL MATCHED AGAINST THE ENVIRONMENT THE MEASUREMENT
	// DID RECORD, so the variables the record carries are still compared.

	if incoherent := loadAxisIncoherence(profile); incoherent != "" {
		return newRefusal(
			RefusalIncompatibleMetric,
			fmt.Sprintf("profile %s %s", profile.Shape(), incoherent),
			detail,
		)
	}

	for _, point := range profile.Points {
		if point.Throughput.Metric != requirement.metric {
			findings = append(findings, fmt.Sprintf("measures %s instead of %s", point.Throughput.Metric, requirement.metric))
			detail["numerator"] = string(point.Throughput.Metric)
			break
		}
	}
	if detail["numerator"] == "" {
		for _, point := range profile.Points {
			if point.Throughput.TokenCountConvention != requirement.convention {
				findings = append(findings, fmt.Sprintf("counts %s instead of %s", point.Throughput.TokenCountConvention, requirement.convention))
				detail["tokenCountConvention"] = string(point.Throughput.TokenCountConvention)
				break
			}
		}
	}

	eligible := eligiblePoints(profile)
	if len(eligible) == 0 {
		findings = append(findings, fmt.Sprintf("measures no load point with the %s numerator", requirement.metric))
		detail["eligiblePoints"] = "0"
	} else {
		qualified, conflict := qualifiedPoints(requirement, eligible, anyMeasuredQuantile)
		if conflict {
			findings = append(findings, "measures the same latency statistic twice with different values")
			detail["conflict"] = "true"
		}
		for _, metric := range requirement.latencies {
			if !measuresStatistic(eligible, metric, anyMeasuredQuantile) {
				findings = append(findings, fmt.Sprintf("does not measure the %s latency", metric))
				detail["missingMetric"] = string(metric)
				break
			}
		}
		if countsPromptTokens(profile.Role) && profile.Workload.CacheBehavior != CacheBehaviorAllMisses {
			findings = append(findings, "does not establish that its prompt tokens were cache misses")
			detail["cacheBehavior"] = string(profile.Workload.CacheBehavior)
		}
		levels := distinctLoadLevels(qualified)
		if len(levels) < 2 {
			findings = append(findings, "has fewer than two distinct qualified measured load levels")
			detail["loadLevels"] = fmt.Sprintf("%d", len(levels))
		}
	}

	if len(findings) == 0 {
		return nil
	}

	reason := RefusalIncompatibleMetric
	if detail["numerator"] == "" && detail["tokenCountConvention"] == "" &&
		detail["missingMetric"] == "" && detail["cacheBehavior"] == "" && detail["conflict"] == "" {
		reason = RefusalInsufficientLoadCoverage
	}
	return newRefusal(reason, fmt.Sprintf("profile %s %s", profile.Shape(), findings[0]), detail)
}

// pointEvaluation is what the resolver worked out about one measured point.
type pointEvaluation struct {
	// point is the measured point.
	point MeasuredPoint

	// eligible says the point measures what the role needs.
	eligible bool

	// perReplica is the point's throughput reconstructed for one replica.
	perReplica float64

	// reason says why the point is not eligible.
	reason string
}

// eligiblePoints returns the points of a profile that measure what the role
// needs.
func eligiblePoints(profile Profile) []pointEvaluation {
	evaluations := make([]pointEvaluation, 0, len(profile.Points))
	for _, point := range profile.Points {
		evaluation := evaluatePoint(profile, point)
		if evaluation.eligible {
			evaluations = append(evaluations, evaluation)
		}
	}
	return evaluations
}

// evaluatePoint works out whether a measured point can carry a capacity.
//
// THE NUMERATOR AND THE TOKEN COUNT CONVENTION ARE NOT CHECKED HERE, because
// they have to agree across the whole profile and are checked before the
// points are read.
func evaluatePoint(profile Profile, point MeasuredPoint) pointEvaluation {
	evaluation := pointEvaluation{point: point}

	if point.Throughput.Unit != UnitTokensPerSecond {
		evaluation.reason = fmt.Sprintf("measured %s instead of %s", point.Throughput.Unit, UnitTokensPerSecond)
		return evaluation
	}
	if !finitePositive(point.Throughput.Value) {
		evaluation.reason = "measured no finite positive throughput"
		return evaluation
	}
	if !finitePositive(point.Load.Value) {
		evaluation.reason = "measured no finite positive load"
		return evaluation
	}

	perReplica, ok := perReplicaTokensPerSecond(point.Throughput, profile)
	if !ok {
		evaluation.reason = fmt.Sprintf("cannot reconstruct one replica from a %s denominator", point.Throughput.Denominator)
		return evaluation
	}

	evaluation.perReplica = perReplica
	evaluation.eligible = true
	return evaluation
}

// resolveProfile derives a capacity from one profile.
func resolveProfile(
	catalog *Catalog,
	profile Profile,
	request Request,
	headroom float64,
	requested float64,
) (Resolution, *RefusalError) {
	requirement, _ := requirementFor(profile.Role)

	if refusal := CheckEligibility(profile); refusal != nil {
		return Resolution{}, refusal
	}

	targets, refusal := latencyTargets(request, requirement)
	if refusal != nil {
		return Resolution{}, refusal
	}

	eligible := eligiblePoints(profile)

	qualified, conflict := qualifiedPoints(requirement, eligible, requested)
	if conflict {
		return Resolution{}, newRefusal(
			RefusalIncompatibleMetric,
			fmt.Sprintf("profile %s measured the same latency statistic twice with different values", profile.Shape()),
			map[string]string{"shape": profile.Shape().String()},
		)
	}

	for _, metric := range requirement.latencies {
		if !measuresStatistic(eligible, metric, requested) {
			return Resolution{}, newRefusal(
				RefusalIncompatibleMetric,
				fmt.Sprintf("profile %s does not measure the %s latency at the requested quantile", profile.Shape(), metric),
				map[string]string{
					"shape":             profile.Shape().String(),
					"missingMetric":     string(metric),
					"requestedQuantile": fmt.Sprintf("%g", requested),
				},
			)
		}
	}

	levels := distinctLoadLevels(qualified)
	if len(levels) < 2 {
		return Resolution{}, newRefusal(
			RefusalInsufficientLoadCoverage,
			fmt.Sprintf("profile %s has fewer than two distinct qualified measured load levels", profile.Shape()),
			map[string]string{
				"shape":             profile.Shape().String(),
				"loadLevels":        fmt.Sprintf("%d", len(levels)),
				"requestedQuantile": fmt.Sprintf("%g", requested),
			},
		)
	}

	feasible := make([]pointEvaluation, 0, len(qualified))
	failed := make([]pointEvaluation, 0, len(qualified))
	for _, evaluation := range qualified {
		outcome, _, _ := satisfiesConstraints(evaluation.point, requirement, targets, requested)
		switch outcome {
		case constraintMet:
			feasible = append(feasible, evaluation)
		case constraintMissed:
			failed = append(failed, evaluation)
		}
	}

	if len(feasible) == 0 {
		if len(failed) == 0 {
			// NO POINT COULD BE DECIDED, because every measurement that
			// reached the requested quantile came from a higher one that
			// missed the target.
			return Resolution{}, newRefusal(
				RefusalCapacityPriorUnresolved,
				fmt.Sprintf("profile %s cannot decide the latency targets at the requested quantile", profile.Shape()),
				map[string]string{
					"shape":             profile.Shape().String(),
					"requestedQuantile": fmt.Sprintf("%g", requested),
				},
			)
		}
		return Resolution{}, newRefusal(
			RefusalCapacityPriorUnresolved,
			fmt.Sprintf("profile %s has no measured load point that satisfies the latency targets", profile.Shape()),
			map[string]string{
				"shape":             profile.Shape().String(),
				"requestedQuantile": fmt.Sprintf("%g", requested),
			},
		)
	}

	boundary, found := capacityBoundary(feasible, failed)
	if !found {
		return Resolution{}, newRefusal(
			RefusalInsufficientLoadCoverage,
			fmt.Sprintf("profile %s has no evidence of the capacity boundary", profile.Shape()),
			map[string]string{"shape": profile.Shape().String()},
		)
	}

	seed := 0.0
	seedPoint := MeasuredPoint{}
	for _, evaluation := range feasible {
		if evaluation.perReplica > seed {
			seed = evaluation.perReplica
			seedPoint = evaluation.point
		}
	}

	basis := Basis{
		Metric:               string(seedPoint.Throughput.Metric),
		TokenCountConvention: string(seedPoint.Throughput.TokenCountConvention),
		Denominator:          string(seedPoint.Throughput.Denominator),
		WorkloadID:           profile.Workload.ID,
		Headroom:             headroom,
		FeasiblePoints:       len(feasible),
		Boundary:             boundary,
		ProfileDigest:        profile.Digest,
		RequestedQuantile:    requested,
	}
	highestQuantile := 0.0
	for _, metric := range requirement.latencies {
		_, quantile, _, _ := boundStatistic(seedPoint, metric, requested)
		highestQuantile = max(highestQuantile, quantile)
		basis.LatencyBounds = append(basis.LatencyBounds, LatencyBound{
			Metric: metric, Quantile: quantile, Conservative: isConservative(quantile, requested),
		})
	}
	if highestQuantile > 0 {
		basis.Quantile = FormatLatencyQuantile(highestQuantile)
		basis.Conservative = isConservative(highestQuantile, requested)
	}
	for _, source := range catalog.Sources {
		if source.ID == profile.SourceID {
			basis.SourceID = source.ID
			basis.SourceRevision = source.Revision
			break
		}
	}

	return Resolution{
		Shape:                    profile.Shape().String(),
		WorkloadRevision:         profile.Workload.Revision,
		GPUsPerReplica:           profile.Topology.GPUsPerReplica,
		MeasuredReplicas:         profile.MeasuredReplicas,
		SeedTokensPerSecond:      seed,
		EffectiveTokensPerSecond: headroom * seed,
		Basis:                    basis,
	}, nil
}

// latencyTargets returns the target each required latency metric is held to.
//
// IT REFUSES WHEN A TARGET IS MISSING, because a role that relies on a
// catalog capacity has to declare the latency it has to keep under.
//
// EVERY REQUIRED METRIC IS HELD TO THE SAME QUANTILE, because one request
// cannot hold two metrics to two different quantiles, and the quantile is
// chosen once by the caller.
func latencyTargets(request Request, requirement roleRequirement) (map[LatencyMetric]float64, *RefusalError) {
	targets := map[LatencyMetric]float64{}
	for _, metric := range requirement.latencies {
		target := 0.0
		switch metric {
		case LatencyMetricTTFT:
			target = request.Constraints.TTFTTargetMilliseconds
		case LatencyMetricITL:
			target = request.Constraints.ITLTargetMilliseconds
		}
		if target <= 0 {
			return nil, newRefusal(
				RefusalCapacityPriorUnresolved,
				fmt.Sprintf("a role that relies on a catalog capacity requires the %s latency target", metric),
				map[string]string{"missingTarget": string(metric)},
			)
		}
		targets[metric] = target
	}
	return targets, nil
}

// perReplicaTokensPerSecond reconstructs a measured rate for one replica.
//
// THE DENOMINATOR DECIDES THE RECONSTRUCTION, because the source states what
// it counted over and the resolver must not pick a multiplier.
func perReplicaTokensPerSecond(throughput Throughput, profile Profile) (float64, bool) {
	switch throughput.Denominator {
	case DenominatorPerGPU:
		if profile.Topology.GPUsPerReplica <= 0 {
			return 0, false
		}
		return throughput.Value * float64(profile.Topology.GPUsPerReplica), true
	case DenominatorPerReplica:
		return throughput.Value, true
	case DenominatorWholeSystem:
		if profile.MeasuredReplicas <= 0 {
			return 0, false
		}
		return throughput.Value / float64(profile.MeasuredReplicas), true
	default:
		return 0, false
	}
}

// satisfiesConstraints checks a qualified point against the latency targets.
//
// IT ONLY SEES QUALIFIED POINTS, because qualification already established
// that every required statistic is directly measured at the requested quantile
// or higher without contradiction. A point that is not satisfied is infeasible;
// it is never a missing measurement.
//
// IT SEPARATES A PROVEN FAILURE FROM AN UNDECIDED MEASUREMENT. A MEASUREMENT
// AT THE REQUESTED QUANTILE THAT MISSED THE TARGET PROVES THE FAILURE, AND A
// MEASUREMENT AT A HIGHER QUANTILE THAT MISSED THE TARGET CANNOT, BECAUSE A
// HIGHER QUANTILE BOUNDS THE REQUESTED ONE FROM ABOVE AND SAYS NOTHING ABOUT
// WHETHER THE REQUESTED ONE MISSED THE TARGET.
//
// THE RECORDED SLO OUTCOME OF A POINT IS NOT READ HERE, because it records
// NEITHER THE QUANTILE IT WAS JUDGED AT NOR THE TARGET IT WAS JUDGED
// AGAINST, and a verdict that does not name its own terms cannot decide a
// different target.
func satisfiesConstraints(
	point MeasuredPoint,
	requirement roleRequirement,
	targets map[LatencyMetric]float64,
	requested float64,
) (outcome constraintOutcome, bound float64, conservative bool) {
	outcome = constraintMet
	for _, metric := range requirement.latencies {
		value, quantile, found, conflicting := boundStatistic(point, metric, requested)
		if !found || conflicting {
			// Unreachable for a qualified point; kept so the loop stays
			// honest if qualification ever changes.
			outcome = worseOutcome(outcome, constraintUndecided)
			continue
		}
		if quantile > bound {
			bound = quantile
		}
		if isConservative(quantile, requested) {
			conservative = true
		}
		if value <= targets[metric] {
			continue
		}
		if quantileIs(quantile, requested) {
			// THE REQUESTED QUANTILE ITSELF MISSED THE TARGET, so the
			// measurement proves the failure.
			outcome = worseOutcome(outcome, constraintMissed)
			continue
		}
		// A HIGHER QUANTILE MISSED THE TARGET, WHICH DOES NOT PROVE THAT THE
		// REQUESTED QUANTILE MISSED IT.
		outcome = worseOutcome(outcome, constraintUndecided)
	}
	return outcome, bound, conservative
}

// boundStatistic returns the value that bounds a latency metric at the
// requested quantile.
//
// IT PICKS THE MEASUREMENT AT THE REQUESTED QUANTILE WHEN THE SOURCE MEASURED
// IT, because that is the measurement and not a bound above it.
//
// IT INTERPOLATES BETWEEN TWO MEASURED QUANTILES WHEN THE SOURCE MEASURED
// NONE AT THE REQUESTED ONE AND MEASURED ONE BELOW IT AND ONE ABOVE IT,
// because a source that published a curve at several quantiles states the
// value between them as well. THE INTERPOLATED VALUE IS REPORTED AT THE
// REQUESTED QUANTILE, so it decides a target in both directions instead of
// leaving the measurement undecided.
//
// IT PICKS THE LOWEST MEASURED QUANTILE THAT IS ABOVE THE REQUESTED ONE WHEN
// THERE IS NO MEASUREMENT BELOW IT, because that is the tightest bound the
// source measured.
//
// IT REPORTS A CONFLICT WHEN THE SOURCE MEASURED THE SAME STATISTIC AT THE
// SAME QUANTILE TWICE WITH DIFFERENT VALUES, because a source that contradicts
// itself cannot decide a constraint.
//
// IT REFUSES TO INTERPOLATE BETWEEN A LOWER AND A HIGHER QUANTILE THAT ARE
// OUT OF ORDER, because a source whose curve falls between two of its own
// measurements cannot state the value in between.
func boundStatistic(
	point MeasuredPoint,
	metric LatencyMetric,
	requested float64,
) (value, quantile float64, found, conflict bool) {
	for i, latency := range point.Latencies {
		if latency.Metric != metric {
			continue
		}
		for _, other := range point.Latencies[i+1:] {
			if other.Metric != metric {
				continue
			}
			if quantileIs(other.Quantile, latency.Quantile) && other.Value != latency.Value {
				return 0, 0, false, true
			}
		}
	}
	for _, latency := range point.Latencies {
		if latency.Metric != metric {
			continue
		}
		if quantileIs(latency.Quantile, requested) {
			return latency.Value, latency.Quantile, true, false
		}
	}
	lower, higher := bracketingQuantiles(point, metric, requested)
	if lower != nil && higher != nil && lower.Value <= higher.Value {
		interpolated := lower.Value + (requested-lower.Quantile)/(higher.Quantile-lower.Quantile)*(higher.Value-lower.Value)
		return interpolated, requested, true, false
	}
	for _, latency := range point.Latencies {
		if latency.Metric != metric {
			continue
		}
		if latency.Quantile <= 0 || latency.Quantile < requested {
			continue
		}
		if !found || latency.Quantile < quantile {
			found = true
			value = latency.Value
			quantile = latency.Quantile
		}
	}
	return value, quantile, found, false
}

// bracketingQuantiles returns the measurements that bound the requested
// quantile from below and from above.
//
// THE LOWER MEASUREMENT IS THE LARGEST ONE BELOW THE REQUESTED QUANTILE AND
// THE HIGHER ONE IS THE SMALLEST ONE ABOVE IT, because those are the two the
// requested quantile lies between.
//
// IT RETURNS NOTHING FOR A QUANTILE THAT IS NOT MEASURED, because a
// measurement at one side alone bounds the requested quantile from above and
// not from both sides.
func bracketingQuantiles(
	point MeasuredPoint, metric LatencyMetric, requested float64,
) (lower, higher *Latency) {
	for i := range point.Latencies {
		latency := &point.Latencies[i]
		if latency.Metric != metric || latency.Quantile <= 0 {
			continue
		}
		switch {
		case latency.Quantile < requested:
			if lower == nil || latency.Quantile > lower.Quantile {
				lower = latency
			}
		case latency.Quantile > requested:
			if higher == nil || latency.Quantile < higher.Quantile {
				higher = latency
			}
		}
	}
	return lower, higher
}

// measuresStatistic says whether any point measured a latency metric at the
// requested quantile or higher.
//
// IT IS GIVEN anyMeasuredQuantile BY THE EVIDENCE CHECK, because the evidence
// a profile carries must not depend on the quantile one request happens to
// ask for.
func measuresStatistic(eligible []pointEvaluation, metric LatencyMetric, requested float64) bool {
	for _, evaluation := range eligible {
		if _, _, found, conflict := boundStatistic(evaluation.point, metric, requested); found && !conflict {
			return true
		}
	}
	return false
}

// capacityBoundary reports how the capacity boundary was established.
//
// IT ACCEPTS A LOAD POINT ABOVE THE FEASIBLE BOUNDARY THAT PROVABLY FAILED
// THE CONSTRAINT, OR A FEASIBLE POINT THAT RECORDED AN EXPLICIT SATURATION
// MEASUREMENT. A POINT THAT COULD NOT BE DECIDED CANNOT ESTABLISH THE
// BOUNDARY, BECAUSE AN UNDECIDED MEASUREMENT IS NOT A FAILURE.
//
// IT ONLY READS QUALIFIED POINTS, because a diagnostic can be neither feasible
// nor a failed load level.
func capacityBoundary(feasible, failed []pointEvaluation) (string, bool) {
	highestFeasible := 0.0
	for _, evaluation := range feasible {
		if evaluation.point.Load.Value > highestFeasible {
			highestFeasible = evaluation.point.Load.Value
		}
	}

	for _, evaluation := range failed {
		if evaluation.point.Load.Value <= highestFeasible {
			continue
		}
		return BoundaryHigherLoadFailure, true
	}

	for _, evaluation := range feasible {
		if evaluation.point.Saturation != nil && evaluation.point.Saturation.Measured {
			return BoundaryExplicitSaturation, true
		}
	}
	return "", false
}

// distinctLoadLevels returns the measured load levels of a set of points.
func distinctLoadLevels(eligible []pointEvaluation) []string {
	levels := map[string]struct{}{}
	for _, evaluation := range eligible {
		levels[loadLevelKey(evaluation.point.Load)] = struct{}{}
	}
	keys := make([]string, 0, len(levels))
	for key := range levels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// loadLevelKey names a measured load level.
func loadLevelKey(load LoadLevel) string {
	return fmt.Sprintf("%s/%g", load.Kind, load.Value)
}

// finitePositive says whether a value is a finite number above zero.
func finitePositive(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0
}
