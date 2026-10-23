package worker

import (
	"math"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

const (
	modelDeploymentScalerDefaultScaleDownWindow = 5 * time.Minute
	modelDeploymentScalerDefaultDecodeGrace     = 2 * time.Minute
	modelDeploymentScalerRatioEpsilon           = 1e-9
	modelDeploymentScalerConstraintSearchBudget = 1 << 14
)

// ModelDeploymentScalerRoleState supplies the current and unconstrained state for one role.
type ModelDeploymentScalerRoleState struct {
	Name                         string
	Kind                         algorithm.RoleKind
	CurrentReplicas              int32
	RawRecommendedReplicas       int32
	HoldScaleDown                bool
	CompleteCoverage             bool
	HasPendingUnadmittedWorkload bool
	LastScaleUpTime              time.Time
	StabilizationHistory         []v1alpha1.ModelDeploymentScalerStabilizationSample
}

// ModelDeploymentScalerConstraintInput is the input to the deterministic constraint solver.
type ModelDeploymentScalerConstraintInput struct {
	Spec                v1alpha1.ModelDeploymentScalerSpec
	Roles               []ModelDeploymentScalerRoleState
	HasActiveRetirement bool
	InAbortCooldown     bool
	CooldownUntil       time.Time
	Now                 time.Time
}

// ModelDeploymentScalerReplicaInterval is the paced interval for one role.
type ModelDeploymentScalerReplicaInterval struct {
	Floor int32
	Ceil  int32
}

// ModelDeploymentScalerConstraintResult contains the projected vector and its explanation.
type ModelDeploymentScalerConstraintResult struct {
	Replicas             map[string]int32
	Reasons              map[string]string
	Intervals            map[string]ModelDeploymentScalerReplicaInterval
	StabilizationHistory map[string][]v1alpha1.ModelDeploymentScalerStabilizationSample
	BlockingReason       string
}

// SolveModelDeploymentScalerConstraints applies bounds, pacing, guards, and ratios atomically.
func SolveModelDeploymentScalerConstraints(input ModelDeploymentScalerConstraintInput) ModelDeploymentScalerConstraintResult {
	result := ModelDeploymentScalerConstraintResult{
		Replicas:             make(map[string]int32, len(input.Roles)),
		Reasons:              make(map[string]string, len(input.Roles)),
		Intervals:            make(map[string]ModelDeploymentScalerReplicaInterval, len(input.Roles)),
		StabilizationHistory: make(map[string][]v1alpha1.ModelDeploymentScalerStabilizationSample, len(input.Roles)),
	}
	if input.Now.IsZero() {
		input.Now = time.Now()
	}

	states := make(map[string]ModelDeploymentScalerRoleState, len(input.Roles))
	bounds := make(map[string]v1alpha1.ModelDeploymentScalerRoleBounds, len(input.Spec.Roles))
	for _, bound := range input.Spec.Roles {
		bounds[bound.Name] = bound
	}
	for _, role := range input.Roles {
		states[role.Name] = role
		result.Replicas[role.Name] = role.CurrentReplicas
		history := append([]v1alpha1.ModelDeploymentScalerStabilizationSample(nil), role.StabilizationHistory...)
		history = append(history, v1alpha1.ModelDeploymentScalerStabilizationSample{
			Replicas:  role.RawRecommendedReplicas,
			Timestamp: metav1Time(input.Now),
		})
		history = pruneStabilizationHistory(history, input.Now, largestStabilizationWindow(input.Spec))
		result.StabilizationHistory[role.Name] = history
		bound, ok := bounds[role.Name]
		if !ok {
			result.BlockingReason = v1alpha1.ModelDeploymentScalerReasonBoundsConflict
			result.Reasons[role.Name] = result.BlockingReason
			continue
		}
		floor, ceil := roleInterval(input, role, bound, history)
		result.Intervals[role.Name] = ModelDeploymentScalerReplicaInterval{Floor: floor, Ceil: ceil}
	}
	if result.BlockingReason == v1alpha1.ModelDeploymentScalerReasonBoundsConflict {
		return result
	}

	roleNames := make([]string, 0, len(input.Roles))
	for _, role := range input.Roles {
		roleNames = append(roleNames, role.Name)
		if interval := result.Intervals[role.Name]; interval.Floor > interval.Ceil {
			result.BlockingReason = v1alpha1.ModelDeploymentScalerReasonBoundsConflict
			for _, name := range roleNames {
				result.Reasons[name] = result.BlockingReason
			}
			return result
		}
	}
	sort.Strings(roleNames)

	graph := ratioGraph(input.Spec.Ratios)
	seen := make(map[string]bool, len(roleNames))
	for _, name := range roleNames {
		if seen[name] {
			continue
		}
		component := connectedComponent(name, graph, seen)
		if len(component) == 1 && len(graph[name]) == 0 {
			interval := result.Intervals[name]
			target := clamp(states[name].RawRecommendedReplicas, interval.Floor, interval.Ceil)
			result.Replicas[name] = target
			continue
		}
		projectComponent(input, component, states, result)
	}
	if result.BlockingReason == "" {
		for _, reason := range result.Reasons {
			if reason == v1alpha1.ModelDeploymentScalerReasonRatioConflict || reason == v1alpha1.ModelDeploymentScalerReasonPacingBounded {
				result.BlockingReason = reason
				break
			}
		}
	}
	if result.BlockingReason == "" {
		for _, role := range input.Roles {
			if role.RawRecommendedReplicas != role.CurrentReplicas && result.Replicas[role.Name] == role.CurrentReplicas {
				result.BlockingReason = v1alpha1.ModelDeploymentScalerReasonPacingBounded
				result.Reasons[role.Name] = result.BlockingReason
			}
		}
	}
	return result
}

// ConstrainModelDeploymentScaler is a short alias for SolveModelDeploymentScalerConstraints.
func ConstrainModelDeploymentScaler(input ModelDeploymentScalerConstraintInput) ModelDeploymentScalerConstraintResult {
	return SolveModelDeploymentScalerConstraints(input)
}

func roleInterval(
	input ModelDeploymentScalerConstraintInput,
	role ModelDeploymentScalerRoleState,
	bound v1alpha1.ModelDeploymentScalerRoleBounds,
	history []v1alpha1.ModelDeploymentScalerStabilizationSample,
) (int32, int32) {
	minimum := bound.MinReplicas
	if minimum <= 0 {
		minimum = 1
	}
	maximum := bound.MaxReplicas
	if maximum < minimum {
		return minimum, maximum
	}
	current := clamp(role.CurrentReplicas, minimum, maximum)

	var ceil int32
	up := input.Spec.Behavior.ScaleUp
	if up.StepPolicy != nil {
		ceil = current + stepAmount(current, *up.StepPolicy)
	} else {
		pods, percent := int32(0), int32(0)
		if up.MaxStepReplicas == 0 && up.MaxStepPercent == 0 {
			pods, percent = 4, 100
		} else {
			pods, percent = up.MaxStepReplicas, up.MaxStepPercent
		}
		ceil = current
		if pods > 0 {
			ceil = maxInt32(ceil, current+pods)
		}
		if percent > 0 {
			ceil = maxInt32(ceil, current+int32(math.Ceil(float64(current)*float64(percent)/100)))
		}
	}
	ceil = clamp(ceil, current, maximum)
	if role.HasPendingUnadmittedWorkload {
		ceil = minInt32(ceil, current)
	}
	upWindow := time.Duration(up.StabilizationWindowSeconds) * time.Second
	if up.StabilizationWindowSeconds > 0 && role.RawRecommendedReplicas > current &&
		!historySpans(history, input.Now, upWindow) {
		ceil = minInt32(ceil, current)
	}

	down := input.Spec.Behavior.ScaleDown
	floor := current - 1
	if down.StepPolicy != nil {
		floor = current - stepAmount(current, *down.StepPolicy)
	}
	floor = maxInt32(floor, minimum)
	window := time.Duration(down.StabilizationWindowSeconds) * time.Second
	if window <= 0 {
		window = modelDeploymentScalerDefaultScaleDownWindow
	}
	if !historySpans(history, input.Now, window) {
		floor = maxInt32(floor, current)
	} else if maxRecommendation, ok := rollingMaximum(history, input.Now, window); ok && maxRecommendation >= current {
		floor = maxInt32(floor, current)
	} else if ok {
		floor = maxInt32(floor, minInt32(current, maxRecommendation))
	}
	inCooldown := input.InAbortCooldown || (!input.CooldownUntil.IsZero() && input.Now.Before(input.CooldownUntil))
	if role.HoldScaleDown || !role.CompleteCoverage || input.HasActiveRetirement || inCooldown {
		floor = maxInt32(floor, current)
	}
	grace := time.Duration(down.DecodePostScaleUpGraceSeconds) * time.Second
	if grace <= 0 {
		grace = modelDeploymentScalerDefaultDecodeGrace
	}
	if role.Kind == algorithm.RoleKindDecode && !role.LastScaleUpTime.IsZero() && input.Now.Sub(role.LastScaleUpTime) < grace {
		floor = maxInt32(floor, current)
	}
	return clamp(floor, minimum, maximum), clamp(ceil, minimum, maximum)
}

func stepAmount(current int32, policy v1alpha1.ModelDeploymentScalerStepPolicy) int32 {
	if policy.Value <= 0 {
		return 0
	}
	if policy.Type == v1alpha1.ModelDeploymentScalerStepPolicyTypePercent {
		return int32(math.Ceil(float64(current) * float64(policy.Value) / 100))
	}
	return policy.Value
}

func projectComponent(
	input ModelDeploymentScalerConstraintInput,
	component []string,
	states map[string]ModelDeploymentScalerRoleState,
	result ModelDeploymentScalerConstraintResult,
) {
	sort.Strings(component)
	anyUp, anyDown := false, false
	for _, name := range component {
		if states[name].RawRecommendedReplicas > states[name].CurrentReplicas {
			anyUp = true
		}
		if states[name].RawRecommendedReplicas < states[name].CurrentReplicas {
			anyDown = true
		}
	}
	if !anyUp && !anyDown {
		values := make(map[string]int32, len(component))
		for _, name := range component {
			result.Replicas[name] = states[name].CurrentReplicas
			values[name] = states[name].CurrentReplicas
		}
		if !ratiosSatisfied(values, input.Spec.Ratios) {
			result.BlockingReason = v1alpha1.ModelDeploymentScalerReasonRatioConflict
			for _, name := range component {
				result.Reasons[name] = result.BlockingReason
			}
		}
		return
	}
	directionUp := anyUp
	low, high := make(map[string]int32), make(map[string]int32)
	for _, name := range component {
		interval := result.Intervals[name]
		low[name], high[name] = interval.Floor, interval.Ceil
		if directionUp {
			low[name] = maxInt32(low[name], states[name].CurrentReplicas)
		} else {
			high[name] = minInt32(high[name], states[name].CurrentReplicas)
		}
	}
	desired := make(map[string]int32, len(component))
	for _, name := range component {
		desired[name] = clamp(states[name].RawRecommendedReplicas, low[name], high[name])
	}
	best, found := searchComponent(component, input.Spec.Ratios, low, high, desired)
	if !found {
		fullLow, fullHigh := make(map[string]int32), make(map[string]int32)
		for _, name := range component {
			interval := result.Intervals[name]
			fullLow[name], fullHigh[name] = interval.Floor, interval.Ceil
		}
		_, fullFound := searchComponent(component, input.Spec.Ratios, fullLow, fullHigh, desired)
		result.BlockingReason = v1alpha1.ModelDeploymentScalerReasonRatioConflict
		if fullFound {
			result.BlockingReason = v1alpha1.ModelDeploymentScalerReasonPacingBounded
		}
		for _, name := range component {
			result.Replicas[name] = states[name].CurrentReplicas
			result.Reasons[name] = result.BlockingReason
		}
		return
	}
	for _, name := range component {
		result.Replicas[name] = best[name]
	}
	held := false
	for _, name := range component {
		if best[name] != desired[name] || (best[name] == states[name].CurrentReplicas && desired[name] != states[name].CurrentReplicas) {
			held = true
			result.Reasons[name] = v1alpha1.ModelDeploymentScalerReasonPacingBounded
		}
	}
	if held {
		result.BlockingReason = v1alpha1.ModelDeploymentScalerReasonPacingBounded
	}
}

func searchComponent(
	component []string,
	ratios []v1alpha1.ModelDeploymentScalerRoleRatio,
	low, high, desired map[string]int32,
) (map[string]int32, bool) {
	for _, name := range component {
		if low[name] > high[name] {
			return nil, false
		}
	}
	graph := ratioGraph(ratios)
	anchor := component[0]
	for _, name := range component[1:] {
		if len(graph[name]) > len(graph[anchor]) {
			anchor = name
		}
	}
	var best map[string]int32
	bestScore := [3]float64{math.Inf(1), math.Inf(1), math.Inf(-1)}
	assignment := make(map[string]int32, len(component))
	budget := modelDeploymentScalerConstraintSearchBudget
	for _, value := range orderedCandidates(low[anchor], high[anchor], desired[anchor]) {
		if budget <= 0 {
			break
		}
		assignment[anchor] = value
		searchNeighbors(component, graph, ratios, low, high, desired, assignment, &best, &bestScore, &budget)
	}
	return best, best != nil
}

func searchNeighbors(
	component []string,
	graph map[string][]string,
	ratios []v1alpha1.ModelDeploymentScalerRoleRatio,
	low, high, desired, assignment map[string]int32,
	best *map[string]int32,
	bestScore *[3]float64,
	budget *int,
) {
	if *budget <= 0 {
		return
	}
	if len(assignment) == len(component) {
		*budget--
		if !ratiosSatisfied(assignment, ratios) {
			return
		}
		score := scoreVector(assignment, desired, ratios)
		betterL1 := score[0] < bestScore[0]
		betterRatio := score[0] == bestScore[0] && score[1] < bestScore[1]
		betterTie := score[0] == bestScore[0] && score[1] == bestScore[1] && score[2] > bestScore[2]
		if betterL1 || betterRatio || betterTie {
			copyAssignment := make(map[string]int32, len(assignment))
			for name, value := range assignment {
				copyAssignment[name] = value
			}
			*best, *bestScore = copyAssignment, score
		}
		return
	}
	var next string
	for _, assigned := range component {
		if _, ok := assignment[assigned]; !ok {
			continue
		}
		for _, neighbor := range graph[assigned] {
			if _, ok := assignment[neighbor]; !ok {
				next = neighbor
				break
			}
		}
		if next != "" {
			break
		}
	}
	if next == "" {
		for _, candidate := range component {
			if _, ok := assignment[candidate]; !ok {
				next = candidate
				break
			}
		}
	}
	if next == "" {
		return
	}

	boundsLow, boundsHigh := low[next], high[next]
	for _, ratio := range ratios {
		if ratio.NumeratorRole == next {
			if den, ok := assignment[ratio.DenominatorRole]; ok {
				boundsLow = maxInt32(boundsLow, ratioLower(den, quantityFloat(ratio.Min), true))
				boundsHigh = minInt32(boundsHigh, ratioUpper(den, ratioMaxFloat(ratio.Max), true))
			}
		}
		if ratio.DenominatorRole == next {
			if num, ok := assignment[ratio.NumeratorRole]; ok {
				boundsLow = maxInt32(boundsLow, ratioLower(num, ratioMaxFloat(ratio.Max), false))
				boundsHigh = minInt32(boundsHigh, ratioUpper(num, quantityFloat(ratio.Min), false))
			}
		}
	}
	for _, value := range orderedCandidates(boundsLow, boundsHigh, desired[next]) {
		if *budget <= 0 {
			break
		}
		assignment[next] = value
		searchNeighbors(component, graph, ratios, low, high, desired, assignment, best, bestScore, budget)
		delete(assignment, next)
	}
}

func orderedCandidates(low, high, target int32) []int32 {
	if low > high {
		return nil
	}
	count := int(high - low + 1)
	if count > modelDeploymentScalerConstraintSearchBudget {
		count = modelDeploymentScalerConstraintSearchBudget
	}
	candidates := make([]int32, 0, count)
	center := clamp(target, low, high)
	candidates = append(candidates, center)
	for offset := int32(1); len(candidates) < count; offset++ {
		if center-offset >= low {
			candidates = append(candidates, center-offset)
			if len(candidates) == count {
				break
			}
		}
		if center+offset <= high {
			candidates = append(candidates, center+offset)
		}
	}
	return candidates
}

func ratioLower(known int32, ratio float64, numerator bool) int32 {
	if numerator {
		return int32(math.Ceil(ratio*float64(known) - modelDeploymentScalerRatioEpsilon))
	}
	if ratio <= 0 || math.IsInf(ratio, 1) {
		return 1
	}
	return int32(math.Ceil(float64(known)/ratio - modelDeploymentScalerRatioEpsilon))
}

func ratioUpper(known int32, ratio float64, numerator bool) int32 {
	if numerator {
		if math.IsInf(ratio, 1) {
			return math.MaxInt32
		}
		return int32(math.Floor(ratio*float64(known) + modelDeploymentScalerRatioEpsilon))
	}
	if ratio <= 0 {
		return math.MaxInt32
	}
	return int32(math.Floor(float64(known)/ratio + modelDeploymentScalerRatioEpsilon))
}

func ratiosSatisfied(values map[string]int32, ratios []v1alpha1.ModelDeploymentScalerRoleRatio) bool {
	for _, ratio := range ratios {
		denominator := values[ratio.DenominatorRole]
		if denominator <= 0 {
			return false
		}
		value := float64(values[ratio.NumeratorRole]) / float64(denominator)
		minimum, maximum := quantityFloat(ratio.Min), ratioMaxFloat(ratio.Max)
		if value < minimum-modelDeploymentScalerRatioEpsilon || value > maximum+modelDeploymentScalerRatioEpsilon {
			return false
		}
	}
	return true
}

func scoreVector(values, desired map[string]int32, ratios []v1alpha1.ModelDeploymentScalerRoleRatio) [3]float64 {
	var score [3]float64
	for name, value := range values {
		score[0] += math.Abs(float64(value - desired[name]))
		score[2] += float64(value)
	}
	for _, ratio := range ratios {
		denominator := values[ratio.DenominatorRole]
		if denominator > 0 {
			target := quantityFloat(ratio.Target)
			if target == 0 {
				target = (quantityFloat(ratio.Min) + quantityFloat(ratio.Max)) / 2
			}
			score[1] += math.Abs(float64(values[ratio.NumeratorRole])/float64(denominator) - target)
		}
	}
	return score
}

func ratioGraph(ratios []v1alpha1.ModelDeploymentScalerRoleRatio) map[string][]string {
	graph := make(map[string][]string)
	for _, ratio := range ratios {
		graph[ratio.NumeratorRole] = append(graph[ratio.NumeratorRole], ratio.DenominatorRole)
		graph[ratio.DenominatorRole] = append(graph[ratio.DenominatorRole], ratio.NumeratorRole)
	}
	return graph
}

func connectedComponent(start string, graph map[string][]string, seen map[string]bool) []string {
	component := []string{start}
	seen[start] = true
	for _, next := range graph[start] {
		if !seen[next] {
			component = append(component, connectedComponent(next, graph, seen)...)
		}
	}
	return component
}

func largestStabilizationWindow(spec v1alpha1.ModelDeploymentScalerSpec) time.Duration {
	upWindow := time.Duration(spec.Behavior.ScaleUp.StabilizationWindowSeconds) * time.Second
	downWindow := time.Duration(spec.Behavior.ScaleDown.StabilizationWindowSeconds) * time.Second
	if downWindow <= 0 {
		downWindow = modelDeploymentScalerDefaultScaleDownWindow
	}
	if upWindow > downWindow {
		return upWindow
	}
	return downWindow
}

func pruneStabilizationHistory(
	history []v1alpha1.ModelDeploymentScalerStabilizationSample, now time.Time, window time.Duration,
) []v1alpha1.ModelDeploymentScalerStabilizationSample {
	if len(history) == 0 {
		return nil
	}
	if window > 0 && !now.IsZero() {
		cutoff := now.Add(-window)
		anchorIndex := -1
		pruned := make([]v1alpha1.ModelDeploymentScalerStabilizationSample, 0, len(history))
		for i, sample := range history {
			if !sample.Timestamp.After(cutoff) {
				anchorIndex = i
				continue
			}
			if !sample.Timestamp.After(now) {
				pruned = append(pruned, sample)
			}
		}
		if anchorIndex >= 0 {
			pruned = append([]v1alpha1.ModelDeploymentScalerStabilizationSample{history[anchorIndex]}, pruned...)
		}
		history = pruned
	}
	if len(history) > modelDeploymentScalerMaxStabilizationSamples {
		kept := make([]v1alpha1.ModelDeploymentScalerStabilizationSample, 0, modelDeploymentScalerMaxStabilizationSamples)
		kept = append(kept, history[0])
		kept = append(kept, history[len(history)-(modelDeploymentScalerMaxStabilizationSamples-1):]...)
		history = kept
	}
	return history
}

func rollingMaximum(
	history []v1alpha1.ModelDeploymentScalerStabilizationSample, now time.Time, window time.Duration,
) (int32, bool) {
	cutoff := now.Add(-window)
	var maximum int32
	found := false
	for _, sample := range history {
		if sample.Timestamp.Before(&metav1.Time{Time: cutoff}) || sample.Timestamp.After(now) {
			continue
		}
		if !found || sample.Replicas > maximum {
			maximum, found = sample.Replicas, true
		}
	}
	return maximum, found
}

func historySpans(
	history []v1alpha1.ModelDeploymentScalerStabilizationSample, now time.Time, window time.Duration,
) bool {
	if window <= 0 {
		return true
	}
	cutoff := now.Add(-window)
	hasOld, hasRecent := false, false
	for _, sample := range history {
		if !sample.Timestamp.After(cutoff) {
			hasOld = true
		}
		if !sample.Timestamp.Before(&metav1.Time{Time: cutoff}) && !sample.Timestamp.After(now) {
			hasRecent = true
		}
	}
	return hasOld && hasRecent
}

func quantityFloat(quantity *resource.Quantity) float64 {
	if quantity == nil {
		return 0
	}
	return quantity.AsApproximateFloat64()
}

func ratioMaxFloat(quantity *resource.Quantity) float64 {
	if quantity == nil {
		return math.Inf(1)
	}
	return quantityFloat(quantity)
}

func metav1Time(value time.Time) metav1.Time { return metav1.NewTime(value) }

func clamp(value, low, high int32) int32 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

func minInt32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}
