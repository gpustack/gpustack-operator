// Command priors maintains the offline capacity prior catalog.
//
// IT HAS THREE MODES. IT FORMATS AND CHECKS THE CATALOG, IT MERGES A MEASURED
// SWEEP INTO THE CATALOG, AND IT RETIRES A PROFILE FROM THE CATALOG. IT
// FETCHES NOTHING, so the catalog depends only on the tree and the files it is
// given, and two runs over the same files produce the same bytes.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/gen/priors/adapter"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

// projectModule is this repository's module import path.
const projectModule = "gpustack.ai/gpustack"

// catalogPath is the catalog the generator writes, relative to the project
// directory.
const catalogPath = "pkg/worker/scaling/priors/priors.yaml"

func main() {
	mergeSource := flag.String("merge", "", "merge the measured sweeps in this file into the catalog")
	retire := flag.Bool("retire", false, "remove the profiles the filter flags name")
	catalogFile := flag.String("catalog", "", "the catalog file to read and write")
	role := flag.String("role", "", "retire the profiles of this serving role")
	model := flag.String("model", "", "retire the profiles of this model")
	quantization := flag.String("quantization", "", "retire the profiles of this quantization")
	engine := flag.String("engine", "", "retire the profiles of this engine")
	hardware := flag.String("hardware", "", "retire the profiles of this hardware")
	tensorParallel := flag.Int("tp", 0, "retire the profiles of this tensor parallel width")
	gpus := flag.Int("gpus", 0, "retire the profiles of this GPU count")
	flag.Parse()

	filter := retireFilter{
		Role:           strings.TrimSpace(*role),
		Model:          strings.TrimSpace(*model),
		Quantization:   strings.TrimSpace(*quantization),
		Engine:         strings.TrimSpace(*engine),
		Hardware:       strings.TrimSpace(*hardware),
		TensorParallel: *tensorParallel,
		GPUsPerReplica: *gpus,
	}

	if err := run(*catalogFile, *mergeSource, *retire, filter); err != nil {
		klog.Fatalf("error: %v", err)
	}
}

// run performs the mode the flags name.
//
// IT REFUSES TWO MODES AT ONCE, because a catalog that was merged and retired
// at the same time is a catalog nobody can say what it holds.
func run(catalogFile, mergeSource string, retire bool, filter retireFilter) error {
	projectDir, err := resolveProjectDir()
	if err != nil {
		return err
	}
	target := resolvePath(projectDir, catalogFile)
	if catalogFile == "" {
		target = filepath.Join(projectDir, catalogPath)
	}

	switch {
	case mergeSource != "" && retire:
		return fmt.Errorf("--merge and --retire cannot be combined")
	case mergeSource != "":
		return mergeInto(target, resolvePath(projectDir, mergeSource))
	case retire:
		return retireFrom(target, filter)
	default:
		return formatCatalog(target)
	}
}

// formatCatalog reads the catalog, checks it and writes it back in the
// canonical form.
//
// IT IS THE DEFAULT MODE, because a catalog that is written by hand and checked
// by a machine is a catalog whose two spellings cannot drift apart.
func formatCatalog(path string) error {
	document, err := readDocument(path)
	if err != nil {
		return err
	}
	if err := writeDocument(path, document); err != nil {
		return err
	}
	klog.Infof("wrote %s with %d profiles", path, len(document.Profiles))
	return nil
}

// mergeInto reads the measured sweeps of a source and merges them into the
// catalog.
//
// IT UPSERTS BY SHAPE, because a profile is one deployment shape and a
// measurement of that shape replaces the measurement the catalog held.
func mergeInto(path, source string) error {
	document, err := readDocument(path)
	if err != nil {
		return err
	}
	rows, err := adapter.LoadInferenceXRows(source)
	if err != nil {
		return err
	}
	imported, summary := adapter.ImportInferenceXRows(rows)
	added, updated := mergeProfiles(&document, imported)
	if err := writeDocument(path, document); err != nil {
		return err
	}
	klog.Infof("merged %d profiles from %s: %d added, %d updated, %d rows read",
		summary.Profiles, source, added, updated, summary.Rows)
	for _, reason := range sortedReasons(summary.Skipped) {
		klog.Infof("skipped %d rows: %s", summary.Skipped[reason], reason)
	}
	return nil
}

// sortedReasons returns the skip reasons in the order they are named.
func sortedReasons(skipped map[string]int) []string {
	reasons := make([]string, 0, len(skipped))
	for reason, count := range skipped {
		if count > 0 {
			reasons = append(reasons, reason)
		}
	}
	sort.Strings(reasons)
	return reasons
}

// mergeProfiles puts the imported profiles into the catalog.
//
// IT REPLACES A PROFILE THE CATALOG ALREADY HOLDS AND APPENDS A PROFILE IT
// DOES NOT, so a measurement of a shape replaces the measurement of that shape
// and a measurement of a new shape joins the catalog.
//
// IT REPLACES ON THE NORMALIZED SHAPE, because the catalog holds one profile per
// shape and two spellings of one shape are one profile.
func mergeProfiles(document *priors.CatalogDocument, imported []priors.ProfileDocument) (int, int) {
	indexes := make(map[string]int, len(document.Profiles))
	for index, profile := range document.Profiles {
		indexes[profile.Shape().String()] = index
	}
	added, updated := 0, 0
	for _, profile := range imported {
		key := profile.Shape().String()
		if index, seen := indexes[key]; seen {
			document.Profiles[index] = profile
			updated++
			continue
		}
		document.Profiles = append(document.Profiles, profile)
		indexes[key] = len(document.Profiles) - 1
		added++
	}
	return added, updated
}

// retireFilter names the profiles a retirement removes.
//
// IT IS A SET OF FIELDS AND NOT ONE STRING, because a profile is known by its
// shape and a shape has six elements. A field the filter leaves out constrains
// nothing.
type retireFilter struct {
	// Role is the serving role to retire.
	Role string

	// Model is the model to retire.
	Model string

	// Quantization is the quantization to retire.
	Quantization string

	// Engine is the engine to retire.
	Engine string

	// Hardware is the accelerator to retire.
	Hardware string

	// TensorParallel is the tensor parallel width to retire.
	TensorParallel int

	// GPUsPerReplica is the GPU count to retire.
	GPUsPerReplica int
}

// stated says whether the filter names anything at all.
//
// IT REFUSES AN EMPTY FILTER, because a retirement that names nothing retires
// every profile and a catalog that lost every profile silently is a catalog
// nobody can account for.
func (f retireFilter) stated() bool {
	return f.Role != "" || f.Model != "" || f.Quantization != "" || f.Engine != "" ||
		f.Hardware != "" || f.TensorParallel > 0 || f.GPUsPerReplica > 0
}

// retireFrom removes the profiles the filter names and writes the catalog back.
//
// IT NAMES WHAT IT REMOVED, because a catalog that lost a profile silently is a
// catalog nobody can account for.
func retireFrom(path string, filter retireFilter) error {
	if !filter.stated() {
		return fmt.Errorf("--retire needs at least one of --role, --model, --quantization, --engine, --hardware, --tp or --gpus")
	}
	document, err := readDocument(path)
	if err != nil {
		return err
	}
	kept := make([]priors.ProfileDocument, 0, len(document.Profiles))
	removed := make([]string, 0, len(document.Profiles))
	for _, profile := range document.Profiles {
		if filter.retires(profile) {
			removed = append(removed, profile.Shape().String())
			continue
		}
		kept = append(kept, profile)
	}
	document.Profiles = kept
	if err := writeDocument(path, document); err != nil {
		return err
	}
	klog.Infof("retired %d profiles: %s", len(removed), strings.Join(removed, ", "))
	return nil
}

// retires says whether a profile is one the filter names.
//
// IT MATCHES EVERY FIELD THE FILTER STATES, because a retirement names one
// measurement and a filter that matched on one field alone would retire a
// family of measurements.
func (f retireFilter) retires(profile priors.ProfileDocument) bool {
	shape := profile.Shape()
	if f.Role != "" && !strings.EqualFold(f.Role, string(shape.Role)) {
		return false
	}
	if f.Model != "" && !priors.ModelNameMatches(f.Model, shape.Model) {
		return false
	}
	if f.Quantization != "" && priors.CanonicalQuantization(f.Quantization) != shape.Quantization {
		return false
	}
	if f.Engine != "" && priors.CanonicalEngineFamily(f.Engine) != shape.Engine {
		return false
	}
	if f.Hardware != "" && priors.CanonicalAcceleratorFamily(f.Hardware) != shape.Hardware {
		return false
	}
	if f.TensorParallel > 0 && f.TensorParallel != shape.Topology.TensorParallel {
		return false
	}
	if f.GPUsPerReplica > 0 && f.GPUsPerReplica != shape.Topology.GPUsPerReplica {
		return false
	}
	return true
}

// readDocument reads the catalog document.
//
// IT READS AN EMPTY CATALOG WHEN THE FILE IS MISSING, because a catalog that
// does not exist yet is a catalog with no profiles and not an error.
func readDocument(path string) (priors.CatalogDocument, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return priors.CatalogDocument{SchemaVersion: priors.SchemaVersion}, nil
	}
	if err != nil {
		return priors.CatalogDocument{}, fmt.Errorf("read catalog %s: %w", path, err)
	}
	document, err := priors.ParseCatalogDocument(raw)
	if err != nil {
		return priors.CatalogDocument{}, fmt.Errorf("parse catalog %s: %w", path, err)
	}
	return document, nil
}

// writeDocument writes the catalog document.
//
// IT CHECKS THE CATALOG BEFORE IT WRITES IT, because a catalog that does not
// parse is a catalog the operator cannot read, and a file that was written and
// then found broken has to be repaired by hand.
func writeDocument(path string, document priors.CatalogDocument) error {
	encoded, err := priors.MarshalCatalogDocument(document)
	if err != nil {
		return err
	}
	if _, err := priors.ParseCatalog(encoded); err != nil {
		return fmt.Errorf("validate catalog: %w", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write catalog %s: %w", path, err)
	}
	return nil
}

// resolvePath returns the path a flag names.
//
// IT KEEPS AN ABSOLUTE PATH AS IT IS, because a file outside the tree is a
// file the operator pointed at.
func resolvePath(projectDir, path string) string {
	if path == "" {
		return path
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(projectDir, path)
}

// resolveProjectDir returns the directory the generator writes against.
//
// IT PREFERS THE DIRECTORY THE SOURCE WAS COMPILED FROM, so the generator
// writes into the tree it belongs to even when it is run from elsewhere. IT
// FALLS BACK TO WALKING UP FROM THE WORKING DIRECTORY, because a trimmed
// build path carries no directory to walk up from.
func resolveProjectDir() (string, error) {
	if _, file, _, ok := runtime.Caller(0); ok {
		candidate := filepath.Dir(filepath.Dir(filepath.Dir(file)))
		if hasProjectModule(candidate) {
			return candidate, nil
		}
	}

	working, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	candidate := working
	for {
		if hasProjectModule(candidate) {
			return candidate, nil
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
		candidate = parent
	}

	return "", fmt.Errorf("no directory holding %s was found from the working directory %q", projectModule, working)
}

// hasProjectModule says whether a directory holds this repository.
func hasProjectModule(dir string) bool {
	content, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == "module "+projectModule {
			return true
		}
	}
	return false
}
