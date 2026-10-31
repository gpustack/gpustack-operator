package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

type manifestPageRequest struct {
	PodNamespace     string `json:"podNamespace"`
	PodName          string `json:"podName"`
	PodUID           string `json:"podUID"`
	SessionID        string `json:"sessionID"`
	TargetInstanceID string `json:"targetInstanceID"`
	SourcePodUID     string `json:"sourcePodUID"`
	WorkerInstanceID string `json:"workerInstanceID"`
	Generation       string `json:"generation"`
	ManifestSHA256   string `json:"manifestSHA256"`
	PageSHA256       string `json:"pageSHA256"`
}

func (b *Broker) manifestPageLocked(ctx context.Context, rec *GenerationRecord, rootDigest, pageDigest string) ([]byte, error) {
	if rec.ManifestSHA256 != rootDigest || rec.ManifestPagesPath == "" {
		return nil, fmt.Errorf("manifest identity mismatch")
	}
	cached, err := b.cachedDerivedManifestContextLocked(ctx, rec)
	if err != nil {
		return nil, err
	}
	for _, page := range cached.manifest.TensorPages {
		if page.SHA256 != pageDigest {
			continue
		}
		raw, err := protocol.LocalPageReader(rec.ManifestPagesPath)(ctx, page)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		if len(raw) != page.Size || hex.EncodeToString(sum[:]) != page.SHA256 {
			return nil, fmt.Errorf("manifest page changed")
		}
		return raw, nil
	}
	return nil, fmt.Errorf("page is not bound by this manifest")
}

func (b *Broker) workerManifestPage(ctx context.Context, token string, req manifestPageRequest) ([]byte, error) {
	if !b.bootstrapMatches(token) {
		return nil, protocol.New(protocol.KindAuthentication, "page", "worker authentication failed")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.records[req.Generation]
	if rec == nil || !liveAssignment(rec) || b.assignmentsFrozen {
		return nil, fmt.Errorf("source assignment is unavailable")
	}
	return b.manifestPageLocked(ctx, rec, req.ManifestSHA256, req.PageSHA256)
}

func (b *Broker) sessionManifestPage(ctx context.Context, req manifestPageRequest, pod authorization.PodIdentity) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, rec, err := b.sessionOwnerLocked(TransferRequest{
		PodNamespace: req.PodNamespace, PodName: req.PodName, PodUID: req.PodUID,
		SessionID: req.SessionID, TargetInstanceID: req.TargetInstanceID, SourcePodUID: req.SourcePodUID,
		WorkerInstanceID: req.WorkerInstanceID, Generation: req.Generation,
	}, pod)
	if err != nil {
		return nil, err
	}
	if b.intentValidator == nil {
		return nil, fmt.Errorf("live source entitlement validator is unavailable")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.intentValidator(checkCtx, LiveIntent{
		Namespace: rec.Namespace, IntentName: rec.IntentName, IntentUID: rec.IntentUID, IntentGeneration: rec.IntentGeneration,
		BindingName: rec.BindingName, BindingUID: rec.BindingUID, ArtifactName: rec.ArtifactName, ArtifactUID: rec.ArtifactUID,
		SourceGeneration: rec.SourceGeneration, Digest: rec.Digest, NodeName: rec.NodeName, WorkerID: rec.WorkerID,
	}); err != nil {
		return nil, fmt.Errorf("live source entitlement is unavailable")
	}
	return b.manifestPageLocked(ctx, rec, req.ManifestSHA256, req.PageSHA256)
}

// registerManifestPages limits concurrent page encodings as well as each page.
// The worker route is loopback-only; the session route requires a live Pod token.
func registerManifestPages(mux *http.ServeMux, b *Broker, authorizer SessionAuthorizer, worker bool) {
	slots := make(chan struct{}, 2)
	mux.HandleFunc("/v1/manifest-pages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-r.Context().Done():
			return
		}
		var req manifestPageRequest
		if !decode(w, r, &req) {
			return
		}
		var raw []byte
		var err error
		if worker {
			raw, err = b.workerManifestPage(r.Context(), r.Header.Get("Authorization"), req)
		} else {
			if authorizer == nil {
				http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
				return
			}
			var pod authorization.PodIdentity
			var authErr *protocol.Error
			pod, authErr = authorizer.AuthenticateActivePod(r.Context(), authorization.Request{
				Token: sessionBearer(r), PodNamespace: req.PodNamespace, PodName: req.PodName, PodUID: req.PodUID,
			})
			if refuse(w, authErr) {
				return
			}
			raw, err = b.sessionManifestPage(r.Context(), req, pod)
		}
		if err != nil {
			refuseTransfer(w, err)
			return
		}
		answer(w, map[string]any{"manifestSHA256": req.ManifestSHA256, "pageSHA256": req.PageSHA256, "pageJSON": string(raw)})
	})
}
