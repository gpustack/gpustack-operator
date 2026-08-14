package procattr

import (
	"io/fs"
	"path"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/sets"
)

// process describes one fake process in a fake /proc tree.
type process struct {
	pid uint32
	// cgroups is served one entry per read of the process's cgroup file, the last entry
	// repeating once the earlier ones are exhausted. More than one entry is how a case makes the
	// process's identity change between the two reads attribution performs.
	cgroups []string
	// state is the /proc/<pid>/stat state letter, defaulting to a running process.
	state byte
	// comm is the executable name, truncated by the kernel to 15 characters.
	comm string
	// stat replaces the generated stat content when a case needs a malformed one.
	stat string
	// omit names the files to leave out of the tree, which is how a case makes a process end
	// part-way through the reads attribution performs.
	omit []string
	// err makes every read of this process's files fail with it.
	err error
}

// procTree is a fake /proc supporting the two things a map-backed fs.FS cannot: a read that fails
// with a permission error, and a file whose content differs between two reads of one path.
type procTree struct {
	files map[string][]string
	errs  map[string]error
}

func newProcTree(procs ...process) *procTree {
	tree := &procTree{files: map[string][]string{}, errs: map[string]error{}}
	for _, p := range procs {
		dir := strconv.FormatUint(uint64(p.pid), 10)
		if p.err != nil {
			for _, name := range []string{"cgroup", "stat", "comm"} {
				tree.errs[path.Join(dir, name)] = p.err
			}
			continue
		}

		state := p.state
		if state == 0 {
			state = 'S'
		}
		comm := p.comm
		if comm == "" {
			comm = "python3"
		}
		stat := p.stat
		if stat == "" {
			// The executable name is parenthesised and may itself contain spaces and
			// parentheses, which is what the parser has to survive.
			stat = strconv.FormatUint(uint64(p.pid), 10) + " (py (thon) 3) " + string(state) + " 1 1 0 0"
		}

		tree.files[path.Join(dir, "cgroup")] = p.cgroups
		tree.files[path.Join(dir, "stat")] = []string{stat}
		tree.files[path.Join(dir, "comm")] = []string{comm + "\n"}
		for _, name := range p.omit {
			delete(tree.files, path.Join(dir, name))
		}
	}
	return tree
}

func (t *procTree) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
}

func (t *procTree) ReadFile(name string) ([]byte, error) {
	if err, ok := t.errs[name]; ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	contents, ok := t.files[name]
	if !ok || len(contents) == 0 {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	if len(contents) > 1 {
		t.files[name] = contents[1:]
	}
	return []byte(contents[0]), nil
}

// containerScope returns the cgroup content of a container running in a Pod.
func containerScope(escapedUID, id string) string {
	return "0::" + systemdPodSlice(escapedUID) + "/cri-containerd-" + id + ".scope"
}

// instancePod indexes one Pod that backs an Instance, carrying the given container IDs.
func instancePod(ids ...string) Pod {
	return Pod{Instance: true, Containers: sets.New(ids...)}
}

func TestResolverResolve(t *testing.T) {
	const (
		pid        = uint32(4242)
		siblingPID = uint32(4343)
	)

	cases := []struct {
		name string

		procs []process
		index PodIndex
		pids  []uint32

		want map[uint32]Result
	}{
		{
			name:  "attributed to its own container",
			procs: []process{{pid: pid, cgroups: []string{containerScope(escapedPodUID, containerID)}}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid},
			want: map[uint32]Result{
				pid: {Outcome: OutcomeAttributed, Identity: Identity{PodUID: podUID, ContainerID: containerID}},
			},
		},
		{
			// A Pod the node is running that backs no Instance: its usage is real but is no
			// slice's, so dropping it leaves the rest of the device measurable.
			name:  "known_non_instance_pod",
			procs: []process{{pid: pid, cgroups: []string{containerScope(escapedPodUID, containerID)}}},
			index: PodIndex{podUID: {Containers: sets.New(containerID)}},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Outcome: OutcomeExcluded}},
		},
		{
			// The case the whole feature exists for: two Instances on one card, each reading
			// its own figure.
			name: "sibling_instance_same_card",
			procs: []process{
				{pid: pid, cgroups: []string{containerScope(escapedPodUID, containerID)}},
				{pid: siblingPID, cgroups: []string{containerScope(escapedSiblingPodUID, siblingContainerID)}},
			},
			index: PodIndex{
				podUID:        instancePod(containerID),
				siblingPodUID: instancePod(siblingContainerID),
			},
			pids: []uint32{pid, siblingPID},
			want: map[uint32]Result{
				pid:        {Outcome: OutcomeAttributed, Identity: Identity{PodUID: podUID, ContainerID: containerID}},
				siblingPID: {Outcome: OutcomeAttributed, Identity: Identity{PodUID: siblingPodUID, ContainerID: siblingContainerID}},
			},
		},
		{
			name:  "unknown_pod_uid",
			procs: []process{{pid: pid, cgroups: []string{containerScope(escapedPodUID, containerID)}}},
			index: PodIndex{siblingPodUID: instancePod(siblingContainerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonUnknownPod}},
		},
		{
			// Never guess a container. An Instance Pod whose status names other containers is
			// not evidence about this one.
			name:  "container_id_unknown",
			procs: []process{{pid: pid, cgroups: []string{containerScope(escapedPodUID, containerID)}}},
			index: PodIndex{podUID: instancePod(siblingContainerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonUnknownContainer}},
		},
		{
			// One PID gone while another is readable is the ordinary race, not invisibility.
			name:  "pid_exited_before_read",
			procs: []process{{pid: pid, cgroups: []string{containerScope(escapedPodUID, containerID)}}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid, siblingPID},
			want: map[uint32]Result{
				pid:        {Outcome: OutcomeAttributed, Identity: Identity{PodUID: podUID, ContainerID: containerID}},
				siblingPID: {Reason: ReasonExited},
			},
		},
		{
			name:  "pid_permission_denied",
			procs: []process{{pid: pid, err: fs.ErrPermission}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonPermission}},
		},
		{
			name: "zombie_process",
			procs: []process{{
				pid:     pid,
				cgroups: []string{containerScope(escapedPodUID, containerID)},
				state:   'Z',
			}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonZombie}},
		},
		{
			// The PID was the first Pod's when the vendor sampled it and is the second Pod's by
			// the time it is read again. Charging it to either is a guess.
			name: "pid_reused_by_sibling_pod",
			procs: []process{{pid: pid, cgroups: []string{
				containerScope(escapedPodUID, containerID),
				containerScope(escapedSiblingPodUID, siblingContainerID),
			}}},
			index: PodIndex{
				podUID:        instancePod(containerID),
				siblingPodUID: instancePod(siblingContainerID),
			},
			pids: []uint32{pid},
			want: map[uint32]Result{pid: {Reason: ReasonUnstable}},
		},
		{
			name: "cgroup_changes_during_read",
			procs: []process{{pid: pid, cgroups: []string{
				containerScope(escapedPodUID, containerID),
				containerScope(escapedPodUID, siblingContainerID),
			}}},
			index: PodIndex{podUID: instancePod(containerID, siblingContainerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonUnstable}},
		},
		{
			// Every reported PID missing at once is a reader that cannot see the vendor's
			// process namespace — a deployment to fix, not a crowd of coincidences.
			name:  "vendor_pid_namespace_hidden",
			procs: nil,
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid, siblingPID},
			want: map[uint32]Result{
				pid:        {Reason: ReasonInvisible},
				siblingPID: {Reason: ReasonInvisible},
			},
		},
		{
			name: "mps_daemon_in_host_cgroup",
			procs: []process{{
				pid:     pid,
				cgroups: []string{"0::/system.slice/nvidia-cuda-mps.service"},
				comm:    "nvidia-cuda-mps",
			}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonNoPodComponent}},
		},
		{
			// The daemon sits in a real Instance Pod but serves others too, so its usage is
			// neither that Pod's nor divisible from the device's process list.
			name: "mps_daemon_in_sibling_pod",
			procs: []process{{
				pid:     pid,
				cgroups: []string{containerScope(escapedPodUID, containerID)},
				comm:    "nvidia-cuda-mps",
			}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonMediated}},
		},
		{
			name: "stat without the executable terminator",
			procs: []process{{
				pid:     pid,
				cgroups: []string{containerScope(escapedPodUID, containerID)},
				stat:    "4242 unterminated",
			}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonUnreadable}},
		},
		{
			name: "stat carrying nothing after the executable name",
			procs: []process{{
				pid:     pid,
				cgroups: []string{containerScope(escapedPodUID, containerID)},
				stat:    "4242 (python3)",
			}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonUnreadable}},
		},
		{
			// Ending part-way through the reads is the same race as ending before them, and must
			// read as the same reason rather than as a malformed process.
			name: "pid exited between the identity and liveness reads",
			procs: []process{{
				pid:     pid,
				cgroups: []string{containerScope(escapedPodUID, containerID)},
				omit:    []string{"stat"},
			}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonExited}},
		},
		{
			name: "pid exited between the liveness and mediation reads",
			procs: []process{{
				pid:     pid,
				cgroups: []string{containerScope(escapedPodUID, containerID)},
				omit:    []string{"comm"},
			}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonExited}},
		},
		{
			// Neither a missing process nor a refused one: /proc answered with something else
			// entirely, and saying so beats folding it into either.
			name:  "proc read fails with neither absence nor refusal",
			procs: []process{{pid: pid, err: fs.ErrInvalid}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid},
			want:  map[uint32]Result{pid: {Reason: ReasonUnreadable}},
		},
		{
			// A device with no processes is not an invisible namespace.
			name:  "no reported processes",
			procs: nil,
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  nil,
			want:  map[uint32]Result{},
		},
		{
			name:  "a repeated pid resolves once",
			procs: []process{{pid: pid, cgroups: []string{containerScope(escapedPodUID, containerID)}}},
			index: PodIndex{podUID: instancePod(containerID)},
			pids:  []uint32{pid, pid},
			want: map[uint32]Result{
				pid: {Outcome: OutcomeAttributed, Identity: Identity{PodUID: podUID, ContainerID: containerID}},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := New(newProcTree(c.procs...)).Resolve(c.index, c.pids)

			assert.Equal(t, c.want, got)
		})
	}
}
