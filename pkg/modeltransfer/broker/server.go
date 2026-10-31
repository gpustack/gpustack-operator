package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// The control service is the local endpoint the transport workers report to:
// registration, health heartbeats, fill completion, drain acknowledgements,
// and quarantine reports. Every message names one generation and carries the
// worker's secret; a foreign worker, a wrong secret, or a generation this
// node does not hold is refused with the protocol kind that says which.
//
// Answers carry no secret, no tenant name, and no pointer: the wire carries
// the verdict and the lease, nothing else.
func NewControlHandler(b *Broker) http.Handler {
	return NewControlHandlerWithInstanceHook(b, nil)
}

// NewControlHandlerWithInstanceHook records the native boot identity before
// acknowledging registration or native start.
func NewControlHandlerWithInstanceHook(b *Broker, hook func(context.Context, string, string, bool) error) http.Handler {
	mux := http.NewServeMux()
	registerManifestPages(mux, b, nil, true)
	mux.HandleFunc("/v1/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			WorkerID         string `json:"workerID"`
			WorkerInstanceID string `json:"workerInstanceID"`
			Generation       string `json:"generation"`
			Secret           string `json:"secret"`
			LeaseID          string `json:"leaseID"`
		}
		if !decode(w, r, &req) {
			return
		}
		lease, err := b.RegisterWorker(req.Generation, req.WorkerID, req.Secret, req.WorkerInstanceID)
		if refuse(w, err) {
			return
		}
		if hook != nil {
			if err := hook(r.Context(), req.WorkerID, req.WorkerInstanceID, false); err != nil {
				klog.ErrorS(err, "record model-transfer worker registration")
				refuse(w, protocol.New(protocol.KindSourceUnavailable, "worker", "the worker instance could not be recorded"))
				return
			}
		}
		answer(w, map[string]any{"leaseID": lease.ID, "expiresAt": lease.Expires})
	})
	mux.HandleFunc("/v1/native-start", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			WorkerID         string `json:"workerID"`
			WorkerInstanceID string `json:"workerInstanceID"`
			Generation       string `json:"generation"`
			Secret           string `json:"secret"`
			LeaseID          string `json:"leaseID"`
		}
		if !decode(w, r, &req) {
			return
		}
		if refuse(w, b.ValidateNativeStart(req.Generation, req.WorkerID, req.LeaseID, req.Secret, req.WorkerInstanceID)) {
			return
		}
		if hook != nil {
			if err := hook(r.Context(), req.WorkerID, req.WorkerInstanceID, true); err != nil {
				klog.ErrorS(err, "record model-transfer native worker instance")
				refuse(w, protocol.New(protocol.KindSourceUnavailable, "worker", "the worker instance could not be recorded"))
				return
			}
		}
		if refuse(w, b.NativeStart(req.Generation, req.WorkerID, req.LeaseID, req.Secret, req.WorkerInstanceID)) {
			return
		}
		answer(w, map[string]any{})
	})
	mux.HandleFunc("/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			WorkerID         string `json:"workerID"`
			WorkerInstanceID string `json:"workerInstanceID"`
			Generation       string `json:"generation"`
			LeaseID          string `json:"leaseID"`
			Secret           string `json:"secret"`
			ResidentBytes    int64  `json:"residentBytes"`
			Registrations    int32  `json:"registrations"`
			ActiveSessions   int32  `json:"activeSessions"`
			UnknownSessions  int32  `json:"unknownSessions"`
			NativeOperations int32  `json:"nativeOperations"`
			DrainRequested   bool   `json:"drainRequested"`
		}
		if !decode(w, r, &req) {
			return
		}
		result, err := b.Heartbeat(req.Generation, req.WorkerID, req.LeaseID,
			req.Secret, Health{
				ResidentBytes: req.ResidentBytes, Registrations: req.Registrations,
				ActiveSessions: req.ActiveSessions, DrainRequested: req.DrainRequested,
			}, req.WorkerInstanceID)
		if refuse(w, err) {
			return
		}
		answer(w, map[string]any{
			"action": result.Action, "expiresAt": result.Lease.Expires,
			"activeSessions": result.ActiveSessions, "unknownSessions": result.UnknownSessions,
		})
	})
	mux.HandleFunc("/v1/filled", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			WorkerID         string                   `json:"workerID"`
			WorkerInstanceID string                   `json:"workerInstanceID"`
			Generation       string                   `json:"generation"`
			LeaseID          string                   `json:"leaseID"`
			Secret           string                   `json:"secret"`
			Registrations    int32                    `json:"registrations"`
			Descriptors      []RegistrationDescriptor `json:"descriptors"`
		}
		if !decode(w, r, &req) {
			return
		}
		if refuse(w, b.FillVerified(req.Generation, req.WorkerID, req.LeaseID,
			req.Secret, req.Registrations, req.Descriptors, req.WorkerInstanceID)) {
			return
		}
		answer(w, map[string]any{})
	})
	mux.HandleFunc("/v1/assignments", func(w http.ResponseWriter, r *http.Request) {
		// The loopback work order: the bearer is the Pod-local bootstrap
		// credential from the shared emptyDir file, never a generation
		// secret and never a remote caller's token.
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		assignments, err := b.AssignmentsFor(r.Header.Get("Authorization"))
		if refuse(w, err) {
			return
		}
		answer(w, map[string]any{"assignments": assignments})
	})
	mux.HandleFunc("/v1/drain-ack", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			WorkerID         string `json:"workerID"`
			WorkerInstanceID string `json:"workerInstanceID"`
			Generation       string `json:"generation"`
			LeaseID          string `json:"leaseID"`
			Secret           string `json:"secret"`
			NativeReleased   bool   `json:"nativeReleased"`
		}
		if !decode(w, r, &req) {
			return
		}
		if !req.NativeReleased {
			refuse(w, protocol.New(protocol.KindOperationState, "drain", "native release is required"))
			return
		}
		if refuse(w, b.DrainAckContext(r.Context(), req.Generation, req.WorkerID, req.LeaseID,
			req.Secret, req.WorkerInstanceID)) {
			return
		}
		answer(w, map[string]any{})
	})
	mux.HandleFunc("/v1/quarantine", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			WorkerID         string `json:"workerID"`
			WorkerInstanceID string `json:"workerInstanceID"`
			Generation       string `json:"generation"`
			LeaseID          string `json:"leaseID"`
			Secret           string `json:"secret"`
			Reason           string `json:"reason"`
		}
		if !decode(w, r, &req) {
			return
		}
		if refuse(w, b.Quarantine(req.Generation, req.WorkerID, req.LeaseID,
			req.Secret, req.Reason, req.WorkerInstanceID)) {
			return
		}
		answer(w, map[string]any{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown control path", http.StatusNotFound)
	})
	return mux
}

// maxControlBody bounds every control and session message: a worker or
// engine never needs more, and an unbounded body is an unbounded read.
const maxControlBody = 1 << 20

// sessionRetryAfterSeconds is the wait a capacity-exhausted session refusal
// names: the node is busy, not broken, so the client asks again after it,
// or on another source endpoint that serves the same digest.
const sessionRetryAfterSeconds = 5

func decode(w http.ResponseWriter, r *http.Request, req any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return false
	}
	return decodeBody(w, r, req)
}

func decodeBody(w http.ResponseWriter, r *http.Request, req any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil || rejectDuplicateJSONKeys(string(raw)) != nil {
		http.Error(w, "malformed control message", http.StatusBadRequest)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(req); err != nil {
		http.Error(w, "malformed control message", http.StatusBadRequest)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		http.Error(w, "malformed control message", http.StatusBadRequest)
		return false
	}
	return true
}

// refuse writes a typed refusal: an authentication failure is forbidden, any
// other protocol verdict is a conflict the worker may retry or report. The
// reason is the protocol error's own text, which carries no secret.
func refuse(w http.ResponseWriter, err *protocol.Error) bool {
	if err == nil {
		return false
	}
	status := http.StatusConflict
	if err.Kind == protocol.KindAuthentication {
		status = http.StatusForbidden
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"kind":   string(err.Kind),
		"reason": err.Error(),
	})
	return true
}

// refuseTransfer keeps unobserved source operations retriable. A typed protocol
// refusal carries its kind and reason; other failures stay plain conflicts.
func refuseTransfer(w http.ResponseWriter, err error) {
	var observationErr *privateObservationError
	if errors.As(err, &observationErr) {
		http.Error(w, "transfer observation unavailable", http.StatusServiceUnavailable)
		return
	}
	var protocolErr *protocol.Error
	if errors.As(err, &protocolErr) {
		refuse(w, protocolErr)
		return
	}
	http.Error(w, "transfer unavailable", http.StatusConflict)
}

func answer(w http.ResponseWriter, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("encode answer: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(raw, '\n'))
}

// SessionAuthorizer validates one engine's claimed workload identity
// against the live cluster: the projected token's review, the live Pod,
// and the artifact binding. The authorization package owns the rules;
// this interface exists so the handler stays testable without a cluster.
type SessionAuthorizer interface {
	AuthorizeSession(ctx context.Context, req authorization.Request) (
		authorization.Result, *protocol.Error,
	)
	AuthenticatePod(ctx context.Context, req authorization.Request) (
		authorization.PodIdentity, *protocol.Error,
	)
	AuthenticateActivePod(ctx context.Context, req authorization.Request) (
		authorization.PodIdentity, *protocol.Error,
	)
}

// NewSessionHandler is the engine-facing endpoint, served over TLS only
// and never muxed with the worker control service. A session opens under
// the caller's own authenticated identity and receives the verified portable
// manifest. Releasing requires the session's exact owner and target instance.
func NewSessionHandler(b *Broker, authorizer SessionAuthorizer) http.Handler {
	mux := http.NewServeMux()
	registerManifestPages(mux, b, authorizer, false)
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			RequestID        string `json:"requestID"`
			TargetInstanceID string `json:"targetInstanceID"`
			PodNamespace     string `json:"podNamespace"`
			PodName          string `json:"podName"`
			PodUID           string `json:"podUID"`
			ArtifactName     string `json:"artifactName"`
			ArtifactUID      string `json:"artifactUID"`
			ManifestDigest   string `json:"manifestDigest"`
			SourceGeneration int64  `json:"sourceGeneration"`
			Tensor           string `json:"tensor"`
			Offset           int64  `json:"offset"`
			Length           int64  `json:"length"`
		}
		if !decode(w, r, &req) {
			return
		}
		if !sessionNoncePattern.MatchString(req.RequestID) || !sessionNoncePattern.MatchString(req.TargetInstanceID) {
			http.Error(w, "invalid session request identity", http.StatusBadRequest)
			return
		}
		// The bearer is the Pod's projected token, minted for the
		// gpustack-model-transfer audience; the authorizer's review is
		// what proves the audience, the live Pod, and the artifact.
		if authorizer == nil {
			refuse(w, protocol.New(protocol.KindSourceUnavailable, "session", "no authorizer is configured"))
			return
		}
		bearer := sessionBearer(r)
		raw, marshalErr := json.Marshal(req)
		if marshalErr != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		fingerprint := sha256.Sum256(raw)
		refusalOwner := SessionOwner{
			PodUID: req.PodUID, Namespace: req.PodNamespace, RequestID: req.RequestID,
			TargetInstanceID: req.TargetInstanceID, OpenFingerprint: hex.EncodeToString(fingerprint[:]),
		}
		refuseUnadmitted := func(reason *protocol.Error) bool {
			pod, authErr := authorizer.AuthenticatePod(r.Context(), authorization.Request{
				Token:        bearer,
				PodNamespace: req.PodNamespace, PodName: req.PodName, PodUID: req.PodUID,
			})
			if authErr != nil || pod.UID != req.PodUID || b == nil || !b.RecordUnadmitted(refusalOwner) {
				return false
			}
			payload := map[string]any{
				"requestID": req.RequestID, "admission": "not-created",
				"kind": string(reason.Kind), "reason": reason.Error(),
			}
			if reason.Kind == protocol.KindCapacityExhausted {
				payload["retryAfter"] = sessionRetryAfterSeconds
				w.Header().Set("Retry-After", strconv.Itoa(sessionRetryAfterSeconds))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(payload)
			return true
		}
		result, err := authorizer.AuthorizeSession(r.Context(), authorization.Request{
			Token:            bearer,
			PodNamespace:     req.PodNamespace,
			PodName:          req.PodName,
			PodUID:           req.PodUID,
			ArtifactName:     req.ArtifactName,
			ArtifactUID:      req.ArtifactUID,
			ManifestDigest:   req.ManifestDigest,
			SourceGeneration: req.SourceGeneration,
			Tensor:           req.Tensor,
			Offset:           req.Offset,
			Length:           req.Length,
		})
		if err != nil && err.Kind == protocol.KindSourceUnavailable && refuseUnadmitted(err) {
			return
		}
		if refuse(w, err) {
			return
		}
		generation, gerr := b.GenerationForArtifact(req.PodNamespace, "sha256:"+result.DigestHex, req.ArtifactUID, result.SourceGeneration)
		if gerr != nil && refuseUnadmitted(gerr) {
			return
		}
		if refuse(w, gerr) {
			return
		}
		owner := brokerSessionOwner(req.ArtifactUID, result)
		owner.RequestID, owner.TargetInstanceID, owner.OpenFingerprint = req.RequestID, req.TargetInstanceID, refusalOwner.OpenFingerprint
		session, serr := b.AdmitSessionContext(r.Context(), owner, req.PodNamespace, generation)
		if serr != nil && (serr.Kind == protocol.KindSourceUnavailable || serr.Kind == protocol.KindCapacityExhausted) && refuseUnadmitted(serr) {
			return
		}
		if refuse(w, serr) {
			return
		}
		view, verr := b.SessionView(session.ID)
		if verr != nil {
			// Another replay may already have exposed this same session.
			// Keep its charge until the exact owner closes it.
			refuse(w, verr)
			return
		}
		opened := map[string]any{
			"sessionID": session.ID, "requestID": req.RequestID, "generation": view.Generation,
			"digest": view.Digest, "artifactUID": view.ArtifactUID, "sourceGeneration": view.SourceGeneration,
			"manifestSHA256": view.ManifestSHA256, "manifestJSON": view.ManifestJSON,
			"sourcePodUID": view.SourcePodUID, "workerInstanceID": view.WorkerInstanceID,
		}
		// The window this node can actually back right now, so a client
		// whose own budget is larger clamps to it instead of submitting
		// transfers the source buffer cannot serve.
		if window, ok := b.SourceWindowBytes(); ok && window > 0 {
			opened["sourceWindowBytes"] = window
		}
		answer(w, opened)
	})
	mux.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		closing := strings.HasSuffix(r.URL.Path, "/closing")
		if (closing && r.Method != http.MethodPost) || (!closing && r.Method != http.MethodDelete) {
			http.Error(w, "invalid session method", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			PodNamespace     string `json:"podNamespace"`
			PodName          string `json:"podName"`
			PodUID           string `json:"podUID"`
			ArtifactUID      string `json:"artifactUID"`
			ManifestDigest   string `json:"manifestDigest"`
			TargetInstanceID string `json:"targetInstanceID"`
			RequestID        string `json:"requestID"`
			TargetReleased   bool   `json:"targetReleased"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		if closing {
			sessionID = strings.TrimSuffix(sessionID, "/closing")
		}
		if sessionID == "" || strings.Contains(sessionID, "/") {
			http.Error(w, "a session id is required", http.StatusBadRequest)
			return
		}
		// Closing grants no further content access. Authenticate the Pod and
		// match its persisted owner even when the artifact has been removed.
		if authorizer == nil {
			refuse(w, protocol.New(protocol.KindSourceUnavailable, "session", "no authorizer is configured"))
			return
		}
		pod, err := authorizer.AuthenticatePod(r.Context(), authorization.Request{
			Token:        sessionBearer(r),
			PodNamespace: req.PodNamespace,
			PodName:      req.PodName,
			PodUID:       req.PodUID,
		})
		if refuse(w, err) {
			return
		}
		owner := SessionOwner{
			Namespace: pod.Namespace, PodUID: pod.UID,
			ArtifactUID: req.ArtifactUID, Digest: req.ManifestDigest,
			TargetInstanceID: req.TargetInstanceID, CloseRequestID: req.RequestID, TargetReleased: req.TargetReleased,
		}
		if closing {
			if !sessionNoncePattern.MatchString(req.RequestID) || !sessionNoncePattern.MatchString(req.TargetInstanceID) {
				http.Error(w, "invalid close request identity", http.StatusBadRequest)
				return
			}
			closeErr := b.ClosePrivateSession(r.Context(), owner, sessionID)
			if closeErr != nil && closeErr.Kind == protocol.KindSourceUnavailable {
				http.Error(w, "source close pending", http.StatusServiceUnavailable)
				return
			}
			if refuse(w, closeErr) {
				return
			}
			answer(w, map[string]any{"state": "closing"})
			return
		}
		if refuse(w, b.ReleaseSessionContext(r.Context(), owner, sessionID)) {
			return
		}
		answer(w, map[string]any{"state": "closed"})
	})
	mux.HandleFunc("/v1/transfers", func(w http.ResponseWriter, r *http.Request) {
		var req TransferRequest
		if !decode(w, r, &req) {
			return
		}
		if b == nil || authorizer == nil {
			http.Error(w, "source unavailable", http.StatusServiceUnavailable)
			return
		}
		entitlement, err := b.TransferSession(req.SessionID, req.PodUID, req.TargetInstanceID)
		if err != nil || entitlement.Namespace != req.PodNamespace {
			http.Error(w, "session unavailable", http.StatusConflict)
			return
		}
		result, authErr := authorizer.AuthorizeSession(r.Context(), authorization.Request{
			Token: sessionBearer(r), PodNamespace: req.PodNamespace, PodName: req.PodName, PodUID: req.PodUID,
			ArtifactName: entitlement.ArtifactName, ArtifactUID: entitlement.ArtifactUID,
			ManifestDigest: entitlement.Digest, SourceGeneration: entitlement.SourceGeneration,
			Tensor: req.Tensor, Offset: req.TensorOffset, Length: req.Length,
		})
		if refuse(w, authErr) {
			return
		}
		operation, err := b.SubmitTransfer(r.Context(), req, result.Pod)
		if err != nil {
			refuseTransfer(w, err)
			return
		}
		answer(w, operationView(operation))
	})
	mux.HandleFunc("/v1/transfers/", func(w http.ResponseWriter, r *http.Request) {
		if b == nil || authorizer == nil {
			http.Error(w, "source unavailable", http.StatusServiceUnavailable)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1/transfers/")
		ack := strings.HasSuffix(path, "/ack")
		id := strings.TrimSuffix(path, "/ack")
		if !sessionNoncePattern.MatchString(id) || (ack && r.Method != http.MethodPost) || (!ack && r.Method != http.MethodGet) {
			http.Error(w, "invalid transfer request", http.StatusBadRequest)
			return
		}
		var req TransferRequest
		if ack {
			var body struct {
				SessionID        string `json:"sessionID"`
				PodNamespace     string `json:"podNamespace"`
				PodName          string `json:"podName"`
				PodUID           string `json:"podUID"`
				TargetInstanceID string `json:"targetInstanceID"`
				RegistrationID   string `json:"registrationID"`
				WindowID         string `json:"windowID"`
			}
			if !decode(w, r, &body) {
				return
			}
			req = TransferRequest{
				SessionID: body.SessionID, PodNamespace: body.PodNamespace, PodName: body.PodName,
				PodUID: body.PodUID, TargetInstanceID: body.TargetInstanceID, RegistrationID: body.RegistrationID, WindowID: body.WindowID,
			}
		} else {
			query := r.URL.Query()
			req = TransferRequest{
				SessionID: query.Get("sessionID"), PodNamespace: query.Get("podNamespace"), PodName: query.Get("podName"),
				PodUID: query.Get("podUID"), TargetInstanceID: query.Get("targetInstanceID"),
			}
		}
		identity, err := b.OperationIdentity(id, req.SessionID, req.PodUID, req.TargetInstanceID)
		if err != nil || identity.Namespace != req.PodNamespace || identity.Name != req.PodName {
			http.Error(w, "transfer unavailable", http.StatusConflict)
			return
		}
		pod, authErr := authorizer.AuthenticatePod(r.Context(), authorization.Request{
			Token: sessionBearer(r), PodNamespace: identity.Namespace, PodName: identity.Name, PodUID: identity.UID,
		})
		if refuse(w, authErr) {
			return
		}
		var operation OperationRecord
		if ack {
			operation, err = b.AcknowledgeTransfer(r.Context(), id, req, pod)
		} else {
			operation, err = b.OperationStatus(r.Context(), id, req.SessionID, req.PodUID, req.TargetInstanceID)
		}
		if err != nil {
			refuseTransfer(w, err)
			return
		}
		answer(w, operationView(operation))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown session path", http.StatusNotFound)
	})
	return mux
}

var sessionNoncePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func sessionBearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) != token {
		return ""
	}
	return token
}

// brokerSessionOwner binds the request's claimed identity to the
// authorizer's verified answer.
func brokerSessionOwner(artifactUID string, result authorization.Result) SessionOwner {
	return SessionOwner{
		PodUID: result.Pod.UID, Namespace: result.Pod.Namespace,
		ArtifactUID: artifactUID, Digest: "sha256:" + result.DigestHex,
	}
}

// NewSessionTLSServer serves the engine session handler over TLS 1.2+ with
// the chart-managed serving certificate: authenticated server provenance
// is mandatory, and no plaintext session or descriptor exchange exists.
// Serve the returned server through its TLS listener.
func NewSessionTLSServer(addr string, handler http.Handler, certFile, keyFile string) (
	*http.Server, error,
) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load the session serving certificate: %w", err)
	}
	return &http.Server{
		Addr: addr,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{pair},
		},
		Handler:           handler,
		ErrorLog:          klog.NewStandardLogger("WARNING"),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}, nil
}
