package modelmanager

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name    string
		edit    func(*Options)
		wantErr string
	}{
		{name: "a node name and the defaults", edit: func(o *Options) { o.NodeName = "node-1" }},
		{name: "no node name", edit: func(*Options) {}, wantErr: "--node-name"},
		{name: "a relative kubelet directory", edit: func(o *Options) { o.NodeName = "n"; o.KubeletDir = "kubelet" }, wantErr: "--kubelet-dir"},
		{name: "a relative cache root", edit: func(o *Options) { o.NodeName = "n"; o.CacheRoot = "models" }, wantErr: "--cache-root"},
		{name: "a relative socket", edit: func(o *Options) { o.NodeName = "n"; o.CSISocket = "csi.sock" }, wantErr: "--csi-socket"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := NewOptions()
			c.edit(o)
			err := o.Validate(context.Background())
			if c.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, c.wantErr)
		})
	}
}
