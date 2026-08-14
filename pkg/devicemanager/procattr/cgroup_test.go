package procattr

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	// podUID and containerID are the fixture identities every case below is built from.
	podUID      = "01234567-89ab-cdef-0123-456789abcdef"
	containerID = "deadbeef"
	// escapedPodUID is podUID as the systemd cgroup driver escapes it, the hyphen being
	// systemd's own separator inside a unit name.
	escapedPodUID = "01234567_89ab_cdef_0123_456789abcdef"
	// hashedPodUID is the shape a static Pod's UID takes: 32 unseparated hexadecimal digits
	// kubelet derives from the Pod's name rather than a UUID it allocated.
	hashedPodUID = "c52f1c85c116e70df9bd9c41c3b0be02"
	// longContainerID is the full-length ID a container runtime actually writes, as opposed to
	// the short containerID the other cases keep for readability.
	longContainerID = "d69c73a11a0829217920c272ed0ddbca54750c9dde8dd1a806e42cdc8564fafa"

	// siblingPodUID and siblingContainerID stand for a second workload, used wherever a case
	// needs two identities to disagree about.
	siblingPodUID        = "fedcba98-7654-3210-fedc-ba9876543210"
	escapedSiblingPodUID = "fedcba98_7654_3210_fedc_ba9876543210"
	siblingContainerID   = "cafebabe"
)

// systemdPodSlice returns the systemd-driver slice path of a Pod, the form kubelet writes when the
// cgroup driver is systemd.
func systemdPodSlice(escapedUID string) string {
	return "/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + escapedUID + ".slice"
}

// cgroupfsPodPath returns the cgroupfs-driver path of a Pod.
func cgroupfsPodPath(uid string) string {
	return "/kubepods/burstable/pod" + uid
}

// v1Lines returns one cgroup v1 line per controller, all naming the same path.
func v1Lines(path string, controllers ...string) string {
	lines := make([]string, 0, len(controllers))
	for i, controller := range controllers {
		lines = append(lines, strings.Join([]string{
			// The hierarchy IDs are arbitrary; only the third field is read.
			string(rune('0' + i%10)), controller, path,
		}, ":"))
	}
	return strings.Join(lines, "\n")
}

func TestParseCgroup(t *testing.T) {
	cases := []struct {
		name string

		content string

		wantIdentity Identity
		wantReason   Reason
	}{
		{
			name:         "v2_systemd_containerd",
			content:      "0::" + systemdPodSlice(escapedPodUID) + "/cri-containerd-" + containerID + ".scope",
			wantIdentity: Identity{PodUID: podUID, ContainerID: containerID},
		},
		{
			name:         "v2_systemd_crio",
			content:      "0::" + systemdPodSlice(escapedPodUID) + "/crio-" + containerID + ".scope",
			wantIdentity: Identity{PodUID: podUID, ContainerID: containerID},
		},
		{
			// Both halves are normalized rather than matched case-sensitively, so an attribution
			// keyed on either one matches what the API reports whatever spelling the path carries.
			name: "v2_uppercase_ids_are_normalized_not_rejected",
			content: "0::" + systemdPodSlice(strings.ToUpper(escapedPodUID)) +
				"/cri-containerd-" + strings.ToUpper(containerID) + ".scope",
			wantIdentity: Identity{PodUID: podUID, ContainerID: containerID},
		},
		{
			name:         "v2_cgroupfs_containerd",
			content:      "0::" + cgroupfsPodPath(podUID) + "/" + containerID,
			wantIdentity: Identity{PodUID: podUID, ContainerID: containerID},
		},
		{
			// One process, one attribution — not one per controller hierarchy.
			name:         "v1_cgroupfs_multiple_controllers",
			content:      v1Lines(cgroupfsPodPath(podUID)+"/"+containerID, "cpu,cpuacct", "memory", "devices"),
			wantIdentity: Identity{PodUID: podUID, ContainerID: containerID},
		},
		{
			name: "v1_systemd_underscore_uid",
			content: v1Lines(
				systemdPodSlice(escapedPodUID)+"/cri-containerd-"+containerID+".scope",
				"cpu,cpuacct", "memory",
			),
			wantIdentity: Identity{PodUID: podUID, ContainerID: containerID},
		},
		{
			name: "same_uid_all_v1_controllers",
			content: v1Lines(
				cgroupfsPodPath(podUID)+"/"+containerID,
				"name=systemd", "cpu,cpuacct", "memory", "devices", "pids", "blkio", "freezer",
			),
			wantIdentity: Identity{PodUID: podUID, ContainerID: containerID},
		},
		{
			// A thread's cgroup file is its group leader's, verbatim. The assertion is that
			// nothing special happens: a TID resolves exactly as its process does.
			name:         "thread_id_same_domain_cgroup",
			content:      "0::" + systemdPodSlice(escapedPodUID) + "/cri-containerd-" + containerID + ".scope",
			wantIdentity: Identity{PodUID: podUID, ContainerID: containerID},
		},
		{
			// Segments below the container do not obscure it, as long as it stays the only one.
			name:         "threaded_cgroup_child",
			content:      "0::" + systemdPodSlice(escapedPodUID) + "/cri-containerd-" + containerID + ".scope/threaded",
			wantIdentity: Identity{PodUID: podUID, ContainerID: containerID},
		},
		{
			// A static Pod's UID is not a UUID: kubelet derives it from the Pod's name, and both
			// the cgroup path and the API report those 32 unseparated digits.
			name:         "static_pod_hashed_uid",
			content:      "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + hashedPodUID + ".slice/cri-containerd-" + longContainerID + ".scope",
			wantIdentity: Identity{PodUID: hashedPodUID, ContainerID: longContainerID},
		},
		{
			// The full-length shape a live cgroup v1 node writes, one line per controller
			// hierarchy, with the runtime's real 64-digit container ID.
			name: "v1_systemd_full_length_ids",
			content: v1Lines(
				"/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod"+escapedPodUID+
					".slice/cri-containerd-"+longContainerID+".scope",
				"name=systemd", "cpuset", "devices", "net_cls,net_prio", "memory", "pids",
			),
			wantIdentity: Identity{PodUID: podUID, ContainerID: longContainerID},
		},
		{
			// On a cgroup v2 node the container runtime gives each Pod its own cgroup namespace,
			// so the kernel writes another Pod's path relative to the reader's root — everything
			// above their common ancestor collapsing into ".." entries. The Pod is still named,
			// and the ".." entries name nothing, so the path resolves unchanged.
			name: "v2_private_cgroupns_relative_path",
			content: "0::/../../../kubepods-besteffort.slice/kubepods-besteffort-pod" + escapedPodUID +
				".slice/cri-containerd-" + longContainerID + ".scope",
			wantIdentity: Identity{PodUID: podUID, ContainerID: longContainerID},
		},
		{
			// The one shape that relativisation does hide: a process in the reader's own Pod,
			// whose Pod segment is above the reader's namespace root and so is elided entirely.
			// Refusing beats inferring "the Pod I am in" from the absence of a segment.
			name:       "v2_private_cgroupns_pod_segment_elided",
			content:    "0::/../cri-containerd-" + longContainerID + ".scope",
			wantReason: ReasonNoPodComponent,
		},
		{
			name:       "host_process_no_pod_component",
			content:    "0::/system.slice/nvidia-persistenced.service",
			wantReason: ReasonNoPodComponent,
		},
		{
			// "pod<uid>" inside a longer segment is not a Pod component. Matching it by
			// substring would attribute a cache directory's processes to a workload.
			name:       "pod_uid_substring",
			content:    "0::/kubepods/burstable/worker-pod" + podUID + "-cache/" + containerID,
			wantReason: ReasonNoPodComponent,
		},
		{
			// A container unit whose own name carries the Pod UID names no Pod: a container is
			// only looked for below a Pod, and a Pod is never looked for by substring.
			name:       "uid_in_container_id",
			content:    "0::/kubepods.slice/cri-containerd-pod" + podUID + ".scope",
			wantReason: ReasonNoPodComponent,
		},
		{
			name: "two_different_pod_ancestors",
			content: "0::/kubepods.slice/kubepods-pod" + escapedPodUID + ".slice/kubepods-pod" +
				escapedSiblingPodUID + ".slice/cri-containerd-" + containerID + ".scope",
			wantReason: ReasonAmbiguousPod,
		},
		{
			name: "v1_controllers_disagree",
			content: "1:cpu,cpuacct:" + cgroupfsPodPath(podUID) + "/" + containerID + "\n" +
				"2:memory:" + cgroupfsPodPath(siblingPodUID) + "/" + siblingContainerID,
			wantReason: ReasonAmbiguousPod,
		},
		{
			name: "v1_containers_disagree",
			content: "1:cpu,cpuacct:" + cgroupfsPodPath(podUID) + "/" + containerID + "\n" +
				"2:memory:" + cgroupfsPodPath(podUID) + "/" + siblingContainerID,
			wantReason: ReasonAmbiguousContainer,
		},
		{
			// A VM-isolating runtime runs the whole Pod in one process under the Pod slice, so
			// there is no container to charge its usage to.
			name:       "kata_qemu_pid",
			content:    "0::" + systemdPodSlice(escapedPodUID),
			wantReason: ReasonNoContainerComponent,
		},
		{
			name:       "gvisor_sandbox_shared_container_identity",
			content:    "0::" + systemdPodSlice(escapedPodUID) + "/runsc-sandbox",
			wantReason: ReasonNoContainerComponent,
		},
		{
			name:       "pod segment too short to be a uid",
			content:    "0::/kubepods/burstable/pod0123/" + containerID,
			wantReason: ReasonNoPodComponent,
		},
		{
			name:       "pod segment carries a non-hexadecimal digit",
			content:    "0::/kubepods/burstable/pod0123456g-89ab-cdef-0123-456789abcdef/" + containerID,
			wantReason: ReasonNoPodComponent,
		},
		{
			name:       "pod segment separated by neither hyphen nor underscore",
			content:    "0::/kubepods/burstable/pod01234567x89ab-cdef-0123-456789abcdef/" + containerID,
			wantReason: ReasonNoPodComponent,
		},
		{
			// A scope this code cannot name the runtime of is left unparsed rather than read as
			// a container ID, which is what an arbitrary unit name would become.
			name:       "container scope of an unrecognised runtime",
			content:    "0::" + systemdPodSlice(escapedPodUID) + "/podman-" + containerID + ".scope",
			wantReason: ReasonNoContainerComponent,
		},
		{
			name:       "empty content",
			content:    "",
			wantReason: ReasonNoPodComponent,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			identity, reason := parseCgroup(c.content)

			assert.Equal(t, c.wantReason, reason)
			assert.Equal(t, c.wantIdentity, identity)
		})
	}
}
