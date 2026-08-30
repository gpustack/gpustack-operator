package preflight

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/sets"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// fakePreflighter stands in for a manufacturer's real preflighter, which drives a binding this
// package's test binary cannot link. It records the options it was built with, so the pass-through
// the preflighter options a manufacturer is built with are observable.
type fakePreflighter struct {
	opts   device.PreflighterOptions
	groups device.DevicesGroupList
	checks []device.PreflightCheck
	note   string
}

// panickingPreflighter stands in for vendor code that dies on a node whose driver is in a state it
// did not expect -- the case this command exists to be run in.
type panickingPreflighter struct{}

func (panickingPreflighter) PreflightAccelerator(device.DevicesGroupList) device.PreflightGroup {
	panic("boom: nil map read in the vendor driver")
}

func (f *fakePreflighter) PreflightAccelerator(groups device.DevicesGroupList) device.PreflightGroup {
	f.groups = groups
	return device.PreflightGroup{
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Checks:       f.checks,
		Note:         f.note,
	}
}

// withRegistry swaps the manufacturer registry for the duration of one test. It is a package var
// for the build-tag split, and off linux it is empty, so a test that did not swap it could only
// ever exercise the no-creator branch.
func withRegistry(
	t *testing.T, creators map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter,
) {
	t.Helper()
	restore := supportedPreflighterCreators
	t.Cleanup(func() { supportedPreflighterCreators = restore })
	supportedPreflighterCreators = creators
}

// A manufacturer that carries no check has four different reasons for it, and an empty group is
// read as a node that passed. Each reason is therefore pinned to the words that carry it.
func TestPreflight_Dispatch(t *testing.T) {
	oneCheck := []device.PreflightCheck{{
		Accelerator: "GPU-0",
		Capability:  "mig-partitioning",
		State:       device.PreflightStateOK,
		Depth:       device.PreflightDepthDeclared,
	}}
	// Named, because an unnamed group is a failed detection and a failed detection stops the pass
	// before any of the dispatch below is reached -- which is not what these cases are about.
	oneGroup := device.DevicesGroupList{{
		ID:           "a100",
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Name:         "NVIDIA A100",
		Accelerators: []workercore.Accelerator{{ID: "GPU-0"}},
	}}

	testCases := []struct {
		name       string
		registered bool
		groups     device.DevicesGroupList
		unmeasured bool
		checks     []device.PreflightCheck
		note       string
		wantNote   string
		wantChecks []device.PreflightCheck
	}{
		{
			// Checked before the three below: what is read for this manufacturer does not depend on
			// whether its hardware answered.
			name:     "a manufacturer with no preflighter says so even with accelerators present",
			groups:   oneGroup,
			checks:   oneCheck,
			wantNote: noteNoPreflighter,
		},
		{
			name:       "a manufacturer with no preflighter says so when its detect pass failed too",
			unmeasured: true,
			checks:     oneCheck,
			wantNote:   noteNoPreflighter,
		},
		{
			// Its accelerators may well be here; the pass that would have named them failed, so
			// nothing was read and the group must not read as a node that passed.
			name:       "a detect pass that could not measure is not a preflight that passed",
			registered: true,
			unmeasured: true,
			checks:     oneCheck,
			wantNote:   noteUnmeasured,
		},
		{
			name:       "a manufacturer with no accelerator detected says so",
			registered: true,
			checks:     oneCheck,
			wantNote:   noteNoAccelerator,
		},
		{
			// The fourth reason, and the one only the runner can catch: the preflighter ran over
			// real accelerators and read nothing. Left unsaid it is indistinguishable from a node
			// whose every capability is fine.
			name:       "a preflighter that read nothing says so rather than passing by omission",
			registered: true,
			groups:     oneGroup,
			wantNote:   noteNoCheck,
		},
		{
			// It knows why its own accelerators declared nothing; the generic sentence only has to
			// cover the manufacturer that did not say.
			name:       "a preflighter that explained why it read nothing keeps its own words",
			registered: true,
			groups:     oneGroup,
			note:       "this driver generation declares no shareable capability",
			wantNote:   "this driver generation declares no shareable capability",
		},
		{
			// The injection-only tier: no driver was read, and later stages append simulated rows
			// from the responder. Observed on a Hygon node, where the note explaining that no
			// driver precondition exists was absent from the report precisely because those rows
			// had made the list non-empty -- leaving a reader to infer a driver read that never
			// happened.
			name:       "a note survives the rows a later stage appends",
			registered: true,
			groups:     oneGroup,
			note:       "the hygon allocator reads no driver at allocation time",
			checks:     oneCheck,
			wantChecks: oneCheck,
			wantNote:   "the hygon allocator reads no driver at allocation time",
		},
		{
			name:       "a manufacturer with accelerators is preflighted",
			registered: true,
			groups:     oneGroup,
			checks:     oneCheck,
			wantChecks: oneCheck,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePreflighter{checks: tc.checks, note: tc.note}
			creators := map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{}
			if tc.registered {
				creators[nodefeature.ManufacturerNVIDIA] = func(opts device.PreflighterOptions) device.AcceleratorPreflighter {
					fake.opts = opts
					return fake
				}
			}
			withRegistry(t, creators)

			p := &Preflighter{manufacturers: sets.New(nodefeature.ManufacturerNVIDIA)}
			grp := p.preflight(context.Background(), nodefeature.ManufacturerNVIDIA,
				detection(tc.groups, tc.unmeasured), tc.groups, tc.unmeasured)

			assert.Equal(t, nodefeature.ManufacturerNVIDIA, grp.Manufacturer)
			assert.Equal(t, tc.wantNote, grp.Note, "note")
			assert.Equal(t, tc.wantChecks, grp.Checks, "checks")
			if tc.wantChecks == nil {
				return
			}
			assert.Equal(t, tc.groups, fake.groups, "the manufacturer's own groups, and no others")
		})
	}
}

// The five notes are only five answers if they are five different sentences. A duplicate would
// collapse two distinct facts into one, which is the failure this guards.
func TestPreflight_NotesAreDistinct(t *testing.T) {
	notes := []string{
		noteNoPreflighter, noteUnmeasured, noteNoAccelerator, noteNoCheck, noteDetectionFailed,
	}
	seen := sets.New[string]()
	for _, n := range notes {
		assert.NotEmpty(t, n)
		assert.False(t, seen.Has(n), "two reasons share one sentence: %q", n)
		seen.Insert(n)
	}
}

// The library tree is never removed once written, and both container questions skip an accelerator
// that can host no logical slice -- so a node whose cards all report none must not be left with a
// permanent directory it had nothing to measure with. Reachable on an unsupported family or runtime
// combination, where the detect pass names the accelerator but reports no sliceable capacity.
func TestPreflight_StagesNothingWhereNothingCanBeSliced(t *testing.T) {
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{
		nodefeature.ManufacturerAscend: func(device.PreflighterOptions) device.AcceleratorPreflighter {
			return &fakePreflighter{}
		},
	})
	imageRoot := withStagingRoots(t)
	require.NoError(t, os.MkdirAll(filepath.Join(imageRoot, nodefeature.ManufacturerAscend), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(imageRoot, nodefeature.ManufacturerAscend, "ld.so.preload"), []byte("x"), 0o644))

	root := fakeHostRoot(t)
	p := &Preflighter{
		host:          newHostExec(root),
		manufacturers: sets.New(nodefeature.ManufacturerAscend),
		runtime:       &hostRuntime{Name: "docker"},
	}

	// Detected, named, and carrying no sliceable capacity at all.
	groups := device.DevicesGroupList{{
		ID:           "910b",
		Manufacturer: nodefeature.ManufacturerAscend,
		Name:         "Ascend 910B",
		Accelerators: []workercore.Accelerator{{ID: "npu-0"}},
	}}

	p.preflight(context.Background(), nodefeature.ManufacturerAscend,
		detection(groups, false), groups, false)

	assert.NoDirExists(t, filepath.Join(root, deviceplugin.OperatorLibDir, nodefeature.ManufacturerAscend),
		"a tree that is never removed was staged for a node with nothing to measure")
}

// Detection is the floor the other two questions stand on, so a manufacturer that fails it is
// reported with them marked unanswerable rather than answered anyway: rows hung off a failed
// detection describe accelerators the report cannot identify, and an unnamed group has no id at all.
func TestPreflight_AFailedDetectionStopsTheRemainingQuestions(t *testing.T) {
	fake := &fakePreflighter{checks: []device.PreflightCheck{{
		Accelerator: "GPU-0", Capability: "mig-partitioning", State: device.PreflightStateOK,
	}}}
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{
		nodefeature.ManufacturerNVIDIA: func(device.PreflighterOptions) device.AcceleratorPreflighter {
			return fake
		},
	})

	// Accelerators that answered and carry no group name: counted, and unnameable.
	unnamed := device.DevicesGroupList{{
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Accelerators: []workercore.Accelerator{{ID: "GPU-0"}},
	}}

	p := &Preflighter{manufacturers: sets.New(nodefeature.ManufacturerNVIDIA)}
	grp := p.preflight(context.Background(), nodefeature.ManufacturerNVIDIA,
		detection(unnamed, false), unnamed, false)

	assert.Equal(t, device.PreflightStateUnavailable, grp.Detection.State)
	assert.Equal(t, detectionUnnamed, grp.Detection.Reason, "the detection reason is the one to fix")
	assert.Equal(t, noteDetectionFailed, grp.Note)
	assert.Empty(t, grp.Checks, "a question standing on a failed detection is not answered")
	assert.Nil(t, fake.groups, "the manufacturer's own preflighter was never driven")
}

// Detection is an answer in its own right, reported for every manufacturer asked about -- including the
// ones no capability is read for. Its three outcomes are three different facts and are never
// collapsed: a manufacturer whose detect pass could not measure is not one that has no hardware.
func TestPreflight_Detection(t *testing.T) {
	twoAccelerators := device.DevicesGroupList{{
		ID:           "a100",
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Name:         "NVIDIA A100",
		Accelerators: []workercore.Accelerator{{ID: "GPU-0"}, {ID: "GPU-1"}},
	}}

	testCases := []struct {
		name             string
		groups           device.DevicesGroupList
		unmeasured       bool
		wantState        device.PreflightState
		wantAccelerators int
		wantReason       string
	}{
		{
			name:             "accelerators detected",
			groups:           twoAccelerators,
			wantState:        device.PreflightStateOK,
			wantAccelerators: 2,
		},
		{
			// Nothing is wrong with this node; this manufacturer's hardware is simply not on it.
			name:       "nothing detected is an answer, not a failure",
			wantState:  device.PreflightStateNotDeclared,
			wantReason: detectionNoAccelerator,
		},
		{
			// The pass that would have named them failed, so this says nothing about the hardware
			// -- which is exactly why it must not be reported as "none found".
			name:       "a pass that could not measure is a failure, not an empty node",
			unmeasured: true,
			wantState:  device.PreflightStateUnavailable,
			wantReason: detectionUnmeasured,
		},
		{
			// Measured on a Hygon node whose HSA runtime did not load: eight accelerators answered,
			// every one of them anonymous. The count alone reported this as a healthy node, and the
			// only thing distinguishing it from one was a leading space in " x8".
			name: "accelerators that answered but could not be named is a failure",
			groups: device.DevicesGroupList{{
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{{ID: "GPU-0"}, {ID: "GPU-1"}},
			}},
			wantState:        device.PreflightStateUnavailable,
			wantAccelerators: 2,
			wantReason:       detectionUnnamed,
		},
		{
			// One named group does not excuse an unnamed one: the flavor named after the unnamed
			// group is the one that breaks, whatever its neighbor is called.
			name: "one unnamed group among named ones still fails",
			groups: device.DevicesGroupList{
				twoAccelerators[0],
				{
					Manufacturer: nodefeature.ManufacturerNVIDIA,
					Accelerators: []workercore.Accelerator{{ID: "GPU-2"}},
				},
			},
			wantState:        device.PreflightStateUnavailable,
			wantAccelerators: 3,
			wantReason:       detectionUnnamed,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			det := detection(tc.groups, tc.unmeasured)

			assert.Equal(t, tc.wantState, det.State, "state")
			assert.Equal(t, tc.wantAccelerators, det.Accelerators, "accelerators")
			assert.Equal(t, tc.wantReason, det.Reason, "reason")
			assert.Equal(t, device.PreflightDepthDeclared, det.Depth,
				"a detect pass asks the driver and observes nothing further")
			if tc.wantState == device.PreflightStateOK {
				assert.Contains(t, det.Detail, "NVIDIA A100",
					"what was detected, in the detector's own words")
			}
		})
	}
}

// Nothing may infer a deeper label than it earned, and nothing may escape without one. A
// manufacturer that returns a check with no depth means the shallowest -- a driver read -- because
// that is the only depth a preflighter that says nothing can have reached.
func TestPreflight_DepthNeverBlankNeverInflated(t *testing.T) {
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{
		nodefeature.ManufacturerNVIDIA: func(device.PreflighterOptions) device.AcceleratorPreflighter {
			return &fakePreflighter{checks: []device.PreflightCheck{
				{Accelerator: "GPU-0", Capability: "unlabelled"},
				{Accelerator: "GPU-1", Capability: "labelled", Depth: device.PreflightDepthMeasured},
			}}
		},
	})

	p := &Preflighter{manufacturers: sets.New(nodefeature.ManufacturerNVIDIA)}
	groups := device.DevicesGroupList{{
		ID:           "a100",
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Name:         "NVIDIA A100",
		Accelerators: []workercore.Accelerator{{ID: "GPU-0"}},
	}}
	grp := p.preflight(context.Background(), nodefeature.ManufacturerNVIDIA,
		detection(groups, false), groups, false)

	require.Len(t, grp.Checks, 2)
	assert.Equal(t, device.PreflightDepthDeclared, grp.Checks[0].Depth,
		"a blank depth reads as the shallowest, never as a deeper one")
	assert.Equal(t, device.PreflightDepthMeasured, grp.Checks[1].Depth,
		"a depth the preflighter set is left alone")
}

// The exit code is the only part of the result a script reads, so which answers are failures and
// which are merely answers has to be pinned rather than inferred from the words around them.
func TestFailed(t *testing.T) {
	testCases := []struct {
		name       string
		group      device.PreflightGroup
		wantFailed bool
	}{
		{
			name: "a capability that could not be read is a failure",
			group: device.PreflightGroup{Checks: []device.PreflightCheck{
				{Accelerator: "GPU-0", Capability: "mig", State: device.PreflightStateUnavailable},
			}},
			wantFailed: true,
		},
		{
			name: "a detect pass that could not measure is a failure",
			group: device.PreflightGroup{
				Detection: device.PreflightDetection{State: device.PreflightStateUnavailable},
			},
			wantFailed: true,
		},
		{
			name: "a capability this generation does not declare is an answer",
			group: device.PreflightGroup{Checks: []device.PreflightCheck{
				{Accelerator: "GPU-0", Capability: "mig", State: device.PreflightStateNotDeclared},
			}},
		},
		{
			name: "no accelerator detected is an answer",
			group: device.PreflightGroup{
				Detection: device.PreflightDetection{State: device.PreflightStateNotDeclared},
				Note:      noteNoAccelerator,
			},
		},
		{
			name: "nothing checked here is an answer",
			group: device.PreflightGroup{
				Detection: device.PreflightDetection{State: device.PreflightStateOK},
				Note:      noteNoPreflighter,
			},
		},
		{
			name: "every capability readable is a pass",
			group: device.PreflightGroup{
				Detection: device.PreflightDetection{State: device.PreflightStateOK},
				Checks: []device.PreflightCheck{
					{Accelerator: "GPU-0", Capability: "mig", State: device.PreflightStateOK},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.group.Manufacturer = nodefeature.ManufacturerNVIDIA

			failed := Failed(device.PreflightGroupList{tc.group})

			if !tc.wantFailed {
				assert.Empty(t, failed)
				return
			}
			require.Len(t, failed, 1)
			assert.Contains(t, failed[0], nodefeature.ManufacturerNVIDIA,
				"a failure names the manufacturer it belongs to")
		})
	}
}

// A failing node still owes its operator the answer. Withholding the document on failure would
// leave the exit code as the only thing to debug from, which is the situation this command exists
// to end.
func TestReport_WritesTheDocumentEvenWhenTheNodeFailed(t *testing.T) {
	grpList := device.PreflightGroupList{{
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Detection:    device.PreflightDetection{State: device.PreflightStateOK, Accelerators: 1},
		Checks: []device.PreflightCheck{{
			Accelerator: "GPU-0",
			Capability:  "mig-partitioning",
			State:       device.PreflightStateUnavailable,
			Depth:       device.PreflightDepthDeclared,
			Reason:      "nvml: function not found",
		}},
	}}

	var buf bytes.Buffer
	err := Report(&buf, grpList)

	require.Error(t, err, "an unavailable capability is a failure")
	assert.Contains(t, err.Error(), "nvml: function not found", "the driver's own words reach the caller")

	var decoded device.PreflightGroupList
	require.NoError(t, yaml.Unmarshal(buf.Bytes(), &decoded), "the document is still valid YAML")
	assert.Equal(t, grpList, decoded, "and it is the whole result, not a truncated one")
}

// Every manufacturer asked about appears in the result, whatever it had to report. A manufacturer
// left out is one an operator reads as checked and passing.
// Every check names the allocation mode it is a precondition for, and a check that names none is a
// row a reader cannot place: Capability is the vendor's own word, so it is Mode that makes two
// manufacturers' answers comparable and makes a missing mode visible as a gap.
//
// Enforced at the boundary rather than trusted to each manufacturer, because the registry is a
// static map a vertical adds a line to, and the line that forgets Mode would otherwise ship.
func TestPreflightAccelerator_EveryCheckNamesItsMode(t *testing.T) {
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{
		nodefeature.ManufacturerNVIDIA: func(device.PreflighterOptions) device.AcceleratorPreflighter {
			return &fakePreflighter{checks: []device.PreflightCheck{
				{Accelerator: "GPU-0", Capability: "invented-by-a-vertical", State: device.PreflightStateOK},
			}}
		},
	})

	p, err := New(&Config{Manufacturers: sets.New(nodefeature.ManufacturerNVIDIA)})
	require.NoError(t, err)

	groups := device.DevicesGroupList{{
		ID:           "grp-0",
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Name:         "NVIDIA A100",
		Accelerators: []workercore.Accelerator{{ID: "GPU-0"}},
	}}
	grp := p.preflightSurviving(context.Background(), nodefeature.ManufacturerNVIDIA, groups, false)

	require.Len(t, grp.Checks, 1)
	assert.Equal(t, device.PreflightModeUnnamed, grp.Checks[0].Mode,
		"a check that named no mode has to be visible as one, not left blank beside the rest")
	assert.Contains(t, grp.Checks[0].Reason, "names no allocation mode",
		"and it has to say so, since a reader cannot place the row otherwise")
}

// The names a report prints come from the allocator's own enum, so a mode renamed there is renamed
// in the report. Writing them by hand would leave a report naming a mode the allocator no longer has.
func TestPreflightModeOf(t *testing.T) {
	testCases := []struct {
		mode workercore.DeviceAllocationMode
		want string
	}{
		{workercore.DeviceAllocationModeSliced, "sliced"},
		{workercore.DeviceAllocationModePartitioned, "partitioned"},
		{workercore.DeviceAllocationModeVisibility, "visibility"},
		{workercore.DeviceAllocationModeExclusive, "exclusive"},
		{workercore.DeviceAllocationModeShared, "shared"},
		// The zero value is not a mode a manufacturer established, and must never read as one.
		{workercore.DeviceAllocationModeNone, device.PreflightModeUnnamed},
	}
	for _, tc := range testCases {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, device.PreflightModeOf(tc.mode))
			assert.NotEmpty(t, device.PreflightModeOf(tc.mode), "a blank column tells a reader nothing")
		})
	}
}

// This command's whole value is answering on a node that may be broken, so a manufacturer that
// panics must not take the answer down with it. The vendor code reached here is cgo over a driver
// that a half-installed node can have in any state, and one nil dereference in it would leave the
// operator with a stack trace instead of the eight other manufacturers' verdicts.
func TestPreflightAccelerator_AManufacturerThatPanicsDoesNotTakeTheRunDown(t *testing.T) {
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{
		nodefeature.ManufacturerNVIDIA: func(device.PreflighterOptions) device.AcceleratorPreflighter {
			return &panickingPreflighter{}
		},
		nodefeature.ManufacturerAscend: func(device.PreflighterOptions) device.AcceleratorPreflighter {
			return &fakePreflighter{}
		},
	})

	p, err := New(&Config{Manufacturers: sets.New(
		nodefeature.ManufacturerNVIDIA, nodefeature.ManufacturerAscend)})
	require.NoError(t, err)

	// Driven per manufacturer rather than through the whole pass, because the detect pass finds no
	// hardware here and a manufacturer with no accelerator never reaches its vendor code at all.
	groups := device.DevicesGroupList{{
		ID:           "grp-0",
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Name:         "NVIDIA A100",
		Accelerators: []workercore.Accelerator{{ID: "GPU-0"}},
	}}
	crashed := p.preflightSurviving(context.Background(), nodefeature.ManufacturerNVIDIA, groups, false)

	assert.Contains(t, crashed.Note, "panicked", "the row has to say what happened to it")
	assert.Contains(t, crashed.Note, "boom", "and carry what the panic itself said")
	assert.Equal(t, nodefeature.ManufacturerNVIDIA, crashed.Manufacturer)

	// Nothing the crashed preflighter produced survives -- but the crash itself is a verdict, and it
	// has to be one Failed can read. A note alone leaves a run whose vendor code died exiting zero.
	require.Len(t, crashed.Checks, 1, "one row per accelerator the crashed manufacturer was asked about")
	assert.Equal(t, "GPU-0", crashed.Checks[0].Accelerator)
	assert.Equal(t, capPreflightPanicked, crashed.Checks[0].Capability)
	assert.Equal(t, device.PreflightStateUnavailable, crashed.Checks[0].State)
	assert.Contains(t, crashed.Checks[0].Reason, "boom")
	assert.NotContains(t, crashed.Checks[0].Capability, "mig",
		"the crashed preflighter's own rows are discarded, not merged")

	assert.NotEmpty(t, Failed([]device.PreflightGroup{crashed}),
		"a contained panic must reach the exit code, not only the note")

	// The note and the rows are two fields of one document, and a reader takes them together. A note
	// saying nothing was reported, printed above a row per accelerator, is a document contradicting
	// itself -- so what is asserted here is the agreement rather than the wording: whatever the note
	// says, it may not deny the rows beside it.
	assert.NotContains(t, crashed.Note, "nothing is reported",
		"the note may not deny the rows it is printed above")
	assert.Contains(t, crashed.Note, "unavailable",
		"and has to name what those rows say, since that is what decides the exit code")

	// The detect pass finished before the vendor library was ever loaded, so its answer is not the
	// crash's to take. Dropped, the group reads as one nobody asked -- and a detection that had
	// failed would take its non-zero exit down with it.
	assert.Equal(t, device.PreflightStateOK, crashed.Detection.State,
		"the detection that completed before the crash is still the answer")
	assert.Equal(t, 1, crashed.Detection.Accelerators)
	assert.Equal(t, device.PreflightDepthDeclared, crashed.Detection.Depth)

	// And the loop that calls it keeps going, so the other manufacturers are still reported.
	grpList := p.PreflightAccelerator(context.Background())
	assert.Len(t, grpList, 2, "the panicking manufacturer took the others' answers with it")
}

// A panic during construction is the same class: the driver library is loaded there, which is
// exactly where a half-installed node fails.
func TestPreflightAccelerator_APreflighterThatPanicsOnConstruction(t *testing.T) {
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{
		nodefeature.ManufacturerNVIDIA: func(device.PreflighterOptions) device.AcceleratorPreflighter {
			panic("loading the driver library blew up")
		},
	})

	p, err := New(&Config{Manufacturers: sets.New(nodefeature.ManufacturerNVIDIA)})
	require.NoError(t, err)

	groups := device.DevicesGroupList{{
		ID:           "grp-0",
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Name:         "NVIDIA A100",
		Accelerators: []workercore.Accelerator{{ID: "GPU-0"}},
	}}
	grp := p.preflightSurviving(context.Background(), nodefeature.ManufacturerNVIDIA, groups, false)

	assert.Contains(t, grp.Note, "panicked")
	assert.Contains(t, grp.Note, "driver library blew up")
}

// The sweep has to be wired into the pass, not merely available: a pass that never calls it leaves
// what a responder rendered on the host however correct the sweep itself is.
//
// And it reaches its own tree only. The pod directory is what an allocator reads as its record of
// what other Pods hold, so a sweep that reached in there would hand a live workload's placement to
// the next allocation -- which is why preflight promotes nothing into it in the first place.
func TestPreflightAccelerator_SweepsWhatItRendered(t *testing.T) {
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{})

	root := fakeHostRoot(t)
	p, err := New(&Config{Manufacturers: sets.New(nodefeature.ManufacturerNVIDIA), HostRoot: root})
	require.NoError(t, err)

	rendered := filepath.Join(root, deviceplugin.OperatorPreflightDir, string(deviceplugin.PreflightPodUID))
	require.NoError(t, os.MkdirAll(rendered, 0o755))

	// Under the very UID preflight fabricates for itself, which is the name a sweep pointed at the
	// wrong root would remove: if this survives, the sweep is addressing its own tree and no other.
	occupancy := filepath.Join(root, deviceplugin.OperatorPodsDir, string(deviceplugin.PreflightPodUID))
	require.NoError(t, os.MkdirAll(occupancy, 0o755))

	p.PreflightAccelerator(context.Background())

	_, statErr := os.Stat(rendered)
	assert.True(t, os.IsNotExist(statErr), "the pass left behind what it rendered")
	assert.DirExists(t, occupancy,
		"the pod tree an allocator reads as occupancy is not preflight's to remove")
}

func TestPreflightAccelerator_ReportsEveryManufacturer(t *testing.T) {
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{
		nodefeature.ManufacturerNVIDIA: func(device.PreflighterOptions) device.AcceleratorPreflighter {
			return &fakePreflighter{}
		},
	})

	asked := sets.New(nodefeature.ManufacturerAscend, nodefeature.ManufacturerNVIDIA)
	p, err := New(&Config{Manufacturers: asked})
	require.NoError(t, err)

	grpList := p.PreflightAccelerator(context.Background())

	require.Len(t, grpList, asked.Len())
	reported := sets.New[string]()
	for i := range grpList {
		reported.Insert(grpList[i].Manufacturer)
		assert.NotEmpty(t, grpList[i].Note, "a group carrying no check must say why")
		assert.False(t, grpList[i].Timestamp.IsZero(), "a reading is worth what its time claims")
	}
	assert.Equal(t, asked, reported)
}
