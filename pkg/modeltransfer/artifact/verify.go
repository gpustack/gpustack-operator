package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// safetensorsSuffix marks the only tensor format the transfer contract
// carries. No other naming, and no pickle fallback, is ever parsed.
const safetensorsSuffix = ".safetensors"

// verifiedFile holds the bounded layout of one published file.
type verifiedFile struct {
	path    string
	size    int64
	tensors []protocol.TensorDescriptor
}

// verifyTree validates published file extents and bounded headers. Content
// verification belongs to the model cache publisher. No payload is scanned.
func verifyTree(
	ctx context.Context, st *store.Store, artifactUID string, sourceGeneration int64,
	contentDigest string, canonical modelartifact.Manifest,
) (*View, error) {
	treeDir := st.PublishedTree(store.HexOf(contentDigest))
	root, err := os.OpenRoot(treeDir)
	if err != nil {
		return nil, fmt.Errorf("%w: the published tree could not be opened: %w", ErrNotPublished, err)
	}
	defer func() { _ = root.Close() }()

	verified, err := verifyFiles(ctx, canonical.Entries,
		func(ctx context.Context, entry modelartifact.ManifestEntry, registeredBytes int64) (*verifiedFile, error) {
			return verifyOne(ctx, root, entry, registeredBytes)
		})
	if err != nil {
		return nil, err
	}
	files := verified.files

	manifest := &protocol.Manifest{
		ContractVersion:  protocol.ContractVersion,
		ArtifactUID:      artifactUID,
		SourceGeneration: sourceGeneration,
		Files:            make([]protocol.FileDescriptor, 0, len(files)),
		Tensors:          make([]protocol.TensorDescriptor, 0, maxTensorSlack),
	}
	for _, file := range files {
		manifest.Files = append(manifest.Files, protocol.FileDescriptor{
			Path: file.path, Size: file.size,
		})
		manifest.Tensors = append(manifest.Tensors, file.tensors...)
	}
	slices.SortFunc(manifest.Tensors, func(a, b protocol.TensorDescriptor) int {
		return strings.Compare(a.Name, b.Name)
	})
	raw, sha256hex, err := encodeManifest(manifest)
	if err != nil {
		return nil, err
	}
	manifest.ManifestSHA256 = sha256hex

	return &View{
		Manifest: manifest, Raw: raw, SHA256: sha256hex,
		ContentDigest: contentDigest, TreeDir: treeDir,
		RegisteredBytes: verified.registeredBytes, Registrations: verified.registrations,
	}, nil
}

// verifiedFiles holds an ordered verification result and its registration totals.
type verifiedFiles struct {
	files           []verifiedFile
	registeredBytes int64
	registrations   int
}

// verifyFiles bounds concurrent IO and applies aggregate limits in listing order.
func verifyFiles(
	ctx context.Context, entries []modelartifact.ManifestEntry,
	verify func(context.Context, modelartifact.ManifestEntry, int64) (*verifiedFile, error),
) (*verifiedFiles, error) {
	return verifyFilesWithConsumer(ctx, entries, verify, nil)
}

func verifyFilesWithConsumer(
	ctx context.Context, entries []modelartifact.ManifestEntry,
	verify func(context.Context, modelartifact.ManifestEntry, int64) (*verifiedFile, error),
	consume func(*verifiedFile) error,
) (*verifiedFiles, error) {
	// Keep at most one pending result per worker. Consume them in listing
	// order so the first refusal and the manifest never depend on scheduling.
	ctx, cancel := context.WithCancel(ctx)
	workers := min(len(entries), runtime.GOMAXPROCS(0))
	if consume != nil {
		// A paged build retains only one parsed file header at a time.
		workers = min(workers, 1)
	}
	jobs := make(chan int, workers)
	type result struct {
		file *verifiedFile
		err  error
	}
	results := make([]chan result, workers)
	listedRegistered := make([]int64, len(entries))
	var listedBytes int64
	var err error
	for i, entry := range entries {
		if isSafetensors(entry.Path) {
			listedBytes, err = addChecked(listedBytes, entry.Size)
			if err != nil {
				cancel()
				return nil, err
			}
		}
		listedRegistered[i] = listedBytes
	}
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	for i := range workers {
		results[i] = make(chan result, 1)
		jobs <- i
	}
	for range workers {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case i := <-jobs:
					file, err := verify(ctx, entries[i], listedRegistered[i])
					results[i%workers] <- result{file: file, err: err}
				}
			}
		})
	}
	files := make([]verifiedFile, 0, len(entries))
	registeredBytes, registrations, tensorCount := int64(0), 0, 0
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var verified result
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case verified = <-results[i%workers]:
		}
		if verified.err != nil {
			return nil, verified.err
		}
		if next := i + workers; consume == nil && next < len(entries) {
			jobs <- next
		}
		file := verified.file
		if file == nil {
			continue
		}
		if len(file.tensors) == 0 {
			files = append(files, *file)
			if next := i + workers; consume != nil && next < len(entries) {
				jobs <- next
			}
			continue
		}
		registeredBytes, err = addChecked(registeredBytes, file.size)
		if err != nil {
			return nil, err
		}
		if registeredBytes > MaxRegisteredBytes {
			return nil, fmt.Errorf("%w: the safetensors files total %d bytes, above the %d bound",
				ErrBoundsExceeded, registeredBytes, MaxRegisteredBytes)
		}
		registrations++
		tensorCount += len(file.tensors)
		if tensorCount > MaxTensors {
			return nil, fmt.Errorf("%w: the tree holds more than %d tensors", ErrBoundsExceeded, MaxTensors)
		}
		if consume != nil {
			if err := consume(file); err != nil {
				return nil, err
			}
			file.tensors = nil
		}
		files = append(files, *file)
		if next := i + workers; consume != nil && next < len(entries) {
			jobs <- next
		}
	}
	if registrations == 0 {
		return nil, ErrNoTensors
	}

	return &verifiedFiles{files: files, registeredBytes: registeredBytes, registrations: registrations}, nil
}

// maxTensorSlack is the assembly-time capacity hint; the bound the manifest
// may not exceed is MaxTensors, enforced where the tensors are counted.
const maxTensorSlack = 8

// verifyOne checks a published file and parses only its bounded header.
// Empty ancillary files retain their layout entry; empty safetensors are refused.
func verifyOne(
	ctx context.Context, root *os.Root, entry modelartifact.ManifestEntry,
	registeredBytes int64,
) (*verifiedFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := lstatWithin(root, entry.Path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrFileMismatch, entry.Path)
	}
	if info.Size() != entry.Size {
		return nil, fmt.Errorf("%w: %s holds %d bytes, its listing says %d",
			ErrFileMismatch, entry.Path, info.Size(), entry.Size)
	}
	if entry.Size == 0 {
		if isSafetensors(entry.Path) {
			return nil, fmt.Errorf("%w: %s is empty", ErrInvalidSafetensors, entry.Path)
		}
		return &verifiedFile{path: entry.Path, size: 0}, nil
	}

	f, err := root.Open(entry.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s could not be opened: %w", ErrFileMismatch, entry.Path, err)
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Size() != entry.Size {
		return nil, fmt.Errorf("%w: %s is not the %d-byte regular file its listing says",
			ErrFileMismatch, entry.Path, entry.Size)
	}

	file := &verifiedFile{path: entry.Path, size: entry.Size}
	if isSafetensors(entry.Path) {
		if registeredBytes > MaxRegisteredBytes {
			return nil, fmt.Errorf("%w: the safetensors files total %d bytes, above the %d bound",
				ErrBoundsExceeded, registeredBytes, MaxRegisteredBytes)
		}
		file.tensors, err = safetensorsTensors(f, entry)
		if err != nil {
			return nil, err
		}
	}

	return file, nil
}

// lstatWithin checks every path component inside the root: each directory on
// the way must be a real directory and the last component a plain entry, so
// a symlink — escape or not — is refused before anything is opened.
func lstatWithin(root *os.Root, path string) (fs.FileInfo, error) {
	for i := 0; i < len(path); i++ {
		if path[i] != '/' {
			continue
		}
		info, err := root.Lstat(path[:i])
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrFileMismatch, path, err)
		}
		if !info.Mode().IsDir() {
			return nil, fmt.Errorf("%w: %s runs through %q, which is not a directory",
				ErrFileMismatch, path, path[:i])
		}
	}
	info, err := root.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrFileMismatch, path, err)
	}

	return info, nil
}

func isSafetensors(path string) bool {
	return strings.HasSuffix(path, safetensorsSuffix)
}

// safetensorsTensors reads the file's header through the metadata reader's
// rules and turns each header entry into a tensor descriptor whose offsets
// are absolute in the file: data begins at 8 plus the header length. The
// header is bounded before it is read, and every extent is checked against
// the file the listing verified.
func safetensorsTensors(f *os.File, entry modelartifact.ManifestEntry) ([]protocol.TensorDescriptor, error) {
	header, err := readSafetensorsHeader(f, entry)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(header.Entries))
	for name := range header.Entries {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: %s declares no tensor", ErrInvalidSafetensors, entry.Path)
	}
	tensors := make([]protocol.TensorDescriptor, 0, len(names))
	for _, name := range names {
		e := header.Entries[name]
		dataBegin, err := addChecked(header.DataOffset, e.Begin)
		if err != nil {
			return nil, fmt.Errorf("%w: %s's tensor %q starts beyond the int64 range",
				ErrInvalidSafetensors, entry.Path, name)
		}
		dataEnd, err := addChecked(dataBegin, e.End-e.Begin)
		if err != nil || dataEnd > entry.Size {
			return nil, fmt.Errorf("%w: %s's tensor %q ends past the file's %d bytes",
				ErrInvalidSafetensors, entry.Path, name, entry.Size)
		}
		tensors = append(tensors, protocol.TensorDescriptor{
			Name: name, File: entry.Path, Dtype: e.Dtype, Shape: e.Shape,
			DataBegin: dataBegin, DataEnd: dataEnd,
		})
	}
	if err := protocol.ValidateFileTensors(tensors, entry.Size); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidSafetensors, entry.Path, err)
	}

	return tensors, nil
}

// readSafetensorsHeader reads the 8-byte length prefix and the header it
// announces, bounding the header before it is allocated: the rules are the
// metadata reader's, and a header longer than the bound is refused without
// reading it. The reads are at fixed offsets, so the file may be positioned
// anywhere.
func readSafetensorsHeader(f *os.File, entry modelartifact.ManifestEntry) (*metadata.Header, error) {
	prefix := make([]byte, 8)
	if _, err := f.ReadAt(prefix, 0); err != nil {
		return nil, fmt.Errorf("%w: %s is shorter than its length prefix", ErrInvalidSafetensors, entry.Path)
	}
	length := int64(0)
	for i, b := range prefix {
		length |= int64(b) << (8 * i)
	}
	if length < 2 {
		return nil, fmt.Errorf("%w: %s's header length is %d", ErrInvalidSafetensors, entry.Path, length)
	}
	if length > metadata.MaxHeaderBytes {
		return nil, fmt.Errorf("%w: %s's header claims %d bytes, above the %d bound",
			ErrInvalidSafetensors, entry.Path, length, metadata.MaxHeaderBytes)
	}
	stub := make([]byte, 8+length)
	copy(stub, prefix)
	if _, err := f.ReadAt(stub[8:], 8); err != nil {
		return nil, fmt.Errorf("%w: %s's header is truncated", ErrInvalidSafetensors, entry.Path)
	}
	header, err := metadata.ParseHeader(stub)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidSafetensors, entry.Path, err)
	}

	return header, nil
}

// sha256Hex is the lowercase hex sha256 of raw.
func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
