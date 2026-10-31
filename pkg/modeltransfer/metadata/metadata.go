// Package metadata builds and verifies the metadata-only view of a
// published checkpoint tree: the small configuration files verbatim and each
// safetensors file as its 8-byte header length plus JSON header, without any
// weight payload bytes.
//
// A view is a directory with a record (view.json) that names the artifact
// digest it was derived from, lists every file it holds with its digest, and
// marks itself incomplete: a view is never a complete checkpoint tree, never
// lands in the store's published layout, and never makes a digest read as
// disk-ready. Verification re-hashes every file against the record, so a
// tampered, stale, or foreign file is refused by name.
package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// The view record's file name and kind.
const (
	viewFile = "view.json"
	ViewKind = "gpustack-model-transfer-metadata-view/v1"
)

// Bounds shared with the reader side: a metadata file is kilobytes and a
// safetensors header is kilobytes; anything larger is corruption, not a big
// model. MaxHeaderBytes matches the Python probe's bound.
const (
	MaxHeaderBytes     int64 = 64 << 20
	MaxMetadataBytes   int64 = 64 << 20
	maxHeaderStubFiles       = 1024
)

// The refusals, each naming its own rule.
var (
	// ErrUnknownRequiredFile is a request for a file the view never carries.
	ErrUnknownRequiredFile = errors.New("unknown required metadata file")
	// ErrMissingRequiredFile is a required file the source tree lacks; the
	// message names the file so the refusal is actionable.
	ErrMissingRequiredFile = errors.New("missing required metadata file")
	// ErrPathEscape is a name that would leave the tree it is resolved in.
	ErrPathEscape = errors.New("path escapes the tree")
	// ErrNotRegularFile is a symlink, directory, or other non-regular file
	// where verbatim bytes are required.
	ErrNotRegularFile = errors.New("not a regular file")
	// ErrStaleDigest is a source tree verified against another digest.
	ErrStaleDigest = errors.New("stale or mismatched digest")
	// ErrTampered is a view whose bytes no longer match its record.
	ErrTampered = errors.New("the view does not match its record")
	// ErrHeaderTooLarge is an implausible safetensors header length.
	ErrHeaderTooLarge = errors.New("implausible safetensors header length")
	// ErrInvalidHeader is a safetensors header that is not the JSON the
	// reader contract expects.
	ErrInvalidHeader = errors.New("invalid safetensors header")
	// ErrNotAMetadataView is a directory that is not a metadata view at all,
	// or one whose record claims to be a complete checkpoint.
	ErrNotAMetadataView = errors.New("the directory is not a metadata view")
)

// DefaultRequiredFiles is what a metadata-only delivery must answer with
// before any engine initialization: the tree's manifest record and the model
// configuration.
var DefaultRequiredFiles = []string{"manifest.json", "config.json"}

// knownFiles is every file a view may carry. A request outside this set is
// refused rather than copied: an unknown name is a configuration mistake or
// an escape attempt, never a metadata need.
var knownFiles = map[string]bool{
	"manifest.json": true, "config.json": true,
	"quantize_config.json": true, "quantization_config.json": true,
	"quant_model_description.json": true,
	"tokenizer.json":               true, "tokenizer_config.json": true,
	"generation_config.json": true, "special_tokens_map.json": true,
	"model.safetensors.index.json": true, "tokenizer.model": true,
	"vocab.json": true, "merges.txt": true, "added_tokens.json": true,
	"chat_template.jinja": true, "preprocessor_config.json": true,
	"processor_config.json": true,
}

// ValidateRequired checks a required-file list ahead of any IO: every name
// must be a known metadata file and a safe relative path, so an unknown or
// escaping name is refused as bad input before a tree is touched.
func ValidateRequired(required []string) error {
	for _, name := range required {
		if !safeRelative(name) {
			return fmt.Errorf("%w: %q", ErrPathEscape, name)
		}
		if !knownFiles[name] {
			return fmt.Errorf("%w: %q is not a metadata file a view carries",
				ErrUnknownRequiredFile, name)
		}
	}
	return nil
}

// ViewFile is one file the view holds.
type ViewFile struct {
	// Name is the file's path relative to the view.
	Name string `json:"name"`
	// Size is the byte size as the view holds it: the whole file, or the
	// header stub for a truncated checkpoint file.
	Size int64 `json:"size"`
	// SHA256 digests the bytes the view holds.
	SHA256 string `json:"sha256"`
	// Truncated marks a safetensors file held as its header only, without
	// the weight payload.
	Truncated bool `json:"truncated,omitempty"`
}

// View is the view record: what the view holds and what it was derived from.
type View struct {
	Kind           string `json:"kind"`
	ArtifactDigest string `json:"artifactDigest"`
	// Incomplete is always true: a metadata view is never a complete
	// checkpoint tree, and a record claiming otherwise is refused.
	Incomplete bool       `json:"incomplete"`
	Files      []ViewFile `json:"files"`

	// raw is the record's own bytes, for the content-addressed ID.
	raw []byte
}

// ID is the view's content address: the sha256 of the record's exact bytes.
func (v *View) ID() string {
	sum := sha256.Sum256(v.raw)
	return hex.EncodeToString(sum[:])
}

// viewSatisfies says whether a verified view already answers a request:
// derived from the same artifact digest and holding every required file.
func viewSatisfies(view *View, artifactDigest string, required []string) bool {
	if view == nil || view.ArtifactDigest != artifactDigest {
		return false
	}
	held := map[string]bool{}
	for _, f := range view.Files {
		held[f.Name] = true
	}
	for _, name := range required {
		if !held[name] {
			return false
		}
	}
	return true
}

// newBuildStage stages a build beside its destination under the canonical
// staging prefix, so crash residue is recognized by recovery no matter
// which caller created it, creating the destination's parent when needed.
func newBuildStage(viewDir string) (*Staging, error) {
	if err := os.MkdirAll(filepath.Dir(viewDir), 0o755); err != nil {
		return nil, err
	}
	return newStaging(filepath.Dir(viewDir), stagingPrefix)
}

// Build materializes the metadata view of srcDir into viewDir.
//
// Every required file must be a known metadata file present in srcDir as a
// regular file; each safetensors file in srcDir is carried as its header
// stub, byte-identical to the source prefix and bounded by MaxHeaderBytes;
// nothing else is copied. manifestSource, when given, supplies
// manifest.json when srcDir itself lacks it: the tree stores the manifest
// beside the marker, not inside the sealed files.
//
// Building into a directory that already holds a verified view of the same
// inputs is a no-op. Concurrent builds each stage under their own unique
// temporary directory, and the loser of the final rename finds the winner
// in place, verifies it, and succeeds too. The record is written last, so
// a directory without one is an incomplete build, never a view.
func Build(
	srcDir, viewDir, artifactDigest string, required []string, manifestSource ...string,
) (*View, error) {
	if required == nil {
		required = DefaultRequiredFiles
	}
	if existing, err := Verify(viewDir); err == nil &&
		viewSatisfies(existing, artifactDigest, required) {
		return existing, nil
	}
	for _, name := range required {
		if !safeRelative(name) {
			return nil, fmt.Errorf("%w: %q", ErrPathEscape, name)
		}
		if !knownFiles[name] {
			return nil, fmt.Errorf("%w: %q is not a metadata file a view carries",
				ErrUnknownRequiredFile, name)
		}
	}
	var manifest string
	if len(manifestSource) > 0 {
		manifest = manifestSource[0]
	}

	stage, err := newBuildStage(viewDir)
	if err != nil {
		return nil, err
	}
	defer stage.Release()
	staging := stage.Dir()

	var files []ViewFile
	// Whole files first: the required set plus known optional files present.
	// A name asked for twice is copied once: a repeated ask is redundant,
	// not a second copy.
	copied := map[string]bool{}
	for _, name := range required {
		if copied[name] {
			continue
		}
		entry, err := copyWhole(srcDir, staging, name, manifest, true)
		if err != nil {
			return nil, err
		}
		files = append(files, entry)
		copied[name] = true
	}
	optional, err := sortDirEntries(srcDir)
	if err != nil {
		return nil, err
	}
	for _, name := range optional {
		if copied[name] || !knownFiles[name] {
			continue
		}
		entry, err := copyWhole(srcDir, staging, name, manifest, false)
		if errors.Is(err, fs.ErrNotExist) {
			// copyWhole's permitted sentinel: an optional file vanished
			// between the listing and the open, so the view simply carries
			// one optional file fewer. Required files never take this
			// path, and identity, symlink, and other I/O failures still
			// refuse the build.
			continue
		}
		if err != nil {
			return nil, err
		}
		files = append(files, entry)
		copied[name] = true
	}
	// Then one header stub per safetensors file in the tree.
	stubs := 0
	err = filepath.WalkDir(srcDir, func(path string, e fs.DirEntry, err error) error {
		if err != nil || path == srcDir || !strings.HasSuffix(e.Name(), ".safetensors") {
			return err
		}
		if !e.Type().IsRegular() {
			return fmt.Errorf("%w: %s", ErrNotRegularFile, e.Name())
		}
		if stubs++; stubs > maxHeaderStubFiles {
			return fmt.Errorf("the tree holds more safetensors files than a view carries: %d", maxHeaderStubFiles)
		}
		entry, err := copyHeaderStub(srcDir, staging, path)
		if err != nil {
			return err
		}
		files = append(files, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	view := &View{
		Kind: ViewKind, ArtifactDigest: artifactDigest,
		Incomplete: true, Files: files,
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	view.raw = raw
	if err := writeSealed(filepath.Join(staging, viewFile), raw); err != nil {
		return nil, err
	}
	if err := publishView(staging, viewDir, artifactDigest, required); err != nil {
		return nil, err
	}
	return Verify(viewDir)
}

// Load reads a view's record without verifying its files.
func Load(viewDir string) (*View, error) {
	raw, err := os.ReadFile(filepath.Join(viewDir, viewFile))
	if err != nil {
		return nil, fmt.Errorf("%w: no %s", ErrNotAMetadataView, viewFile)
	}
	var view View
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("%w: the record is not valid JSON", ErrNotAMetadataView)
	}
	view.raw = raw
	return &view, nil
}

// Verify reads a view's record and checks every byte it claims against it:
// every listed file present, regular, and digesting to its recorded sha256;
// no unlisted file beside the record; the record of this kind and marked
// incomplete. The artifact digest of record is returned for the caller's
// own staleness check.
func Verify(viewDir string) (*View, error) {
	view, err := Load(viewDir)
	if err != nil {
		return nil, err
	}
	if view.Kind != ViewKind {
		return nil, fmt.Errorf("%w: the record is of kind %q", ErrNotAMetadataView, view.Kind)
	}
	if !view.Incomplete {
		return nil, fmt.Errorf("%w: the record claims to be complete", ErrNotAMetadataView)
	}
	if !strings.HasPrefix(view.ArtifactDigest, "sha256:") || len(view.ArtifactDigest) != 71 {
		return nil, fmt.Errorf("%w: the record's artifact digest is malformed", ErrNotAMetadataView)
	}
	listed := map[string]ViewFile{}
	for _, f := range view.Files {
		if !safeRelative(f.Name) {
			return nil, fmt.Errorf("%w: %q", ErrPathEscape, f.Name)
		}
		if f.Name == viewFile {
			return nil, fmt.Errorf("%w: %q is the record, not a file it may list",
				ErrTampered, f.Name)
		}
		if _, duplicate := listed[f.Name]; duplicate {
			return nil, fmt.Errorf("%w: %q is listed twice", ErrTampered, f.Name)
		}
		listed[f.Name] = f
	}
	// A directory is recorded only by the files under it: anything else in
	// the view is unrecorded content.
	recordedDirs := map[string]bool{}
	for name := range listed {
		for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
			recordedDirs[dir] = true
		}
	}
	err = filepath.WalkDir(viewDir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == viewDir {
			return nil
		}
		rel, err := filepath.Rel(viewDir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if e.IsDir() {
			if !recordedDirs[name] {
				return fmt.Errorf("%w: the directory %q holds no recorded file",
					ErrTampered, name)
			}
			return nil
		}
		if name == viewFile {
			return nil
		}
		if !e.Type().IsRegular() {
			return fmt.Errorf("%w: %s", ErrNotRegularFile, name)
		}
		f, ok := listed[name]
		if !ok {
			return fmt.Errorf("%w: %s is not in the record", ErrTampered, name)
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Size() != f.Size {
			return fmt.Errorf("%w: %s holds %d bytes, the record says %d",
				ErrTampered, name, info.Size(), f.Size)
		}
		sum, err := fileDigest(path)
		if err != nil {
			return err
		}
		if sum != f.SHA256 {
			return fmt.Errorf("%w: %s digests to %s, the record says %s",
				ErrTampered, name, sum, f.SHA256)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for name := range listed {
		if _, err := os.Lstat(filepath.Join(viewDir, filepath.FromSlash(name))); err != nil {
			return nil, fmt.Errorf("%w: %s is in the record but not in the view",
				ErrTampered, name)
		}
	}
	return view, nil
}

// HeaderEntry is one tensor as the safetensors header describes it.
type HeaderEntry struct {
	Dtype string
	Shape []int64
	// Begin and End are the tensor's byte offsets within the file's data
	// section.
	Begin, End int64
}

// Header is a parsed safetensors header.
type Header struct {
	// Length is the header's byte length as the 8-byte prefix carries it.
	Length int64
	// DataOffset is where the file's data section begins: 8 + Length.
	DataOffset int64
	// Entries maps tensor names to their header entries. The __metadata__
	// key is not an entry and is not included.
	Entries map[string]HeaderEntry
}

// headerLength reads and bounds the 8-byte little-endian header length a
// safetensors file carries, before its JSON has been read.
func headerLength(prefix []byte) (int64, error) {
	if int64(len(prefix)) < 8 {
		return 0, fmt.Errorf("%w: shorter than its length prefix", ErrInvalidHeader)
	}
	length := int64(le64(prefix[:8]))
	if length < 2 {
		return 0, fmt.Errorf("%w: header length %d", ErrInvalidHeader, length)
	}
	if length > MaxHeaderBytes {
		return 0, fmt.Errorf("%w: %d bytes", ErrHeaderTooLarge, length)
	}
	return length, nil
}

// ParseHeader validates and parses a safetensors file prefix: its 8-byte
// little-endian header length and the JSON header that follows. The rules
// match the reader the adapter probes with: the length is plausible, the
// JSON is an object without duplicate keys, and every entry carries a dtype,
// a non-negative shape, and an ascending offset pair.
func ParseHeader(prefix []byte) (*Header, error) {
	length, err := headerLength(prefix)
	if err != nil {
		return nil, err
	}
	if int64(len(prefix)) < 8+length {
		return nil, fmt.Errorf("%w: the header runs past the bytes held", ErrInvalidHeader)
	}
	raw := prefix[8 : 8+length]
	entries, err := parseHeaderEntries(raw)
	if err != nil {
		return nil, err
	}
	return &Header{Length: length, DataOffset: 8 + length, Entries: entries}, nil
}

// parseHeaderEntries decodes the header JSON with duplicate-key refusal and
// entry validation, the way the adapter's probe does.
func parseHeaderEntries(raw []byte) (map[string]HeaderEntry, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	tokens, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: the header is not valid JSON: %w", ErrInvalidHeader, err)
	}
	if delimiter, ok := tokens.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("%w: the header is not a JSON object", ErrInvalidHeader)
	}
	entries := map[string]HeaderEntry{}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: the header is not valid JSON: %w", ErrInvalidHeader, err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("%w: a header key is not a string", ErrInvalidHeader)
		}
		var entry json.RawMessage
		if err := decoder.Decode(&entry); err != nil {
			return nil, fmt.Errorf("%w: the header is not valid JSON: %w", ErrInvalidHeader, err)
		}
		if seen[key] {
			return nil, fmt.Errorf("%w: duplicate header key %q", ErrInvalidHeader, key)
		}
		seen[key] = true
		if key == "__metadata__" {
			continue
		}
		parsed, err := parseHeaderEntry(key, entry)
		if err != nil {
			return nil, err
		}
		entries[key] = parsed
	}
	return entries, nil
}

func parseHeaderEntry(name string, raw json.RawMessage) (HeaderEntry, error) {
	var entry struct {
		Dtype       *string  `json:"dtype"`
		Shape       *[]int64 `json:"shape"`
		DataOffsets *[]int64 `json:"data_offsets"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entry); err != nil {
		return HeaderEntry{}, fmt.Errorf("%w: entry %q is malformed: %w",
			ErrInvalidHeader, name, err)
	}
	switch {
	case entry.Dtype == nil || *entry.Dtype == "":
		return HeaderEntry{}, fmt.Errorf("%w: entry %q lacks a dtype", ErrInvalidHeader, name)
	case entry.Shape == nil:
		return HeaderEntry{}, fmt.Errorf("%w: entry %q lacks a shape", ErrInvalidHeader, name)
	case entry.DataOffsets == nil || len(*entry.DataOffsets) != 2:
		return HeaderEntry{}, fmt.Errorf("%w: entry %q lacks a data_offsets pair", ErrInvalidHeader, name)
	}
	for _, dim := range *entry.Shape {
		if dim < 0 {
			return HeaderEntry{}, fmt.Errorf("%w: entry %q has a negative shape", ErrInvalidHeader, name)
		}
	}
	begin, end := (*entry.DataOffsets)[0], (*entry.DataOffsets)[1]
	if begin < 0 || end < begin {
		return HeaderEntry{}, fmt.Errorf("%w: entry %q has invalid data_offsets", ErrInvalidHeader, name)
	}
	return HeaderEntry{Dtype: *entry.Dtype, Shape: *entry.Shape, Begin: begin, End: end}, nil
}

// openMetadataSource opens one source file verbatim for a view; indirected
// the way this repository indirects syscalls, so tests can hold the
// list-then-open race still.
var openMetadataSource = func(path string) (int, error) {
	return unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
}

// copyWhole copies one known metadata file verbatim, refusing what is not a
// regular file and what exceeds the metadata bound. manifestSource, when
// given, supplies manifest.json when srcDir lacks it: a published tree
// stores its manifest beside the marker rather than inside the sealed
// files, and a view must carry it either way.
func copyWhole(srcDir, staging, name, manifestSource string, required bool) (ViewFile, error) {
	source := filepath.Join(srcDir, filepath.FromSlash(name))
	if _, err := os.Lstat(source); errors.Is(err, fs.ErrNotExist) &&
		name == "manifest.json" && manifestSource != "" {
		if fallback, ferr := os.Lstat(manifestSource); ferr == nil &&
			fallback.Mode().IsRegular() {
			source = manifestSource
		}
	}
	fd, err := openMetadataSource(source)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !required:
		return ViewFile{}, fs.ErrNotExist
	case errors.Is(err, fs.ErrNotExist):
		return ViewFile{}, fmt.Errorf("%w: %s", ErrMissingRequiredFile, name)
	case errors.Is(err, unix.ELOOP):
		return ViewFile{}, fmt.Errorf("%w: %s", ErrNotRegularFile, name)
	case err != nil:
		return ViewFile{}, err
	}
	file := os.NewFile(uintptr(fd), source)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ViewFile{}, err
	}
	if !info.Mode().IsRegular() {
		return ViewFile{}, fmt.Errorf("%w: %s", ErrNotRegularFile, name)
	}
	if info.Size() > MaxMetadataBytes {
		return ViewFile{}, fmt.Errorf("%w: %s", ErrTooLarge, name)
	}
	b, err := io.ReadAll(io.LimitReader(file, MaxMetadataBytes+1))
	if err != nil {
		return ViewFile{}, err
	}
	if int64(len(b)) > MaxMetadataBytes {
		return ViewFile{}, fmt.Errorf("%w: %s", ErrTooLarge, name)
	}

	target := filepath.Join(staging, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ViewFile{}, err
	}
	if err := writeSealed(target, b); err != nil {
		return ViewFile{}, err
	}
	sum := sha256.Sum256(b)
	return ViewFile{
		Name: name, Size: int64(len(b)),
		SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// copyHeaderStub carries one safetensors file as its header stub: the 8-byte
// length prefix plus the JSON header, byte-identical to the source prefix
// and never the payload.
func copyHeaderStub(srcDir, staging, path string) (ViewFile, error) {
	rel, err := filepath.Rel(srcDir, path)
	if err != nil {
		return ViewFile{}, err
	}
	name := filepath.ToSlash(rel)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return ViewFile{}, fmt.Errorf("%w: %s", ErrNotRegularFile, name)
	}
	if err != nil {
		return ViewFile{}, err
	}
	source := os.NewFile(uintptr(fd), path)
	defer func() { _ = source.Close() }()
	info, err := source.Stat()
	if err != nil {
		return ViewFile{}, err
	}
	if !info.Mode().IsRegular() {
		return ViewFile{}, fmt.Errorf("%w: %s", ErrNotRegularFile, name)
	}
	prefix := make([]byte, 8)
	if err := readFull(source, prefix); err != nil {
		return ViewFile{}, err
	}
	length, err := headerLength(prefix)
	if err != nil {
		return ViewFile{}, err
	}
	if info, err = source.Stat(); err != nil {
		return ViewFile{}, err
	} else if info.Size() < 8+length {
		return ViewFile{}, fmt.Errorf("%w: %s holds %d bytes, its header claims %d",
			ErrInvalidHeader, name, info.Size(), 8+length)
	}
	stub := make([]byte, 8+length)
	copy(stub[:8], prefix)
	if err := readFull(source, stub[8:]); err != nil {
		return ViewFile{}, fmt.Errorf("%w: %s: %w", ErrInvalidHeader, name, err)
	}
	// The stub must parse: it is what a destination probes.
	header, err := ParseHeader(stub)
	if err != nil {
		return ViewFile{}, err
	}
	// The header's own data extents must fit the file: the largest entry
	// end, past the length prefix and header, cannot overflow int64 or run
	// past the bytes that are actually there.
	info, err = source.Stat()
	if err != nil {
		return ViewFile{}, err
	}
	var maxEnd int64
	for _, entry := range header.Entries {
		if entry.End > maxEnd {
			maxEnd = entry.End
		}
	}
	if maxEnd > math.MaxInt64-8-length {
		return ViewFile{}, fmt.Errorf(
			"%w: %s declares a data end beyond the int64 range", ErrInvalidHeader, name,
		)
	}
	if 8+length+maxEnd > info.Size() {
		return ViewFile{}, fmt.Errorf(
			"%w: %s declares %d data bytes but holds %d",
			ErrInvalidHeader, name, 8+length+maxEnd, info.Size(),
		)
	}
	target := filepath.Join(staging, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ViewFile{}, err
	}
	if err := writeSealed(target, stub); err != nil {
		return ViewFile{}, err
	}
	sum := sha256.Sum256(stub)
	return ViewFile{
		Name: name, Size: header.Length + 8,
		SHA256: hex.EncodeToString(sum[:]), Truncated: true,
	}, nil
}

// ErrTooLarge is a metadata file above the bound a view carries.
var ErrTooLarge = errors.New("metadata file too large")

func writeSealed(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o444)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	// The bytes are as durable as the seal: a view the record names exists
	// whole after a crash, not half-written.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func sortDirEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if knownFiles[e.Name()] {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func safeRelative(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\x00") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func fileDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func readFull(f *os.File, buf []byte) error {
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			return err
		}
	}
	return nil
}

func le64(b []byte) uint64 {
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}
