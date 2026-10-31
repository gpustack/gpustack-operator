package protocol

import (
	"math"
	"sort"
	"strings"
)

// dtypeSizes maps known whole-byte safetensors dtype names to their element
// size in bytes. Sub-byte or newer safetensors dtypes are sized from their
// header data_offsets span directly.
var dtypeSizes = map[string]int64{
	"F64": 8, "F32": 4, "F16": 2, "BF16": 2,
	"F8_E4M3": 1, "F8_E5M2": 1, "F8_E8M0": 1,
	"F8_E4M3FNUZ": 1, "F8_E5M2FNUZ": 1,
	"I64": 8, "I32": 4, "I16": 2, "I8": 1,
	"U64": 8, "U32": 4, "U16": 2, "U8": 1,
	"BOOL": 1, "C64": 8,
}

// DtypeSize returns the element size of a dtype name, and whether the dtype
// has a fixed whole-byte element size in dtypeSizes.
func DtypeSize(dtype string) (int64, bool) {
	size, ok := dtypeSizes[dtype]
	return size, ok
}

// FileDescriptor describes a published file by relative path and byte size.
type FileDescriptor struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Validate checks the path stays inside the artifact and the size is non-negative.
func (f FileDescriptor) Validate() error {
	if !validRelativePath(f.Path) {
		return New(KindInvalidDescriptor, "file.path",
			"path must be relative without escapes: %q", f.Path)
	}
	if f.Size < 0 {
		return New(KindInvalidDescriptor, "file.size",
			"must be non-negative, got %d", f.Size)
	}
	return nil
}

// TensorDescriptor describes one tensor: its file, dtype, shape, and the
// absolute byte range [DataBegin, DataEnd) inside that file.
type TensorDescriptor struct {
	Name      string  `json:"name"`
	File      string  `json:"file"`
	Dtype     string  `json:"dtype"`
	Shape     []int64 `json:"shape"`
	DataBegin int64   `json:"data_begin"`
	DataEnd   int64   `json:"data_end"`
}

// ElementCount returns prod(Shape) with an int64 overflow check.
func (t TensorDescriptor) ElementCount() (int64, error) {
	if t.Shape == nil {
		return 0, New(KindInvalidDescriptor, "tensor.shape", "shape is required")
	}
	count := int64(1)
	for _, dim := range t.Shape {
		if dim < 0 {
			return 0, New(KindInvalidDescriptor, "tensor.shape",
				"dimensions must be non-negative: %v", t.Shape)
		}
		if dim == 0 {
			count = 0
			continue
		}
		if count > math.MaxInt64/dim {
			return 0, New(KindRangeOverflow, "tensor.shape",
				"element count exceeds the int64 range")
		}
		count *= dim
	}
	return count, nil
}

// ByteSize returns the shape size for known whole-byte dtypes, or the offset
// span for other dtypes, checking element count and multiplication overflow.
func (t TensorDescriptor) ByteSize() (int64, error) {
	if strings.TrimSpace(t.Dtype) == "" || strings.ContainsRune(t.Dtype, 0) {
		return 0, New(KindInvalidDescriptor, "tensor.dtype",
			"unknown dtype %q", t.Dtype)
	}
	count, err := t.ElementCount()
	if err != nil {
		return 0, err
	}
	if size, ok := dtypeSizes[t.Dtype]; ok {
		if count > 0 && count > math.MaxInt64/size {
			return 0, New(KindRangeOverflow, "tensor.byte_size", "byte size exceeds the int64 range")
		}
		return count * size, nil
	}
	if count == 0 {
		return 0, nil
	}
	if t.DataEnd >= t.DataBegin && t.DataBegin >= 0 {
		return t.DataEnd - t.DataBegin, nil
	}
	return 0, nil
}

// Validate checks the descriptor against the size of its file: non-empty dtype,
// non-negative shape, and valid offsets inside the file.
func (t TensorDescriptor) Validate(fileSize int64) error {
	if t.Name == "" {
		return New(KindInvalidDescriptor, "tensor.name", "must be non-empty")
	}
	if t.File == "" {
		return New(KindInvalidDescriptor, "tensor.file", "must be non-empty")
	}
	// Names and file references must be usable without NUL terminators.
	if strings.ContainsRune(t.Name, 0) || strings.ContainsRune(t.File, 0) {
		return New(KindInvalidDescriptor, "tensor.name",
			"tensor name and file must not contain a NUL byte")
	}
	if t.DataBegin < 0 || t.DataEnd < t.DataBegin {
		return New(KindInvalidDescriptor, "tensor.offsets",
			"invalid range [%d, %d)", t.DataBegin, t.DataEnd)
	}
	count, err := t.ElementCount()
	if err != nil {
		return err
	}
	byteSize, err := t.ByteSize()
	if err != nil {
		return err
	}
	if t.DataEnd > fileSize {
		return New(KindInvalidDescriptor, "tensor.offsets",
			"tensor %q ends at %d beyond file size %d", t.Name, t.DataEnd,
			fileSize)
	}
	if (count == 0 && t.DataEnd != t.DataBegin) || (count > 0 && t.DataEnd == t.DataBegin) || t.DataEnd-t.DataBegin != byteSize {
		return New(KindInvalidDescriptor, "tensor.offsets",
			"tensor %q span %d != byte size %d", t.Name,
			t.DataEnd-t.DataBegin, byteSize)
	}
	return nil
}

// ValidateFileTensors validates each tensor against fileSize and rejects
// overlapping byte ranges. Manifest order is irrelevant; overlap is what
// matters.
func ValidateFileTensors(tensors []TensorDescriptor, fileSize int64) error {
	for _, tensor := range tensors {
		if err := tensor.Validate(fileSize); err != nil {
			return err
		}
	}
	ordered := make([]TensorDescriptor, len(tensors))
	copy(ordered, tensors)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].DataBegin != ordered[j].DataBegin {
			return ordered[i].DataBegin < ordered[j].DataBegin
		}
		return ordered[i].DataEnd < ordered[j].DataEnd
	})
	for i := 1; i < len(ordered); i++ {
		if ordered[i].DataBegin < ordered[i-1].DataEnd {
			return New(KindInvalidDescriptor, "tensor.offsets",
				"tensors %q and %q overlap", ordered[i-1].Name,
				ordered[i].Name)
		}
	}
	return nil
}

// validRelativePath reports whether path is a plain relative path with no
// empty, current, parent, or NUL segments.
func validRelativePath(path string) bool {
	if path == "" || path[0] == '/' || strings.ContainsRune(path, 0) {
		return false
	}
	start := 0
	for i := 0; i <= len(path); i++ {
		if i == len(path) || path[i] == '/' {
			switch path[start:i] {
			case "", ".", "..":
				return false
			}
			start = i + 1
		}
	}
	return true
}
