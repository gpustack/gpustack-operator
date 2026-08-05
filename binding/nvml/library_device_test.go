package nvml

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole reason a v3 read has to be cross-checked is that the two struct versions are the same
// size, so the version word a caller writes is the only thing telling them apart and a driver
// dispatching on the size alone cannot be caught by the call's return. If a regeneration ever makes
// the sizes differ, the ambiguity is gone and the cross-check can go with it — so assert the premise
// rather than leaving it as a comment.
func TestGpuInstanceProfileInfoVersionsShareASize(t *testing.T) {
	var v2 GpuInstanceProfileInfo_v2
	var v3 GpuInstanceProfileInfo_v3
	assert.Equal(t, unsafe.Sizeof(v2), unsafe.Sizeof(v3),
		"v2 and v3 are the same size, which is what lets a driver confuse them")
}

// profileName renders a profile name into the C char array the driver reports it in.
func profileName(s string) [96]int8 {
	var out [96]int8
	for i := range s {
		out[i] = int8(s[i])
	}
	return out
}

// asV3Buffer returns what a v3-typed buffer holds after a driver has filled it with the v2 layout:
// the same bytes, read at v3's offsets. This is the misread itself, not an approximation of it.
func asV3Buffer(v2 GpuInstanceProfileInfo_v2) GpuInstanceProfileInfo_v3 {
	var v3 GpuInstanceProfileInfo_v3
	*(*GpuInstanceProfileInfo_v2)(unsafe.Pointer(&v3)) = v2
	return v3
}

// v2Read returns what V2 reports for a v2 buffer: the same values, copied field by field into the
// wider struct, which is the read that cannot be laid out wrongly.
func v2Read(v2 GpuInstanceProfileInfo_v2) GpuInstanceProfileInfo_v3 {
	return GpuInstanceProfileInfo_v3{
		Version:             v2.Version,
		Id:                  v2.Id,
		SliceCount:          v2.SliceCount,
		InstanceCount:       v2.InstanceCount,
		MultiprocessorCount: v2.MultiprocessorCount,
		CopyEngineCount:     v2.CopyEngineCount,
		DecoderCount:        v2.DecoderCount,
		EncoderCount:        v2.EncoderCount,
		JpegCount:           v2.JpegCount,
		OfaCount:            v2.OfaCount,
		MemorySizeMB:        v2.MemorySizeMB,
		Name:                v2.Name,
	}
}

func TestDescribesSameProfile(t *testing.T) {
	// A profile whose memory size ends in a zero byte, and the same one a single MiB larger. The
	// difference matters only to the misread: the name a v3 buffer reads out of MemorySizeMB's bytes
	// is empty for the first and non-empty for the second, which is exactly where a test on the name
	// alone stops working.
	evenSize := GpuInstanceProfileInfo_v2{
		Id: 19, IsP2pSupported: 0, SliceCount: 1, InstanceCount: 7,
		MultiprocessorCount: 16, CopyEngineCount: 1, JpegCount: 1,
		MemorySizeMB: 4864, Name: profileName("1g.5gb"),
	}
	oddSize := evenSize
	oddSize.MemorySizeMB = 4865

	cases := []struct {
		name string
		a, b GpuInstanceProfileInfo_v3
		want bool
	}{
		{
			name: "two reads of one profile agree",
			a:    v2Read(evenSize),
			b:    v2Read(evenSize),
			want: true,
		},
		{
			// The case the code exists for: the driver reported success and filled the buffer with
			// the other layout, so every field from the third on sits at the wrong offset.
			name: "a v3 buffer filled with the v2 layout is refused",
			a:    v2Read(evenSize),
			b:    asV3Buffer(evenSize),
			want: false,
		},
		{
			// Same misread, one MiB along. A test on "did the name come back non-empty" ACCEPTS this
			// one — the name reads out of 4865's low byte — and it is wrong in every field.
			name: "the same misread is refused when it does produce a name",
			a:    v2Read(oddSize),
			b:    asV3Buffer(oddSize),
			want: false,
		},
		{
			name: "a disagreeing memory size is refused",
			a:    v2Read(evenSize),
			b: func() GpuInstanceProfileInfo_v3 {
				out := v2Read(evenSize)
				out.MemorySizeMB++
				return out
			}(),
			want: false,
		},
		{
			name: "a disagreeing name is refused",
			a:    v2Read(evenSize),
			b: func() GpuInstanceProfileInfo_v3 {
				out := v2Read(evenSize)
				out.Name = profileName("1g.5gb+me")
				return out
			}(),
			want: false,
		},
		{
			// The name is what is compared, not the array holding it, so whatever a driver leaves
			// after the terminator cannot decide whether a read is trusted.
			name: "bytes past the name's terminator do not matter",
			a:    v2Read(evenSize),
			b: func() GpuInstanceProfileInfo_v3 {
				out := v2Read(evenSize)
				out.Name[len("1g.5gb")+1] = 'x'
				return out
			}(),
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, describesSameProfile(c.a, c.b))
		})
	}
}

// The misread's shape is asserted directly, so the cases above rest on a demonstrated mechanism
// rather than on this file's own arithmetic: a v3 buffer holding the v2 layout reads its slice count
// out of IsP2pSupported and its name out of MemorySizeMB.
func TestV2LayoutMisreadAtV3Offsets(t *testing.T) {
	v2 := GpuInstanceProfileInfo_v2{
		Id: 19, IsP2pSupported: 1, SliceCount: 3, InstanceCount: 7,
		MemorySizeMB: 4864, Name: profileName("1g.5gb"),
	}
	got := asV3Buffer(v2)

	require.Equal(t, v2.Id, got.Id, "the first two fields share an offset and survive")
	assert.Equal(t, v2.IsP2pSupported, got.SliceCount, "the slice count is read out of IsP2pSupported")
	assert.Equal(t, v2.SliceCount, got.InstanceCount, "every later field is shifted by one word")
	assert.Empty(t, got.GetName(), "4864's low byte is zero, so the name reads as terminated at once")

	v2.MemorySizeMB = 4865
	odd := asV3Buffer(v2)
	assert.NotEmpty(t, odd.GetName(),
		"one MiB more and the misread produces a name, which is why a test on the name alone is not enough")
}
