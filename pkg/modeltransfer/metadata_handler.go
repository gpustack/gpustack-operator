package modeltransfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

type metadataAuthorizer interface {
	AuthorizeMetadata(context.Context, authorization.Request, string) *protocol.Error
}

var errSourceIneligible = errors.New("source Pod is not eligible")

func metadataPrefix(workerID string, vendorScope ...string) string {
	sum := sha256.Sum256([]byte(workerID))
	if len(vendorScope) > 0 && vendorScope[0] != "" {
		return "model-transfer.metadata." + vendorScope[0] + "." + hex.EncodeToString(sum[:16]) + "."
	}
	return "model-transfer.metadata." + hex.EncodeToString(sum[:16]) + "."
}

func buildRefPrefix(vendorScope ...string) string {
	if len(vendorScope) > 0 && vendorScope[0] != "" {
		return "model-transfer.build." + vendorScope[0] + "."
	}
	return "model-transfer.build."
}

func isOwnedTransientRef(ref store.Ref, workerID, scope string) bool {
	if ref.PodUID == workerID && (strings.HasPrefix(ref.VolumeID, metadataPrefix(workerID)) ||
		(scope != "" && strings.HasPrefix(ref.VolumeID, metadataPrefix(workerID, scope))) ||
		strings.HasPrefix(ref.VolumeID, "model-transfer.build.")) {
		return true
	}
	if scope != "" && (strings.HasPrefix(ref.VolumeID, "model-transfer.metadata."+scope+".") ||
		strings.HasPrefix(ref.VolumeID, "model-transfer.build."+scope+".")) {
		return true
	}
	return false
}

// cleanupMetadataRequests removes transient metadata and build claims belonging
// to this locked broker Pod or its vendor scope across Pod recreations.
func cleanupMetadataRequests(st *store.Store, workerID string, vendorScope ...string) error {
	if workerID == "" {
		return errors.New("metadata owner is unavailable")
	}
	scope := ""
	if len(vendorScope) > 0 {
		scope = vendorScope[0]
	}
	refs, err := st.Refs()
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if isOwnedTransientRef(ref, workerID, scope) {
			if err := st.RemoveRef(ref.VolumeID); err != nil {
				return err
			}
		}
	}
	dir := filepath.Join(st.Root(), "model-transfer", "metadata-temp")
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, metadataPrefix(workerID)) ||
			(scope != "" && strings.HasPrefix(name, "model-transfer.metadata."+scope+".")) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	// A crashed metadata view build leaves its staging directory and its
	// ownership lock behind; recovery reclaims exactly the staging whose
	// lock can be taken: a builder holds that lock, without release, for
	// the whole life of its staging, so an uncontended take is proof the
	// staging is inactive. A lock another process still held proves its
	// tree active, and staging without a lock predates the protocol and is
	// preserved. A missing metadata-temp never skips this sweep.
	return metadata.ReclaimAbandonedStaging(filepath.Join(st.Root(), "model-transfer", "views"))
}

// localSourcePod verifies the live Pod and its controller before using the
// downward-API identity as the source of a metadata response.
func (c *Config) localSourcePod(ctx context.Context) (*core.Pod, error) {
	pod, _, err := c.localSourcePodAndDaemonSet(ctx)
	return pod, err
}

func (c *Config) localSourcePodAndDaemonSet(ctx context.Context) (*core.Pod, *apps.DaemonSet, error) {
	if c.Client == nil || c.WorkerNamespace == "" || c.WorkerID == "" || c.NodeName == "" || c.VendorScope == "" {
		return nil, nil, errSourceIneligible
	}
	pods, err := c.Client.CoreV1().Pods(c.WorkerNamespace).List(ctx, meta.ListOptions{LabelSelector: SourceWorkerLabel.String()})
	if err != nil {
		return nil, nil, err
	}
	node, err := c.Client.CoreV1().Nodes().Get(ctx, c.NodeName, meta.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	if modelstore.NodeSourceManufacturer(node) != c.VendorScope {
		return nil, nil, errSourceIneligible
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if string(pod.UID) != c.WorkerID || pod.Spec.NodeName != c.NodeName ||
			pod.Labels["gpustack.ai/manufacturer"] != c.VendorScope || pod.DeletionTimestamp != nil ||
			pod.Status.Phase == core.PodFailed || pod.Status.Phase == core.PodSucceeded {
			continue
		}
		owner := meta.GetControllerOf(pod)
		if owner == nil || owner.Kind != "DaemonSet" || owner.APIVersion != "apps/v1" || owner.UID == "" {
			continue
		}
		ds, err := c.Client.AppsV1().DaemonSets(c.WorkerNamespace).Get(ctx, owner.Name, meta.GetOptions{})
		if err != nil {
			return nil, nil, err
		}
		if ds.UID != owner.UID || ds.DeletionTimestamp != nil ||
			ds.Labels["app.kubernetes.io/component"] != SourceWorkerComponent ||
			ds.Labels["gpustack.ai/manufacturer"] != c.VendorScope {
			continue
		}
		return pod, ds, nil
	}
	return nil, nil, errSourceIneligible
}

func (c *Config) sourcePod(ctx context.Context) error {
	pod, ds, err := c.localSourcePodAndDaemonSet(ctx)
	if err != nil {
		return err
	}
	if pod.Annotations["model-transfer.gpustack.ai/drain-for"] != "" {
		return errSourceIneligible
	}
	var desiredImage string
	for _, container := range ds.Spec.Template.Spec.Containers {
		if container.Name == "worker" {
			desiredImage = container.Image
			break
		}
	}
	if desiredImage == "" {
		return errSourceIneligible
	}
	matches := 0
	for _, container := range pod.Spec.Containers {
		if container.Name == "worker" && container.Image == desiredImage {
			matches++
		}
	}
	if matches != 1 {
		return errSourceIneligible
	}
	return nil
}

func (c *Config) metadataHandler() http.Handler {
	c.metadataOnce.Do(func() { c.metadataSlots = make(chan struct{}, 4) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			PodNamespace   string   `json:"podNamespace"`
			PodName        string   `json:"podName"`
			PodUID         string   `json:"podUID"`
			ArtifactName   string   `json:"artifactName"`
			ArtifactUID    string   `json:"artifactUID"`
			ManifestDigest string   `json:"manifestDigest"`
			SourcePodUID   string   `json:"sourcePodUID"`
			Required       []string `json:"required"`
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil || uniqueTopLevelJSON(raw) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.SourcePodUID == "" || req.SourcePodUID != c.WorkerID {
			http.Error(w, "source identity mismatch", http.StatusConflict)
			return
		}
		select {
		case c.metadataSlots <- struct{}{}:
			defer func() { <-c.metadataSlots }()
		default:
			http.Error(w, "metadata capacity unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := c.sourcePod(r.Context()); err != nil {
			http.Error(w, "source unavailable", http.StatusServiceUnavailable)
			return
		}
		auth, ok := c.Authorizer.(metadataAuthorizer)
		if !ok {
			http.Error(w, "source unavailable", http.StatusServiceUnavailable)
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer == r.Header.Get("Authorization") {
			bearer = ""
		}
		verdict := auth.AuthorizeMetadata(r.Context(), authorization.Request{
			Token: bearer, PodNamespace: req.PodNamespace, PodName: req.PodName, PodUID: req.PodUID,
			ArtifactName: req.ArtifactName, ArtifactUID: req.ArtifactUID, ManifestDigest: req.ManifestDigest,
		}, c.NodeName)
		if verdict != nil {
			http.Error(w, "metadata access denied", http.StatusForbidden)
			return
		}
		if err := metadata.ValidateRequired(req.Required); err != nil {
			http.Error(w, "invalid metadata request", http.StatusBadRequest)
			return
		}
		hexDigest := store.HexOf(req.ManifestDigest)
		if hexDigest == "" || c.Store == nil {
			http.Error(w, "source unavailable", http.StatusServiceUnavailable)
			return
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			http.Error(w, "source unavailable", http.StatusServiceUnavailable)
			return
		}
		ref := metadataPrefix(c.WorkerID, c.VendorScope) + hex.EncodeToString(id[:])
		claimed, err := c.Store.Claim(r.Context(), store.Ref{VolumeID: ref, Hex: hexDigest, PodUID: c.WorkerID})
		if err != nil || !claimed {
			http.Error(w, "source unavailable", http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = c.Store.RemoveRef(ref) }()
		view, err := metadata.EnsureView(c.Store, filepath.Join(c.Store.Root(), "model-transfer", "views"), hexDigest, req.ManifestDigest, req.Required)
		if err != nil {
			http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		tempDir := filepath.Join(c.Store.Root(), "model-transfer", "metadata-temp")
		if err := os.MkdirAll(tempDir, 0o700); err != nil {
			http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		archive, err := os.CreateTemp(tempDir, metadataPrefix(c.WorkerID, c.VendorScope))
		if err != nil {
			http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
		if err := metadata.ExportView(r.Context(), view, archive); err != nil {
			http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-GPUStack-Source-Pod-UID", c.WorkerID)
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = io.Copy(w, archive)
	})
}
