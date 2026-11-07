package protocol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fileSize = int64(1000)

func validTensor() TensorDescriptor {
	return TensorDescriptor{
		Name:      "w",
		File:      "checkpoint.safetensors",
		Dtype:     "F32",
		Shape:     []int64{2, 3},
		DataBegin: 100,
		DataEnd:   124,
	}
}

func TestTensorDescriptorValidate(t *testing.T) {
	valid := validTensor()
	require.NoError(t, valid.Validate(fileSize))

	cases := []struct {
		name     string
		mutate   func(*TensorDescriptor)
		wantKind Kind
	}{
		{"valid", func(*TensorDescriptor) {}, ""},
		{"nul in name", func(d *TensorDescriptor) { d.Name = "a\x00b" }, KindInvalidDescriptor},
		{"nul in file", func(d *TensorDescriptor) { d.File = "a\x00b" }, KindInvalidDescriptor},
		{"empty dtype", func(d *TensorDescriptor) { d.Dtype = "" }, KindInvalidDescriptor},
		{"quantized and sub-byte dtypes valid", func(d *TensorDescriptor) {
			d.Dtype = "F8_E4M3"
			d.DataBegin, d.DataEnd = 100, 106
		}, ""},
		{"known dtype span too short", func(d *TensorDescriptor) { d.DataEnd = 123 }, KindInvalidDescriptor},
		{"known dtype span too long", func(d *TensorDescriptor) { d.DataEnd = 125 }, KindInvalidDescriptor},
		{"negative dimension", func(d *TensorDescriptor) { d.Shape = []int64{2, -1} }, KindInvalidDescriptor},
		{"empty span for non-empty tensor", func(d *TensorDescriptor) { d.DataEnd = 100 }, KindInvalidDescriptor},
		{"end beyond file", func(d *TensorDescriptor) {
			d.DataBegin, d.DataEnd = 990, 1014
		}, KindInvalidDescriptor},
		{"negative begin", func(d *TensorDescriptor) { d.DataBegin = -4 }, KindInvalidDescriptor},
		{"inverted offsets", func(d *TensorDescriptor) {
			d.DataBegin, d.DataEnd = 124, 100
		}, KindInvalidDescriptor},
		{"zero elements valid", func(d *TensorDescriptor) {
			d.Shape = []int64{0}
			d.DataBegin, d.DataEnd = 100, 100
		}, ""},
		{"shape overflow", func(d *TensorDescriptor) {
			d.Shape = []int64{1 << 40, 1 << 40}
		}, KindRangeOverflow},
		{"byte size overflow", func(d *TensorDescriptor) {
			d.Dtype = "I64"
			d.Shape = []int64{1 << 62}
		}, KindRangeOverflow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			descriptor := validTensor()
			tc.mutate(&descriptor)
			err := descriptor.Validate(fileSize)
			if tc.wantKind == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.wantKind, KindOf(err))
		})
	}
}

func TestTensorDescriptorSizes(t *testing.T) {
	zero := validTensor()
	zero.Shape = []int64{0}
	zero.DataBegin, zero.DataEnd = 100, 100
	require.NoError(t, zero.Validate(fileSize))
	count, err := zero.ElementCount()
	require.NoError(t, err)
	assert.EqualValues(t, 0, count)
	size, err := zero.ByteSize()
	require.NoError(t, err)
	assert.EqualValues(t, 0, size)

	full := validTensor()
	count, err = full.ElementCount()
	require.NoError(t, err)
	assert.EqualValues(t, 6, count)
	size, err = full.ByteSize()
	require.NoError(t, err)
	assert.EqualValues(t, 24, size)
}

func TestValidateFileTensors(t *testing.T) {
	a := validTensor()
	b := validTensor()
	b.Name = "b"
	b.DataBegin, b.DataEnd = 124, 148
	require.NoError(t, ValidateFileTensors([]TensorDescriptor{a, b}, fileSize))

	b.DataBegin, b.DataEnd = 120, 148
	err := ValidateFileTensors([]TensorDescriptor{a, b}, fileSize)
	require.Error(t, err)
	assert.Equal(t, KindInvalidDescriptor, KindOf(err))
}

func TestValidateFileTensorsOrderIrrelevant(t *testing.T) {
	a := validTensor()
	b := validTensor()
	b.Name = "b"
	b.DataBegin, b.DataEnd = 124, 148
	require.NoError(t, ValidateFileTensors([]TensorDescriptor{b, a}, fileSize))
}

func TestFileDescriptorValidate(t *testing.T) {
	valid := FileDescriptor{Path: "checkpoint.safetensors", Size: 734}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name   string
		mutate func(*FileDescriptor)
	}{
		{"escape path", func(f *FileDescriptor) { f.Path = "../etc/passwd" }},
		{"absolute path", func(f *FileDescriptor) { f.Path = "/etc/passwd" }},
		{"dot segment", func(f *FileDescriptor) { f.Path = "./x" }},
		{"empty segment", func(f *FileDescriptor) { f.Path = "a//b" }},
		{"nul in path", func(f *FileDescriptor) { f.Path = "a\x00b" }},
		{"negative size", func(f *FileDescriptor) { f.Size = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := valid
			tc.mutate(&candidate)
			assert.Error(t, candidate.Validate())
		})
	}
}

func TestDtypeSizes(t *testing.T) {
	for _, dtype := range []string{"F64", "F32", "F16", "BF16", "I64", "I32", "I16", "I8", "U8", "BOOL"} {
		size, ok := DtypeSize(dtype)
		require.True(t, ok, dtype)
		assert.GreaterOrEqual(t, size, int64(1), dtype)
	}
	_, ok := DtypeSize("F128")
	assert.False(t, ok)
}
