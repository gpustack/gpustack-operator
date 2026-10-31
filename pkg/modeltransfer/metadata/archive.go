package metadata

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxArchiveBytes bounds one metadata delivery, including tokenizer files and
// header stubs. Weight payloads never belong in this archive.
const MaxArchiveBytes int64 = 256 << 20

// ExportView streams a verified metadata view. It refuses complete safetensors
// files even when a forged view record hashes them correctly.
func ExportView(ctx context.Context, viewDir string, output io.Writer) error {
	view, err := verifyTransferView(viewDir)
	if err != nil {
		return err
	}
	writer := tar.NewWriter(output)
	for _, name := range append([]string{viewFile}, viewNames(view)...) {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := os.Open(filepath.Join(viewDir, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		err = exportFile(ctx, writer, name, file)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return writer.Close()
}

func exportFile(ctx context.Context, writer *tar.Writer, name string, file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrNotRegularFile
	}
	if err := writer.WriteHeader(&tar.Header{Name: name, Size: info.Size(), Mode: 0o444, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(writer, contextReader{ctx, file})
	return err
}

// ImportView stages a bounded archive into the metadata-only layout, verifies
// its contents and artifact identity, and publishes it atomically. Authentication
// of the source is the caller's TLS and workload-authorization responsibility.
// It never creates a cache publication marker or calls a checkpoint downloader.
func ImportView(ctx context.Context, input io.Reader, viewRoot, digest string, required []string) (string, error) {
	if err := ValidateRequired(required); err != nil {
		return "", err
	}
	if err := os.MkdirAll(viewRoot, 0o755); err != nil {
		return "", err
	}
	owned, err := NewStaging(viewRoot)
	if err != nil {
		return "", err
	}
	defer owned.Release()
	stage := owned.Dir()
	limited := &io.LimitedReader{R: contextReader{ctx, input}, N: MaxArchiveBytes + 1}
	reader := tar.NewReader(limited)
	seen := map[string]bool{}
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if len(seen) >= maxHeaderStubFiles+len(knownFiles)+1 || header.Typeflag != tar.TypeReg ||
			!safeRelative(header.Name) || filepath.ToSlash(filepath.Clean(header.Name)) != header.Name || seen[header.Name] {
			return "", errors.New("metadata archive contains an invalid or duplicate entry")
		}
		if header.Name != viewFile && !knownFiles[header.Name] && !strings.HasSuffix(header.Name, ".safetensors") {
			return "", ErrUnknownRequiredFile
		}
		maxSize := MaxMetadataBytes
		if strings.HasSuffix(header.Name, ".safetensors") {
			maxSize = MaxHeaderBytes + 8
		}
		if header.Size < 0 || header.Size > maxSize || header.Size > MaxArchiveBytes-total {
			return "", errors.New("metadata archive exceeds its byte bound")
		}
		total += header.Size
		seen[header.Name] = true
		name := filepath.Join(stage, filepath.FromSlash(header.Name))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return "", err
		}
		file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
		if err != nil {
			return "", err
		}
		_, copyErr := io.CopyN(file, reader, header.Size)
		closeErr := file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	if limited.N <= 0 {
		return "", errors.New("metadata archive exceeds its transport bound")
	}
	view, err := verifyTransferView(stage)
	if err != nil {
		return "", err
	}
	if !viewSatisfies(view, digest, required) {
		return "", ErrStaleDigest
	}
	final := filepath.Join(viewRoot, view.ID())
	if err := publishView(stage, final, digest, required); err != nil {
		return "", err
	}
	return final, nil
}

func verifyTransferView(dir string) (*View, error) {
	view, err := Verify(dir)
	if err != nil {
		return nil, err
	}
	if len(view.Files) > maxHeaderStubFiles+len(knownFiles) {
		return nil, errors.New("metadata view exceeds its file bound")
	}
	var total int64
	for _, file := range view.Files {
		if file.Size < 0 || file.Size > MaxArchiveBytes-total {
			return nil, errors.New("metadata view exceeds its byte bound")
		}
		total += file.Size
		if strings.HasSuffix(file.Name, ".safetensors") {
			if !file.Truncated || file.Size > MaxHeaderBytes+8 {
				return nil, errors.New("metadata view contains checkpoint payload")
			}
			raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file.Name)))
			if err != nil {
				return nil, err
			}
			header, err := ParseHeader(raw)
			if err != nil {
				return nil, err
			}
			if header.DataOffset != file.Size {
				return nil, errors.New("metadata stub contains checkpoint payload")
			}
		} else if !knownFiles[file.Name] || file.Truncated || file.Size > MaxMetadataBytes {
			return nil, fmt.Errorf("%w: metadata-only file policy", ErrUnknownRequiredFile)
		}
	}
	return view, nil
}

func viewNames(view *View) []string {
	names := make([]string, 0, len(view.Files))
	for _, file := range view.Files {
		names = append(names, file.Name)
	}
	return names
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(output []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(output)
}
