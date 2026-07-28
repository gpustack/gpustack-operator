package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPatch(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		blocks  []Block
		want    string
		wantErr bool
	}{
		{
			name: "replaces content between markers, re-indented to the begin marker",
			src: "top:\n" +
				"  # gpustack:chartvalues:demo:begin\n" +
				"  stale: true\n" +
				"  # gpustack:chartvalues:demo:end\n" +
				"bottom: true\n",
			blocks: []Block{{Name: "demo", Content: "fresh:\n- one\n- two\n"}},
			want: "top:\n" +
				"  # gpustack:chartvalues:demo:begin\n" +
				"  fresh:\n" +
				"  - one\n" +
				"  - two\n" +
				"  # gpustack:chartvalues:demo:end\n" +
				"bottom: true\n",
		},
		{
			name:   "leaves the file untouched when a block's markers are absent",
			src:    "top: true\n",
			blocks: []Block{{Name: "demo", Content: "fresh: true\n"}},
			want:   "top: true\n",
		},
		{
			name: "replaces every occurrence of a repeated marker name identically",
			src: "a:\n" +
				"  # gpustack:chartvalues:demo:begin\n" +
				"  old: 1\n" +
				"  # gpustack:chartvalues:demo:end\n" +
				"b:\n" +
				"  # gpustack:chartvalues:demo:begin\n" +
				"  old: 2\n" +
				"  # gpustack:chartvalues:demo:end\n",
			blocks: []Block{{Name: "demo", Content: "new: 1\n"}},
			want: "a:\n" +
				"  # gpustack:chartvalues:demo:begin\n" +
				"  new: 1\n" +
				"  # gpustack:chartvalues:demo:end\n" +
				"b:\n" +
				"  # gpustack:chartvalues:demo:begin\n" +
				"  new: 1\n" +
				"  # gpustack:chartvalues:demo:end\n",
		},
		{
			name: "only patches the block whose name matches, leaving other markers alone",
			src: "# gpustack:chartvalues:demo:begin\n" +
				"old: 1\n" +
				"# gpustack:chartvalues:demo:end\n" +
				"# gpustack:chartvalues:other:begin\n" +
				"untouched: true\n" +
				"# gpustack:chartvalues:other:end\n",
			blocks: []Block{{Name: "demo", Content: "new: 1\n"}},
			want: "# gpustack:chartvalues:demo:begin\n" +
				"new: 1\n" +
				"# gpustack:chartvalues:demo:end\n" +
				"# gpustack:chartvalues:other:begin\n" +
				"untouched: true\n" +
				"# gpustack:chartvalues:other:end\n",
		},
		{
			name: "errors on a begin marker with no matching end",
			src: "top:\n" +
				"  # gpustack:chartvalues:demo:begin\n" +
				"  old: 1\n",
			blocks:  []Block{{Name: "demo", Content: "new: 1\n"}},
			wantErr: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := Patch([]byte(c.src), c.blocks)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, string(got))
		})
	}
}
