// Package procattr attributes a host process to the Kubernetes Pod and container it runs inside,
// by reading the process's cgroup membership under /proc.
//
// The vendor accelerator libraries report which host PIDs hold a device and how much of it they
// hold, but not whose workload those processes are. This package supplies the missing half of that
// join, under one rule: a wrong-but-plausible attribution is worse than none. Every ambiguous
// cgroup shape — two Pod ancestors, cgroup v1 controllers naming different Pods, a Pod UID
// appearing only inside a longer segment — is refused with a named reason rather than resolved by
// preference.
//
// The outcomes are three, not two, and the third is the reason this package can be used to build a
// sum at all. A process attributed to a container contributes its usage. A process belonging to a
// Pod the node knows and that backs no Instance is excluded, and excluding it changes nothing. A
// process that could not be classified at all makes every figure on its device unmeasurable —
// because a sum missing one of its terms is not a smaller sum, it is the wrong number, and a
// caller cannot tell the two apart from the figure alone.
package procattr

import (
	"errors"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// Identity is the Kubernetes identity of a host process: the Pod it belongs to and, within that
// Pod, the container.
//
// Both halves are required. Accelerator allocations are persisted per container and only later
// aggregated Pod-wide, so a Pod UID alone cannot say which allocation owns a process, nor whether
// the one-accelerator-per-sliced-container invariant holds.
type Identity struct {
	// PodUID is the Pod's UID, canonically lower-case and hyphenated whichever cgroup driver
	// wrote the path it was read from.
	PodUID string
	// ContainerID is the container's runtime ID as a cgroup path spells it: the bare ID, with
	// neither the systemd unit's runtime prefix nor the "<runtime>://" scheme a Pod status
	// reports it with.
	ContainerID string
}

// Outcome is what attribution concluded about one process.
type Outcome uint8

const (
	// OutcomeUnattributed means the process could not be classified. It is the zero value, so a
	// Result that was never populated cannot read as an attribution. Reason says which refusal
	// it was, and a caller must treat the process's device as unmeasurable for that sample.
	OutcomeUnattributed Outcome = iota
	// OutcomeAttributed means the process belongs to the container named by Identity.
	OutcomeAttributed
	// OutcomeExcluded means the process belongs to a Pod the node is running that backs no
	// Instance. Its usage is real, but it is nobody's slice and no slice figure depends on it,
	// so dropping it leaves the device's remaining figures measurable.
	OutcomeExcluded
)

// Reason names why attribution refused to produce an Identity. The values are stable strings
// because they are published as a label on the Device Manager's capability gauges: an absent figure
// with a named reason is diagnosable, an absent figure without one is a mystery.
type Reason string

const (
	// reasonNone is the absence of a refusal, and never reaches a caller inside a Result.
	reasonNone Reason = ""

	// ReasonNoPodComponent means no segment of the process's cgroup path named a Pod, which is
	// what a process on the host rather than in a container looks like.
	ReasonNoPodComponent Reason = "no_pod_component"
	// ReasonAmbiguousPod means the path — or, on cgroup v1, the set of controller paths — named
	// more than one Pod.
	ReasonAmbiguousPod Reason = "ambiguous_pod"
	// ReasonNoContainerComponent means a Pod was named but no container below it was. A sandbox
	// runtime that runs the whole Pod in one process looks like this, and it cannot be charged
	// to a container's allocation.
	ReasonNoContainerComponent Reason = "no_container_component"
	// ReasonAmbiguousContainer means more than one container was named below the Pod.
	ReasonAmbiguousContainer Reason = "ambiguous_container"
	// ReasonUnknownPod means the cgroup path named a syntactically valid Pod UID the node's Pod
	// index does not carry. The index is a snapshot that may not have caught up, so the process
	// is unattributable rather than assumed to belong to no Instance.
	ReasonUnknownPod Reason = "unknown_pod"
	// ReasonUnknownContainer means the Pod is a known Instance's but the container ID matches
	// none of the containers its status reports. There is no safe guess to make here.
	ReasonUnknownContainer Reason = "unknown_container"
	// ReasonExited means /proc had no entry for the process, the ordinary race of a process
	// ending between the vendor's query and this read.
	ReasonExited Reason = "exited"
	// ReasonPermission means /proc refused the read. Unlike ReasonExited this will refuse every
	// process on the node until the deployment is fixed, which is why the two are distinct.
	ReasonPermission Reason = "permission"
	// ReasonUnreadable means /proc answered with neither the data nor either of the two
	// failures above.
	ReasonUnreadable Reason = "unreadable"
	// ReasonZombie means the process is being kept only for its exit status, so it holds nothing
	// and a vendor row still naming it is stale.
	ReasonZombie Reason = "zombie"
	// ReasonUnstable means the process's identity changed while it was being read, the shape of
	// a PID recycled into a different Pod.
	ReasonUnstable Reason = "unstable"
	// ReasonMediated means the process holds the device on behalf of other Pods, so its usage is
	// neither its own Pod's nor divisible between the Pods it serves.
	ReasonMediated Reason = "mediated"
	// ReasonInvisible means the vendor reported PIDs from a process namespace this process cannot
	// see, so none of them was in /proc at all. It is a deployment fault, not a race.
	ReasonInvisible Reason = "pid_namespace_invisible"
)

// Result is the conclusion attribution reached for one process.
type Result struct {
	// Outcome is the conclusion.
	Outcome Outcome
	// Identity is populated only when Outcome is OutcomeAttributed.
	Identity Identity
	// Reason is populated only when Outcome is OutcomeUnattributed.
	Reason Reason
}

// Pod is what attribution needs to know about one Pod the node is running.
type Pod struct {
	// Instance reports whether this Pod backs an Instance. A Pod that does not is still worth
	// carrying: knowing that a process belongs to one is exactly what makes the process safe to
	// drop instead of fatal to its device's sample.
	Instance bool
	// Containers holds the runtime IDs of the Pod's containers with the "<runtime>://" scheme
	// its status reports them with already stripped, so they compare directly against what a
	// cgroup path spells.
	Containers sets.Set[string]
}

// PodIndex holds every Pod the node is running, keyed by Pod UID.
//
// A UID the index does not carry is unknown, not absent: the index is a snapshot and may not have
// caught up with a Pod that has only just started, so a process in an unlisted Pod is
// unattributable rather than assumed to be no Instance's.
type PodIndex map[string]Pod

// mediatingProcesses are the executables that hold a device on behalf of other processes, matched
// against /proc/<pid>/comm.
//
// The names are stored as the kernel reports them — truncated to 15 characters — which is why one
// entry covers both the MPS control daemon and the MPS server it spawns.
var mediatingProcesses = sets.New("nvidia-cuda-mps")

// Resolver attributes processes by reading a /proc tree. It keeps no state between calls, so the
// Pod index a caller passes is always the one in effect for that sample.
type Resolver struct {
	fsys fs.FS
}

// New returns a Resolver reading process identity from fsys, which a caller on a node roots at
// the host's /proc:
//
//	procattr.New(os.DirFS("/proc"))
func New(fsys fs.FS) *Resolver {
	return &Resolver{fsys: fsys}
}

// Resolve attributes every PID the vendor libraries reported for a device, against the Pods the
// node is running. The result carries one entry per distinct PID.
//
// The PIDs are resolved as a set rather than one at a time for one reason: a vendor library
// reporting PIDs from a process namespace the caller cannot see leaves every one of them missing
// from /proc, and that — a deployment to fix, since the reader must run with the host's PID
// namespace — must not read as a crowd of processes that each happened to exit.
func (r *Resolver) Resolve(index PodIndex, pids []uint32) map[uint32]Result {
	results := make(map[uint32]Result, len(pids))
	observed := false
	for _, pid := range pids {
		if _, seen := results[pid]; seen {
			continue
		}
		result, existed := r.resolveOne(index, pid)
		observed = observed || existed
		results[pid] = result
	}

	// Every reported PID missing from /proc, and not one of them ever observed to exist: the
	// vendor is reporting from a process namespace this reader cannot see. Unanimity alone is not
	// the evidence — a lone process that ends between two of the reads below leaves exactly the
	// same set of reasons — so a single successful read anywhere is enough to rule it out and keep
	// an ordinary race from being reported as a deployment fault.
	if !observed && allExited(results) {
		for pid, result := range results {
			result.Reason = ReasonInvisible
			results[pid] = result
		}
	}
	return results
}

// resolveOne attributes a single process, and reports whether the process was observed to exist.
//
// The order of the checks is the order in which a wrong answer would be most expensive: the
// identity has to exist before it can be trusted, and it has to be trusted before it is looked up.
func (r *Resolver) resolveOne(index PodIndex, pid uint32) (Result, bool) {
	identity, reason := r.readIdentity(pid)
	switch {
	case reason == ReasonExited, reason == ReasonPermission, reason == ReasonUnreadable:
		// A /proc entry that could not be read is no evidence either way about the process.
		return Result{Reason: reason}, false
	case reason != reasonNone:
		// The entry was read; only its content was unusable.
		return Result{Reason: reason}, true
	}

	if reason := r.readLiveness(pid); reason != reasonNone {
		return Result{Reason: reason}, true
	}

	if reason := r.readMediation(pid); reason != reasonNone {
		return Result{Reason: reason}, true
	}

	// Re-read the identity last, so the window this closes covers every read above it. A PID
	// recycled into another Pod between the vendor's query and here would otherwise be charged
	// to whoever holds it now; a disagreement rejects the row instead.
	//
	// A second read that fails is not a disagreement — a process may simply have ended, and
	// ending does not change which Pod it ran in — so only a successful, differing read rejects.
	// The residual window, a reuse completed before the first read, cannot be closed from a
	// numeric PID alone and is bounded by the gap between the two reads.
	if second, reason := r.readIdentity(pid); reason == reasonNone && second != identity {
		return Result{Reason: ReasonUnstable}, true
	}

	pod, known := index[identity.PodUID]
	switch {
	case !known:
		return Result{Reason: ReasonUnknownPod}, true
	case !pod.Instance:
		return Result{Outcome: OutcomeExcluded}, true
	case !pod.Containers.Has(identity.ContainerID):
		return Result{Reason: ReasonUnknownContainer}, true
	}
	return Result{Outcome: OutcomeAttributed, Identity: identity}, true
}

// readIdentity reads and parses the process's cgroup membership.
func (r *Resolver) readIdentity(pid uint32) (Identity, Reason) {
	content, err := r.readProcFile(pid, "cgroup")
	if err != nil {
		return Identity{}, readFailureReason(err)
	}
	return parseCgroup(string(content))
}

// readLiveness rejects a process the kernel is keeping only for its exit status. Such a process
// holds no device memory, so a vendor row naming it is a stale row and its figure is not this
// sample's truth.
func (r *Resolver) readLiveness(pid uint32) Reason {
	content, err := r.readProcFile(pid, "stat")
	if err != nil {
		return readFailureReason(err)
	}
	state, ok := parseProcState(string(content))
	if !ok {
		return ReasonUnreadable
	}
	if state == 'Z' {
		return ReasonZombie
	}
	return reasonNone
}

// readMediation rejects a process that holds the device on behalf of other Pods' processes.
//
// Charging its usage to its own Pod would be wrong, and dividing it between the Pods it serves is
// not possible from the device's process list — those Pods' own processes never appear there at
// all. So the honest answer for the whole device is "not measurable", which is what refusing here
// produces.
func (r *Resolver) readMediation(pid uint32) Reason {
	content, err := r.readProcFile(pid, "comm")
	if err != nil {
		return readFailureReason(err)
	}
	if mediatingProcesses.Has(strings.TrimSpace(string(content))) {
		return ReasonMediated
	}
	return reasonNone
}

func (r *Resolver) readProcFile(pid uint32, name string) ([]byte, error) {
	return fs.ReadFile(r.fsys, path.Join(strconv.FormatUint(uint64(pid), 10), name))
}

// readFailureReason classifies a /proc read failure. The first two cases are told apart because
// they mean opposite things: a process that ended between the vendor's query and this read is an
// ordinary race that the next sample recovers from, while a permission failure refuses every
// process on the node until the deployment changes.
func readFailureReason(err error) Reason {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ReasonExited
	case errors.Is(err, fs.ErrPermission):
		return ReasonPermission
	}
	return ReasonUnreadable
}

// parseProcState returns the state field of a /proc/<pid>/stat line. The scan starts after the
// last ')' because the field ahead of it is the executable name, which may itself contain both
// spaces and parentheses.
func parseProcState(stat string) (byte, bool) {
	i := strings.LastIndex(stat, ")")
	if i < 0 {
		return 0, false
	}
	rest := strings.TrimLeft(stat[i+1:], " ")
	if rest == "" {
		return 0, false
	}
	return rest[0], true
}

// allExited reports whether every process was missing from the /proc tree, which is what a
// process namespace the reader cannot see looks like from the outside.
func allExited(results map[uint32]Result) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		if result.Reason != ReasonExited {
			return false
		}
	}
	return true
}
