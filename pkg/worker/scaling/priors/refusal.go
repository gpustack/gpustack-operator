package priors

import (
	"fmt"
	"sort"
	"strings"
)

// RefusalReason is the bounded set of reasons a resolution can be refused for.
//
// THE REASONS ARE BOUNDED, so a status field or a condition message built from
// a refusal cannot grow with the size of a catalog.
type RefusalReason string

const (
	// RefusalCapacityPriorUnresolved says no capacity could be derived.
	//
	// IT COVERS A PROFILE THAT DOES NOT MATCH THE DEPLOYMENT, A PROFILE THAT
	// MATCHES BUT HAS NO FEASIBLE LOAD POINT, AND A ROLE THAT NEEDS NO
	// CAPACITY.
	RefusalCapacityPriorUnresolved RefusalReason = "CapacityPriorUnresolved"

	// RefusalInsufficientLoadCoverage says the measured load curve is too
	// sparse to carry a capacity.
	//
	// IT COVERS A PROFILE WITH FEWER THAN TWO MEASURED LOAD LEVELS AND A
	// PROFILE WITH NO EVIDENCE OF THE CAPACITY BOUNDARY.
	RefusalInsufficientLoadCoverage RefusalReason = "InsufficientLoadCoverage"

	// RefusalIncompatibleMetric says the profile does not measure what the
	// role needs.
	//
	// IT COVERS A PROFILE WHOSE THROUGHPUT NUMERATOR IS NOT THE ROLE'S
	// NUMERATOR, A PROFILE THAT DOES NOT MEASURE THE REQUIRED LATENCY
	// STATISTIC, AND A PROFILE WHOSE CACHE CONVENTION CANNOT BE ESTABLISHED.
	RefusalIncompatibleMetric RefusalReason = "IncompatibleMetric"
)

// maxRefusalDetail bounds how many keys a refusal detail may carry.
const maxRefusalDetail = 8

// RefusalError explains why no capacity prior was resolved.
//
// IT IS AN ERROR, so a caller that cannot use a refusal returns it unchanged
// and a caller that can report it reads its reason.
type RefusalError struct {
	// Reason is the bounded reason the resolution was refused for.
	Reason RefusalReason `json:"reason"`

	// Message says what is missing, in one sentence.
	Message string `json:"message"`

	// Role is the role the resolution was for.
	Role string `json:"role,omitempty"`

	// Shape is the shape the resolution was about, when one was found.
	Shape string `json:"shape,omitempty"`

	// Detail carries the specific evidence that was missing.
	//
	// IT IS BOUNDED, because it ends up in a status field.
	Detail map[string]string `json:"detail,omitempty"`
}

// Error returns the refusal message.
//
// IT EXISTS SO A REFUSAL CAN BE RETURNED WHERE AN ERROR IS EXPECTED.
func (r *RefusalError) Error() string {
	if r == nil {
		return ""
	}
	if r.Message != "" {
		return fmt.Sprintf("%s: %s", r.Reason, r.Message)
	}
	return string(r.Reason)
}

// NewRefusal builds a refusal with a bounded detail map.
//
// IT EXISTS SO AN ADAPTER OUTSIDE THIS PACKAGE CAN REPORT WHY IT REFUSED A
// SOURCE, because the reason and the detail end up in the catalog.
func NewRefusal(reason RefusalReason, message string, detail map[string]string) *RefusalError {
	return newRefusal(reason, message, detail)
}

// newRefusal builds a refusal with a bounded detail map.
func newRefusal(reason RefusalReason, message string, detail map[string]string) *RefusalError {
	refusal := &RefusalError{
		Reason:  reason,
		Message: message,
		Detail:  map[string]string{},
	}
	for key, value := range detail {
		if len(refusal.Detail) >= maxRefusalDetail {
			break
		}
		refusal.Detail[key] = value
	}
	if len(refusal.Detail) == 0 {
		refusal.Detail = nil
	}
	return refusal
}

// identityMismatch lists the identity fields that differ, in a stable order.
//
// THE ORDER IS FIXED, so two runs of the same comparison report the same
// first difference.
//
// THE REQUEST SIDE IS THE DEPLOYMENT, and an element the deployment does not
// state constrains nothing, because a deployment that states no quantization
// has contradicted no measurement.
func identityMismatch(want, got Identity) []string {
	mismatched := make([]string, 0, 9)
	appendIf := func(name string, equal bool) {
		if !equal {
			mismatched = append(mismatched, name)
		}
	}
	// THE MODEL IS COMPARED THROUGH ITS NORMALIZED FORM, because a deployment
	// declares the model without its organization and a source declares it with
	// one, and the same recipe is spelled several ways.
	appendIf("model.name", ModelNameMatches(want.Model.Name, got.Model.Name))
	appendIf("model.revision", RevisionMatches(want.Model.Revision, got.Model.Revision))
	appendIf("model.tokenizer", TokenizerMatches(want.Model.Tokenizer, got.Model.Tokenizer))
	if got.Model.Quantization != "" {
		appendIf("model.quantization", CanonicalQuantization(want.Model.Quantization) == CanonicalQuantization(got.Model.Quantization))
	}
	// THE ENGINE IS COMPARED BY FAMILY, because a deployment names the image it
	// runs and a source names the image it measured, and the two images differ
	// in a tag or a digest while the engine is the same one.
	appendIf("engine.image", engineImageMatches(want.Engine.Image, got.Engine.Image))
	// THE ENGINE BUILD IS COMPARED ONLY WHEN BOTH SIDES STATE ONE, because the
	// catalog is a measurement and not a configuration, and a deployment that
	// names no build says nothing about the build it runs.
	if want.Engine.Build != "" && got.Engine.Build != "" {
		appendIf("engine.build", want.Engine.Build == got.Engine.Build)
	}
	appendIf("hardware.accelerator", NormalizeHardwareIdentity(want.Hardware).Accelerator == NormalizeHardwareIdentity(got.Hardware).Accelerator)
	// THE MEMORY AND THE FORM FACTOR ARE COMPARED ONLY WHEN THE DEPLOYMENT
	// STATES ONE, because a deployment whose instance type names no memory
	// agrees with a measurement of any memory.
	if got.Hardware.MemoryGB > 0 {
		appendIf("hardware.memoryGB", want.Hardware.MemoryGB == got.Hardware.MemoryGB)
	}
	if got.Hardware.FormFactor != "" {
		appendIf("hardware.formFactor", want.Hardware.FormFactor == got.Hardware.FormFactor)
	}
	return mismatched
}

// engineImageMatches says whether two engine images belong to the same engine
// family.
//
// IT ACCEPTS THE EXACT IMAGE, because a source and a deployment that name the
// same image agree on more than the family.
//
// IT ACCEPTS A SHARED FAMILY, because the tag and the digest of one engine
// build are written many ways and a comparison of two spellings says nothing
// about the engine behind them.
//
// IT REFUSES AN IMAGE THAT NAMES NO KNOWN FAMILY, because an image this
// operator cannot place is not one it may match against a measurement.
func engineImageMatches(want, got string) bool {
	if want == got {
		return true
	}
	wantFamily, gotFamily := EngineFamily(want), EngineFamily(got)
	return wantFamily != "" && wantFamily == gotFamily
}

// servingConfigMismatch lists the serving configuration fields that differ.
//
// IT COMPARES ONLY THE FIELDS THE PROFILE STATES, because the catalog is a
// measurement and not a configuration: a profile that states no serving setting
// constrains nothing, and a profile that states one is compared on that one
// alone.
func servingConfigMismatch(want, got ServingConfig) []string {
	mismatched := make([]string, 0, 6)
	appendIf := func(name, stated, deployed string) {
		if stated == "" {
			return
		}
		if stated != deployed {
			mismatched = append(mismatched, name)
		}
	}
	appendIf("servingConfig.router", want.Router, got.Router)
	appendIf("servingConfig.kvCacheDType", want.KVCacheDType, got.KVCacheDType)
	appendIf("servingConfig.offload", want.Offload, got.Offload)
	appendIf("servingConfig.prefixCaching", want.PrefixCaching, got.PrefixCaching)
	appendIf("servingConfig.speculativeDecoding", want.SpeculativeDecoding, got.SpeculativeDecoding)
	if conflicts := launchParameterConflicts(want, got); len(conflicts) > 0 {
		mismatched = append(mismatched, "servingConfig.launchParameters")
	}
	// THE EFFECTIVE LAUNCH PARAMETERS ARE NOT COMPARED, because they say what
	// the engine did with the launch arguments rather than what the
	// deployment asked for.
	return mismatched
}

// launchParameterConflicts returns the flags the deployment passes that the
// profile records with a different value.
//
// IT COMPARES THE FLAGS THE PROFILE RECORDS AND NOT THE WHOLE LIST, because a
// deployment names the flags it sets and a source names the flags it ran
// with, and the two lists differ in every flag the other one says nothing
// about.
//
// IT READS THE EFFECTIVE DEFAULTS AS WELL, because a default the engine chose
// for itself is the value a deployment that omits the flag runs with.
//
// IT NAMES NO VALUE, because a value the user wrote belongs to nobody else.
func launchParameterConflicts(want, got ServingConfig) []string {
	recorded := map[string]string{}
	launch, err := ParseLaunchParameters(want.LaunchParameters)
	if err != nil {
		return []string{"the catalog records unreadable launch arguments"}
	}
	for _, parameter := range launch {
		recorded[parameter.Flag] = parameter.Value
	}
	for flag, value := range want.EffectiveDefaults {
		if _, found := recorded[flag]; !found {
			recorded[flag] = value
		}
	}
	deployment, err := ParseLaunchParameters(got.LaunchParameters)
	if err != nil {
		return []string{"the deployment carries unreadable launch arguments"}
	}
	var conflicts []string
	for _, parameter := range deployment {
		prior, known := recorded[parameter.Flag]
		if !known || prior == parameter.Value {
			continue
		}
		conflicts = append(conflicts, parameter.Flag)
	}
	sort.Strings(conflicts)
	return conflicts
}

// topologyMismatch lists the topology fields that differ.
func topologyMismatch(want, got Topology) []string {
	mismatched := make([]string, 0, 7)
	appendIf := func(name string, equal bool) {
		if !equal {
			mismatched = append(mismatched, name)
		}
	}
	appendIf("topology.gpusPerReplica", want.GPUsPerReplica == got.GPUsPerReplica)
	appendIf("topology.tensorParallel", want.TensorParallel == got.TensorParallel)
	appendIf("topology.pipelineParallel", want.PipelineParallel == got.PipelineParallel)
	appendIf("topology.dataParallel", want.DataParallel == got.DataParallel)
	appendIf("topology.localDataParallel", want.LocalDataParallel == got.LocalDataParallel)
	appendIf("topology.contextParallel", want.ContextParallel == got.ContextParallel)
	appendIf("topology.expertParallel", want.ExpertParallel == got.ExpertParallel)
	return mismatched
}

// formatMismatch renders the mismatched identity fields for a refusal detail.
func formatMismatch(mismatched []string) string {
	if len(mismatched) == 0 {
		return ""
	}
	sorted := append([]string(nil), mismatched...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}
