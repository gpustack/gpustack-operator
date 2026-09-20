package deviceplugin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// sysfsAttrMaxBytes caps one attribute read. An attribute is a single short value; a
	// larger read means the path is not the attribute it was taken for.
	sysfsAttrMaxBytes = 64 << 10

	sysfsInfinibandDir = "class/infiniband"

	// sysfsVerbsClassName is the kernel class the verbs character devices belong to. It names
	// two places, which is why it is a constant of its own: the class directory below, and the
	// directory of that name the kernel creates under a device's hardware parent. A class
	// device appears at <parent>/<class>/<name>, never directly under <parent> -- the same
	// rule that puts the RDMA device itself at <parent>/infiniband/<name>.
	sysfsVerbsClassName = "infiniband_verbs"
	sysfsVerbsClassDir  = "class/" + sysfsVerbsClassName

	// devInfinibandDir is where the kernel places the RDMA character devices; the uverbs
	// class names each device's node infiniband/uverbsN, so the answer is derived from the
	// entry's own name rather than probed under /dev.
	devInfinibandDir = "/dev/infiniband"

	ibdevAttr        = "ibdev"
	verbsEntryPrefix = "uverbs"
)

// outsideRootError marks a path whose symlink chain resolved to a location outside the sysfs
// root. It is distinct from an unreadable path so the resolution can refuse the whole tree
// rather than degrade a refusal into a miss.
type outsideRootError struct {
	rel      string
	resolved string
	root     string
}

func (e *outsideRootError) Error() string {
	return fmt.Sprintf("sysfs path %q resolves to %q, outside %q", e.rel, e.resolved, e.root)
}

func isOutsideRoot(err error) bool {
	var outside *outsideRootError
	return errors.As(err, &outside)
}

// verbsDevicePath resolves an RDMA device name, as it appears in DeviceInterface.RDMADevice,
// to the path of that device's verbs character device under /dev/infiniband.
//
// The sysfs root is a parameter rather than a compiled-in constant for the reason the
// detector states for its own tree: the file selecting the real root is constrained to Linux
// and does not compile on the platform this code is written on, so passing the root in is
// what lets the resolution run against a fixture.
//
// Two sysfs layouts are consulted, and a negative answer names both, because a single
// hard-coded path that is wrong on one distribution fails exactly like a host that has no
// RDMA at all:
//
//   - class/infiniband_verbs/uverbsN, whose ibdev attribute carries the RDMA device name.
//     It answers first, as it is the layout every kernel examined exposes and the one
//     covering RDMA devices with no hardware parent beside which to look.
//   - the infiniband_verbs directory under class/infiniband/<name>/device, where the kernel
//     hangs each character device off the RDMA device's own hardware parent. It answers for a
//     tree that exposes the device hierarchy but not the verbs class. The class name is a
//     directory level: the entries are at <parent>/infiniband_verbs/uverbsN, not directly
//     under <parent>, exactly as the RDMA device itself sits at <parent>/infiniband/<name>.
//
// Nothing under /dev is opened; the returned path is derived, and opening it stays with the
// container runtime.
func verbsDevicePath(sysfsRoot, name string) (string, error) {
	if name == "" {
		return "", errors.New("the RDMA device name is empty")
	}
	root, err := filepath.EvalSymlinks(sysfsRoot)
	if err != nil {
		return "", fmt.Errorf("resolve the sysfs root %q: %w", sysfsRoot, err)
	}

	entry, unresolvable, err := scanVerbsEntries(root, filepath.Join(root, sysfsVerbsClassDir), name)
	if err != nil {
		return "", err
	}
	if entry != "" {
		return filepath.Join(devInfinibandDir, entry), nil
	}

	// The second layout anchors on the RDMA device's own class entry, whose device attribute
	// points at the hardware parent the kernel hangs the character devices off.
	anchor := filepath.Join(root, sysfsInfinibandDir, name, "device")
	parent, aerr := resolveInside(root, anchor)
	switch {
	case aerr == nil:
		entry, unresolvable2, err := scanVerbsEntries(
			root, filepath.Join(parent, sysfsVerbsClassName), name)
		if err != nil {
			return "", err
		}
		if entry != "" {
			return filepath.Join(devInfinibandDir, entry), nil
		}
		unresolvable = errors.Join(unresolvable, unresolvable2)
	case isOutsideRoot(aerr):
		return "", aerr
	case os.IsNotExist(aerr) && !presentNoFollow(anchor) && !danglingClassEntry(root, name):
		// The device link is not there and the RDMA class entry is a real directory: a
		// device with no hardware parent, which this layout has nothing to say about.
	default:
		// A link that is there and would not resolve might have located the parent, so it
		// may not be read as the layout's absence.
		unresolvable = errors.Join(unresolvable, fmt.Errorf(
			"resolve the device link of %q: %w", filepath.Join(sysfsInfinibandDir, name), aerr))
	}

	if unresolvable != nil {
		return "", fmt.Errorf(
			"whether RDMA device %q has a verbs character device could not be established "+
				"under %q: %w",
			name, sysfsRoot, unresolvable)
	}
	return "", fmt.Errorf(
		"no verbs character device for RDMA device %q under %q: tried both sysfs layouts, "+
			"%q (the verbs class directory) and the %q directory under %q (the RDMA "+
			"device's hardware parent)",
		name, sysfsRoot, sysfsVerbsClassDir, sysfsVerbsClassName,
		filepath.Join(sysfsInfinibandDir, name, "device"))
}

// scanVerbsEntries lists one directory of candidate uverbs devices and returns the entry
// whose ibdev attribute carries name.
//
// unresolvable carries the candidates that could not be consulted. A candidate that cannot
// be read might have been the match, so it is returned rather than skipped: a miss over a
// directory holding one may not be reported as the device's absence. A match anywhere in the
// directory still answers, and no failure is reported for it. err is a refusal rather than a
// miss: a symlink steered a read outside the root, and the tree is not this process's to
// trust.
func scanVerbsEntries(realRoot, dir, name string) (entry string, unresolvable, err error) {
	if _, derr := resolveInside(realRoot, dir); derr != nil {
		if isOutsideRoot(derr) {
			return "", nil, derr
		}
		if !os.IsNotExist(derr) || presentNoFollow(dir) {
			return "", fmt.Errorf("resolve the uverbs candidates under %s: %w", dir, derr), nil
		}
		// The layout is not on this tree, which is an ordinary answer for a layout a host
		// may not expose.
		return "", nil, nil
	}

	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		return "", fmt.Errorf("list %s: %w", dir, rerr), nil
	}
	for _, candidate := range entries {
		if !strings.HasPrefix(candidate.Name(), verbsEntryPrefix) {
			continue
		}
		ibdev, aerr := readAttr(realRoot, filepath.Join(dir, candidate.Name(), ibdevAttr))
		if aerr != nil {
			if isOutsideRoot(aerr) {
				return "", nil, aerr
			}
			unresolvable = errors.Join(unresolvable, aerr)
			continue
		}
		if ibdev == name {
			return candidate.Name(), nil, nil
		}
	}
	return "", unresolvable, nil
}

// resolveInside follows the symlinks along one path and refuses a landing outside the root.
// Following is unavoidable — every interesting path in sysfs is a symlink — which is what
// makes validating where the chain landed required: without it, a link in a tree this
// process does not own decides which file is read.
func resolveInside(realRoot, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(os.PathSeparator)) {
		return "", &outsideRootError{rel: path, resolved: resolved, root: realRoot}
	}
	return resolved, nil
}

// readAttr reads one attribute, capped and trimmed. The cap keeps a path that is not the
// attribute it was taken for from being read wholesale into memory.
func readAttr(realRoot, path string) (string, error) {
	p, err := resolveInside(realRoot, path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	buf, err := io.ReadAll(io.LimitReader(f, sysfsAttrMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(buf) > sysfsAttrMaxBytes {
		return "", fmt.Errorf("sysfs attribute %q exceeds %d bytes", path, sysfsAttrMaxBytes)
	}
	return strings.TrimSpace(string(buf)), nil
}

// presentNoFollow reports whether a path is there without following it, which is what
// separates a layout this tree does not carry from one that is there and would not resolve.
func presentNoFollow(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// danglingClassEntry reports whether the RDMA device's class entry is a symlink that does not
// resolve. Such an entry might have led to the hardware parent, so an anchor failing through
// it may not be read as the layout's absence — unlike a real directory carrying no device
// link, which is a device with no hardware parent and nothing for this layout to find.
func danglingClassEntry(root, name string) bool {
	info, err := os.Lstat(filepath.Join(root, sysfsInfinibandDir, name))
	return err == nil && info.Mode()&os.ModeSymlink != 0
}
