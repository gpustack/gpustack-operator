package worker

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/datax"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

// modelDeploymentScalerMaxStabilizationSamples is how many stabilization samples the window of
// a role holds.
//
// IT IS THE BOUND OF THE WINDOW, because the window is read as a whole: a role that samples
// faster than its window expects still holds a bounded number of samples.
const modelDeploymentScalerMaxStabilizationSamples = 256

// The bounds of one sampling pass.
const (
	// maxSamplerPodsPerRole is how many ready answering engine Pods one role is asked about.
	//
	// IT IS A BOUND AND NOT A TARGET: a role with more ready Pods than this is reported as
	// incompletely covered, because the numbers gathered from the Pods below it would not
	// describe the role.
	maxSamplerPodsPerRole = 64

	// maxSamplerScrapeConcurrency is how many Pods one pass scrapes at the same time.
	maxSamplerScrapeConcurrency = 32

	// samplerPodTimeout bounds one Pod's scrape.
	samplerPodTimeout = 2 * time.Second

	// samplerPassTimeout bounds one whole pass over every role's Pods.
	samplerPassTimeout = 8 * time.Second

	// samplerMaxBodyBytes bounds the response body one scrape may read.
	samplerMaxBodyBytes = 1 << 20

	// modelDeploymentScalerSamplerDefaultIntervalSeconds is the interval a scaler is read as
	// sampling at when its own spec does not say one.
	modelDeploymentScalerSamplerDefaultIntervalSeconds = 15
)

// modelDeploymentScalerScrapeFunc reads one engine Pod's metrics exposition.
//
// IT RECEIVES THE POD RATHER THAN AN ADDRESS, because what may be read from a Pod is a
// question about the Pod: an address it does not advertise is not one this may read.
type modelDeploymentScalerScrapeFunc func(ctx context.Context, pod *core.Pod) ([]byte, error)

// modelDeploymentScalerSampler reads the engine metrics of a scaler's target and turns them into
// one observation per role.
//
// IT KEEPS ITS OWN SAMPLE RING PER SCALER, which is not the metrics subresource's window: a
// scaler needs the rate of a signal over the interval it samples at, and the subresource
// answers a different question over a different window.
type modelDeploymentScalerSampler struct {
	// HTTPClient fetches an engine Pod's metrics endpoint.
	//
	// IT IS THE CLIENT THE DEFAULT SCRAPE USES, and it is a field so that a deployment can
	// bound the transport and a test can answer a pass without a server.
	HTTPClient *http.Client

	// Scrape reads one engine Pod's metrics exposition.
	//
	// IT REPLACES THE TRANSPORT WHILE IT IS SET, so that a test can drive a pass from fixed
	// expositions. It receives the Pod and returns the exposition the Pod answered with.
	Scrape modelDeploymentScalerScrapeFunc

	// Clock reads the current time.
	//
	// IT IS THE SAMPLER'S OWN CLOCK, and it is used when a caller does not supply the time a
	// pass is evaluated at.
	Clock func() time.Time

	mu sync.Mutex
	// rings holds what the sampler remembers about each scaler, keyed by the scaler's UID.
	rings map[string]*modelDeploymentScalerRing
	// passing holds the targets a pass is running for, keyed by the target's UID.
	passing map[string]bool
	// lastPass holds the time the last pass over a target started, keyed by the target's UID.
	//
	// IT IS THE FLOOR ON HOW OFTEN A TARGET IS SAMPLED, because a scaler asks for its
	// observations as fast as it reconciles and the cost of a pass is the scrape load it puts
	// on the engine Pods.
	lastPass map[string]time.Time
}

// modelDeploymentScalerRing is what the sampler remembers about one scaler.
type modelDeploymentScalerRing struct {
	// pods holds the last sample of every engine Pod, keyed by the Pod's UID.
	pods map[string]modelDeploymentScalerPodSample
	// roles holds what the last pass knew about every role, keyed by the role's name.
	roles map[string]modelDeploymentScalerRoleSample
	// windows holds the sliding window of every role, keyed by the role's name.
	windows map[string]*modelDeploymentScalerRoleWindow
	// targetKey is the key the target of the scaler is remembered under.
	//
	// IT IS THE KEY OF THE TARGET AND NOT OF THE SCALER, because the passes are counted per
	// target: two scalers of one target share the floor on how often it is sampled.
	targetKey string
}

// newModelDeploymentScalerRing returns an empty ring.
func newModelDeploymentScalerRing() *modelDeploymentScalerRing {
	return &modelDeploymentScalerRing{
		pods:    map[string]modelDeploymentScalerPodSample{},
		roles:   map[string]modelDeploymentScalerRoleSample{},
		windows: map[string]*modelDeploymentScalerRoleWindow{},
	}
}

// modelDeploymentScalerRoleWindow is the sliding window of one role.
//
// IT IS THE STATE A SCALER NEEDS BETWEEN TICKS, because the stabilization history and the
// demand history are windows over the last samples of a role: they are read as a window and
// grown by one sample per tick. They are kept in memory so that a steady scaler does not
// have to write them to the apiserver on every tick.
type modelDeploymentScalerRoleWindow struct {
	// stabilization holds the stabilization samples of the role.
	stabilization *datax.RingBuffer[workercore.ModelDeploymentScalerStabilizationSample]
	// predictor is the predictor state of the role.
	predictor *algorithm.PredictorState
	// seeded says whether the window was filled from the status of the scaler.
	seeded bool
}

// modelDeploymentScalerRoleSample is what the sampler knows about one role after a pass.
//
// IT CARRIES WHAT THE OBSERVATION CANNOT, which is the reasoning behind it: why a signal is
// not the reading it would be, how long the window the rates were read over was, and how
// many times the role's engine containers restarted. A scaler that reports why it is waiting
// for metrics is a scaler whose reader can tell a missing signal from a broken one.
type modelDeploymentScalerRoleSample struct {
	// Observation is what the role's signals say.
	Observation algorithm.RoleObservation
	// Reasons says why the signals are not the readings they would be.
	Reasons []string
	// WindowSeconds is the shortest window the role's rates were read over.
	WindowSeconds float64
	// ContainerRestarts is the most times any of the role's engine containers restarted.
	ContainerRestarts int32
	// ReadyPods is how many ready engine Pods answered the pass.
	ReadyPods int
}

// SampleRoleMetrics scrapes the target's engine Pods and returns one observation per role.
//
// IT IS THE WHOLE OF WHAT THE SAMPLER DOES FOR A PASS: the caller supplies the target, the
// Pods it holds and the time the pass is evaluated at, and receives what those Pods say.
//
// A PASS THAT WOULD OVERLAP ANOTHER ONE FOR THE SAME TARGET IS REFUSED, and the previous
// pass's observations are returned instead: two passes over one target would double the
// scrape load for a reading the first pass is already taking.
func (s *modelDeploymentScalerSampler) SampleRoleMetrics(
	ctx context.Context,
	scaler *workercore.ModelDeploymentScaler,
	md *workercore.ModelDeployment,
	pods []core.Pod,
	now time.Time,
) map[string]algorithm.RoleObservation {
	if scaler == nil || md == nil {
		return nil
	}
	if now.IsZero() {
		now = s.now()
	}

	scalerKey := string(scaler.UID)
	targetKey := string(md.UID)
	if targetKey == "" {
		targetKey = scalerKey
	}

	if !s.beginPass(targetKey, now, s.samplingInterval(scaler)) {
		return s.cachedObservations(scalerKey, now, s.freshnessBound(scaler))
	}
	defer s.endPass(targetKey)

	return s.observe(ctx, scalerKey, targetKey, scaler, md, pods, now)
}

// Forget drops everything the sampler remembers about a scaler.
//
// IT IS CALLED WHEN THE SCALER IS DELETED, because the ring is keyed by the scaler's UID: a
// scaler deleted and recreated under the same name carries a different UID and is answered
// from a ring of its own, so what the deleted one left behind is only ever memory.
//
// THE FLOOR ON HOW OFTEN THE TARGET IS SAMPLED STAYS, because the floor is the target's and
// not the scaler's: a second scaler of the same target still samples it under that floor.
func (s *modelDeploymentScalerSampler) Forget(scaler *workercore.ModelDeploymentScaler) {
	if scaler == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(scaler.UID)
	ring, ok := s.rings[key]
	if !ok {
		return
	}
	targetKey := ring.targetKey
	delete(s.rings, key)
	if targetKey == "" {
		return
	}
	for _, other := range s.rings {
		if other.targetKey == targetKey {
			return
		}
	}
	delete(s.lastPass, targetKey)
}

// observe scrapes the target's Pods and reads one observation per declared role.
func (s *modelDeploymentScalerSampler) observe(
	ctx context.Context,
	scalerKey string,
	targetKey string,
	scaler *workercore.ModelDeploymentScaler,
	md *workercore.ModelDeployment,
	pods []core.Pod,
	now time.Time,
) map[string]algorithm.RoleObservation {
	engine := modelDeploymentScalerEngine(md)
	ready := s.readyEnginePods(md, pods)
	freshness := s.freshnessBound(scaler)
	previous := s.snapshot(scalerKey)

	observations := make(map[string]algorithm.RoleObservation, len(md.Spec.Roles))
	samples := make(map[string]modelDeploymentScalerPodSample, len(ready))
	roles := make(map[string]modelDeploymentScalerRoleSample, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		roleCtx, cancel := context.WithTimeout(ctx, samplerPassTimeout)
		observation, read, sample := s.observeRole(
			roleCtx, role, engine, ready[role.Name], previous, now, freshness)
		cancel()
		observations[role.Name] = observation
		roles[role.Name] = sample
		for uid, podSample := range read {
			samples[uid] = podSample
		}
	}
	s.commit(scalerKey, targetKey, samples, roles)
	return observations
}

// observeRole reads one role's observation from the Pods that answer for it.
func (s *modelDeploymentScalerSampler) observeRole(
	ctx context.Context,
	role *workercore.ModelDeploymentRole,
	engine string,
	ready []*core.Pod,
	previous map[string]modelDeploymentScalerPodSample,
	now time.Time,
	freshness time.Duration,
) (algorithm.RoleObservation, map[string]modelDeploymentScalerPodSample, modelDeploymentScalerRoleSample) {
	observation := algorithm.RoleObservation{
		Role:            role.Name,
		Kind:            modelDeploymentScalerRoleKind(role.Kind),
		CurrentReplicas: role.Replicas,
		ReadyReplicas:   int32(len(ready)),
		ObservedAt:      now,
		GaugeState:      algorithm.SignalMissing,
		ThroughputState: algorithm.SignalMissing,
		LatencyState:    algorithm.SignalMissing,
	}
	sample := modelDeploymentScalerRoleSample{ReadyPods: len(ready)}
	var reasons []string
	if len(ready) == 0 {
		sample.Reasons = []string{"the role has no ready engine pod"}
		return observation, nil, sample
	}

	// A role with more ready Pods than the cap is asked about the Pods below it and reported
	// as incompletely covered, because the numbers read from them do not describe the role.
	capped := len(ready) > maxSamplerPodsPerRole
	asked := ready
	if capped {
		asked = ready[:maxSamplerPodsPerRole]
		reasons = append(reasons, fmt.Sprintf(
			"the role has %d ready engine pods and the sampler asks about %d of them",
			len(ready), maxSamplerPodsPerRole))
	}

	reads, passErr := s.scrapePods(ctx, asked, engine, now)
	if passErr != nil {
		reasons = append(reasons, fmt.Sprintf(
			"the engine pods could not all be read: %v", passErr))
	}
	samples := make(map[string]modelDeploymentScalerPodSample, len(reads))
	windows := make([]modelDeploymentScalerWindow, 0, len(reads))
	for i := range reads {
		read := &reads[i]
		if read.err != nil {
			continue
		}
		samples[string(read.pod.UID)] = read.sample

		var prior *modelDeploymentScalerPodSample
		if previousSample, ok := previous[string(read.pod.UID)]; ok {
			prior = &previousSample
		}
		windows = append(windows, modelDeploymentScalerWindowOf(prior, read.sample, now, freshness))
	}

	ttft := modelDeploymentScalerHistogram{buckets: map[float64]float64{}}
	itl := modelDeploymentScalerHistogram{buckets: map[float64]float64{}}
	kvUsage, kvUsageCount := 0.0, 0
	gaugeState := algorithm.SignalFresh
	throughputState := algorithm.SignalFresh
	latencyState := algorithm.SignalFresh
	idle := true
	for i := range reads {
		read := &reads[i]
		if read.err != nil {
			gaugeState = algorithm.SignalMissing
			throughputState = algorithm.SignalMissing
			latencyState = algorithm.SignalMissing
			idle = false
			reasons = append(reasons, fmt.Sprintf(
				"the engine pod %s did not answer: %v", read.pod.Name, read.err))
			continue
		}
		observation.RunningRequests += read.sample.running
		observation.WaitingRequests += read.sample.waiting
		if observation.Kind == algorithm.DecodeRole {
			observation.WaitingRequests += read.sample.decodeTransfer
		}
		if read.sample.kvUsageFound {
			kvUsage += read.sample.kvUsage
			kvUsageCount++
		}
	}
	if kvUsageCount > 0 {
		observation.KVCacheUsage = kvUsage / float64(kvUsageCount)
	}

	for i := range windows {
		window := &windows[i]
		if !window.gaugesReadable {
			gaugeState = algorithm.SignalMissing
			idle = false
			reasons = append(reasons, "an engine pod did not report its request gauges")
		}
		if !window.tokensReadable {
			throughputState = algorithm.SignalMissing
			idle = false
			reasons = append(reasons, "an engine pod did not report its token counters")
		}
		if !window.latencyReadable {
			latencyState = algorithm.SignalMissing
			idle = false
			reasons = append(reasons, "an engine pod did not report its latency histograms")
		}
		switch window.state {
		case algorithm.SignalMissing:
			throughputState = algorithm.SignalMissing
			latencyState = algorithm.SignalMissing
			idle = false
			reasons = append(reasons, window.reason)
		case algorithm.SignalStale:
			throughputState = modelDeploymentScalerWorseState(throughputState, window.state)
			latencyState = modelDeploymentScalerWorseState(latencyState, window.state)
			idle = false
			reasons = append(reasons, window.reason)
		}
		if window.seconds <= 0 {
			idle = false
		}
		if window.promptTokens != 0 || window.generationTokens != 0 {
			idle = false
		}
		if window.ttft.count != 0 || window.itl.count != 0 {
			idle = false
		}
		ttft = modelDeploymentScalerHistogramMerge(ttft, window.ttft)
		itl = modelDeploymentScalerHistogramMerge(itl, window.itl)
		if window.seconds > 0 {
			observation.PromptTokensPerSecond += window.cacheMissedPromptTokens / window.seconds
			observation.GenerationTokensPerSecond += window.generationTokens / window.seconds
		}
	}

	// A ROLE THAT SERVED NOTHING IS IDLE RATHER THAN MISSING, so that a deployment with
	// nothing to do can scale down to its floor. The gauges say so as well as the counters:
	// an engine that is running nothing and has run nothing is idle, and a reading of zero
	// is the truth about it rather than a gap in the reading.
	if idle && observation.RunningRequests == 0 && observation.WaitingRequests == 0 {
		observation.GaugeState = algorithm.SignalIdle
		observation.ThroughputState = algorithm.SignalIdle
		observation.LatencyState = algorithm.SignalIdle
		observation.PromptTokensPerSecond = 0
		observation.GenerationTokensPerSecond = 0
		observation.TTFTMilliseconds = 0
		observation.ITLMilliseconds = 0
	} else {
		observation.GaugeState = gaugeState
		observation.ThroughputState = throughputState
		observation.LatencyState = latencyState
		// BOTH LATENCIES ARE READ ON THE LATENCY SIGNAL ALONE, because a first-token latency
		// and an inter-token latency are latency readings: an engine that reports its
		// histograms but not its token counters has said what it knows about its latency, and
		// withholding it would answer a question the engine did answer.
		if latencyState == algorithm.SignalFresh {
			if value, ok := modelDeploymentScalerQuantileMilliseconds(ttft, modelDeploymentScalerLatencyQuantile); ok {
				observation.TTFTMilliseconds = value
			}
			if value, ok := modelDeploymentScalerQuantileMilliseconds(itl, modelDeploymentScalerLatencyQuantile); ok {
				observation.ITLMilliseconds = value
			}
		}
	}

	observation.CompleteCoverage = len(windows) == len(asked) && !capped &&
		throughputState == algorithm.SignalFresh && latencyState == algorithm.SignalFresh

	sample.Observation = observation
	sample.Reasons = reasons
	sample.WindowSeconds = shortestWindow(windows)
	sample.ContainerRestarts = highestRestarts(reads)
	return observation, samples, sample
}

// shortestWindow returns the shortest window a role's rates were read over.
//
// IT IS THE SHORTEST AND NOT THE LONGEST, because the shortest window is the one that says
// the least about the role: a rate read over a short window is the one that a slow pass or a
// restarted engine would have made unreliable.
func shortestWindow(windows []modelDeploymentScalerWindow) float64 {
	shortest := 0.0
	for i := range windows {
		if windows[i].seconds <= 0 {
			continue
		}
		if shortest == 0 || windows[i].seconds < shortest {
			shortest = windows[i].seconds
		}
	}
	return shortest
}

// highestRestarts returns the most times any of a role's engine containers restarted.
func highestRestarts(reads []modelDeploymentScalerPodRead) int32 {
	highest := int32(0)
	for i := range reads {
		if reads[i].err != nil {
			continue
		}
		if reads[i].sample.restartCount > highest {
			highest = reads[i].sample.restartCount
		}
	}
	return highest
}

// scrapePods reads every Pod of a role, with the concurrency and the time bounds a pass runs
// under.
//
// IT IS THE PASS'S OWN TIME THAT THE SAMPLES CARRY, because the window a rate is read over is
// the time between two passes and not the time a scrape happened to take.
//
// THE RETURNED ERROR IS THE FIRST FAILURE OF THE PASS, and it does not stop the other Pods
// from being read: a Pod that does not answer is a fact about that Pod and not about the
// pass, and the pass's own reading says so.
func (s *modelDeploymentScalerSampler) scrapePods(
	ctx context.Context, pods []*core.Pod, engine string, now time.Time,
) ([]modelDeploymentScalerPodRead, error) {
	reads := make([]modelDeploymentScalerPodRead, len(pods))
	var group errgroup.Group
	group.SetLimit(maxSamplerScrapeConcurrency)
	for i := range pods {
		group.Go(func() error {
			body, err := s.scrape(ctx, pods[i])
			read := modelDeploymentScalerPodRead{pod: pods[i], err: err}
			if err == nil {
				read.sample, err = modelDeploymentScalerParseSample(pods[i], body, engine, now)
				read.err = err
			}
			reads[i] = read
			return read.err
		})
	}
	return reads, group.Wait()
}

// readyEnginePods selects the engine Pods each role is asked about.
//
// IT IS THE LEADER OF EVERY READY REPLICA, because a replica of several members runs one
// engine spread across them and exposes it at member zero: the others hold ranks and answer
// nothing, so asking them would return no account of the role rather than a different one.
func (s *modelDeploymentScalerSampler) readyEnginePods(
	md *workercore.ModelDeployment, pods []core.Pod,
) map[string][]*core.Pod {
	byRole := make(map[string][]*core.Pod, len(md.Spec.Roles))
	for _, pod := range modelDeploymentReadyReplicas(md, pods) {
		role := modelDeploymentPodRole(pod)
		byRole[role] = append(byRole[role], pod)
	}
	return byRole
}

// modelDeploymentScalerEngine reads the engine the target runs, as the metrics families name
// it.
func modelDeploymentScalerEngine(md *workercore.ModelDeployment) string {
	if mapped, ok := ModelDeploymentEngineUpstreamName(md.Spec.Engine.Name); ok {
		return mapped
	}
	return ""
}

// freshnessBound is how old a sample may be and still be read as the current state.
func (s *modelDeploymentScalerSampler) freshnessBound(
	scaler *workercore.ModelDeploymentScaler,
) time.Duration {
	seconds := int32(modelDeploymentScalerSamplerDefaultIntervalSeconds)
	if scaler != nil && scaler.Spec.Sampling.IntervalSeconds > 0 {
		seconds = scaler.Spec.Sampling.IntervalSeconds
	}
	return 2 * time.Duration(seconds) * time.Second
}

// samplingInterval is how often a scaler is read as sampling its target.
//
// IT IS THE INTERVAL THE SCALER ASKED FOR, because the interval is the floor on how fast the
// scaler reacts and the floor on how often it reads the target's engine Pods.
func (s *modelDeploymentScalerSampler) samplingInterval(
	scaler *workercore.ModelDeploymentScaler,
) time.Duration {
	seconds := int32(modelDeploymentScalerSamplerDefaultIntervalSeconds)
	if scaler != nil && scaler.Spec.Sampling.IntervalSeconds > 0 {
		seconds = scaler.Spec.Sampling.IntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

// now reads the current time.
func (s *modelDeploymentScalerSampler) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// beginPass claims the right to run a pass over a target.
//
// IT IS REFUSED WHILE ANOTHER PASS HOLDS THE TARGET, because two passes over one target
// would double the scrape load for a reading the first pass is already taking.
//
// IT IS ALSO REFUSED BEFORE THE INTERVAL OF THE SCALER HAS PASSED, because a scaler asks for
// its observations as fast as it reconciles and a pass over a target costs one scrape per
// engine Pod it holds.
func (s *modelDeploymentScalerSampler) beginPass(
	targetKey string, now time.Time, interval time.Duration,
) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.passing == nil {
		s.passing = map[string]bool{}
	}
	if s.passing[targetKey] {
		return false
	}
	if last, ok := s.lastPass[targetKey]; ok && now.Before(last.Add(interval)) {
		return false
	}
	s.passing[targetKey] = true
	if s.lastPass == nil {
		s.lastPass = map[string]time.Time{}
	}
	s.lastPass[targetKey] = now
	return true
}

// endPass gives the target back.
func (s *modelDeploymentScalerSampler) endPass(targetKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.passing, targetKey)
}

// snapshot reads the samples the last pass left behind.
func (s *modelDeploymentScalerSampler) snapshot(
	scalerKey string,
) map[string]modelDeploymentScalerPodSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	ring := s.rings[scalerKey]
	if ring == nil {
		return map[string]modelDeploymentScalerPodSample{}
	}
	snapshot := make(map[string]modelDeploymentScalerPodSample, len(ring.pods))
	for uid, sample := range ring.pods {
		snapshot[uid] = sample
	}
	return snapshot
}

// commit keeps what a pass read, so that the next pass can read the change over the window
// between them.
func (s *modelDeploymentScalerSampler) commit(
	scalerKey string,
	targetKey string,
	samples map[string]modelDeploymentScalerPodSample,
	roles map[string]modelDeploymentScalerRoleSample,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rings == nil {
		s.rings = map[string]*modelDeploymentScalerRing{}
	}
	ring, ok := s.rings[scalerKey]
	if !ok {
		ring = newModelDeploymentScalerRing()
		s.rings[scalerKey] = ring
	}
	ring.targetKey = targetKey
	ring.pods = samples
	ring.roles = roles
}

// StabilizationHistory returns the stabilization history the sampler holds for one role.
//
// IT IS FILLED FROM THE STATUS OF THE SCALER the first time the role is asked for, so that
// a scaler keeps its window across a restart of the operator.
func (s *modelDeploymentScalerSampler) StabilizationHistory(
	scaler *workercore.ModelDeploymentScaler, role string,
) []workercore.ModelDeploymentScalerStabilizationSample {
	if scaler == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.roleWindow(scaler, role).stabilization.Data()
}

// RecordStabilizationHistory keeps the stabilization history of every role.
//
// IT REPLACES THE HISTORY OF A ROLE with the one the constraint solver pruned, because the
// solver is what knows which samples fell out of the window.
func (s *modelDeploymentScalerSampler) RecordStabilizationHistory(
	scaler *workercore.ModelDeploymentScaler,
	histories map[string][]workercore.ModelDeploymentScalerStabilizationSample,
) {
	if scaler == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for role, history := range histories {
		window := s.roleWindow(scaler, role)
		window.stabilization.Reset()
		for _, sample := range history {
			window.stabilization.Push(sample)
		}
	}
}

// PredictorState returns the predictor state the sampler holds for one role.
//
// IT IS FILLED FROM THE STATUS OF THE SCALER the first time the role is asked for, so that
// a scaler keeps its predictor across a restart of the operator.
func (s *modelDeploymentScalerSampler) PredictorState(
	scaler *workercore.ModelDeploymentScaler, role string,
) *algorithm.PredictorState {
	if scaler == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	window := s.roleWindow(scaler, role)
	if window.predictor == nil {
		window.predictor = scalerPredictor(scaler.Status.Predictors, role)
	}
	return window.predictor
}

// RecordPredictorStates keeps the predictor state of every role.
//
// IT KEEPS THE STATE OF THE ROLES THAT HAVE ONE, because a role whose algorithm does not
// predict anything has no state to keep.
func (s *modelDeploymentScalerSampler) RecordPredictorStates(
	scaler *workercore.ModelDeploymentScaler, states map[string]*algorithm.PredictorState,
) {
	if scaler == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for role, state := range states {
		if state == nil {
			continue
		}
		s.roleWindow(scaler, role).predictor = state
	}
}

// roleWindow returns the window the sampler keeps for one role of one scaler.
//
// IT IS FILLED FROM THE STATUS OF THE SCALER the first time the role is asked for, because a
// scaler that was observed before the operator restarted has a window the operator does not
// know of.
func (s *modelDeploymentScalerSampler) roleWindow(
	scaler *workercore.ModelDeploymentScaler, role string,
) *modelDeploymentScalerRoleWindow {
	if s.rings == nil {
		s.rings = map[string]*modelDeploymentScalerRing{}
	}
	key := string(scaler.UID)
	ring, ok := s.rings[key]
	if !ok {
		ring = newModelDeploymentScalerRing()
		s.rings[key] = ring
	}
	window, ok := ring.windows[role]
	if !ok {
		window = &modelDeploymentScalerRoleWindow{
			stabilization: datax.NewRingBuffer[workercore.ModelDeploymentScalerStabilizationSample](
				modelDeploymentScalerMaxStabilizationSamples),
		}
		ring.windows[role] = window
	}
	if !window.seeded {
		window.seeded = true
		for _, sample := range scalerStabilizationHistory(scaler.Status.Roles, role) {
			window.stabilization.Push(sample)
		}
	}
	return window
}

// RoleSamples says what the last pass knew about every role of the target.
//
// IT IS THE REASONING AN OBSERVATION CANNOT CARRY, because an observation says what a signal
// is and not why it is not what it would be, how long the window it was read over was, or how
// many times the engine restarted under it.
func (s *modelDeploymentScalerSampler) RoleSamples(
	scaler *workercore.ModelDeploymentScaler,
) map[string]modelDeploymentScalerRoleSample {
	if scaler == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ring := s.rings[string(scaler.UID)]
	if ring == nil {
		return nil
	}
	roles := make(map[string]modelDeploymentScalerRoleSample, len(ring.roles))
	for role, sample := range ring.roles {
		sample.Reasons = append([]string(nil), sample.Reasons...)
		roles[role] = sample
	}
	return roles
}

// cachedObservations reads the observations of the last pass, and says of them that they are
// stale once they are older than the freshness bound.
//
// IT IS WHAT A REFUSED PASS RETURNS, because a reading that is too old is not a reading of
// now, and a caller that cannot tell an old reading from a current one would act on it.
func (s *modelDeploymentScalerSampler) cachedObservations(
	scalerKey string, now time.Time, freshness time.Duration,
) map[string]algorithm.RoleObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	ring := s.rings[scalerKey]
	if ring == nil {
		return nil
	}
	cached := make(map[string]algorithm.RoleObservation, len(ring.roles))
	for role, sample := range ring.roles {
		observation := sample.Observation
		if now.Sub(observation.ObservedAt) > freshness {
			observation.GaugeState = modelDeploymentScalerStaleState(observation.GaugeState)
			observation.ThroughputState = modelDeploymentScalerStaleState(observation.ThroughputState)
			observation.LatencyState = modelDeploymentScalerStaleState(observation.LatencyState)
		}
		cached[role] = observation
	}
	return cached
}

// modelDeploymentScalerStaleState says of a signal state that it is stale.
//
// A MISSING SIGNAL STAYS MISSING, because there was nothing to read and an old nothing is
// still nothing.
func modelDeploymentScalerStaleState(state algorithm.SignalState) algorithm.SignalState {
	if state == algorithm.SignalMissing {
		return state
	}
	return algorithm.SignalStale
}

// modelDeploymentScalerRoleKind reads the kind of a role in the vocabulary the algorithms use.
func modelDeploymentScalerRoleKind(kind workercore.ModelDeploymentRoleKind) algorithm.RoleKind {
	switch kind {
	case workercore.ModelDeploymentRoleKindPrefill:
		return algorithm.PrefillRole
	case workercore.ModelDeploymentRoleKindDecode:
		return algorithm.DecodeRole
	case workercore.ModelDeploymentRoleKindEncode:
		return algorithm.EncodeRole
	case workercore.ModelDeploymentRoleKindServer:
		return algorithm.ServerRole
	default:
		return algorithm.RoleKindUnknown
	}
}
