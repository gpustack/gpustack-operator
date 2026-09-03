package ctrlclix

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
)

type ResourceVersion string

func (v ResourceVersion) ApplyToGet(opts *ctrlcli.GetOptions) {
	if opts.Raw == nil {
		opts.Raw = &meta.GetOptions{}
	}
	opts.Raw.ResourceVersion = string(v)
}

func (v ResourceVersion) ApplyToList(opts *ctrlcli.ListOptions) {
	if opts.Raw == nil {
		opts.Raw = &meta.ListOptions{}
	}
	opts.Raw.ResourceVersion = string(v)
}

// WithoutQuorum is a GetOption and ListOption that sets ResourceVersion to "0",
// which means the result is served on APIServer, without waiting for the quorum of etcd to acknowledge the read.
// This can be useful for reducing latency when stale data is acceptable.
//
// It only takes effect on a reader that actually reaches the APIServer — an APIReader, or a type the
// manager excludes from its cache through Client.Cache.DisableFor. On a manager's cache-backed client
// it is a NO-OP: that Get is served out of the informer indexer, which reads only
// UnsafeDisableDeepCopy off the options and never sees a ResourceVersion. Passing it there buys
// nothing, and it misleads — it tells the next reader the call trades staleness for latency, which is
// a trade that call is not making.
const WithoutQuorum = ResourceVersion("0")

// ToPatchOptions converts ctrlcli.UpdateOptions to ctrlcli.PatchOptions,
// as they share some common fields.
func ToPatchOptions(in ctrlcli.UpdateOptions) *ctrlcli.PatchOptions {
	return &ctrlcli.PatchOptions{
		DryRun:          in.DryRun,
		FieldManager:    in.FieldManager,
		FieldValidation: in.FieldValidation,
	}
}

// Terminated is a DeleteOption that sets GracePeriodSeconds to 0,
// which means the object will be deleted immediately without waiting for the grace period.
const Terminated = ctrlcli.GracePeriodSeconds(0)

// InForeground is a DeleteOption that sets PropagationPolicy to meta.DeletePropagationForeground,
// which means the object will be deleted in foreground,
// and all its dependents will be deleted before the object itself is deleted.
const InForeground = ctrlcli.PropagationPolicy(meta.DeletePropagationForeground)
