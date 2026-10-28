package priors

import (
	"fmt"
	"strings"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

// Shape is the six-tuple that identifies one deployment shape.
//
// IT IS WHAT A PROFILE IS KNOWN BY, because a catalog that names its profiles
// by a name lets two profiles claim one name, and a deployment that selects a
// profile by a name has to know that name before it knows anything else about
// the catalog.
//
// THE SIX ELEMENTS ARE THE ROLE, THE MODEL, THE QUANTIZATION, THE ENGINE, THE
// HARDWARE AND THE TOPOLOGY, because a measurement of one model under one
// recipe on one engine on one accelerator in one shape is one measurement.
type Shape struct {
	// Role is the serving role the shape serves.
	Role RoleKind `json:"role"`

	// Model is the canonical model repository.
	Model string `json:"model"`

	// Quantization is the canonical quantization token.
	Quantization string `json:"quantization"`

	// Engine is the engine family.
	Engine string `json:"engine"`

	// Hardware is the canonical accelerator family.
	Hardware string `json:"hardware"`

	// Topology is the normalized shape of one scaling replica.
	Topology Topology `json:"topology"`
}

// Shape returns the shape of a profile.
//
// IT NORMALIZES EVERY ELEMENT, because a profile written under one spelling of
// a model and a deployment written under another name one shape.
func (p Profile) Shape() Shape {
	return Shape{
		Role:         p.Role,
		Model:        CanonicalModelName(p.Identity.Model.Name),
		Quantization: CanonicalQuantization(p.Identity.Model.Quantization),
		Engine:       CanonicalEngineFamily(p.Identity.Engine.Image),
		Hardware:     CanonicalAcceleratorFamily(p.Identity.Hardware.Accelerator),
		Topology:     p.Topology.Normalized(),
	}
}

// DocumentShape returns the shape of a written profile.
//
// IT NORMALIZES EVERY ELEMENT, because the shape is what a profile is known by
// and a reader has to reach the same shape from the file and from the runtime.
func DocumentShape(d ProfileDocument) Shape {
	return Shape{
		Role:         RoleKind(d.Role),
		Model:        CanonicalModelName(d.Model),
		Quantization: CanonicalQuantization(d.Quantization),
		Engine:       CanonicalEngineFamily(d.Engine),
		Hardware:     CanonicalAcceleratorFamily(d.Hardware),
		Topology:     NormalizeTopologyDocument(d.Topology),
	}
}

// NormalizeTopologyDocument returns a written topology in its canonical form.
//
// IT REPLACES AN UNSTATED WIDTH WITH ONE, because a width a source did not
// state is the width of one and a width of zero names no deployment.
func NormalizeTopologyDocument(d TopologyDocument) Topology {
	topology := Topology(d)
	return topology.Normalized()
}

// Shape returns the shape a request names.
//
// IT READS THE REQUEST AND NOT A PROFILE, because the request is what a
// deployment states and the profile is what the catalog holds.
func (r Request) Shape() Shape {
	return Shape{
		Role:         r.Role,
		Model:        CanonicalModelName(r.Identity.Model.Name),
		Quantization: CanonicalQuantization(r.Identity.Model.Quantization),
		Engine:       CanonicalEngineFamily(r.Identity.Engine.Image),
		Hardware:     CanonicalAcceleratorFamily(r.Identity.Hardware.Accelerator),
		Topology:     r.Topology.Normalized(),
	}
}

// String renders the shape in a stable form.
//
// IT NAMES THE SIX ELEMENTS, because a refusal that names the shape it could
// not resolve tells the reader what to change.
func (s Shape) String() string {
	return fmt.Sprintf(
		"%s/%s/%s/%s/%s/gpus=%d,tp=%d,ep=%d,pp=%d,dp=%d,ldp=%d,cp=%d",
		s.Role, s.Model, s.Quantization, s.Engine, s.Hardware,
		s.Topology.GPUsPerReplica, s.Topology.TensorParallel, s.Topology.ExpertParallel,
		s.Topology.PipelineParallel, s.Topology.DataParallel, s.Topology.LocalDataParallel,
		s.Topology.ContextParallel,
	)
}

// CompareShapes orders two shapes.
//
// IT ORDERS BY THE SIX ELEMENTS AND THEN BY THE TOPOLOGY WIDTHS, because a
// catalog that is written in the order the profiles were measured changes its
// bytes every time a measurement is added.
func CompareShapes(left, right Shape) int {
	for _, pair := range [][2]string{
		{string(left.Role), string(right.Role)},
		{left.Model, right.Model},
		{left.Quantization, right.Quantization},
		{left.Engine, right.Engine},
		{left.Hardware, right.Hardware},
	} {
		if pair[0] != pair[1] {
			return strings.Compare(pair[0], pair[1])
		}
	}
	leftWidths := shapeWidths(left.Topology)
	rightWidths := shapeWidths(right.Topology)
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
	if want.Role != got.Role {
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
	if want.Hardware != "" && want.Hardware != got.Hardware {
		return false
	}
	return want.Topology == got.Topology
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
