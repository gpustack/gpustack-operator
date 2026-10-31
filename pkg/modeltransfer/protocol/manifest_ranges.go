package protocol

import (
	"context"
	"sort"
)

// ReferencedFiles returns files carrying tensors, in manifest file order.
func (m *Manifest) ReferencedFiles() []FileDescriptor {
	paths := make(map[string]bool)
	for _, tensor := range m.Tensors {
		paths[tensor.File] = true
	}
	for _, page := range m.TensorPages {
		paths[page.File] = true
	}
	files := make([]FileDescriptor, 0, len(paths))
	for _, file := range m.Files {
		if paths[file.Path] {
			files = append(files, file)
		}
	}
	return files
}

// ValidTensorSpan accepts a tensor slice, or a contiguous run ending on a
// tensor boundary. Page reads are bounded and canceled with ctx.
func (m *Manifest) ValidTensorSpan(ctx context.Context, tensor TensorDescriptor, offset, length int64) bool {
	size, err := tensor.ByteSize()
	if err != nil || offset < 0 || length < 0 || offset > size {
		return false
	}
	if length <= size-offset {
		return true
	}
	if offset != 0 || tensor.DataBegin > (1<<63-1)-length {
		return false
	}
	targetEnd := tensor.DataBegin + length
	end := tensor.DataEnd
	found := false
	visit := func(items []TensorDescriptor) (bool, bool) {
		for _, item := range items {
			if !found {
				if item.Name == tensor.Name {
					found = true
				}
				continue
			}
			if item.DataBegin != end {
				return true, false
			}
			end = item.DataEnd
			if end >= targetEnd {
				return true, end == targetEnd
			}
		}
		return false, false
	}
	if len(m.TensorPages) == 0 {
		var items []TensorDescriptor
		for _, item := range m.Tensors {
			if item.File == tensor.File {
				items = append(items, item)
			}
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].DataBegin != items[j].DataBegin {
				return items[i].DataBegin < items[j].DataBegin
			}
			return items[i].DataEnd < items[j].DataEnd
		})
		_, valid := visit(items)
		return valid
	}
	for number, page := range m.TensorPages {
		if page.File != tensor.File || page.End < tensor.DataBegin {
			continue
		}
		if err := ctx.Err(); err != nil {
			return false
		}
		items, err := m.page(ctx, number)
		if err != nil {
			return false
		}
		if done, valid := visit(items); done {
			return valid
		}
	}
	return false
}
