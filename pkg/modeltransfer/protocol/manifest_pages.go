package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
)

const (
	// MaxPagedManifestBytes bounds the small root document independently of legacy manifests.
	MaxPagedManifestBytes = 4 << 20
	// MaxManifestPageBytes bounds each independently authenticated tensor page.
	MaxManifestPageBytes = 4 << 20
	// MaxManifestPageTensors bounds parsed descriptors retained per page.
	MaxManifestPageTensors = 4096
	// MaxManifestPages bounds root references and page fetches.
	MaxManifestPages = 16384
	// MaxManifestTotalPageBytes bounds the complete descriptor store on disk.
	MaxManifestTotalPageBytes = 512 << 20
)

// TensorPage binds one ordered, nonoverlapping portion of a file's tensors.
// SHA256 authenticates the exact JSON array; the manifest digest binds all pages.
type TensorPage struct {
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
	Count  int    `json:"count"`
	File   string `json:"file"`
	Begin  int64  `json:"begin"`
	End    int64  `json:"end"`
}

// PageReader reads only the specified immutable page. Implementations must
// bound response reads to Size+1 and authenticate remote access independently.
type PageReader func(context.Context, TensorPage) ([]byte, error)

type manifestPageIndex struct {
	reader PageReader
	names  map[[32]byte]int
	mu     sync.Mutex
	cache  []manifestPageCache
}

type manifestPageCache struct {
	number  int
	tensors []TensorDescriptor
}

func (m *Manifest) checkPageReferences(files map[string]int64) error {
	if len(m.Tensors) != 0 || len(m.TensorPages) > MaxManifestPages {
		return New(KindInvalidDescriptor, "manifest.pages", "mixed layouts or too many pages")
	}
	count, size := 0, 0
	ends := make(map[string]int64)
	seen := make(map[string]bool)
	for _, page := range m.TensorPages {
		fileSize, ok := files[page.File]
		if !ok || len(page.SHA256) != 64 || strings.Trim(page.SHA256, "0123456789abcdef") != "" ||
			page.Size < 2 || page.Size > MaxManifestPageBytes || page.Count < 1 || page.Count > MaxManifestPageTensors ||
			page.Begin < 0 || page.End < page.Begin || page.End > fileSize || page.Begin < ends[page.File] || seen[page.SHA256] {
			return New(KindInvalidDescriptor, "manifest.pages", "invalid page identity, bounds or order")
		}
		ends[page.File] = page.End
		seen[page.SHA256] = true
		count += page.Count
		size += page.Size
		if count > MaxManifestTensors || size > MaxManifestTotalPageBytes {
			return New(KindInvalidDescriptor, "manifest.pages", "descriptor count or total page bytes exceed bound")
		}
	}
	return nil
}

// TensorCount returns the complete descriptor count without loading any page.
func (m *Manifest) TensorCount() int {
	count := len(m.Tensors)
	for _, page := range m.TensorPages {
		count += page.Count
	}
	return count
}

// BindPages verifies all pages once, retaining only fixed-size name hashes and
// at most two decoded pages. No complete tensor document is reconstructed.
// Call before publishing the manifest to concurrent readers.
func (m *Manifest) BindPages(ctx context.Context, reader PageReader) error {
	if err := m.Check(); err != nil {
		return err
	}
	if len(m.TensorPages) == 0 {
		return nil
	}
	if reader == nil {
		return New(KindSourceUnavailable, "manifest.pages", "page reader is required")
	}
	index := &manifestPageIndex{reader: reader, names: make(map[[32]byte]int, m.TensorCount())}
	for number, page := range m.TensorPages {
		tensors, err := m.readPage(ctx, reader, page)
		if err != nil {
			return err
		}
		for _, tensor := range tensors {
			key := sha256.Sum256([]byte(tensor.Name))
			if _, exists := index.names[key]; exists {
				return New(KindInvalidDescriptor, "manifest.pages", "duplicate tensor name or name hash collision")
			}
			index.names[key] = number
		}
	}
	m.pageIndex = index
	return nil
}

func (m *Manifest) readPage(ctx context.Context, reader PageReader, page TensorPage) ([]TensorDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := reader(ctx, page)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) != page.Size || len(raw) > MaxManifestPageBytes || digestHex(raw) != page.SHA256 {
		return nil, New(KindAuthentication, "manifest.page", "page size or digest mismatch")
	}
	if err := rejectPageDuplicateKeys(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var tensors []TensorDescriptor
	if err := decoder.Decode(&tensors); err != nil {
		return nil, New(KindInvalidDescriptor, "manifest.page", "invalid tensor page: %v", err)
	}
	if len(tensors) != page.Count {
		return nil, New(KindInvalidDescriptor, "manifest.page", "page count mismatch")
	}
	file, err := m.File(page.File)
	if err != nil {
		return nil, err
	}
	end := page.Begin
	for i, tensor := range tensors {
		if tensor.File != page.File || tensor.DataBegin < end || (i == 0 && tensor.DataBegin != page.Begin) {
			return nil, New(KindInvalidDescriptor, "manifest.page", "file, order or beginning mismatch")
		}
		if err := tensor.Validate(file.Size); err != nil {
			return nil, err
		}
		end = tensor.DataEnd
	}
	if end != page.End {
		return nil, New(KindInvalidDescriptor, "manifest.page", "page end mismatch")
	}
	return tensors, nil
}

func (m *Manifest) page(ctx context.Context, number int) ([]TensorDescriptor, error) {
	index := m.pageIndex
	if index == nil {
		return nil, New(KindSourceUnavailable, "manifest.pages", "pages have not been verified")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index.mu.Lock()
	for _, cached := range index.cache {
		if cached.number == number {
			index.mu.Unlock()
			return cached.tensors, nil
		}
	}
	index.mu.Unlock()
	tensors, err := m.readPage(ctx, index.reader, m.TensorPages[number])
	if err != nil {
		return nil, err
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	for _, cached := range index.cache {
		if cached.number == number {
			return cached.tensors, nil
		}
	}
	if len(index.cache) == 2 {
		index.cache = index.cache[1:]
	}
	index.cache = append(index.cache, manifestPageCache{number: number, tensors: tensors})
	return tensors, nil
}

// TensorContext resolves one name using a bounded page cache. A hash collision
// cannot select another name because the descriptor itself is checked again.
func (m *Manifest) TensorContext(ctx context.Context, name string) (TensorDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return TensorDescriptor{}, err
	}
	if len(m.TensorPages) == 0 {
		return m.Tensor(name)
	}
	if m.pageIndex == nil {
		return TensorDescriptor{}, New(KindSourceUnavailable, "manifest.pages", "pages have not been verified")
	}
	number, ok := m.pageIndex.names[sha256.Sum256([]byte(name))]
	if ok {
		tensors, err := m.page(ctx, number)
		if err != nil {
			return TensorDescriptor{}, err
		}
		for _, tensor := range tensors {
			if tensor.Name == name {
				return tensor, nil
			}
		}
	}
	return TensorDescriptor{}, New(KindInvalidDescriptor, "manifest.tensor", "unknown tensor %q", name)
}

// WalkTensors visits every tensor without assembling a complete layout. The
// callback must not mutate descriptors or retain the complete traversal.
func (m *Manifest) WalkTensors(ctx context.Context, visit func(TensorDescriptor) error) error {
	if len(m.TensorPages) == 0 {
		for _, tensor := range m.Tensors {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(tensor); err != nil {
				return err
			}
		}
		return nil
	}
	for number := range m.TensorPages {
		tensors, err := m.page(ctx, number)
		if err != nil {
			return err
		}
		for _, tensor := range tensors {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(tensor); err != nil {
				return err
			}
		}
	}
	return nil
}

// LocalPageReader reads content-addressed pages below a private directory.
// Symlinks and nonregular entries are rejected before a bounded read.
func LocalPageReader(directory string) PageReader {
	return func(ctx context.Context, page TensorPage) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(page.SHA256) != 64 || strings.Trim(page.SHA256, "0123456789abcdef") != "" || page.Size < 2 || page.Size > MaxManifestPageBytes {
			return nil, New(KindInvalidDescriptor, "manifest.page", "invalid page reference")
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		name := page.SHA256 + ".json"
		info, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() != int64(page.Size) {
			return nil, New(KindInvalidDescriptor, "manifest.page", "page is not a regular file of the declared size")
		}
		file, err := root.Open(name)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		return io.ReadAll(io.LimitReader(file, int64(page.Size)+1))
	}
}

func rejectPageDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) error
	value = func(depth int) error {
		if depth > 16 {
			return New(KindInvalidDescriptor, "manifest.page", "JSON nesting exceeds bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			keys := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return New(KindInvalidDescriptor, "manifest.page", "duplicate JSON key")
				}
				keys[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return New(KindInvalidDescriptor, "manifest.page", "invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return New(KindInvalidDescriptor, "manifest.page", "trailing JSON data")
	}
	return nil
}
