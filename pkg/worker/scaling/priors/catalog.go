// Package priors resolves capacity priors from an offline catalog of measured
// serving profiles.
//
// A prior is a measured capacity for one deployment shape: an exact model,
// engine, hardware and topology, a declared workload envelope, and the load
// points that were measured on it.
//
// The catalog never guesses. A profile that cannot be matched exactly, or that
// lacks the evidence the resolver needs, is refused with a typed reason
// instead of being filled in.
package priors

import (
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

// SchemaVersion is the catalog schema this package reads.
//
// A catalog written by another schema version is refused rather than guessed
// at, because the fields it carries decide which measurements are usable.
const SchemaVersion = "gpustack.ai/v1"

// The bounds a decoded catalog may not exceed.
//
// They exist so a catalog read from a ConfigMap cannot make the controller
// hold an unbounded amount of data.
const (
	// MaxCatalogBytes is the largest decoded catalog the reader accepts.
	MaxCatalogBytes = 512 * 1024

	// MaxProfiles is the largest number of profiles a catalog may hold.
	MaxProfiles = 256

	// MaxPointsPerProfile is the largest number of measured load points one
	// profile may hold.
	MaxPointsPerProfile = 128
)

// RoleKind is the serving role a profile describes.
//
// IT REUSES THE ALGORITHM ROLE KIND, because the roles the catalog describes
// and the roles the algorithms scale are the same set, and a second set of
// names would let the two drift apart.
type RoleKind = algorithm.RoleKind

// The serving roles a catalog profile may describe.
const (
	RoleKindServer  = algorithm.RoleKindServer
	RoleKindPrefill = algorithm.RoleKindPrefill
	RoleKindDecode  = algorithm.RoleKindDecode
	RoleKindEncode  = algorithm.RoleKindEncode
)

// ThroughputMetric names the numerator of a measured throughput.
//
// THE NUMERATOR MUST MATCH THE SAMPLER, because the sampler reads the demand
// the capacity is divided by. A source that measures a different quantity
// cannot fill the same field.
type ThroughputMetric string

const (
	// ThroughputMetricCacheMissedInputTokensPerSecond counts the prompt tokens
	// a replica prefetched, per second, after the cache did not serve them.
	//
	// IT IS THE PREFILL NUMERATOR, because the sampler reads the prompt
	// tokens the cache did not serve.
	ThroughputMetricCacheMissedInputTokensPerSecond ThroughputMetric = "CacheMissedInputTokensPerSecond"

	// ThroughputMetricOutputTokensPerSecond counts the tokens a replica
	// generated, per second.
	//
	// IT IS THE DECODE NUMERATOR.
	ThroughputMetricOutputTokensPerSecond ThroughputMetric = "OutputTokensPerSecond"

	// ThroughputMetricCacheMissedTotalTokensPerSecond counts the prompt tokens
	// the cache did not serve plus the generated tokens, per second.
	//
	// IT IS THE SERVER NUMERATOR, because the sampler adds both for a server
	// role. IT NAMES THE CACHE MISSES EXPLICITLY, because a total that
	// includes cached prompt tokens is a different quantity.
	ThroughputMetricCacheMissedTotalTokensPerSecond ThroughputMetric = "CacheMissedTotalTokensPerSecond"

	// ThroughputMetricInputTokensPerSecondIncludingCacheHits counts the prompt
	// tokens of every request, whether the cache served them or not.
	//
	// IT MATCHES NO SAMPLER NUMERATOR, so a profile that only carries it is
	// ineligible for every role.
	ThroughputMetricInputTokensPerSecondIncludingCacheHits ThroughputMetric = "InputTokensPerSecondIncludingCacheHits"

	// ThroughputMetricTotalTokensPerSecond counts the prompt tokens of every
	// request plus the generated tokens.
	//
	// IT IS THE WEAKEST TOTAL A SOURCE CAN PUBLISH, because it does not say
	// whether the prompt part contains tokens the cache served. IT MATCHES NO
	// SAMPLER NUMERATOR ON ITS OWN.
	ThroughputMetricTotalTokensPerSecond ThroughputMetric = "TotalTokensPerSecond"
)

// TokenCountConvention names which tokens a measured rate counted.
//
// IT IS SEPARATE FROM THE METRIC, because the metric says what the rate
// means to the resolver and the convention says which tokens the source
// counted to produce it.
type TokenCountConvention string

const (
	// TokenCountConventionCacheMissedInput counts the prompt tokens the cache
	// did not serve.
	TokenCountConventionCacheMissedInput TokenCountConvention = "CacheMissedInput"

	// TokenCountConventionOutput counts the generated tokens.
	TokenCountConventionOutput TokenCountConvention = "Output"

	// TokenCountConventionCacheMissedInputPlusOutput counts the prompt tokens
	// the cache did not serve plus the generated tokens.
	TokenCountConventionCacheMissedInputPlusOutput TokenCountConvention = "CacheMissedInputPlusOutput"

	// TokenCountConventionInputIncludingCacheHits counts the prompt tokens of
	// every request.
	TokenCountConventionInputIncludingCacheHits TokenCountConvention = "InputIncludingCacheHits"

	// TokenCountConventionInputPlusOutput counts the prompt tokens of every
	// request plus the generated tokens.
	TokenCountConventionInputPlusOutput TokenCountConvention = "InputPlusOutput"
)

// Denominator names what a measured throughput is counted over.
//
// IT DECIDES HOW THE MEASUREMENT IS RECONSTRUCTED for one scaling replica.
type Denominator string

const (
	// DenominatorPerGPU counts tokens per second over one GPU of the measured
	// system.
	DenominatorPerGPU Denominator = "PerGPU"

	// DenominatorPerReplica counts tokens per second over one complete
	// scaling replica.
	DenominatorPerReplica Denominator = "PerReplica"

	// DenominatorWholeSystem counts tokens per second over the whole measured
	// system.
	DenominatorWholeSystem Denominator = "WholeSystem"
)

// LatencyMetric names a measured latency.
type LatencyMetric string

const (
	// LatencyMetricTTFT is the time to first token.
	LatencyMetricTTFT LatencyMetric = "TTFT"

	// LatencyMetricITL is the inter-token latency.
	//
	// IT IS NOT TIME PER OUTPUT TOKEN, which is a mean over the tokens of one
	// request rather than a distribution of the gaps between them.
	LatencyMetricITL LatencyMetric = "ITL"

	// LatencyMetricTPOT is the time per output token.
	//
	// IT IS RECORDED SO A SOURCE THAT ONLY PUBLISHES IT CAN BE REFUSED WITH
	// THE REASON, because it does not establish any ITL quantile.
	LatencyMetricTPOT LatencyMetric = "TPOT"
)

// LoadKind names what a measured load point varies.
type LoadKind string

const (
	// LoadKindConcurrency is a fixed number of concurrent requests.
	LoadKindConcurrency LoadKind = "Concurrency"

	// LoadKindRequestRate is a fixed arrival rate.
	LoadKindRequestRate LoadKind = "RequestRate"
)

// ClientModel names how the load was offered.
type ClientModel string

const (
	// ClientModelFixedConcurrency is a closed-loop client that sends the next
	// request when the previous one finishes.
	ClientModelFixedConcurrency ClientModel = "FixedConcurrency"

	// ClientModelOpenLoop is a client that sends requests at a fixed rate
	// regardless of how fast the server answers.
	ClientModelOpenLoop ClientModel = "OpenLoop"
)

// CacheBehavior names how the measured workload treated the prefix cache.
type CacheBehavior string

const (
	// CacheBehaviorAllMisses means every prompt token the run counted was a
	// cache miss.
	CacheBehaviorAllMisses CacheBehavior = "AllMisses"

	// CacheBehaviorIncludesHits means the counted prompt tokens include tokens
	// the cache served.
	CacheBehaviorIncludesHits CacheBehavior = "IncludesCacheHits"

	// CacheBehaviorUnknown means the source does not say how the cache
	// behaved.
	//
	// IT IS NOT A WILDCARD. A profile with an unknown cache behavior cannot
	// fill a numerator that counts cache-missed prompt tokens.
	CacheBehaviorUnknown CacheBehavior = "Unknown"
)

// The units the catalog stores measurements in.
//
// A source is converted on import, so a stored value never carries a unit the
// reader has to interpret.
const (
	// UnitTokensPerSecond is a token rate.
	UnitTokensPerSecond = "tokens/second"

	// UnitMilliseconds is a duration.
	UnitMilliseconds = "milliseconds"

	// UnitQueriesPerSecond is an arrival rate.
	UnitQueriesPerSecond = "queries/second"
)

// DefaultLatencyQuantile is the latency quantile the resolver requires when a
// request does not name one.
//
// A HIGHER QUANTILE OF THE SAME METRIC MAY BE USED INSTEAD, because it bounds
// the required quantile from above.
//
// IT IS THE DEFAULT AND NOT A REQUIREMENT, because a request names the
// quantile it is held to and the resolver holds every required latency metric
// to that one quantile.
const DefaultLatencyQuantile = 0.95

// Catalog is a versioned, bounded set of measured profiles.
type Catalog struct {
	// SchemaVersion is the schema the catalog was written in.
	SchemaVersion string `json:"schemaVersion"`

	// Digest covers the catalog content.
	//
	// IT IS NOT PART OF THE COVERED CONTENT, so the digest can be checked
	// against the content it describes.
	Digest string `json:"digest"`

	// Sources records where the profiles came from.
	Sources []Source `json:"sources"`

	// Profiles holds the measured profiles.
	Profiles []Profile `json:"profiles"`
}

// Source records the attribution of one imported measurement set.
//
// IT IS RECORDED EVEN WHEN THE IMPORT PRODUCED NO PROFILE, because a refused
// source has to say why it was refused.
type Source struct {
	// ID identifies the source inside the catalog.
	ID string `json:"id"`

	// Name is the human readable name of the source.
	Name string `json:"name"`

	// URL is where the source was read from.
	URL string `json:"url"`

	// Revision pins the exact source revision the artifacts came from.
	Revision string `json:"revision"`

	// CaptureDate is when the measurement was taken.
	CaptureDate string `json:"captureDate"`

	// License is the SPDX identifier of the license the artifacts carry.
	License string `json:"license"`

	// AdapterVersion is the version of the adapter that read the source.
	AdapterVersion string `json:"adapterVersion"`

	// Artifacts records the pinned files the import read.
	Artifacts []Artifact `json:"artifacts"`

	// Outcome is Accepted when the import produced a profile and Refused when
	// it did not.
	Outcome string `json:"outcome"`

	// RefusalError explains a Refused outcome.
	RefusalError *RefusalError `json:"refusal,omitempty"`
}

// The outcomes a source import can have.
const (
	SourceOutcomeAccepted = "Accepted"
	SourceOutcomeRefused  = "Refused"
)

// Artifact is one pinned source file.
type Artifact struct {
	// Path is the path of the file inside the upstream repository.
	Path string `json:"path"`

	// SHA256 is the digest of the upstream bytes the import read.
	SHA256 string `json:"sha256"`
}

// Profile is one immutable measured profile.
//
// IT COVERS ONE SERVING CONFIGURATION AND ONE WORKLOAD ENVELOPE, because a
// frontier that combines serving configurations is not one deployment's
// capacity curve.
type Profile struct {
	// Digest covers the profile content.
	Digest string `json:"digest"`

	// SourceID names the source the profile was imported from.
	SourceID string `json:"sourceID"`

	// Role is the serving role the measurements describe.
	Role RoleKind `json:"role"`

	// Identity is what the deployment must match.
	Identity Identity `json:"identity"`

	// ServingConfig is the serving configuration the measurements were taken
	// under.
	ServingConfig ServingConfig `json:"servingConfig"`

	// Topology is the shape of one scaling replica.
	Topology Topology `json:"topology"`

	// MeasuredReplicas is how many scaling replicas the measured system ran.
	//
	// IT IS NOT PART OF THE MATCH, because the same shape measured on a
	// different number of replicas is still the same profile.
	MeasuredReplicas int `json:"measuredReplicas"`

	// Workload is the workload envelope the measurements were taken under.
	Workload Workload `json:"workload"`

	// Points are the measured load points.
	Points []MeasuredPoint `json:"points"`
}

// Identity is what a deployment must match to use a profile.
//
// EVERY FIELD IS REQUIRED, because an unknown value is not a wildcard and
// must not become a permissive default.
type Identity struct {
	// Model is the served model.
	Model ModelIdentity `json:"model"`

	// Engine is the serving engine.
	Engine EngineIdentity `json:"engine"`

	// Hardware is the accelerator the replica runs on.
	Hardware HardwareIdentity `json:"hardware"`
}

// ModelIdentity names the served model.
type ModelIdentity struct {
	// Name is the model name, with its variant suffixes kept.
	Name string `json:"name"`

	// Revision is the weight revision or digest.
	Revision string `json:"revision"`

	// Tokenizer is the tokenizer identity.
	Tokenizer string `json:"tokenizer"`

	// Quantization is the quantization recipe.
	Quantization string `json:"quantization"`
}

// EngineIdentity names the serving engine.
type EngineIdentity struct {
	// Image is the engine image.
	Image string `json:"image"`

	// Build is the engine build or version.
	Build string `json:"build"`
}

// HardwareIdentity names the accelerator.
type HardwareIdentity struct {
	// Accelerator is the accelerator model, with its form factor suffix kept.
	Accelerator string `json:"accelerator"`

	// MemoryGB is the accelerator memory in gibibytes.
	MemoryGB int `json:"memoryGB"`

	// FormFactor is the accelerator form factor.
	FormFactor string `json:"formFactor"`
}

// ServingConfig is the serving configuration a measurement was taken under.
//
// EVERY FIELD IS REQUIRED, because an omitted performance-critical setting is
// not a wildcard and a permissive default would let an incompatible
// deployment reuse the measurement.
type ServingConfig struct {
	// Router is the router or scheduler in front of the engine.
	Router string `json:"router"`

	// KVCacheDType is the KV cache dtype.
	KVCacheDType string `json:"kvCacheDType"`

	// Offload says how the KV cache is offloaded.
	Offload string `json:"offload"`

	// PrefixCaching says whether prefix caching is on.
	PrefixCaching string `json:"prefixCaching"`

	// SpeculativeDecoding says how speculative decoding is configured.
	SpeculativeDecoding string `json:"speculativeDecoding"`

	// LaunchParameters preserves ordered arguments that affect performance.
	// Reordering can change repeated flags and option values.
	LaunchParameters []string `json:"launchParameters"`

	// EffectiveLaunchParameters records the arguments the engine actually ran
	// with when they differ from the launch arguments.
	//
	// IT IS EVIDENCE AND NOT A MATCHING CRITERION, because it says what the
	// engine did with the launch arguments rather than what the deployment
	// asked for. A deployment that launches with the recorded launch
	// arguments runs the recorded effective arguments on the recorded engine
	// build.
	//
	// IT IS ABSENT WHEN THEY AGREE, because a copy of the launch arguments
	// says nothing the launch arguments do not.
	EffectiveLaunchParameters []string `json:"effectiveLaunchParameters,omitempty"`

	// EngineEnv records the engine environment the measurement ran under.
	//
	// IT IS THE COMPLETE SET THE SOURCE CAN PROVE, because a partial set
	// would let a deployment with an unknown engine variable reuse the
	// measurement. A deployment has to match every entry and must carry
	// nothing the measurement did not.
	//
	// IT IS EVIDENCE AND NOT A MATCHING CRITERION, because the environment
	// of the measurement is what the measurement ran under and the deployment
	// has to be checked against it by the caller that owns the deployment.
	EngineEnv []string `json:"engineEnv,omitempty"`

	// EngineEnvComplete says that EngineEnv is every environment variable the
	// measurement ran under.
	//
	// IT IS SET ONLY BY A SOURCE THAT OBSERVED THE WHOLE ENVIRONMENT, because
	// an environment the source did not observe is not an environment that was
	// empty. A source that could not watch the environment says so here and
	// the profile cannot be matched against a deployment, because the
	// deployment would be checked against a set that does not say what the
	// measurement ran under.
	EngineEnvComplete bool `json:"engineEnvComplete,omitempty"`

	// EffectiveDefaults records the effective values of the engine arguments
	// the source did not pass.
	//
	// IT IS EVIDENCE AND NOT A MATCHING CRITERION, because a default the
	// engine chose for itself is not a default of the engine build. A
	// deployment may rely on one only when its image, its launch arguments
	// and its environment are the ones the measurement ran under.
	EffectiveDefaults map[string]string `json:"effectiveDefaults,omitempty"`
}

// Topology is the shape of one scaling replica.
//
// EVERY FIELD IS REQUIRED, because a parallelism width the source did not
// state is not a wildcard.
type Topology struct {
	// GPUsPerReplica is how many GPUs one scaling replica uses.
	GPUsPerReplica int `json:"gpusPerReplica"`

	// TensorParallel is the tensor parallel width.
	TensorParallel int `json:"tensorParallel"`

	// PipelineParallel is the pipeline parallel width.
	PipelineParallel int `json:"pipelineParallel"`

	// DataParallel is the data parallel width.
	DataParallel int `json:"dataParallel"`

	// LocalDataParallel is the local data parallel width.
	LocalDataParallel int `json:"localDataParallel"`

	// ContextParallel is the context parallel width.
	ContextParallel int `json:"contextParallel"`

	// ExpertParallel is the expert parallel width.
	ExpertParallel int `json:"expertParallel"`
}

// Normalized returns the topology in its canonical form.
//
// IT REPLACES AN UNSTATED WIDTH WITH ONE, because a width a source did not
// state is the width of one and a width of zero names no deployment.
func (t Topology) Normalized() Topology {
	if t.TensorParallel <= 0 {
		t.TensorParallel = 1
	}
	if t.PipelineParallel <= 0 {
		t.PipelineParallel = 1
	}
	if t.DataParallel <= 0 {
		t.DataParallel = 1
	}
	if t.LocalDataParallel <= 0 {
		t.LocalDataParallel = 1
	}
	if t.ContextParallel <= 0 {
		t.ContextParallel = 1
	}
	if t.ExpertParallel <= 0 {
		t.ExpertParallel = 1
	}
	return t
}

// Workload is the workload envelope the measurements were taken under.
type Workload struct {
	// ID identifies the workload.
	ID string `json:"id"`

	// Revision pins the workload definition.
	Revision string `json:"revision"`

	// Modality is the modality of the requests.
	Modality string `json:"modality"`

	// InputLengths describes the input token lengths.
	InputLengths LengthDistribution `json:"inputLengths"`

	// OutputLengths describes the output token lengths.
	OutputLengths LengthDistribution `json:"outputLengths"`

	// CacheBehavior says how the prefix cache treated the requests.
	CacheBehavior CacheBehavior `json:"cacheBehavior"`

	// ArrivalModel says how the load was offered.
	ArrivalModel ClientModel `json:"arrivalModel"`
}

// LengthDistribution describes a token length distribution.
//
// A LENGTH THE SOURCE DID NOT PUBLISH IS ABSENT, because a zero length is a
// measurement and a missing one is not.
type LengthDistribution struct {
	// Min is the shortest measured length.
	Min *int `json:"min,omitempty"`

	// Max is the longest measured length.
	Max *int `json:"max,omitempty"`

	// Mean is the mean measured length.
	Mean *float64 `json:"mean,omitempty"`

	// Count is how many requests the distribution was measured over.
	Count int `json:"count"`
}

// MeasuredPoint is one measured load point.
type MeasuredPoint struct {
	// Load is the offered load.
	Load LoadLevel `json:"load"`

	// Throughput is what the system sustained at that load.
	Throughput Throughput `json:"throughput"`

	// Latencies are the measured latency statistics.
	Latencies []Latency `json:"latencies"`

	// Requests counts the requests the load point ran.
	Requests RequestCounts `json:"requests"`

	// Duration is how long the load point ran.
	Duration MeasurementDuration `json:"duration"`

	// SLO records whether the run met its latency constraint.
	SLO *SLOOutcome `json:"slo,omitempty"`

	// Saturation records an explicit saturation measurement.
	Saturation *Saturation `json:"saturation,omitempty"`
}

// LoadLevel is the offered load of a measured point.
type LoadLevel struct {
	// Kind is what the load varies.
	Kind LoadKind `json:"kind"`

	// Value is the offered load.
	Value float64 `json:"value"`

	// ClientModel says how the load was offered.
	ClientModel ClientModel `json:"clientModel"`

	// Unit is the unit of Value.
	Unit string `json:"unit"`
}

// Throughput is a measured throughput.
type Throughput struct {
	// Metric is the numerator.
	Metric ThroughputMetric `json:"metric"`

	// Value is the measured rate.
	Value float64 `json:"value"`

	// Unit is the unit of Value.
	Unit string `json:"unit"`

	// Denominator says what the rate is counted over.
	Denominator Denominator `json:"denominator"`

	// TokenCountConvention says which tokens the rate counted.
	TokenCountConvention TokenCountConvention `json:"tokenCountConvention"`

	// TokenTotals holds the token totals the rate was computed from.
	//
	// IT IS ABSENT WHEN THE SOURCE DID NOT PUBLISH THEM, because a missing
	// measurement stays missing.
	TokenTotals *TokenTotals `json:"tokenTotals,omitempty"`
}

// TokenTotals holds the token totals behind a measured rate.
type TokenTotals struct {
	// InputTokens is the counted prompt token total.
	InputTokens *float64 `json:"inputTokens,omitempty"`

	// OutputTokens is the counted generated token total.
	OutputTokens *float64 `json:"outputTokens,omitempty"`
}

// RequestCounts counts the requests a load point ran.
type RequestCounts struct {
	// Successful is how many requests completed.
	Successful int `json:"successful"`

	// Failed is how many requests failed.
	Failed int `json:"failed"`
}

// MeasurementDuration is how long a load point ran.
type MeasurementDuration struct {
	// WarmUpSeconds is the warm-up time that was excluded.
	WarmUpSeconds float64 `json:"warmUpSeconds"`

	// MeasuredSeconds is the measured duration.
	MeasuredSeconds float64 `json:"measuredSeconds"`
}

// Latency is one measured latency statistic.
type Latency struct {
	// Metric is what was measured.
	Metric LatencyMetric `json:"metric"`

	// Quantile is the quantile the value is the quantile of.
	Quantile float64 `json:"quantile"`

	// Value is the measured value.
	Value float64 `json:"value"`

	// Unit is the unit of Value.
	Unit string `json:"unit"`

	// SampleCount is how many samples the statistic was computed over.
	SampleCount int `json:"sampleCount"`

	// Censoring says how the samples were censored.
	Censoring string `json:"censoring"`
}

// SLOOutcome records whether a run met its latency constraint.
type SLOOutcome struct {
	// ConstraintsSatisfied says whether the run met the constraint.
	ConstraintsSatisfied bool `json:"constraintsSatisfied"`

	// TTFTConstraintMilliseconds is the time to first token constraint.
	TTFTConstraintMilliseconds float64 `json:"ttftConstraintMilliseconds"`

	// TPOTConstraintMilliseconds is the time per output token constraint.
	TPOTConstraintMilliseconds float64 `json:"tpotConstraintMilliseconds"`
}

// Saturation records an explicit saturation measurement.
type Saturation struct {
	// Measured says whether saturation was measured.
	Measured bool `json:"measured"`

	// Note says how it was established.
	Note string `json:"note,omitempty"`
}
