// Package driver implements the model-manager's CSI Identity and Node services: it authorizes a
// mount against the API, mounts published content read-only, and hands everything else to the
// materializer.
package driver

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	klog "k8s.io/klog/v2"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelmanager/metrics"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

// secretTokenKey is the key of the artifact's Secret that holds the hub token, the key kubelet hands
// over from nodePublishSecretRef.
const secretTokenKey = "token"

// Request is a materialization request, everything one mount call knows about what it asks for.
type Request struct {
	// Hex is the manifest digest's hexadecimal part.
	Hex string
	// Artifact is the resolved artifact that authorized the mount.
	Artifact *workercore.ModelArtifact
	// Token is the credential kubelet handed over with this call, "" for none.
	Token string
}

// Progress is where a materialization stands after a mount call joined or started it.
type Progress struct {
	// Published says the content is now published and can be mounted.
	Published bool
	// Code and Message are what the mount call answers otherwise: Aborted while materializing,
	// Unavailable while backing off after a failure, ResourceExhausted without room.
	Code    codes.Code
	Message string
}

// Materializer starts or joins the materialization of a digest.
type Materializer interface {
	Ensure(ctx context.Context, req Request) Progress
}

// Events is told when a digest gains or loses a mount, for status and collection.
type Events interface {
	Mounted(hex string)
	Unmounted(hex string)
}

// Mounter bind-mounts and unmounts. It is an interface so the service's logic is tested without
// privileges; the Linux implementation is the only one that mounts.
type Mounter interface {
	IsMountPoint(path string) (bool, error)
	// IsReadOnly reports whether the mount at path is read-only, without set-user-ID or device files.
	IsReadOnly(path string) (bool, error)
	BindReadOnly(source, target string) error
	Unmount(target string) error
}

// Driver serves CSI Identity and Node.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedNodeServer

	Name       string
	Version    string
	NodeID     string
	KubeletDir string

	Store *store.Store
	// Artifacts reads ModelArtifacts from the informer cache, never with a GET per call.
	Artifacts ctrlcli.Reader
	// Ready reports whether the caches synced and references were rebuilt; until then every mount
	// answers Unavailable rather than a refusal the cache could not yet back.
	Ready        func() bool
	Mounter      Mounter
	Materializer Materializer
	Events       Events

	targetsMu sync.Mutex
	targets   map[string]*targetLock // target path -> its lock, while a call holds or waits for it
}

var (
	_ csi.IdentityServer = (*Driver)(nil)
	_ csi.NodeServer     = (*Driver)(nil)
)

// GetPluginInfo implements csi.IdentityServer.
func (d *Driver) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: d.Name, VendorVersion: d.Version}, nil
}

// GetPluginCapabilities implements csi.IdentityServer. A node-only plugin has none.
func (d *Driver) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

// Probe implements csi.IdentityServer.
func (d *Driver) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

// NodeGetInfo implements csi.NodeServer.
func (d *Driver) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: d.NodeID}, nil
}

// NodeGetCapabilities implements csi.NodeServer. An inline volume needs no stage step.
func (d *Driver) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

// NodePublishVolume implements csi.NodeServer.
//
// kubelet may call it for one target concurrently and repeatedly, and retries it until it succeeds,
// so it is idempotent: a target already mounted succeeds at once. A digest the node does not hold is
// handed to the materializer, and the call answers at once with a retryable code: one call has two
// minutes, and a download may take hours.
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	target := req.GetTargetPath()
	vc := req.GetVolumeContext()
	switch {
	case req.GetVolumeId() == "" || !isUnder(target, filepath.Join(d.KubeletDir, "pods")):
		return nil, status.Errorf(codes.InvalidArgument, "a volume ID and a target under %s/pods are required", d.KubeletDir)
	case req.GetVolumeCapability().GetMount() == nil:
		return nil, status.Error(codes.InvalidArgument, "only a mount volume is supported")
	case vc[attrEphemeral] != "true":
		return nil, status.Error(codes.InvalidArgument, "only a CSI inline ephemeral volume is supported")
	case d.Ready == nil || !d.Ready():
		return nil, status.Error(codes.Unavailable, "the plugin is starting: its caches have not synced yet")
	}

	unlock := d.lockTarget(target)
	defer unlock()

	namespace, attrs := vc[attrPodNamespace], attributesOf(vc)
	ma, err := d.artifact(ctx, namespace, attrs.Artifact)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read ModelArtifact %s/%s: %v", namespace, attrs.Artifact, err)
	}
	if rule, msg := authorizeMount(namespace, attrs, ma); rule != "" {
		metrics.Mounts.WithLabelValues(metrics.MountDenied).Inc()
		klog.V(2).InfoS("mount refused", "rule", rule, "pod", namespace+"/"+vc[attrPodName], "volumeID", req.GetVolumeId())
		return nil, status.Error(codes.PermissionDenied, msg)
	}

	mounted, err := d.Mounter.IsMountPoint(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check the target: %v", err)
	}
	if mounted {
		// Only a mount this call would have made answers a repeated call: a writable one a failed
		// remount could not undo is removed and refused, so kubelet's retry mounts it again.
		readOnly, err := d.Mounter.IsReadOnly(target)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "check the target's mount: %v", err)
		}
		if readOnly {
			return &csi.NodePublishVolumeResponse{}, nil
		}
		if err := d.Mounter.Unmount(target); err != nil {
			return nil, status.Errorf(codes.Internal, "the target is mounted writable and cannot be unmounted: %v", err)
		}
		return nil, status.Error(codes.Unavailable, "the target was mounted writable and is unmounted; the next call mounts it read-only")
	}

	hex := store.HexOf(attrs.Digest)
	ref := store.Ref{
		VolumeID: req.GetVolumeId(), TargetPath: target, Hex: hex,
		PodNamespace: namespace, PodName: vc[attrPodName], PodUID: vc[attrPodUID],
	}
	// The reference is recorded before the bind, under the lock the collector removes trees under:
	// a tree is claimed while it is still published, and a mount whose reference cannot be written
	// fails rather than serve a tree the collector does not know is mounted.
	result := metrics.MountHit
	published, err := d.Store.Claim(ref)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "record the reference: %v", err)
	}
	if !published {
		p := d.Materializer.Ensure(ctx, Request{Hex: hex, Artifact: ma, Token: req.GetSecrets()[secretTokenKey]})
		if !p.Published {
			metrics.Mounts.WithLabelValues(metrics.MountPending).Inc()
			return nil, status.Error(p.Code, p.Message)
		}
		result = metrics.MountMaterialized
		if published, err = d.Store.Claim(ref); err != nil {
			return nil, status.Errorf(codes.Internal, "record the reference: %v", err)
		}
		if !published {
			metrics.Mounts.WithLabelValues(metrics.MountPending).Inc()
			return nil, status.Error(codes.Unavailable, "the published tree was removed before the mount; the next call materializes it again")
		}
	}

	if err := os.MkdirAll(target, 0o750); err != nil {
		d.dropRef(ref)
		return nil, status.Errorf(codes.Internal, "create the target: %v", err)
	}
	if err := d.Mounter.BindReadOnly(d.Store.PublishedTree(hex), target); err != nil {
		d.dropRef(ref)
		return nil, status.Errorf(codes.Internal, "mount: %v", err)
	}
	metrics.Mounts.WithLabelValues(result).Inc()
	if d.Events != nil {
		d.Events.Mounted(hex)
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

// dropRef removes the reference of a mount that did not happen, so the tree is not kept for it.
func (d *Driver) dropRef(ref store.Ref) {
	if err := d.Store.RemoveRef(ref.VolumeID); err != nil {
		klog.ErrorS(err, "remove the reference of a failed mount", "volumeID", ref.VolumeID)
	}
}

// NodeUnpublishVolume implements csi.NodeServer.
//
// It works from the target alone and succeeds for a target it never mounted: kubelet calls it for a
// mount it marked uncertain, which is every mount answered with a retryable code while the Pod went
// away.
func (d *Driver) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if req.GetVolumeId() == "" || !isUnder(target, filepath.Join(d.KubeletDir, "pods")) {
		return nil, status.Errorf(codes.InvalidArgument, "a volume ID and a target under %s/pods are required", d.KubeletDir)
	}

	unlock := d.lockTarget(target)
	defer unlock()

	mounted, err := d.Mounter.IsMountPoint(target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, status.Errorf(codes.Internal, "check the target: %v", err)
	}
	if mounted {
		if err := d.Mounter.Unmount(target); err != nil {
			return nil, status.Errorf(codes.Internal, "unmount: %v", err)
		}
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, status.Errorf(codes.Internal, "remove the target: %v", err)
	}

	ref, found, err := d.Store.ReadRef(req.GetVolumeId())
	if err != nil {
		klog.ErrorS(err, "read the reference", "volumeID", req.GetVolumeId())
	}
	if err := d.Store.RemoveRef(req.GetVolumeId()); err != nil {
		klog.ErrorS(err, "remove the reference", "volumeID", req.GetVolumeId())
	}
	// A removed reference is a use that ended, whether or not the target was still mounted: a mount
	// lost to a reboot or cleaned up by hand was still in use until then.
	if found && d.Events != nil {
		d.Events.Unmounted(ref.Hex)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (d *Driver) artifact(ctx context.Context, namespace, name string) (*workercore.ModelArtifact, error) {
	if namespace == "" || name == "" {
		return nil, nil
	}
	ma := new(workercore.ModelArtifact)
	if err := d.Artifacts.Get(ctx, ctrlcli.ObjectKey{Namespace: namespace, Name: name}, ma); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	return ma, nil
}

// targetLock serializes the calls for one target, counting the calls that hold or wait for it.
type targetLock struct {
	mu    sync.Mutex
	users int
}

// lockTarget serializes the calls for one target; different targets proceed in parallel. A target's
// lock is dropped once no call holds or waits for it, since every Pod volume has a target of its own.
func (d *Driver) lockTarget(target string) func() {
	d.targetsMu.Lock()
	if d.targets == nil {
		d.targets = map[string]*targetLock{}
	}
	l := d.targets[target]
	if l == nil {
		l = &targetLock{}
		d.targets[target] = l
	}
	l.users++
	d.targetsMu.Unlock()

	l.mu.Lock()

	return func() {
		l.mu.Unlock()
		d.targetsMu.Lock()
		defer d.targetsMu.Unlock()
		if l.users--; l.users == 0 {
			delete(d.targets, target)
		}
	}
}

// isUnder reports whether target is strictly inside dir after cleaning.
func isUnder(target, dir string) bool {
	if target == "" {
		return false
	}
	rel, err := filepath.Rel(dir, filepath.Clean(target))

	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
