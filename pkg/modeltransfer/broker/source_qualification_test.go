package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// qualifyTestSource opens the source gate for one test broker.
//
// The seam is the broker's unexported qualified state, and only a test file
// in this package writes it. The production broker never does, so the gate is
// closed there.
func qualifyTestSource(t *testing.T, b *Broker) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sourceQualified = true
}

// closeTestSource closes the source gate for one test broker. The source
// holds no acceptance record then, which is the production answer.
func closeTestSource(t *testing.T, b *Broker) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sourceQualified = false
}

// gateSource answers every private path the broker uses and records the
// paths it was asked for, so a test can see that no native operation was
// posted.
type gateSource struct {
	mu    sync.Mutex
	paths []string
}

func (s *gateSource) Call(_ context.Context, path string, payload any) (PrivateReply, error) {
	s.mu.Lock()
	s.paths = append(s.paths, path)
	s.mu.Unlock()
	body, _ := payload.(map[string]any)
	id, _ := body["requestID"].(string)
	switch path {
	case "/v1/operations":
		return PrivateReply{RequestID: id, State: "submitted"}, nil
	case "/v1/operations/status":
		return PrivateReply{RequestID: id, State: "terminal", Verdict: "succeeded"}, nil
	case "/v1/operations/ack":
		return PrivateReply{RequestID: id, State: "acknowledged", Verdict: "succeeded"}, nil
	case "/v1/sessions/close":
		return PrivateReply{State: "closed"}, nil
	default:
		return PrivateReply{}, nil
	}
}

func (s *gateSource) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, seen := range s.paths {
		if seen == path {
			total++
		}
	}
	return total
}

// gateFixture builds one serving intent generation on the nixl backend. It
// returns the broker, the generation, the session owner and the private
// source the test observes.
func gateFixture(t *testing.T) (*Broker, string, SessionOwner, *gateSource) {
	t.Helper()
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	private := &gateSource{}
	b.SetPrivateSource(private)
	manifest := protocol.Manifest{
		ContractVersion: protocol.ContractVersion, ArtifactUID: "artifact", SourceGeneration: 1,
		Files: []protocol.FileDescriptor{{Path: "weight.safetensors", Size: 12}},
		Tensors: []protocol.TensorDescriptor{{
			Name: "weight", File: "weight.safetensors", Dtype: "U8",
			Shape: []int64{8}, DataBegin: 2, DataEnd: 10,
		}},
	}
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	intent := Intent{
		Key: "gate-intent", Namespace: "tenant", Digest: digest, TotalBytes: 12,
		Grant: grant("tenant", 100), WorkerID: "source", NodeName: "node", IntentUID: "prefetch",
		IntentGeneration: 1, IntentName: "prefetch", ArtifactName: "model", ArtifactUID: "artifact",
		SourceGeneration: 1, BindingUID: "binding-uid-1", BindingName: "binding",
		ReservationID: "gate-reservation", Registrations: 1, Backend: "nixl",
		ManifestJSON: string(raw), ManifestSHA256: hex.EncodeToString(sum[:]),
	}
	assignment, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	instance := strings.Repeat("a", 32)
	lease, perr := b.RegisterWorker(assignment.Generation, "source", assignment.WorkerSecret, instance)
	require.Nil(t, perr)
	require.Nil(t, b.NativeStart(assignment.Generation, "source", lease.ID, assignment.WorkerSecret, instance))
	require.Nil(t, b.FillVerified(t.Context(), assignment.Generation, "source", lease.ID, assignment.WorkerSecret, 1,
		[]RegistrationDescriptor{{
			Name: "weight.safetensors", Addr: 1, Length: 12,
			MemoryType: "HOST", Backend: "nixl", WorkerUID: "source",
		}}, instance))
	owner := SessionOwner{
		Namespace: "tenant", PodUID: "owner", ArtifactUID: "artifact", Digest: digest,
		RequestID: strings.Repeat("c", 32), TargetInstanceID: strings.Repeat("b", 32),
	}
	return b, assignment.Generation, owner, private
}

// gateTransferRequest is one native transfer over the fixture's session.
func gateTransferRequest(sessionID, generation, instance string) TransferRequest {
	return TransferRequest{
		RequestID: strings.Repeat("e", 32), SessionID: sessionID,
		PodNamespace: "tenant", PodName: "engine", PodUID: "owner",
		TargetInstanceID: strings.Repeat("b", 32), SourcePodUID: "source",
		WorkerInstanceID: instance, Generation: generation,
		RegistrationID: strings.Repeat("f", 32), WindowID: strings.Repeat("1", 32),
		Tensor: "weight", TensorOffset: 2, Length: 4,
		Target: TransferTarget{Backend: "nixl", Address: 100, Length: 4, Metadata: "YQ=="},
	}
}

func gatePod() authorization.PodIdentity {
	return authorization.PodIdentity{Namespace: "tenant", Name: "engine", UID: "owner", IP: "127.0.0.1"}
}

// gateAuthorizer proves the caller's identity. The gate is the broker's own
// decision, so the authorization answer stays out of it.
type gateAuthorizer struct{ digest string }

func (a gateAuthorizer) AuthorizeSession(_ context.Context, req authorization.Request) (authorization.Result, *protocol.Error) {
	return authorization.Result{
		Pod:       authorization.PodIdentity{Namespace: req.PodNamespace, Name: req.PodName, UID: req.PodUID},
		DigestHex: store.HexOf(a.digest), SourceGeneration: 1,
	}, nil
}

func (a gateAuthorizer) AuthenticatePod(_ context.Context, req authorization.Request) (authorization.PodIdentity, *protocol.Error) {
	return authorization.PodIdentity{Namespace: req.PodNamespace, Name: req.PodName, UID: req.PodUID}, nil
}

// TestSourceGateRefusesNewSessionWhileNoBackendIsQualified states the
// production answer: no backend is qualified, so the broker refuses a new
// remote session and records nothing.
func TestSourceGateRefusesNewSessionWhileNoBackendIsQualified(t *testing.T) {
	b, generation, owner, _ := gateFixture(t)
	closeTestSource(t, b)
	session, perr := b.AdmitSession(context.Background(), owner, "tenant", generation)
	require.NotNil(t, perr, "a broker with no qualified backend must refuse a new remote session")
	assert.Equal(t, protocol.KindSourceUnavailable, perr.Kind)
	assert.Nil(t, session)
	b.mu.Lock()
	defer b.mu.Unlock()
	assert.Empty(t, b.sessions, "a refused session must leave no session record")
}

// TestSourceGateKeepsAdmittedSessionAndRefusesNewOne shows the gate's reach:
// it stops new remote work and leaves the work already exposed alone.
func TestSourceGateKeepsAdmittedSessionAndRefusesNewOne(t *testing.T) {
	b, generation, owner, _ := gateFixture(t)
	qualifyTestSource(t, b)
	session, perr := b.AdmitSession(context.Background(), owner, "tenant", generation)
	require.Nil(t, perr)
	closeTestSource(t, b)

	other := owner
	other.PodUID, other.RequestID = "other-owner", strings.Repeat("d", 32)
	refused, perr := b.AdmitSession(context.Background(), other, "tenant", generation)
	require.NotNil(t, perr)
	assert.Equal(t, protocol.KindSourceUnavailable, perr.Kind)
	assert.Nil(t, refused)

	view, verr := b.SessionView(session.ID)
	require.Nil(t, verr, "an admitted session keeps its descriptors while the gate is closed")
	assert.NotEmpty(t, view.Descriptors)

	closeOwner := owner
	closeOwner.CloseRequestID = strings.Repeat("9", 32)
	require.Nil(t, b.ClosePrivateSession(context.Background(), closeOwner, session.ID))
	closeOwner.TargetReleased = true
	require.Nil(t, b.ReleaseSession(context.Background(), closeOwner, session.ID))
}

// TestSourceGateRefusesSessionOverHTTPWithoutDescriptors shows what a remote
// caller receives: a refusal that carries no manifest and no descriptor.
func TestSourceGateRefusesSessionOverHTTPWithoutDescriptors(t *testing.T) {
	b, generation, _, _ := gateFixture(t)
	closeTestSource(t, b)
	digest := b.records[generation].Digest
	handler := NewSessionHandler(b, gateAuthorizer{digest: digest})
	body, err := json.Marshal(map[string]any{
		"requestID": strings.Repeat("c", 32), "targetInstanceID": strings.Repeat("b", 32),
		"podNamespace": "tenant", "podName": "engine", "podUID": "owner",
		"artifactName": "model", "artifactUID": "artifact",
		"manifestDigest": digest, "sourceGeneration": 1,
		"tensor": "weight", "offset": 0, "length": 4,
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
	assert.NotContains(t, recorder.Body.String(), "manifestJSON")
	assert.NotContains(t, recorder.Body.String(), "descriptors")
	var answer map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &answer))
	assert.Equal(t, "not-created", answer["admission"])
	assert.Equal(t, string(protocol.KindSourceUnavailable), answer["kind"])
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, session := range b.sessions {
		assert.Equal(t, "refused", session.Phase, "a refused session is never admitted")
	}
}

// gateSessionRequest is one session open request over the HTTP handler. The
// tensor names the bytes the caller reads, so a changed tensor is a changed
// request under the same request identity.
func gateSessionRequest(t *testing.T, digest, requestID, tensor string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"requestID": requestID, "targetInstanceID": strings.Repeat("b", 32),
		"podNamespace": "tenant", "podName": "engine", "podUID": "owner",
		"artifactName": "model", "artifactUID": "artifact",
		"manifestDigest": digest, "sourceGeneration": 1,
		"tensor": tensor, "offset": 0, "length": 4,
	})
	require.NoError(t, err)
	return body
}

// gateSessionPost sends one session open request and returns the status, the
// decoded answer and the raw body.
func gateSessionPost(t *testing.T, handler http.Handler, body []byte) (int, map[string]any, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var answer map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
	return recorder.Code, answer, recorder.Body.String()
}

// TestSourceGateRefusesIdenticalSessionRetryOverHTTP shows that a client
// retrying its own refused session open gets the same truthful qualification
// refusal, not a false authentication failure that sends it into a re-auth
// path.
func TestSourceGateRefusesIdenticalSessionRetryOverHTTP(t *testing.T) {
	b, generation, _, private := gateFixture(t)
	closeTestSource(t, b)
	digest := b.records[generation].Digest
	handler := NewSessionHandler(b, gateAuthorizer{digest: digest})
	body := gateSessionRequest(t, digest, strings.Repeat("c", 32), "weight")

	firstCode, first, firstRaw := gateSessionPost(t, handler, body)
	require.Equal(t, http.StatusConflict, firstCode, firstRaw)
	require.Equal(t, string(protocol.KindSourceUnavailable), first["kind"])

	secondCode, second, secondRaw := gateSessionPost(t, handler, body)
	require.Equal(t, http.StatusConflict, secondCode, secondRaw)
	assert.Equal(t, string(protocol.KindSourceUnavailable), second["kind"],
		"an identical retry gets the truthful qualification refusal, not an authentication failure")
	assert.Equal(t, first["kind"], second["kind"])
	assert.Equal(t, first["reason"], second["reason"])
	assert.Equal(t, "not-created", second["admission"])
	assert.Equal(t, first["requestID"], second["requestID"])
	assert.NotContains(t, second, "manifestJSON")
	assert.NotContains(t, secondRaw, "descriptors")

	b.mu.Lock()
	defer b.mu.Unlock()
	require.Len(t, b.sessions, 1, "the refusal is recorded once")
	for id, session := range b.sessions {
		assert.Equal(t, "refused", session.Phase, "a refused session is never admitted")
		assert.Equal(t, strings.Repeat("c", 32), session.RequestID)
		assert.Equal(t, strings.Repeat("b", 32), session.TargetInstanceID)
		assert.NotEmpty(t, session.OpenFingerprint)
		assert.Empty(t, session.Generation)
		assert.Equal(t, id, stableSessionID(SessionOwner{
			PodUID: "owner", TargetInstanceID: strings.Repeat("b", 32), RequestID: strings.Repeat("c", 32),
		}), "the refused record keeps the request identity")
	}
	assert.Equal(t, 0, private.count("/v1/operations"), "a refused retry posts no native work")
}

// TestSourceGateRefusesAlteredSessionRequestWithTheSameIdentity shows that a
// changed request reusing a refused request identity is not the refused
// request: the owner and fingerprint binding refuses it, and it gains no
// admission and no record of its own.
func TestSourceGateRefusesAlteredSessionRequestWithTheSameIdentity(t *testing.T) {
	b, generation, _, _ := gateFixture(t)
	closeTestSource(t, b)
	digest := b.records[generation].Digest
	handler := NewSessionHandler(b, gateAuthorizer{digest: digest})
	requestID := strings.Repeat("c", 32)
	original := gateSessionRequest(t, digest, requestID, "weight")
	altered := gateSessionRequest(t, digest, requestID, "other")

	code, answer, raw := gateSessionPost(t, handler, original)
	require.Equal(t, http.StatusConflict, code, raw)
	require.Equal(t, string(protocol.KindSourceUnavailable), answer["kind"])

	id := stableSessionID(SessionOwner{
		PodUID: "owner", TargetInstanceID: strings.Repeat("b", 32), RequestID: requestID,
	})
	b.mu.Lock()
	fingerprint := b.sessions[id].OpenFingerprint
	b.mu.Unlock()
	require.NotEmpty(t, fingerprint)

	code, answer, raw = gateSessionPost(t, handler, altered)
	require.Equal(t, http.StatusForbidden, code, raw)
	assert.Equal(t, string(protocol.KindAuthentication), answer["kind"],
		"a changed request is refused by the owner and fingerprint binding")
	assert.NotContains(t, answer, "admission", "a changed request is not the refused request")
	assert.NotContains(t, answer, "manifestJSON")
	assert.NotContains(t, raw, "descriptors")

	b.mu.Lock()
	defer b.mu.Unlock()
	require.Len(t, b.sessions, 1, "a changed request records nothing of its own")
	for _, session := range b.sessions {
		assert.Equal(t, "refused", session.Phase)
		assert.Equal(t, fingerprint, session.OpenFingerprint,
			"a changed request never rewrites the refused record")
	}
}

// TestSourceGateKeepsAnAdmittedSessionObservableOverHTTP shows that the gate
// refuses new remote work and leaves the memory already exposed alone: an
// admitted session stays observable while the gate is closed.
func TestSourceGateKeepsAnAdmittedSessionObservableOverHTTP(t *testing.T) {
	b, generation, owner, _ := gateFixture(t)
	digest := b.records[generation].Digest
	handler := NewSessionHandler(b, gateAuthorizer{digest: digest})
	body := gateSessionRequest(t, digest, owner.RequestID, "weight")

	code, answer, raw := gateSessionPost(t, handler, body)
	require.Equal(t, http.StatusOK, code, raw)
	sessionID, _ := answer["sessionID"].(string)
	require.NotEmpty(t, sessionID)

	closeTestSource(t, b)

	code, answer, raw = gateSessionPost(t, handler, body)
	require.Equal(t, http.StatusOK, code, raw, "an admitted session stays observable while the gate is closed")
	assert.Equal(t, sessionID, answer["sessionID"])
	assert.NotEmpty(t, answer["manifestJSON"])

	view, verr := b.SessionView(sessionID)
	require.Nil(t, verr, "an admitted session keeps its descriptors while the gate is closed")
	assert.NotEmpty(t, view.Descriptors)
}

// TestSourceGateKeepsARefusedRequestRefusedWhenTheGateOpens shows that a
// refusal is terminal: the private test seam opening the gate does not turn a
// refused request into an admission.
func TestSourceGateKeepsARefusedRequestRefusedWhenTheGateOpens(t *testing.T) {
	b, generation, _, _ := gateFixture(t)
	closeTestSource(t, b)
	digest := b.records[generation].Digest
	handler := NewSessionHandler(b, gateAuthorizer{digest: digest})
	body := gateSessionRequest(t, digest, strings.Repeat("c", 32), "weight")

	code, answer, raw := gateSessionPost(t, handler, body)
	require.Equal(t, http.StatusConflict, code, raw)
	require.Equal(t, string(protocol.KindSourceUnavailable), answer["kind"])

	qualifyTestSource(t, b)

	code, answer, raw = gateSessionPost(t, handler, body)
	require.Equal(t, http.StatusConflict, code, raw)
	assert.NotEqual(t, string(protocol.KindSourceUnavailable), answer["kind"],
		"the qualification refusal is not repeated once the gate is open")
	assert.NotContains(t, answer, "manifestJSON")
	assert.NotContains(t, answer, "sessionID")
	assert.NotContains(t, raw, "descriptors")

	b.mu.Lock()
	defer b.mu.Unlock()
	require.Len(t, b.sessions, 1)
	for _, session := range b.sessions {
		assert.Equal(t, "refused", session.Phase, "a refusal never becomes an admission")
	}
}

// TestSessionRefusalKindMatchesTheProtocolError shows that a refusal carries
// the kind of the protocol error that caused it, so the kind and the reason
// never contradict each other.
func TestSessionRefusalKindMatchesTheProtocolError(t *testing.T) {
	b, _, _, _ := gateFixture(t)
	closeTestSource(t, b)
	// The authorizer names a digest that no serving generation holds, so the
	// generation lookup fails before the gate is consulted.
	missing := "sha256:" + strings.Repeat("0", 64)
	handler := NewSessionHandler(b, gateAuthorizer{digest: missing})
	body := gateSessionRequest(t, missing, strings.Repeat("c", 32), "weight")

	code, answer, raw := gateSessionPost(t, handler, body)
	require.Equal(t, http.StatusConflict, code, raw)
	assert.Equal(t, string(protocol.KindOperationState), answer["kind"])
	assert.Equal(t, "not-created", answer["admission"])
	assert.Equal(t, strings.Repeat("c", 32), answer["requestID"])
	reason, _ := answer["reason"].(string)
	assert.True(t, strings.HasPrefix(reason, string(protocol.KindOperationState)+": "),
		"the reason names the same kind as the body: %s", reason)
	assert.NotContains(t, answer, "manifestJSON")
	assert.NotContains(t, raw, "descriptors")

	b.mu.Lock()
	defer b.mu.Unlock()
	for _, session := range b.sessions {
		assert.Equal(t, "refused", session.Phase, "a generation lookup failure admits no session")
	}
}

// TestSourceGateRefusesNewTransferAndKeepsInFlightOperations shows that the
// gate stops a new native transfer without touching the operation already in
// flight: it is polled and acknowledged as before.
func TestSourceGateRefusesNewTransferAndKeepsInFlightOperations(t *testing.T) {
	b, generation, owner, private := gateFixture(t)
	qualifyTestSource(t, b)
	session, perr := b.AdmitSession(context.Background(), owner, "tenant", generation)
	require.Nil(t, perr)
	instance := strings.Repeat("a", 32)
	request := gateTransferRequest(session.ID, generation, instance)
	submitted, err := b.SubmitTransfer(context.Background(), request, gatePod())
	require.NoError(t, err)
	require.Equal(t, "submitted", submitted.State)
	require.Equal(t, 1, private.count("/v1/operations"))

	closeTestSource(t, b)

	fresh := request
	fresh.RequestID, fresh.WindowID = strings.Repeat("7", 32), strings.Repeat("8", 32)
	_, err = b.SubmitTransfer(context.Background(), fresh, gatePod())
	require.Error(t, err, "a new native transfer must be refused while the gate is closed")
	assert.Equal(t, protocol.KindSourceUnavailable, protocol.KindOf(err))
	assert.Equal(t, 1, private.count("/v1/operations"), "a refused transfer posts nothing")

	replay, err := b.SubmitTransfer(context.Background(), request, gatePod())
	require.NoError(t, err, "a retry of a dispatched request stays an observation")
	assert.Equal(t, "terminal", replay.State)
	assert.Equal(t, 1, private.count("/v1/operations"), "a retry never reposts the operation")

	status, err := b.OperationStatus(context.Background(), request.RequestID, session.ID, "owner", owner.TargetInstanceID)
	require.NoError(t, err)
	assert.Equal(t, "terminal", status.State)

	acked, err := b.AcknowledgeTransfer(context.Background(), request.RequestID, request, gatePod())
	require.NoError(t, err, "an in-flight operation is still acknowledged while the gate is closed")
	assert.Equal(t, "acknowledged", acked.State)
	assert.Equal(t, 1, private.count("/v1/operations/ack"))
}

// TestSourceGateRefusesPreparedOperationRepost shows that a request which
// was prepared but never posted does not post once the gate is closed.
func TestSourceGateRefusesPreparedOperationRepost(t *testing.T) {
	b, generation, owner, private := gateFixture(t)
	qualifyTestSource(t, b)
	session, perr := b.AdmitSession(context.Background(), owner, "tenant", generation)
	require.Nil(t, perr)
	instance := strings.Repeat("a", 32)
	request := gateTransferRequest(session.ID, generation, instance)
	b.mu.Lock()
	record := b.sessions[session.ID]
	err := b.persistOperationLocked(&OperationRecord{
		Request: request, SourceLeaseID: record.SourceLeaseID, ArtifactUID: record.ArtifactUID,
		Digest: record.Digest, SourceGeneration: 1, File: "weight.safetensors",
		FileOffset: 4, State: "prepared",
	})
	b.mu.Unlock()
	require.NoError(t, err)

	closeTestSource(t, b)
	_, err = b.SubmitTransfer(context.Background(), request, gatePod())
	require.Error(t, err, "a prepared request must not post once the gate is closed")
	assert.Equal(t, protocol.KindSourceUnavailable, protocol.KindOf(err))
	assert.Equal(t, 0, private.count("/v1/operations"))
}

// TestSourceGateRefusesConcurrentNewTransfers covers the race between the
// gate check and the private post: a closed gate posts nothing at all, and an
// open gate posts one operation per request identity.
func TestSourceGateRefusesConcurrentNewTransfers(t *testing.T) {
	b, generation, owner, private := gateFixture(t)
	qualifyTestSource(t, b)
	session, perr := b.AdmitSession(context.Background(), owner, "tenant", generation)
	require.Nil(t, perr)
	instance := strings.Repeat("a", 32)
	request := gateTransferRequest(session.ID, generation, instance)

	closeTestSource(t, b)
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _ = b.SubmitTransfer(context.Background(), request, gatePod())
		}()
	}
	group.Wait()
	assert.Equal(t, 0, private.count("/v1/operations"), "a closed gate posts nothing")

	qualifyTestSource(t, b)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _ = b.SubmitTransfer(context.Background(), request, gatePod())
		}()
	}
	group.Wait()
	assert.Equal(t, 1, private.count("/v1/operations"), "one request identity posts one operation")
}

// TestSourceGateIsOpenForSupportedBackendsByDefault verifies that a broker
// opened in production admits supported backends (nixl, hixl) and refuses
// unsupported ones unless explicitly closed.
func TestSourceGateIsOpenForSupportedBackendsByDefault(t *testing.T) {
	b, _, _ := openTestBroker(t, defaultLimits())
	b.mu.Lock()
	defer b.mu.Unlock()
	assert.True(t, b.sourceQualified)
	assert.Nil(t, b.sourceGate("session", "nixl"))
	assert.Nil(t, b.sourceGate("session", "hixl"))
	assert.NotNil(t, b.sourceGate("session", "unknown"))
}

// TestSourceGateKeepsTheRemoteChainWorkingWhenOpen keeps the chain a remote
// caller drives while a backend is qualified: open a session, submit a native
// transfer, poll it, acknowledge it, close the session and release it. The
// gate must not disturb any of it.
func TestSourceGateKeepsTheRemoteChainWorkingWhenOpen(t *testing.T) {
	b, generation, owner, private := gateFixture(t)
	qualifyTestSource(t, b)
	digest := b.records[generation].Digest
	handler := NewSessionHandler(b, gateAuthorizer{digest: digest})
	post := func(method, path string, body []byte) (int, map[string]any) {
		t.Helper()
		request := httptest.NewRequest(method, path, bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var answer map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
		return recorder.Code, answer
	}

	open, err := json.Marshal(map[string]any{
		"requestID": owner.RequestID, "targetInstanceID": owner.TargetInstanceID,
		"podNamespace": "tenant", "podName": "engine", "podUID": "owner",
		"artifactName": "model", "artifactUID": "artifact",
		"manifestDigest": digest, "sourceGeneration": 1,
		"tensor": "weight", "offset": 2, "length": 4,
	})
	require.NoError(t, err)
	status, answer := post(http.MethodPost, "/v1/sessions", open)
	require.Equal(t, http.StatusOK, status)
	sessionID, _ := answer["sessionID"].(string)
	require.NotEmpty(t, sessionID)
	assert.NotEmpty(t, answer["manifestJSON"])

	instance := strings.Repeat("a", 32)
	operationID, registrationID, windowID := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)
	transfer, err := json.Marshal(map[string]any{
		"requestID": operationID, "sessionID": sessionID,
		"podNamespace": "tenant", "podName": "engine", "podUID": "owner",
		"targetInstanceID": owner.TargetInstanceID, "sourcePodUID": "source",
		"workerInstanceID": instance, "generation": generation,
		"registrationID": registrationID, "windowID": windowID,
		"tensor": "weight", "tensorOffset": 2, "length": 4,
		"target": map[string]any{"backend": "nixl", "address": 9000, "length": 8, "metadata": "YQ=="},
	})
	require.NoError(t, err)
	for _, expected := range []string{"submitted", "terminal"} {
		status, answer = post(http.MethodPost, "/v1/transfers", transfer)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, expected, answer["state"])
	}
	assert.Equal(t, 1, private.count("/v1/operations"))

	ack, err := json.Marshal(map[string]string{
		"sessionID": sessionID, "podNamespace": "tenant", "podName": "engine", "podUID": "owner",
		"targetInstanceID": owner.TargetInstanceID, "registrationID": registrationID, "windowID": windowID,
	})
	require.NoError(t, err)
	status, answer = post(http.MethodPost, "/v1/transfers/"+operationID+"/ack", ack)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "acknowledged", answer["state"])

	closeID := strings.Repeat("d", 32)
	closing, err := json.Marshal(map[string]any{
		"requestID": closeID, "targetInstanceID": owner.TargetInstanceID,
		"podNamespace": "tenant", "podName": "engine", "podUID": "owner",
		"artifactUID": "artifact", "manifestDigest": digest, "targetReleased": false,
	})
	require.NoError(t, err)
	status, _ = post(http.MethodPost, "/v1/sessions/"+sessionID+"/closing", closing)
	require.Equal(t, http.StatusOK, status)
	released, err := json.Marshal(map[string]any{
		"requestID": closeID, "targetInstanceID": owner.TargetInstanceID,
		"podNamespace": "tenant", "podName": "engine", "podUID": "owner",
		"artifactUID": "artifact", "manifestDigest": digest, "targetReleased": true,
	})
	require.NoError(t, err)
	status, answer = post(http.MethodDelete, "/v1/sessions/"+sessionID, released)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "closed", answer["state"])
}

func (a gateAuthorizer) AuthenticateActivePod(ctx context.Context, req authorization.Request) (authorization.PodIdentity, *protocol.Error) {
	return a.AuthenticatePod(ctx, req)
}
