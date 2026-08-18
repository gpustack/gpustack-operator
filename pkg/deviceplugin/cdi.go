package deviceplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// The Container Device Interface is how a manufacturer describes, on the node, everything a container
// needs to reach one of its accelerators: the device nodes, the driver libraries, and the hooks that
// wire them up. A container engine that supports it resolves a request naming a device against those
// descriptions and performs the injection itself, with no vendor container runtime in the Pod's path.
//
// Two rules govern how this operator uses it, and they are the reason this lives in the shared package
// rather than in one manufacturer's allocator:
//
//   - The manufacturer's own specifications are the only ones we use. Generating specifications of our
//     own would put two writers on one node's description of the same hardware, and the loser of that
//     race is whichever one the engine loaded second. So this reads, and never writes.
//   - A specification is used only when it already names the exact accelerator that was granted. That
//     one positive fact answers "does this node use CDI for this hardware" without inferring it from
//     anything softer, and it is what keeps a node with no specifications on the channel it works on.
//
// A manufacturer that ships no generator therefore takes no CDI path at all, which is the correct
// answer rather than a gap: injecting the device nodes directly needs only the driver, so it is the
// better channel wherever the container's user-space libraries come from its own image.

// CDISpecDirs are the directories a container engine loads specifications from, in the engine's
// own precedence order: /etc/cdi holds what an administrator or a package placed there by hand, and
// /var/run/cdi what a generator refreshes as the node's hardware changes, so a specification in the
// second overrides one of the same file name in the first.
//
// They are vars rather than consts so a test can point them at a temporary tree.
var CDISpecDirs = []string{"/etc/cdi", "/var/run/cdi"}

// CDIDeviceName renders the fully-qualified name a CDI device is requested by: the manufacturer's kind
// and the device's own identifier, which for every manufacturer here is the accelerator id the
// detector recorded.
func CDIDeviceName(kind, id string) string {
	return kind + "=" + id
}

// CDIDeviceNames renders one fully-qualified name per identifier, in the order they were given.
func CDIDeviceNames(kind string, ids []string) []string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, CDIDeviceName(kind, id))
	}

	return names
}

// CDISpecs is what the node's loaded specifications name, as this operator can see them.
type CDISpecs struct {
	// Names holds every fully-qualified device name the loaded specifications carry.
	Names map[string]struct{}
	// Unreadable reports whether any specification could not be read or parsed. A specification that
	// failed to parse is not one that names nothing: treating the two alike would let a single
	// malformed file turn a perfectly good accelerator into "no specification names it", which reads
	// as a definite answer when it is not one.
	Unreadable bool
	// Dirs are the specification directories that existed and were read, for the evidence a decision
	// is logged with.
	Dirs []string
}

// Missing returns the given fully-qualified names the loaded specifications do not carry, sorted so
// the message a user reads does not change between identical runs.
func (s CDISpecs) Missing(names []string) []string {
	var missing []string
	for _, name := range names {
		if _, ok := s.Names[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	return missing
}

// cdiSpecDocument is the part of a specification this needs: the version and kind it declares, and
// the devices it names under that kind.
type cdiSpecDocument struct {
	Version string `yaml:"cdiVersion"`
	Kind    string `yaml:"kind"`
	Devices []struct {
		Name string `yaml:"name"`
	} `yaml:"devices"`
}

// LoadCDISpecs reads the node's specification directories.
//
// It reads specification FILES, never the directories alone. A device manager mounts these
// directories with DirectoryOrCreate, so the mount itself creates an empty directory on a host that
// has no CDI at all — taking a directory's existence as evidence would conclude CDI was available on
// every node this ever ran on.
//
// Files are keyed by name across directories, so a later directory's specification REPLACES an
// earlier one of the same name rather than adding to it. That is the engine's own rule, and a union
// would claim a device the engine will not resolve: exactly the case where a generator rewrote a
// stale hand-placed file with a new set of accelerators.
func LoadCDISpecs() CDISpecs {
	specs := CDISpecs{Names: make(map[string]struct{})}

	byFile := make(map[string][]string)
	for _, dir := range CDISpecDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// A directory that is not there names nothing, which is a fact. One that is there and
			// cannot be listed names something unknown, which is not — so only the second is an
			// unreadable view. What it cannot do is unfind a name an earlier directory carried:
			// which of its files shadow which is exactly what could not be read.
			if !os.IsNotExist(err) {
				specs.Unreadable = true
			}

			continue
		}
		specs.Dirs = append(specs.Dirs, dir)

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			switch filepath.Ext(entry.Name()) {
			case ".yaml", ".yml", ".json":
			default:
				continue
			}
			names, err := readCDISpec(filepath.Join(dir, entry.Name()))
			if err != nil {
				specs.Unreadable = true
				// This file shadows a same-named one an earlier directory carried, whether or not it
				// could be read. Leaving that earlier entry in place would answer from a
				// specification the engine has replaced, which is the stale-file case the keying
				// exists to avoid — only now with the replacement's contents unknown.
				delete(byFile, entry.Name())

				continue
			}
			byFile[entry.Name()] = names
		}
	}

	for _, names := range byFile {
		for _, name := range names {
			specs.Names[name] = struct{}{}
		}
	}

	return specs
}

// readCDISpec returns the fully-qualified device names one specification file carries.
func readCDISpec(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// YAML is a superset of JSON, so one decoder reads both spellings.
	var doc cdiSpecDocument
	if err = yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	// Both fields, because a document declaring neither is one the container engine does not load —
	// and a name read out of a document the engine never loaded is exactly the false positive that
	// would switch a working node onto a channel with nothing behind it.
	if doc.Version == "" || doc.Kind == "" {
		return nil, fmt.Errorf("%q declares no cdiVersion or no kind, so it is not a specification "+
			"the container engine loads", path)
	}

	names := make([]string, 0, len(doc.Devices))
	for i := range doc.Devices {
		if doc.Devices[i].Name == "" {
			continue
		}
		names = append(names, CDIDeviceName(doc.Kind, doc.Devices[i].Name))
	}

	return names, nil
}

// SetCDIRequest asks the container engine to inject the named CDI devices into this container.
//
// The request rides an annotation rather than the response's typed CDI field: the field needs
// Kubernetes >= 1.31, and an older kubelet drops an unknown field silently, which is the one failure
// this whole channel exists to avoid. The annotation's suffix only has to be unique among the plugins
// writing to one container, so it names the requester.
func SetCDIRequest(resp *ContainerAllocateResponse, requester string, names []string) {
	if resp.Annotations == nil {
		resp.Annotations = make(map[string]string, 1)
	}
	resp.Annotations["cdi.k8s.io/"+requester] = strings.Join(names, ",")
}
