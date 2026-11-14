package priors

import (
	"fmt"
	"strings"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

// StageShape is the normalized shape of one stage in a disaggregated profile.
type StageShape struct {
	// Replicas is the replica count of this stage within one minimal coupled serving unit.
	Replicas int `json:"replicas"`

	// Hardware is the canonical accelerator family for this stage.
	Hardware string `json:"hardware"`

	// Topology is the normalized shape of one replica in this stage.
	Topology Topology `json:"topology"`
}

// Shape identifies one deployment shape for catalog indexing and matching.
//
// IT IS WHAT A PROFILE IS KNOWN BY, because a catalog that names its profiles
// by a name lets two profiles claim one name, and a deployment that selects a
// profile by a name has to know that name before it knows anything else about
// the catalog.
type Shape struct {
	// Mode is the serving architecture the shape describes.
	Mode ProfileMode `json:"mode"`

	// Model is the canonical model repository.
	Model string `json:"model"`

	// Quantization is the canonical quantization token.
	Quantization string `json:"quantization"`

	// Engine is the engine family.
	Engine string `json:"engine"`

	// Hardware is the canonical accelerator family for an aggregated shape.
	Hardware string `json:"hardware,omitempty"`

	// Topology is the normalized shape of one scaling replica in an aggregated shape.
	Topology Topology `json:"topology,omitempty"`

	// Prefill is the prefill stage shape in a disaggregated shape.
	Prefill *StageShape `json:"prefill,omitempty"`

	// Decode is the decode stage shape in a disaggregated shape.
	Decode *StageShape `json:"decode,omitempty"`
}

// Shape returns the normalized shape of a profile.
//
// IT NORMALIZES EVERY ELEMENT, because a profile written under one spelling of
// a model and a deployment written under another name one shape.
func (p Profile) Shape() Shape {
	shape := Shape{
		Mode:         p.Mode,
		Model:        CanonicalModelName(p.Identity.Model.Name),
		Quantization: CanonicalQuantization(p.Identity.Model.Quantization),
		Engine:       CanonicalEngineFamily(p.Identity.Engine.Image),
	}
	if p.Mode == ModeDisaggregated {
		shape.Prefill = stageShapeFromStage(p.Prefill)
		shape.Decode = stageShapeFromStage(p.Decode)
		return shape
	}
	shape.Hardware = CanonicalAcceleratorFamily(p.Identity.Hardware.Accelerator)
	shape.Topology = p.Topology.Normalized()
	return shape
}

// DocumentShape returns the normalized shape of a written profile.
//
// IT NORMALIZES EVERY ELEMENT, because the shape is what a profile is known by
// and a reader has to reach the same shape from the file and from the runtime.
func DocumentShape(d ProfileDocument) Shape {
	mode := ProfileMode(d.Mode)
	shape := Shape{
		Mode:         mode,
		Model:        CanonicalModelName(d.Model),
		Quantization: CanonicalQuantization(d.Quantization),
		Engine:       CanonicalEngineFamily(d.Engine),
	}
	if mode == ModeDisaggregated {
		shape.Prefill = stageShapeFromDocument(d.Prefill)
		shape.Decode = stageShapeFromDocument(d.Decode)
		return shape
	}
	shape.Hardware = CanonicalAcceleratorFamily(d.Hardware)
	if d.Topology != nil {
		shape.Topology = NormalizeTopologyDocument(*d.Topology)
	}
	return shape
}

// NormalizeTopologyDocument returns a written topology in its canonical form.
//
// IT REPLACES AN UNSTATED WIDTH WITH ONE, because a width a source did not
// state is the width of one and a width of zero names no deployment.
func NormalizeTopologyDocument(d TopologyDocument) Topology {
	topology := Topology(d)
	return topology.Normalized()
}

// Shape returns the normalized shape a request names.
//
// IT READS THE REQUEST AND NOT A PROFILE, because the request is what a
// deployment states and the profile is what the catalog holds.
func (r Request) Shape() Shape {
	shape := Shape{
		Mode:         r.Mode,
		Model:        CanonicalModelName(r.Identity.Model.Name),
		Quantization: CanonicalQuantization(r.Identity.Model.Quantization),
		Engine:       CanonicalEngineFamily(r.Identity.Engine.Image),
	}
	if r.Mode == ModeDisaggregated {
		shape.Prefill = stageShapeFromStage(r.Prefill)
		shape.Decode = stageShapeFromStage(r.Decode)
		return shape
	}
	shape.Hardware = CanonicalAcceleratorFamily(r.Identity.Hardware.Accelerator)
	shape.Topology = r.Topology.Normalized()
	return shape
}

func stageShapeFromStage(stage *DisaggregatedStage) *StageShape {
	if stage == nil {
		return nil
	}
	return &StageShape{
		Replicas: stage.Replicas,
		Hardware: CanonicalAcceleratorFamily(stage.Hardware.Accelerator),
		Topology: stage.Topology.Normalized(),
	}
}

func stageShapeFromDocument(stage *StageDocument) *StageShape {
	if stage == nil {
		return nil
	}
	return &StageShape{
		Replicas: stage.Replicas,
		Hardware: CanonicalAcceleratorFamily(stage.Hardware),
		Topology: NormalizeTopologyDocument(stage.Topology),
	}
}

// String renders the shape in a stable, compact form suitable for workload IDs.
func (s Shape) String() string {
	if s.Mode == ModeDisaggregated {
		return fmt.Sprintf(
			"disaggregated/%s/%s/%s/p=%s/d=%s",
			s.Model, s.Quantization, s.Engine,
			formatStageShape(s.Prefill),
			formatStageShape(s.Decode),
		)
	}
	return fmt.Sprintf(
		"%s/%s/%s/%s/%s/gpus=%d,tp=%d,ep=%d,pp=%d,dp=%d,ldp=%d,cp=%d",
		s.Mode, s.Model, s.Quantization, s.Engine, s.Hardware,
		s.Topology.GPUsPerReplica, s.Topology.TensorParallel, s.Topology.ExpertParallel,
		s.Topology.PipelineParallel, s.Topology.DataParallel, s.Topology.LocalDataParallel,
		s.Topology.ContextParallel,
	)
}

func formatStageShape(stage *StageShape) string {
	if stage == nil {
		return "missing"
	}
	return fmt.Sprintf(
		"%dx(%s:gpus=%d,tp=%d,ep=%d,pp=%d,dp=%d,ldp=%d,cp=%d)",
		stage.Replicas, stage.Hardware,
		stage.Topology.GPUsPerReplica, stage.Topology.TensorParallel, stage.Topology.ExpertParallel,
		stage.Topology.PipelineParallel, stage.Topology.DataParallel, stage.Topology.LocalDataParallel,
		stage.Topology.ContextParallel,
	)
}

// CompareShapes orders two shapes deterministically across all shape fields.
//
// IT ORDERS BY MODE, MODEL, QUANTIZATION, ENGINE AND THEN STAGE OR TOPOLOGY
// WIDTHS, because a catalog that is written in the order the profiles were
// measured changes its bytes every time a measurement is added.
func CompareShapes(left, right Shape) int {
	for _, pair := range [][2]string{
		{string(left.Mode), string(right.Mode)},
		{left.Model, right.Model},
		{left.Quantization, right.Quantization},
		{left.Engine, right.Engine},
	} {
		if pair[0] != pair[1] {
			return strings.Compare(pair[0], pair[1])
		}
	}
	if left.Mode == ModeDisaggregated {
		if cmp := compareStageShapes(left.Prefill, right.Prefill); cmp != 0 {
			return cmp
		}
		return compareStageShapes(left.Decode, right.Decode)
	}
	if left.Hardware != right.Hardware {
		return strings.Compare(left.Hardware, right.Hardware)
	}
	return compareTopologyWidths(left.Topology, right.Topology)
}

func compareStageShapes(left, right *StageShape) int {
	if left == nil || right == nil {
		if left == right {
			return 0
		}
		if left == nil {
			return -1
		}
		return 1
	}
	if left.Replicas != right.Replicas {
		return left.Replicas - right.Replicas
	}
	if left.Hardware != right.Hardware {
		return strings.Compare(left.Hardware, right.Hardware)
	}
	return compareTopologyWidths(left.Topology, right.Topology)
}

func compareTopologyWidths(left, right Topology) int {
	leftWidths := shapeWidths(left)
	rightWidths := shapeWidths(right)
	for i := range leftWidths {
		if leftWidths[i] != rightWidths[i] {
			return leftWidths[i] - rightWidths[i]
		}
	}
	return 0
}

// shapeWidths returns the topology widths in the order the catalog is sorted by.
func shapeWidths(topology Topology) [7]int {
	return [7]int{
		topology.GPUsPerReplica,
		topology.TensorParallel,
		topology.ExpertParallel,
		topology.PipelineParallel,
		topology.DataParallel,
		topology.LocalDataParallel,
		topology.ContextParallel,
	}
}

// shapeMatches says whether a profile's shape is the one a request names.
//
// IT COMPARES THE MODEL BY NAME, because a deployment names a model without its
// organization and the catalog records it with one.
//
// IT ACCEPTS AN ELEMENT THE REQUEST DOES NOT STATE, because a deployment that
// states no quantization has contradicted nothing and has chosen nothing
// either. The selection refuses when several profiles match, so an unstated
// element never picks a profile on its own.
func shapeMatches(want, got Shape) bool {
	if want.Mode != got.Mode {
		return false
	}
	if !ModelNameMatches(want.Model, got.Model) {
		return false
	}
	if want.Quantization != "" && want.Quantization != got.Quantization {
		return false
	}
	if want.Engine != "" && want.Engine != got.Engine {
		return false
	}
	if want.Mode == ModeDisaggregated {
		if want.Prefill == nil || got.Prefill == nil || want.Decode == nil || got.Decode == nil {
			return false
		}
		return stageShapeMatches(*want.Prefill, *got.Prefill) &&
			stageShapeMatches(*want.Decode, *got.Decode)
	}
	if want.Hardware != "" && want.Hardware != got.Hardware {
		return false
	}
	return want.Topology == (Topology{}) || want.Topology.Normalized() == got.Topology.Normalized()
}

func stageShapeMatches(want, got StageShape) bool {
	if want.Replicas != got.Replicas {
		return false
	}
	if want.Hardware != "" && want.Hardware != got.Hardware {
		return false
	}
	return want.Topology == (Topology{}) || want.Topology.Normalized() == got.Topology.Normalized()
}

// CanonicalEngineFamily returns the engine family an engine name belongs to.
//
// IT KEEPS AN ENGINE THIS OPERATOR DOES NOT KNOW AS IT IS, because an engine
// this operator cannot place is still one engine and two of them are two
// shapes.
func CanonicalEngineFamily(image string) string {
	if family := EngineFamily(image); family != "" {
		return family
	}
	return strings.TrimSpace(image)
}

// CanonicalAcceleratorFamily returns the accelerator family a name belongs to.
//
// IT KEEPS AN ACCELERATOR THIS OPERATOR DOES NOT KNOW AS IT IS, because an
// accelerator this operator cannot name is still one accelerator.
func CanonicalAcceleratorFamily(name string) string {
	if family := nodefeature.ResolveAcceleratorFamily(name); family != "" {
		return family
	}
	return strings.TrimSpace(name)
}
