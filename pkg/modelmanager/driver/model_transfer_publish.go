package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/modelmanager/metrics"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
)

// The model-transfer delivery attributes: a volume asks for a metadata-only delivery,
// and may name extra required metadata files from the known set.
const (
	attrModelTransferDelivery      = "model-transfer.gpustack.ai/delivery"
	attrModelTransferRequiredFiles = "model-transfer.gpustack.ai/required-files"
	modelTransferDeliveryMetadata  = "metadata"
)

// MetadataViews builds and holds the metadata-only views this node serves.
// It is an interface so the service's logic is tested without a store; the
// default serves content-addressed views out of the store's own root.
type MetadataViews interface {
	// Ensure builds (or joins) the view of hex's published tree, verified
	// against digest, and returns its directory.
	Ensure(ctx context.Context, hex, digest string, required []string) (string, error)
}

// storeMetadataViews serves views under the store's root, beside and never
// inside the published layout.
type storeMetadataViews struct {
	store *store.Store
}

// Ensure implements MetadataViews.
func (v *storeMetadataViews) Ensure(_ context.Context, hex, digest string,
	required []string,
) (string, error) {
	root := filepath.Join(v.store.Root(), "model-transfer", "views")
	return metadata.EnsureView(v.store, root, hex, digest, required)
}

// publishModelTransferVolume serves a volume the complete-tree contract must never
// see: a metadata-only delivery, bound to a published tree this node already
// verified, mounted read-only from a view that is never a checkpoint.
//
// The path shares the complete-tree path's authorization and reference
// rules, and differs where the spec demands it: an absent tree is an
// actionable refusal rather than a handoff to the materializer, because a
// metadata-only request must never start a full-checkpoint download, and the
// mount source is the view, so nothing marks the target as a complete tree.
func (d *Driver) publishModelTransferVolume(ctx context.Context, req *csi.NodePublishVolumeRequest,
	vc map[string]string, target string,
) (*csi.NodePublishVolumeResponse, error) {
	if delivery := vc[attrModelTransferDelivery]; delivery != modelTransferDeliveryMetadata {
		return nil, status.Errorf(codes.InvalidArgument,
			"unknown model-transfer delivery %q: only %q is supported", delivery, modelTransferDeliveryMetadata)
	}
	namespace, attrs := vc[attrPodNamespace], attributesOf(vc)
	ma, err := d.artifact(ctx, namespace, attrs.Artifact)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read ModelArtifact %s/%s: %v",
			namespace, attrs.Artifact, err)
	}
	if rule, msg := authorizeMount(namespace, attrs, ma); rule != "" {
		metrics.Mounts.WithLabelValues(metrics.MountDenied).Inc()
		klog.V(2).InfoS("model-transfer metadata mount refused", "rule", rule,
			"pod", namespace+"/"+vc[attrPodName], "volumeID", req.GetVolumeId())
		return nil, status.Error(codes.PermissionDenied, msg)
	}

	hex := store.HexOf(attrs.Digest)
	required, err := requiredFilesOf(vc)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	mounted, err := d.Mounter.IsMountPoint(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check the target: %v", err)
	}
	if mounted {
		readOnly, err := d.Mounter.IsReadOnly(target)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "check the target's mount: %v", err)
		}
		if readOnly {
			return &csi.NodePublishVolumeResponse{}, nil
		}
		if err := d.Mounter.Unmount(target); err != nil {
			return nil, status.Errorf(codes.Internal,
				"the target is mounted writable and cannot be unmounted: %v", err)
		}
		return nil, status.Error(codes.Unavailable,
			"the target was mounted writable and is unmounted; the next call mounts it read-only")
	}

	// Resolve the view before any reference is recorded: a destination
	// node without a full checkpoint serves a pre-staged view here, so the
	// published-tree claim must not come first. The refusal mapping keeps
	// the causes distinguishable: a bad request name is the caller's, a
	// missing or stale source is the node's.
	views := d.MetadataViews
	if views == nil {
		views = &storeMetadataViews{store: d.Store}
	}
	viewDir, err := views.Ensure(ctx, hex, attrs.Digest, required)
	if err != nil && !d.Store.IsPublished(hex) && vc[modelstore.ModelTransferClientConfigAttribute] != "" {
		viewDir, err = d.fetchModelTransferMetadata(ctx, req, attrs.Digest, required)
	}
	if err != nil {
		metrics.Mounts.WithLabelValues(metrics.MountDenied).Inc()
		return nil, d.refuseViewError(err)
	}

	// The view derives from a published tree when one exists, so the tree
	// is referenced for as long as the mount lives: the collector cannot
	// remove the source of a mounted view, exactly as it cannot remove a
	// mounted tree. Without a published tree the reference still records
	// the mount, so Unpublish can release it and the rebuild can keep it.
	ref := store.Ref{
		VolumeID: req.GetVolumeId(), TargetPath: target, Hex: hex,
		PodNamespace: namespace, PodName: vc[attrPodName], PodUID: vc[attrPodUID],
	}
	if d.Store.IsPublished(hex) {
		published, err := d.Store.Claim(ref)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "record the reference: %v", err)
		}
		if !published {
			return nil, status.Error(codes.FailedPrecondition,
				"the published tree vanished while mounting its metadata view; the next call resolves a view again")
		}
	} else if err := d.Store.WriteRef(ref); err != nil {
		return nil, status.Errorf(codes.Internal, "record the reference: %v", err)
	}

	if err := os.MkdirAll(target, 0o750); err != nil {
		d.dropRef(ref)
		return nil, status.Errorf(codes.Internal, "create the target: %v", err)
	}
	if err := d.Mounter.BindReadOnly(viewDir, target); err != nil {
		d.dropRef(ref)
		return nil, status.Errorf(codes.Internal, "mount: %v", err)
	}
	metrics.Mounts.WithLabelValues(metrics.MountHit).Inc()
	if d.Events != nil {
		d.Events.Mounted(hex)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

// refuseViewError maps a view resolution failure to its CSI shape: a name
// the request chose badly is an argument error; a file the source lacks or
// a stale identity is the node's state, retried once it changes; anything
// else is internal. The message never carries a tenant or a credential.
func (d *Driver) refuseViewError(err error) error {
	switch {
	case errors.Is(err, errModelTransferMetadataUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, metadata.ErrUnknownRequiredFile),
		errors.Is(err, metadata.ErrPathEscape):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, metadata.ErrMissingRequiredFile),
		errors.Is(err, metadata.ErrStaleDigest):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Errorf(codes.Internal, "build the metadata view: %v", err)
	}
}

// requiredFilesOf reads the extra-required-files attribute: known metadata
// names only, so a request cannot smuggle an unknown file or an escape.
func requiredFilesOf(vc map[string]string) ([]string, error) {
	required := metadata.DefaultRequiredFiles
	extra := strings.TrimSpace(vc[attrModelTransferRequiredFiles])
	if extra == "" {
		return required, nil
	}
	seen := map[string]bool{}
	for _, name := range required {
		seen[name] = true
	}
	names := strings.Split(extra, ",")
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		required = append(required, name)
		seen[name] = true
	}
	// The metadata package validates the joined set: an unknown name is an
	// actionable refusal naming it, before any tree is touched.
	if err := metadata.ValidateRequired(required); err != nil {
		return nil, err
	}
	return required, nil
}
