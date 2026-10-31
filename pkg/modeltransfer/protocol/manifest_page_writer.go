package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
)

// WriteManifestPages consumes tensors in file-offset order and atomically
// stores bounded pages. The caller owns the private directory and its lifetime.
// On error, the caller must discard its private directory; earlier pages may remain.
// Published pages are immutable and may be shared by roots with different identities.
func WriteManifestPages(
	ctx context.Context, directory string, files []FileDescriptor, tensors iter.Seq2[TensorDescriptor, error],
) ([]TensorPage, error) {
	if len(files) > MaxManifestFiles {
		return nil, New(KindInvalidDescriptor, "manifest.pages", "file count exceeds bound")
	}
	sizes := make(map[string]int64, len(files))
	for _, file := range files {
		if err := file.Validate(); err != nil {
			return nil, err
		}
		if _, ok := sizes[file.Path]; ok {
			return nil, New(KindInvalidDescriptor, "manifest.pages", "duplicate file")
		}
		sizes[file.Path] = file.Size
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	var pages []TensorPage
	var page TensorPage
	var buffer bytes.Buffer
	names := make(map[[32]byte]bool)
	ends := make(map[string]int64)
	totalBytes := 0
	flush := func() error {
		if page.Count == 0 {
			return nil
		}
		buffer.WriteByte(']')
		raw := buffer.Bytes()
		page.Size = len(raw)
		page.SHA256 = digestHex(raw)
		totalBytes += page.Size
		if len(pages) >= MaxManifestPages || totalBytes > MaxManifestTotalPageBytes {
			return New(KindInvalidDescriptor, "manifest.pages", "page count or total bytes exceed bound")
		}
		if err := writeManifestPage(ctx, directory, page.SHA256, raw); err != nil {
			return err
		}
		pages = append(pages, page)
		page = TensorPage{}
		buffer.Reset()
		return nil
	}
	for tensor, err := range tensors {
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fileSize, ok := sizes[tensor.File]
		if !ok {
			return nil, New(KindInvalidDescriptor, "manifest.pages", "tensor references unknown file")
		}
		if err := tensor.Validate(fileSize); err != nil {
			return nil, err
		}
		key := sha256.Sum256([]byte(tensor.Name))
		if names[key] || tensor.DataBegin < ends[tensor.File] {
			return nil, New(KindInvalidDescriptor, "manifest.pages", "duplicate name or overlapping tensor")
		}
		names[key] = true
		if len(names) > MaxManifestTensors {
			return nil, New(KindInvalidDescriptor, "manifest.pages", "tensor count exceeds bound")
		}
		ends[tensor.File] = tensor.DataEnd
		raw, err := json.Marshal(tensor)
		if err != nil {
			return nil, err
		}
		if len(raw)+2 > MaxManifestPageBytes {
			return nil, New(KindInvalidDescriptor, "manifest.pages", "one descriptor exceeds page bound")
		}
		if page.Count > 0 && (page.File != tensor.File || page.Count == MaxManifestPageTensors || buffer.Len()+len(raw)+2 > MaxManifestPageBytes) {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		if page.Count == 0 {
			page.File = tensor.File
			page.Begin = tensor.DataBegin
			buffer.WriteByte('[')
		} else {
			buffer.WriteByte(',')
		}
		buffer.Write(raw)
		page.Count++
		page.End = tensor.DataEnd
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return pages, nil
}

func writeManifestPage(ctx context.Context, directory, digest string, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".page-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = file.Close(); _ = os.Remove(name) }()
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(directory, digest+".json")); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
