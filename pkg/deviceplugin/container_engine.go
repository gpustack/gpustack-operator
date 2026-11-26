package deviceplugin

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"dario.cat/mergo"
	"github.com/BurntSushi/toml"

	"gpustack.ai/gpustack/pkg/utils/osx"
)

// ContainerdConfigDirEnv names the directory holding the container engine's configuration, which a
// device manager reads to learn whether the engine resolves CDI requests and which runtime handler a
// Pod naming no RuntimeClass runs under.
//
// It is a setting rather than a search: distributions put the file wherever they like — k3s and RKE2
// keep it under /var/lib/rancher, and an administrator may put it anywhere at all — and a device
// manager that went looking would have to be given the paths it might find it at, which is the same
// setting spelled less honestly. A wrong or unmounted directory reads as an unknown engine, and an
// unknown engine keeps a node on the channel it already works on.
const ContainerdConfigDirEnv = "GPUSTACK_CONTAINERD_CONFIG_DIR"

// defaultContainerdConfigDir is where containerd itself installs its configuration, and containerdConfigFile
// the name it gives that file. They are the fallback for a device manager started without the setting
// above; the chart always passes it.
const (
	defaultContainerdConfigDir = "/etc/containerd"
	containerdConfigFile       = "config.toml"
)

// ContainerEngineFacts is what the container engine's configuration says about how a container reaches
// its accelerators, with a flag saying whether it could be read at all. An unreadable fact is not a
// false one.
type ContainerEngineFacts struct {
	// Known reports whether the configuration was read and parsed, and Path the file it came from.
	Known bool
	Path  string

	// ResolvesCDI reports whether the engine resolves CDI requests.
	ResolvesCDI bool

	// DefaultHandler is the runtime handler a Pod naming no runtimeClassName runs under, and
	// DefaultHandlerIsVendor whether that handler is a vendor runtime rather than a generic OCI one —
	// which is what decides whether a visible-devices variable reaches anything on its own.
	DefaultHandler         string
	DefaultHandlerIsVendor bool
}

// ReadContainerEngineFacts reads the container engine's configuration together with the files it
// imports. Every read is best-effort: a configuration that cannot be read, or that imports a file
// that cannot be read, is reported as unknown.
func ReadContainerEngineFacts() ContainerEngineFacts {
	path := filepath.Join(osx.Getenv(ContainerdConfigDirEnv, defaultContainerdConfigDir), containerdConfigFile)

	cfg, err := loadContainerdConfig(path)
	if err != nil {
		// The path is carried even though nothing was read from it: it is the one thing an operator
		// needs to see when a node did not take the channel it was expected to, and the evidence a
		// resolution is logged with has nothing else to name.
		return ContainerEngineFacts{Path: path}
	}

	f := ContainerEngineFacts{Known: true, Path: path}
	f.DefaultHandler, _ = findNestedString(cfg, "default_runtime_name")
	f.DefaultHandlerIsVendor = isVendorRuntimeHandler(f.DefaultHandler)

	if enabled, ok := findNestedBool(cfg, "enable_cdi"); ok {
		// An explicit switch wins wherever the configuration version puts it.
		f.ResolvesCDI = enabled
	} else if version, ok := cfg["version"].(int64); ok && version >= 3 {
		// containerd 2.x reads configuration version 3 and removed the switch: CDI is always on.
		// Version 2 and below default it off.
		f.ResolvesCDI = true
	}

	return f
}

// loadContainerdConfig reads one configuration file and every file it imports, directly or through
// further imports, and returns them merged into one document. It mirrors containerd's LoadConfig: the
// files are taken breadth-first from the given one, so it has the lowest priority and whatever is
// imported later overrides it; each file is read at most once, by its exact path, which is what makes
// two files importing each other terminate; and a file that cannot be read fails the whole load, as it
// fails containerd's own start. Each file is decoded into a plain document, as loadConfigFile does, so
// the merge sees keys rather than a fixed schema.
func loadContainerdConfig(path string) (map[string]any, error) {
	var cfg map[string]any
	loaded := map[string]bool{}
	pending := []string{path}

	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]

		if loaded[current] {
			continue
		}
		loaded[current] = true

		var doc map[string]any
		if _, err := toml.DecodeFile(current, &doc); err != nil {
			return nil, err
		}
		if err := mergeContainerdConfig(&cfg, doc); err != nil {
			return nil, err
		}

		imports, err := resolveContainerdImports(current, doc)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", current, err)
		}
		pending = append(pending, imports...)
	}

	return cfg, nil
}

// resolveContainerdImports lists the files a document imports, as paths, the way resolveImports does.
// Only the top-level key counts, as in containerd's schema, so an imports key inside a table is an
// ordinary setting; a value that is not a list of strings fails the load, as it fails containerd's own
// decoding. Each entry is cleaned, a relative one is taken relative to the importing file's directory,
// one naming a glob is expanded in the glob's order (which may be empty), and any other is taken
// literally, so a missing one is not skipped here but fails when it is read.
func resolveContainerdImports(parent string, doc map[string]any) ([]string, error) {
	var items []any
	switch raw := doc["imports"].(type) {
	case nil:
		return nil, nil
	case []any:
		items = raw
	default:
		return nil, fmt.Errorf("imports must be a list of strings, not %T", raw)
	}

	var out []string
	for _, item := range items {
		entry, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("imports must be a list of strings, found %T", item)
		}

		path := filepath.Clean(entry)
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(parent), path)
		}

		if strings.Contains(path, "*") {
			matches, err := filepath.Glob(path)
			if err != nil {
				return nil, err
			}
			out = append(out, matches...)
			continue
		}

		out = append(out, path)
	}

	return out, nil
}

// mergeContainerdConfig folds one file's document into the accumulated one the way mergeConfig does:
// mergo with override, so a later file's keys win and the keys it omits survive. Then the plugin
// sections. Before configuration version 3 containerd held each as a whole tree and replaced it when a
// later file named it, dropping the keys that file did not repeat; from version 3 on they are plain
// maps that mergo merges key by key, which is the merge already done. The version that decides this is
// the accumulated one, since a later file's version overrides too.
func mergeContainerdConfig(acc *map[string]any, doc map[string]any) error {
	if err := mergo.Merge(acc, doc, mergo.WithOverride); err != nil {
		return err
	}

	if version, ok := (*acc)["version"].(int64); ok && version >= 3 {
		return nil
	}

	sections, _ := doc["plugins"].(map[string]any)
	if merged, ok := (*acc)["plugins"].(map[string]any); ok {
		for id, section := range sections {
			merged[id] = section
		}
	}

	return nil
}

// isVendorRuntimeHandler reports whether a runtime handler name is something other than the generic
// OCI runtimes. Anything else is assumed to be a vendor runtime that injects accelerators of its own,
// because being wrong in that direction keeps today's behavior rather than adding a second injection
// path for one container.
func isVendorRuntimeHandler(name string) bool {
	switch name {
	case "", "runc", "crun":
		return false
	default:
		return true
	}
}

// findNestedString and findNestedBool read one key out of a decoded TOML document wherever it sits.
// The container engine has moved its plugin section between configuration versions, so addressing a
// key by its full path would bind this to one of them.
//
// Subtrees are walked in sorted order rather than map order. A configuration carrying the key in two
// places is pathological, but Go randomizes map iteration, so walking it unordered would answer
// differently between two restarts reading the same file — and a fact that changes without the node
// changing is the one thing a resolver must not have.
func findNestedString(node map[string]any, key string) (string, bool) {
	if v, ok := node[key].(string); ok {
		return v, true
	}
	for _, name := range sortedKeys(node) {
		if child, ok := node[name].(map[string]any); ok {
			if found, ok := findNestedString(child, key); ok {
				return found, true
			}
		}
	}

	return "", false
}

func findNestedBool(node map[string]any, key string) (bool, bool) {
	if v, ok := node[key].(bool); ok {
		return v, true
	}
	for _, name := range sortedKeys(node) {
		if child, ok := node[name].(map[string]any); ok {
			if found, ok := findNestedBool(child, key); ok {
				return found, true
			}
		}
	}

	return false, false
}

func sortedKeys(node map[string]any) []string {
	names := make([]string, 0, len(node))
	for name := range node {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}
