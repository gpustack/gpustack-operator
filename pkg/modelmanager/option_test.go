package modelmanager

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name    string
		edit    func(*Options)
		wantErr string
	}{
		{name: "a node name and the defaults", edit: func(o *Options) { o.NodeName = "node-1" }},
		{name: "a valid ingest service account", edit: func(o *Options) {
			o.NodeName = "node-1"
			o.IngestServiceAccount = "custom-release-worker"
		}},
		{name: "an invalid ingest service account", edit: func(o *Options) {
			o.NodeName = "node-1"
			o.IngestServiceAccount = "Invalid_Name"
		}, wantErr: "--ingest-service-account"},
		{name: "no node name", edit: func(*Options) {}, wantErr: "--node-name"},
		{name: "a relative kubelet directory", edit: func(o *Options) { o.NodeName = "n"; o.KubeletDir = "kubelet" }, wantErr: "--kubelet-dir"},
		{name: "a relative cache root", edit: func(o *Options) { o.NodeName = "n"; o.CacheRoot = "models" }, wantErr: "--cache-root"},
		{name: "a relative model-transfer state root", edit: func(o *Options) { o.NodeName = "n"; o.ModelTransferStateRoot = "model-transfer" }, wantErr: "--model-transfer-state-root"},
		{name: "an absolute model-transfer state root", edit: func(o *Options) { o.NodeName = "n"; o.ModelTransferStateRoot = "/var/lib/gpustack/model-transfer" }},
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

func TestCommandExposesIngestServiceAccountFlag(t *testing.T) {
	cmd := NewCommand()
	flag := cmd.Flags().Lookup("ingest-service-account")
	require.NotNil(t, flag)
	assert.Empty(t, flag.DefValue)
}
