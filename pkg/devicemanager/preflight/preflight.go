// Package preflight reads, before any workload lands on a node, the allocation-time preconditions
// each manufacturer's allocator would read when one does.
//
// A precondition is a fact about the host that only the allocator asks for — an Ascend
// container-share flag, a Cambricon sMLU mode, an NVIDIA MIG capability subtree — so until now the
// only way to learn what a host answers was to schedule a workload and read the failure. This runs
// the same reads with no workload, and reports each one as one of three states rather than a bool,
// because the three carry different consequences: see device.PreflightState.
//
// It is deliberately not part of the detect pass. Detecting is a pure read that is safe anywhere;
// a preflight starts containers that hold an accelerator, and asks a driver to toggle a mode and put
// it straight back where reading alone could not answer the question.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
)

var logger = klog.Background().WithName("preflight")

// The five reasons a group carries no check. They are five different facts with five different
// remedies, and each is said in words: an empty list on its own reads as a node that passed.
const (
	noteNoPreflighter = "no allocation-time precondition is checked for this manufacturer"
	noteUnmeasured    = "the detect pass could not measure this manufacturer, " +
		"so none of its accelerators were preflighted"
	noteNoAccelerator = "no accelerator of this manufacturer was detected"
	noteNoCheck       = "this manufacturer's accelerators were preflighted and none of them " +
		"declared a capability to read"
	noteContended = "another preflight holds this node's lock, so nothing was read for this " +
		"manufacturer: two runs at once sweep each other's probe containers"
	noteUnlockable = "this node's preflight lock could not be taken, so nothing was read for this " +
		"manufacturer: the reason above says why, and it is not another preflight"
	noteDetectionFailed = "detection failed for this manufacturer, so the remaining questions are " +
		"unanswerable: whether these accelerators slice, and what happens to a slice, are questions " +
		"about accelerators this report cannot identify -- the detection reason above is the one to fix"
)

// capPreflightPanicked is the capability a contained panic reports under. It is this package's own
// word rather than a manufacturer's: no vendor vocabulary has a name for its own driver crashing.
const capPreflightPanicked = "preflight-panicked"

// capStaleSweep is the capability an incomplete stale-container sweep reports under. It is this
// package's own word for the same reason: what it names is a step of this command, not a property of
// anybody's hardware.
const capStaleSweep = "stale-container-sweep"

// Why a detect pass reports no accelerator. The two are never collapsed: one says the hardware is
// not here, the other says nobody was able to look.
const (
	detectionNoAccelerator = "the detect pass found no accelerator of this manufacturer"
	detectionUnmeasured    = "the detect pass could not measure this manufacturer, " +
		"so whether its accelerators are present is unknown"
	detectionUnnamed = "the detect pass found accelerators it could not name: the part of this " +
		"manufacturer's driver stack that names them did not load, which leaves the group's id " +
		"empty as well -- and every resource flavor and instance type on this node is named after " +
		"that id"
)

type Preflighter struct {
	manufacturers sets.Set[string]
	dryRun        bool
	probeImage    string
	wantRuntime   string
	detector      *detector.Detector
	host          *hostExec
	// runtime is the container runtime resolved on the host, nil when none was. It is resolved
	// once per run rather than per manufacturer: the probe is a pair of host executions, and the
	// answer is the same for every manufacturer on one host.
	runtime *hostRuntime
	// noRuntime is what the resolution said when it produced no runtime, carried so that a row
	// reporting the fallback names the reason the host gave rather than a generic one. "Every probe
	// came up empty" and "the kubelet names a runtime nothing here can drive" are different facts
	// about the node, and only the second tells its reader what to install.
	noRuntime string
	// sweepFailures names every runtime whose stale-container sweep could not be completed, in the
	// sweep's own words. It is filled once, before anything is measured, and read when each group is
	// built: a leftover this pass could not remove is still holding an accelerator, so every answer
	// measured afterwards is one this command cannot stand behind.
	sweepFailures []string
}

// New creates a Preflighter with the given configuration.
//
// It owns a detector because a precondition is read on an accelerator, and only a detect pass says
// which accelerators are here and how each manufacturer addresses them. The detector's looping
// options are left unset: this drives one DetectAccelerator pass and never Start.
func New(c *Config) (*Preflighter, error) {
	d, err := detector.New(&detector.Config{
		NoPCICheck:    c.NoPCICheck,
		Manufacturers: c.Manufacturers,
	})
	if err != nil {
		return nil, err
	}
	return &Preflighter{
		manufacturers: c.Manufacturers,
		dryRun:        c.DryRun,
		probeImage:    c.ProbeImage,
		wantRuntime:   c.Runtime,
		detector:      d,
		host:          newHostExec(c.HostRoot),
	}, nil
}

// PreflightAccelerator reads every manufacturer's preconditions once and returns one group per
// manufacturer it was asked about.
//
// A manufacturer is never left out, whatever the reason it carries no check. An empty result reads
// as a node that passed, so each of the three reasons a group can be empty is said in words on the
// group itself.
func (p *Preflighter) PreflightAccelerator(ctx context.Context) device.PreflightGroupList {
	// Taken before the sweep, because the sweep is the half that hurts: every probe container
	// carries one label, so a second run removes the first run's live probes and the accelerator
	// they were measuring reports as unable to slice. A dry run takes no lock: it starts nothing to
	// be swept and writes nothing to be shared.
	//
	// A pass with no host root has nowhere to put a lock, and that is a reason to withhold the
	// writes rather than a reason to skip the lock. The mode toggles do not reach the driver
	// through the host root -- they are driver calls this process makes directly -- so an
	// unmountable host root leaves them just as able to turn a shared flag on underneath another
	// pass, and only unable to be serialized against one. Nor does an unusable host root stop the
	// writes that do go through it: measured on an AMD host, a pass given a path that is merely a
	// directory created eighteen entries under it, because staging makes the tree it is pointed at.
	// Downgraded to a dry run instead: what a pass with no host root can still answer, it answers,
	// and what it cannot hold the node for, it does not do.
	if !p.dryRun {
		if err := p.host.Validate(); err != nil {
			p.dryRun = true
			logger.Info("no host root, so this pass is downgraded to a dry run: "+
				"the node cannot be locked, and a mode toggle is a write that needs it",
				"error", err.Error())
		} else {
			release, err := lockHost(p.host.root)
			if err != nil {
				return p.contended(err)
			}
			defer release()
		}
	}

	// Resolved before anything is read, because both of the steps that follow depend on it: the
	// container probe runs through it, and the stale sweep removes what an earlier run left behind
	// before this one starts a container of its own. A host with no runtime is not an error here —
	// every step that needed one falls back to being emitted, naming what was probed.
	if rt, err := p.host.ResolveRuntime(ctx, p.wantRuntime); err != nil {
		p.noRuntime = err.Error()
		logger.V(3).Info("no container runtime resolved on the host", "error", p.noRuntime)
	} else {
		p.runtime = rt
	}
	p.sweepStaleContainers(ctx)

	// What a responder rendered has to reach the host for the container to mount it, and is promoted
	// into deviceplugin.OperatorPreflightDir to get there -- a tree of preflight's own, which no
	// allocator reads. That is what makes removing it again hygiene rather than correctness, and it
	// has to be: this is a deferred call, and no deferred call runs after a SIGKILL. Promoted into
	// the pod directory instead, a pass killed mid-run would leave entries an allocator counts as
	// occupancy, under a Pod UID no kubelet ever scheduled.
	defer p.sweepRenderedArtifacts()

	grpList, unmeasured := p.detector.DetectAccelerator(ctx)

	detected := make(map[string]device.DevicesGroupList, len(grpList))
	for i := range grpList {
		m := grpList[i].Manufacturer
		detected[m] = append(detected[m], grpList[i])
	}

	out := make(device.PreflightGroupList, 0, p.manufacturers.Len())
	for _, m := range sets.List(p.manufacturers) {
		grp := p.preflightSurviving(ctx, m, detected[m], unmeasured.Has(m))
		// Added after the manufacturer's own answers rather than inside them, because what it reports
		// is not about this manufacturer: the sweep ran once, before any of them, and the accelerator
		// it may have left occupied is whichever one a leftover was holding. Appended to every group
		// that has accelerators, which is the only way a per-accelerator report can carry a node-wide
		// fact -- and the only way it reaches Failed, which reads states and never notes.
		grp.Checks = append(grp.Checks, p.staleSweepChecks(detected[m])...)
		out = append(out, grp)
	}
	return out
}

// contended is the whole report when this node's lock could not be taken.
//
// A report rather than a bare error, because the exit code is read by the same automation that reads
// the rows, and every manufacturer asked about has to say something: this one says the node could not
// be read, which is true, and names what stopped it. Nothing is started and nothing is swept, so a
// run that does hold the lock is left alone -- which is the entire point of refusing.
//
// The note distinguishes contention from every other way the lock can fail, because only contention
// names a second operator. Measured on hardware: a non-root run was told another preflight held the
// node while its own reason read "permission denied", which sends a reader to look for a process
// that is not there.
func (p *Preflighter) contended(err error) device.PreflightGroupList {
	note := noteUnlockable
	if errors.Is(err, errContended) {
		note = noteContended
	}

	out := make(device.PreflightGroupList, 0, p.manufacturers.Len())
	for _, m := range sets.List(p.manufacturers) {
		out = append(out, device.PreflightGroup{
			Manufacturer: m,
			Timestamp:    time.Now(),
			Detection: device.PreflightDetection{
				State:  device.PreflightStateUnavailable,
				Depth:  device.PreflightDepthDeclared,
				Reason: err.Error(),
			},
			Note: note,
		})
	}
	return out
}

// preflightSurviving is p.preflight with one manufacturer's crash contained to that manufacturer.
//
// The whole value of this command is answering on a node that may be broken, and everything it
// reaches for a manufacturer is cgo over a vendor driver a half-installed node can leave in any
// state. Without this, one nil dereference in one vendor's library hands the operator a stack trace
// instead of the other eight verdicts -- on exactly the node they most needed them for.
//
// The crash becomes that manufacturer's note, so it is reported rather than merely survived: a group
// that says nothing about why it is empty reads as a manufacturer that passed.
func (p *Preflighter) preflightSurviving(
	ctx context.Context, manufacturer string, groups device.DevicesGroupList, unmeasured bool,
) (grp device.PreflightGroup) {
	// Answered before the recovery below is armed, so that a crash in the vendor's own library still
	// reports it. What the detect pass established says nothing about whether that library later
	// crashed, and a group reported without its detection answer reads as one nobody asked -- while
	// a detection that had failed would take its non-zero exit down with it.
	//
	// Neither call is what the recovery is for: one reads what the detect pass already returned, the
	// other runs the host's own CLI through a chroot. The vendor cgo is all below.
	det := detection(groups, unmeasured)
	if p.host != nil {
		crossCheckHost(ctx, p.host, manufacturer, &det)
	}

	defer func() {
		r := recover()
		if r == nil {
			return
		}
		logger.Error(nil, "a manufacturer's preflight panicked", "manufacturer", manufacturer,
			"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		// The checks are rebuilt rather than patched: whatever the crashed pass had filled in
		// describes a reading that never completed, and reporting half of it as a verdict is worse
		// than reporting none. Detection is carried through, because it completed before the crash.
		grp = device.PreflightGroup{
			Manufacturer: manufacturer,
			Timestamp:    time.Now(),
			Detection:    det,
			Checks:       panickedChecks(groups, r),
			Note: "this manufacturer's preflight panicked and was contained: whatever it had " +
				"established is discarded, and every accelerator it was asked about is reported " +
				"unavailable below; the rest of this run is unaffected: " + fmt.Sprint(r),
		}
	}()
	return p.preflight(ctx, manufacturer, det, groups, unmeasured)
}

// staleSweepChecks is what an incomplete stale-container sweep leaves on every accelerator this pass
// went on to measure. It is empty, and costs nothing, on the runs where the sweep completed.
//
// The sweep is the one step that makes a measured answer mean anything: a container an earlier run
// left behind still holds its accelerator, and a slice measured against an occupied card reports a
// healthy accelerator as unavailable while naming the card as the thing at fault. So a sweep that
// could not be completed does not leave the rows below merely uncertain -- it leaves them able to
// say the opposite of the truth, about hardware that is fine.
//
// Reported as unavailable rather than logged, because the exit code is what automation reads and
// Failed reads states. A run that swept nothing, measured anyway and exited zero is the shape this
// whole capability exists to rule out.
//
// One row per accelerator, for the same reason panickedChecks does it: that is what Checks is, and a
// reader filtering the report by accelerator would never find a node-wide row.
func (p *Preflighter) staleSweepChecks(groups device.DevicesGroupList) []device.PreflightCheck {
	if len(p.sweepFailures) == 0 {
		return nil
	}

	reason := "the stale-container sweep could not be completed, so a container an earlier run left " +
		"behind may still be holding this accelerator and every answer measured against it here is " +
		"unsafe to trust: " + strings.Join(p.sweepFailures, "; ")

	var checks []device.PreflightCheck
	for i := range groups {
		accels := groups[i].Accelerators
		for j := range accels {
			checks = append(checks, device.PreflightCheck{
				Accelerator: accels[j].ID,
				Capability:  capStaleSweep,
				Mode:        device.PreflightModeUnnamed,
				State:       device.PreflightStateUnavailable,
				Depth:       device.PreflightDepthDeclared,
				Reason:      reason,
			})
		}
	}
	return checks
}

// anyLogicallySliceable reports whether any accelerator in groups can host a logical slice, which is
// the condition both container questions skip an accelerator on.
func anyLogicallySliceable(groups device.DevicesGroupList) bool {
	for i := range groups {
		for j := range groups[i].Accelerators {
			if groups[i].Accelerators[j].Status.LogicalSliced.Count > 0 {
				return true
			}
		}
	}
	return false
}

// panickedChecks is what a contained panic leaves on every accelerator the crashed manufacturer was
// asked about.
//
// The note beside them says the same thing in words, and words are not what a caller reads: Failed
// looks at states, so a group carrying a completed detection, no checks and a note is a manufacturer
// whose vendor code died while the command exits zero. That is the one outcome automation cannot see
// past, and it is worse than the crash.
//
// One row per accelerator rather than one for the manufacturer, because that is what Checks is --
// and a reader filtering the report by accelerator would not find a manufacturer-wide row at all.
func panickedChecks(groups device.DevicesGroupList, r any) []device.PreflightCheck {
	reason := "this manufacturer's preflight panicked and was contained, so none of this " +
		"accelerator's preconditions were established: " + fmt.Sprint(r)

	var checks []device.PreflightCheck
	for i := range groups {
		for j := range groups[i].Accelerators {
			checks = append(checks, device.PreflightCheck{
				Accelerator: groups[i].Accelerators[j].ID,
				Capability:  capPreflightPanicked,
				Mode:        device.PreflightModeUnnamed,
				State:       device.PreflightStateUnavailable,
				Depth:       device.PreflightDepthDeclared,
				Reason:      reason,
			})
		}
	}
	return checks
}

func (p *Preflighter) preflight(
	ctx context.Context, manufacturer string, det device.PreflightDetection,
	groups device.DevicesGroupList, unmeasured bool,
) device.PreflightGroup {
	grp := device.PreflightGroup{
		Manufacturer: manufacturer,
		Timestamp:    time.Now(),
		Detection:    det,
	}

	creator := supportedPreflighterCreators[manufacturer]
	switch {
	case creator == nil:
		// Checked before the three below: a manufacturer nothing is read for reports the same thing
		// whether or not its accelerators are here.
		grp.Note = noteNoPreflighter
		return grp
	case unmeasured:
		grp.Note = noteUnmeasured
		return grp
	case len(groups) == 0:
		grp.Note = noteNoAccelerator
		return grp
	case grp.Detection.State == device.PreflightStateUnavailable:
		// Detection is the floor the other two questions stand on, so a manufacturer that fails it
		// is reported with them marked unanswerable rather than answered anyway. Answering them
		// would attach rows to accelerators the report cannot identify -- an unnamed group has no
		// id, and a container that cannot see the hardware cannot be asked what a slice of it does.
		grp.Note = noteDetectionFailed
		return grp
	}

	// The manufacturer's library is loaded here rather than at construction, so a host carrying one
	// manufacturer's hardware never initializes another's driver to report that it has none.
	pf := creator(device.PreflighterOptions{
		Logger: logger.V(3),
		DryRun: p.dryRun,
	})
	read := pf.PreflightAccelerator(groups)
	grp.Checks = read.Checks

	// The floor no preflighter can fall through: an answer that names no depth is read as the
	// shallowest one, never as a deeper one it did not earn. Applied before the slice rows are
	// added, because those carry the depth they reached and must not be flattened to this one.
	//
	// Mode gets the same treatment, and for a sharper reason: Capability is the vendor's own word, so
	// Mode is the only thing that makes two manufacturers' rows comparable and a missing mode visible
	// as a gap. A check that names none is left naming none, and says so — inventing one here would
	// file the row under a mode nobody established it belongs to.
	for i := range grp.Checks {
		c := &grp.Checks[i]
		if c.Depth == "" {
			c.Depth = device.PreflightDepthDeclared
		}
		if c.Mode == "" {
			c.Mode = device.PreflightModeUnnamed
			c.Reason = strings.TrimSpace(c.Reason + " (this check names no allocation mode, so it " +
				"cannot be compared with another manufacturer's answer for the same one)")
		}
	}

	// The library tree both container questions mount, put on the host once and handed to each, so
	// neither of them depends on the other having run and neither writes it twice.
	//
	// And only where one of them will start something. Both skip an accelerator that can host no
	// logical slice, so a node whose cards all report none reaches no container at all -- while this
	// tree, once written, is never removed. Staging it there would leave a permanent directory on a
	// host that had nothing to measure.
	var staged StageResult
	if anyLogicallySliceable(groups) {
		staged = p.stageLibFor(manufacturer)
	} else {
		staged = StageResult{Manufacturer: manufacturer}
	}

	// The slice itself, asked rather than the driver, and asked whatever the driver read produced:
	// three manufacturers read no driver at all when they serve an allocation, and every one of the
	// nine still has an injection to drive.
	grp.Checks = append(grp.Checks, p.measureSliced(ctx, manufacturer, pf, staged, groups)...)

	// What an allocation grants a second container, asked after the slice and independently of it:
	// it is a behavior of the allocator rather than of the slice, so a node whose slice could not be
	// measured still has this answer worth having.
	grp.Checks = append(grp.Checks, p.checkManagement(ctx, manufacturer, pf, staged, groups)...)

	// The manufacturer's own words are kept whatever the later stages appended. A manufacturer that
	// reads no driver says so here, and the simulated rows the responder produces afterwards do not
	// answer that -- they are a different question at a different depth. Dropping the note as soon
	// as those rows arrive leaves a reader to infer a driver read that never happened, which is what
	// a Hygon node's report did.
	grp.Note = read.Note

	// The generic sentence only has to cover the manufacturer that read nothing and did not say why.
	// What must never happen is neither: an empty group with no note reads as a node that passed.
	if grp.Note == "" && len(grp.Checks) == 0 {
		grp.Note = noteNoCheck
	}
	return grp
}

// detection turns one manufacturer's detect pass into an answer of its own.
//
// Its three outcomes are the three states, and they line up with what an allocation would do:
// accelerators detected proceeds, none detected has nothing to proceed with, and a pass that could
// not measure is the one an operator has to act on.
func detection(groups device.DevicesGroupList, unmeasured bool) device.PreflightDetection {
	d := device.PreflightDetection{Depth: device.PreflightDepthDeclared}

	switch {
	case unmeasured:
		// Checked first: a failed pass names no group, but a stale one from an earlier pass may
		// still be carried, and reporting that as a live detection would overstate it.
		d.State = device.PreflightStateUnavailable
		d.Reason = detectionUnmeasured
		return d
	case len(groups) == 0:
		d.State = device.PreflightStateNotDeclared
		d.Reason = detectionNoAccelerator
		return d
	}

	var (
		names   = make([]string, 0, len(groups))
		unnamed bool
	)
	for i := range groups {
		d.Accelerators += len(groups[i].Accelerators)
		if groups[i].Name == "" {
			unnamed = true
		}
		names = append(names, fmt.Sprintf("%s x%d", groups[i].Name, len(groups[i].Accelerators)))
	}
	// A group with no name is not a cosmetic gap. The name is what ConstructGroupID makes the
	// group's id out of, so an unnamed group is an id-less one, and the whole scheduling chain
	// names itself after that id. Counting the accelerators and stopping there reports such a node
	// as healthy -- the only thing that told them apart was a leading space in the detail.
	if unnamed {
		d.State = device.PreflightStateUnavailable
		d.Reason = detectionUnnamed
		return d
	}
	d.State = device.PreflightStateOK
	d.Detail = strings.Join(names, ", ")
	return d
}

// preflightResult is the whole document: the per-manufacturer answers, and the node-level facts
// that belong to no manufacturer.
//
// Its top level is a map where earlier releases wrote a bare list. That is a visible change to the
// document's shape, and it was chosen over the alternative of appending a second YAML document to
// the same stream: a reader taking only the first document of a multi-document stream would keep
// working while silently seeing none of the new section, and a silent truncation is worse than a
// shape a reader fails loudly on.
type preflightResult struct {
	Accelerators device.PreflightGroupList `json:"accelerators" yaml:"accelerators"`
	Network      NetworkReport             `json:"network" yaml:"network"`
}

// Report writes the result to w as one YAML document and then reports whether the node failed.
//
// The document is written first and unconditionally. A caller that asked what this node can serve
// is entitled to the answer whether or not it passed, and withholding it on failure would leave
// the exit code as the only thing to debug from.
//
// The network section is reported and does NOT contribute to the failure. What this error answers
// is whether the node can serve the allocation modes its allocators offer, and a down RDMA link
// stops none of them: it withholds a node label, which changes what a flavor selects rather than
// what an allocator can hand out. A link row that failed the pass would make every script gating
// an install on this command start refusing nodes that allocate perfectly well.
func Report(w io.Writer, grpList device.PreflightGroupList, network NetworkReport) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(4)
	if err := enc.Encode(preflightResult{Accelerators: grpList, Network: network}); err != nil {
		return fmt.Errorf("encode result: %w", err)
	}

	if failed := Failed(grpList); len(failed) != 0 {
		return errors.New("preflight failed: " + strings.Join(failed, "; "))
	}
	return nil
}

// Failed names every answer in grpList that is a failure, so a caller can turn the result into an
// exit code without reading the words around it.
//
// Only an unavailable answer is a failure: it is the state an allocation is refused on. A
// capability this generation does not declare, a manufacturer nothing is checked for and a node
// carrying none of its hardware are all answers, and a run that reports them has done its job.
func Failed(grpList device.PreflightGroupList) []string {
	var failed []string
	for i := range grpList {
		grp := &grpList[i]
		if grp.Detection.State == device.PreflightStateUnavailable {
			failed = append(failed, fmt.Sprintf("%s: detection: %s", grp.Manufacturer, grp.Detection.Reason))
		}
		for j := range grp.Checks {
			c := &grp.Checks[j]
			if c.State != device.PreflightStateUnavailable {
				continue
			}
			failed = append(failed,
				fmt.Sprintf("%s: %s on %s: %s", grp.Manufacturer, c.Capability, c.Accelerator, c.Reason))
		}
	}
	return failed
}
