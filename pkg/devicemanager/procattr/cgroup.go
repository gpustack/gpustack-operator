package procattr

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	// podSegmentPrefix is what both cgroup drivers put ahead of the Pod UID: the cgroupfs driver
	// writes the segment "pod<uid>", the systemd driver ends its slice name with "-pod<uid>".
	podSegmentPrefix = "pod"
	// sliceSuffix and scopeSuffix mark the two systemd unit kinds kubelet creates — a slice per
	// Pod, a scope per container.
	sliceSuffix = ".slice"
	scopeSuffix = ".scope"
	// uuidLength is the length of a UUID in its canonical hyphenated form, which is what an
	// ordinary Pod's UID is.
	uuidLength = 36
	// hashedUIDLength is the length of a static Pod's UID, which kubelet derives from the Pod's
	// name instead of allocating a UUID: 32 unseparated hexadecimal digits.
	hashedUIDLength = 32
	// minContainerIDLength is the shortest hexadecimal run accepted as a container ID. It exists
	// to keep an incidental short hex segment — a queue depth, an index — from being read as a
	// container, and is deliberately far below the 64 characters the runtimes actually write.
	minContainerIDLength = 8
)

// containerScopePrefixes are the runtime prefixes a systemd-driver container scope carries ahead
// of the container ID. A scope named by none of them is left unparsed rather than guessed at: the
// alternative is treating an arbitrary unit name as a container ID.
var containerScopePrefixes = []string{"cri-containerd-", "containerd-", "crio-", "docker-"}

// parseCgroup resolves the content of a process's cgroup file to the Pod and container it names,
// or to the reason it could not be resolved. The returned Identity is meaningful only when the
// reason is empty.
//
// Every line is read, not just the first, because cgroup v1 gives a process one line per
// controller hierarchy and they are all supposed to agree. Agreement is checked rather than
// assumed: controllers naming different Pods is the shape of a half-migrated process, and picking
// one of them would be a coin toss dressed as a measurement. Lines that name the same Pod and
// container collapse to a single attribution, so a v1 process is never counted once per
// controller.
func parseCgroup(content string) (Identity, Reason) {
	podUIDs, containerIDs := sets.New[string](), sets.New[string]()
	for _, line := range strings.Split(content, "\n") {
		// A cgroup line is "hierarchy-ID:controller-list:path"; v2 writes the single line
		// "0::<path>". Anything shorter is not a cgroup line and contributes nothing.
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 {
			continue
		}
		pods, containers := parseCgroupPath(fields[2])
		podUIDs.Insert(pods...)
		containerIDs.Insert(containers...)
	}

	switch {
	case podUIDs.Len() == 0:
		return Identity{}, ReasonNoPodComponent
	case podUIDs.Len() > 1:
		return Identity{}, ReasonAmbiguousPod
	case containerIDs.Len() == 0:
		return Identity{}, ReasonNoContainerComponent
	case containerIDs.Len() > 1:
		return Identity{}, ReasonAmbiguousContainer
	}

	return Identity{
		PodUID:      podUIDs.UnsortedList()[0],
		ContainerID: containerIDs.UnsortedList()[0],
	}, reasonNone
}

// parseCgroupPath collects the distinct Pod UIDs a single cgroup path names, and the distinct
// container IDs found below the first of them.
//
// Container candidates are taken only from segments beneath the Pod segment, which is what makes
// a Pod UID appearing inside a container's own unit name harmless: it is never in a position where
// a container is looked for, and a Pod is never looked for by substring.
func parseCgroupPath(path string) (podUIDs, containerIDs []string) {
	segments := strings.Split(strings.Trim(path, "/"), "/")

	podAt := -1
	for i := range segments {
		uid, ok := parsePodUID(segments[i])
		if !ok {
			continue
		}
		podUIDs = append(podUIDs, uid)
		if podAt < 0 {
			podAt = i
		}
	}
	if podAt < 0 {
		return podUIDs, nil
	}

	for _, segment := range segments[podAt+1:] {
		if id, ok := parseContainerID(segment); ok {
			containerIDs = append(containerIDs, id)
		}
	}
	return podUIDs, containerIDs
}

// parsePodUID reads the Pod UID a single path segment names, in either cgroup driver's spelling:
// the cgroupfs driver's bare "pod<uid>", or the systemd driver's
// "kubepods-<qos>-pod<uid>.slice", where the UID's hyphens are escaped to underscores because
// systemd unit names use the hyphen as their own separator.
//
// The match is anchored at both ends of the UID in both spellings, so a segment that merely
// contains "pod<uid>" — a cache directory, a container unit named after the Pod — yields nothing.
func parsePodUID(segment string) (string, bool) {
	if uid, ok := strings.CutPrefix(segment, podSegmentPrefix); ok {
		return canonicalPodUID(uid)
	}

	slice, ok := strings.CutSuffix(segment, sliceSuffix)
	if !ok {
		return "", false
	}
	// Only the last hyphen-separated component of the slice name can hold the UID; everything
	// ahead of it is the enclosing slice hierarchy ("kubepods-besteffort-").
	if i := strings.LastIndex(slice, "-"); i >= 0 {
		slice = slice[i+1:]
	}
	uid, ok := strings.CutPrefix(slice, podSegmentPrefix)
	if !ok {
		return "", false
	}
	return canonicalPodUID(uid)
}

// canonicalPodUID validates a Pod UID in any spelling a cgroup path carries and returns it in the
// form Kubernetes itself reports, so an attribution keyed on it matches a Pod UID read from the API
// whichever cgroup driver wrote the path.
//
// Two spellings occur on a real node. An ordinary Pod's UID is a UUID, which the systemd driver
// writes with its hyphens escaped to underscores because the hyphen is systemd's own separator
// inside a unit name. A static Pod's UID is not a UUID at all — kubelet derives it from the Pod's
// name — and the API reports those 32 unseparated digits verbatim, so they are returned unchanged.
//
// Refusing the second spelling would be worse than merely incomplete. A static Pod is a Pod the
// node knows and no Instance owns, so recognizing its UID is exactly what lets a process of its be
// excluded safely; unrecognized, it would instead make every figure on its device unmeasurable.
func canonicalPodUID(uid string) (string, bool) {
	separated := len(uid) == uuidLength
	if !separated && len(uid) != hashedUIDLength {
		return "", false
	}

	canonical := []byte(strings.ToLower(uid))
	for i := range canonical {
		switch {
		case separated && isUUIDSeparatorPosition(i):
			if canonical[i] != '-' && canonical[i] != '_' {
				return "", false
			}
			canonical[i] = '-'
		case !isHexDigit(canonical[i]):
			return "", false
		}
	}
	return string(canonical), true
}

// isUUIDSeparatorPosition reports whether a UUID carries its separator at the given offset. The
// positions are checked rather than the character, so a separator anywhere else is a rejection
// instead of being read as one more digit position that happens not to be a digit.
func isUUIDSeparatorPosition(i int) bool {
	return i == 8 || i == 13 || i == 18 || i == 23
}

// parseContainerID reads the container ID a single path segment names: the cgroupfs driver's bare
// ID, or the systemd driver's "<runtime>-<id>.scope". The ID must be one unbroken hexadecimal run,
// which is what a runtime writes and what keeps a scope named anything else from being read as a
// container.
func parseContainerID(segment string) (string, bool) {
	id := segment
	if scope, ok := strings.CutSuffix(segment, scopeSuffix); ok {
		id = ""
		for _, prefix := range containerScopePrefixes {
			if rest, ok := strings.CutPrefix(scope, prefix); ok {
				id = rest
				break
			}
		}
	}

	// Normalized BEFORE it is validated, exactly as canonicalPodUID does it: isHexDigit accepts
	// lowercase digits only, so validating first would reject an uppercase spelling that the
	// normalization exists to accept — and reject it as "not a container" rather than as a case
	// difference.
	id = strings.ToLower(id)
	if len(id) < minContainerIDLength {
		return "", false
	}
	for i := range len(id) {
		if !isHexDigit(id[i]) {
			return "", false
		}
	}
	return id, true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}
