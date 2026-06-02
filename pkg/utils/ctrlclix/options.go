package ctrlclix

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
)

type NonQuorumOption string

func (NonQuorumOption) ApplyToGet(opts *ctrlcli.GetOptions) {
	if opts.Raw == nil {
		opts.Raw = &meta.GetOptions{}
	}
	opts.Raw.ResourceVersion = "0"
}

func (NonQuorumOption) ApplyToList(opts *ctrlcli.ListOptions) {
	if opts.Raw == nil {
		opts.Raw = &meta.ListOptions{}
	}
	opts.Raw.ResourceVersion = "0"
}

// NonQuorum is a GetOption and ListOption that sets ResourceVersion to "0",
// which means the result is served on APIServer, without waiting for the quorum of etcd to acknowledge the read.
// This can be useful for reducing latency when stale data is acceptable.
const NonQuorum = NonQuorumOption("")

// ToPatchOptions converts ctrlcli.UpdateOptions to ctrlcli.PatchOptions,
// as they share some common fields.
func ToPatchOptions(in ctrlcli.UpdateOptions) *ctrlcli.PatchOptions {
	return &ctrlcli.PatchOptions{
		DryRun:          in.DryRun,
		FieldManager:    in.FieldManager,
		FieldValidation: in.FieldValidation,
	}
}
