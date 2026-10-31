package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

// liveEngine is one real Python engine process holding one admitted session.
type liveEngine struct {
	proc             *pythonProcess
	sessionID        string
	targetInstanceID string
}

func admitEngineSession(t *testing.T, root string, item pythonFixture, configPath string) *liveEngine {
	t.Helper()
	token := filepath.Join(root, "token-"+item.Namespace)
	require.NoError(t, os.WriteFile(token, []byte("token-"+item.Namespace), 0o600))
	env := map[string]string{
		"GPUSTACK_MODEL_TRANSFER_BACKEND": "nixl", "GPUSTACK_MODEL_TRANSFER_ARTIFACT": item.ArtifactName,
		"GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID": item.ArtifactUID, "GPUSTACK_MODEL_TRANSFER_MANIFEST_DIGEST": item.Digest,
		"GPUSTACK_MODEL_TRANSFER_MODEL_PATH": item.ModelPath, "GPUSTACK_MODEL_TRANSFER_TOKEN": token, "GPUSTACK_MODEL_TRANSFER_CONFIG": configPath,
		"KUBERNETES_POD_UID": "uid-" + item.Namespace, "KUBERNETES_POD_NAME": "engine", "KUBERNETES_POD_NAMESPACE": item.Namespace, "KUBERNETES_POD_IP": "127.0.0.1",
	}
	engine := startPythonFixture(t)
	var answer map[string]any
	engine.call(t, map[string]any{"mode": "engine", "env": env}, &answer)
	require.Equal(t, "ready", answer["event"])
	for _, op := range []string{"probe", "start"} {
		engine.call(t, map[string]any{"op": op}, &answer)
		require.Equal(t, true, answer["ok"], "operation %s: %v", op, answer)
	}
	sessionID, ok := answer["sessionID"].(string)
	require.True(t, ok, "session id: %v", answer["sessionID"])
	targetInstanceID, ok := answer["targetInstanceID"].(string)
	require.True(t, ok, "target instance: %v", answer["targetInstanceID"])
	return &liveEngine{proc: engine, sessionID: sessionID, targetInstanceID: targetInstanceID}
}

// concurrentBrokerState copies the durable broker truth: session phases,
// the operation journal, and generation records. Release and charge
// assertions read this copy, never process liveness.
func concurrentBrokerState(b *Broker) (map[string]SessionRecord, map[string]OperationRecord, map[string]GenerationRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sessions := map[string]SessionRecord{}
	for id, session := range b.sessions {
		sessions[id] = *session
	}
	operations := map[string]OperationRecord{}
	for id, operation := range b.operations {
		operations[id] = *operation
	}
	records := map[string]GenerationRecord{}
	for id, rec := range b.records {
		records[id] = *rec
	}
	return sessions, operations, records
}

func controlPost(t *testing.T, controlURL, path string, body map[string]any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, controlURL+path, bytes.NewReader(raw))
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// drainAckObservation records one real /v1/drain-ack request the source
// worker made and the status the real control handler answered.
type drainAckObservation struct {
	nativeReleased bool
	status         int
}

// drainAckRecorder relays the real control handler unchanged and observes
// the worker's own drain acknowledgements; it never answers on the
// handler's behalf.
type drainAckRecorder struct {
	handler http.Handler
	mu      sync.Mutex
	acks    map[string][]drainAckObservation
	gates   map[string]<-chan struct{}
}

func newDrainAckRecorder(handler http.Handler) *drainAckRecorder {
	return &drainAckRecorder{handler: handler, acks: map[string][]drainAckObservation{}}
}

func (r *drainAckRecorder) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/v1/drain-ack" {
		r.handler.ServeHTTP(w, request)
		return
	}
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(w, "unreadable control message", http.StatusBadRequest)
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(raw))
	var body struct {
		Generation     string `json:"generation"`
		NativeReleased bool   `json:"nativeReleased"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.NativeReleased {
		r.mu.Lock()
		gate := r.gates[body.Generation]
		r.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-request.Context().Done():
				return
			}
		}
	}
	recorder := httptest.NewRecorder()
	r.handler.ServeHTTP(recorder, request)
	r.mu.Lock()
	r.acks[body.Generation] = append(r.acks[body.Generation],
		drainAckObservation{nativeReleased: body.NativeReleased, status: recorder.Code})
	r.mu.Unlock()
	for key, values := range recorder.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

// holdNativeAck delays forwarding a real release receipt until the test has
// observed the independently released session slot and retained generation charge.
func (r *drainAckRecorder) holdNativeAck(generation string) func() {
	gate := make(chan struct{})
	r.mu.Lock()
	if r.gates == nil {
		r.gates = map[string]<-chan struct{}{}
	}
	r.gates[generation] = gate
	r.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// acknowledged reports whether the real worker's own native-release
// acknowledgement for a generation was answered 200 by the real handler.
func (r *drainAckRecorder) acknowledged(generation string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ack := range r.acks[generation] {
		if ack.nativeReleased && ack.status == http.StatusOK {
			return true
		}
	}
	return false
}

// tryCall exchanges one fixture command without failing the test, so an
// Eventually condition can poll the source's redacted status.
func (p *pythonProcess) tryCall(request, response any) bool {
	if err := p.in.Encode(request); err != nil {
		return false
	}
	select {
	case line, ok := <-p.lines:
		if !ok {
			return false
		}
		return json.Unmarshal(line, response) == nil
	case <-time.After(5 * time.Second):
		return false
	}
}

// sourceWorkerStatus reads the real source's redacted worker status for
// one generation: its lifecycle state and the registrations it still
// holds. It is the source-side counterpart of the broker's own records.
func sourceWorkerStatus(source *pythonProcess, generation string) (string, int, bool, bool) {
	var answer struct {
		Ok      bool `json:"ok"`
		Workers []struct {
			Generation    string `json:"generation"`
			State         string `json:"state"`
			Registrations int    `json:"registrations"`
		} `json:"workers"`
	}
	if !source.tryCall(map[string]any{"op": "status"}, &answer) || !answer.Ok {
		return "", 0, false, false
	}
	for _, worker := range answer.Workers {
		if worker.Generation == generation {
			return worker.State, worker.Registrations, true, true
		}
	}
	return "", 0, false, true
}

// drainHoldObserved reports whether the source worker processed the drain and
// still holds its registration. The worker sets its own state to draining
// before it reads the active sessions, so the hold shows as draining with one
// registration left. A worker still serving has not processed the drain yet,
// and a worker that released its registration has lost the hold.
func drainHoldObserved(state string, registrations int, found, ok bool) bool {
	return ok && found && state == "draining" && registrations == 1
}

// fakeWorkerStatus is an isolated source worker status fixture. It reports
// what the worker would report about itself, with no source process.
type fakeWorkerStatus struct {
	state         string
	registrations int
	found         bool
	ok            bool
}

func (f fakeWorkerStatus) observed() (string, int, bool, bool) {
	return f.state, f.registrations, f.found, f.ok
}

// TestDrainHoldObservedRejectsAnUnsafeStatus feeds the drain predicate the
// statuses it must not accept. The fixture reports them directly, so the
// predicate under test stays the one the integration test uses.
func TestDrainHoldObservedRejectsAnUnsafeStatus(t *testing.T) {
	cases := []struct {
		name    string
		fixture fakeWorkerStatus
		want    bool
	}{
		{"draining with no registration held", fakeWorkerStatus{state: "draining", registrations: 0, found: true, ok: true}, false},
		{"serving before the drain is processed", fakeWorkerStatus{state: "serving", registrations: 1, found: true, ok: true}, false},
		{"draining with the registration held", fakeWorkerStatus{state: "draining", registrations: 1, found: true, ok: true}, true},
		{"generation absent from the report", fakeWorkerStatus{state: "draining", registrations: 1, found: false, ok: true}, false},
		{"report unavailable", fakeWorkerStatus{ok: false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, registrations, found, ok := tc.fixture.observed()
			assert.Equal(t, tc.want, drainHoldObserved(state, registrations, found, ok))
		})
	}
}

// TestPythonConcurrentSessionsHoldDrainAndRetireIndependently keeps two
// independently admitted owners live at once over the real Go handlers and
// real Python processes. An active session holds its generation's native
// release, retiring that generation leaves the other owner's live session
// and native path untouched, and every charge or release is proven by
// durable state — session phases, the operation journal, the ledger-backed
// generation records, the Binding authority, the real source worker's own
// redacted status, and the real control handler's answers to the worker's
// own drain acknowledgement — never by a forged control message, a
// process exit, a lease expiry, or a timeout. The only /v1/drain-ack the
// test itself sends is refused, proving the broker's active-session hold;
// every acknowledged release is the Python source worker's own, observed
// passing through the real handler after it revoked its registrations.
func TestPythonConcurrentSessionsHoldDrainAndRetireIndependently(t *testing.T) {
	root := t.TempDir()
	prepare := startPythonFixture(t)
	var prepared struct {
		Fixtures []pythonFixture `json:"fixtures"`
	}
	prepare.call(t, map[string]any{"mode": "prepare", "root": filepath.Join(root, "fixture")}, &prepared)
	require.Len(t, prepared.Fixtures, 2)
	items := prepared.Fixtures

	var manifest struct {
		Tensors []struct {
			Name      string `json:"name"`
			DataBegin int64  `json:"data_begin"`
			DataEnd   int64  `json:"data_end"`
		} `json:"tensors"`
	}
	require.NoError(t, json.Unmarshal([]byte(items[0].ManifestJSON), &manifest))
	require.NotEmpty(t, manifest.Tensors)
	first := manifest.Tensors[0]

	b, st, _ := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	publishTree(t, st, store.HexOf(items[0].Digest), "fixture", items[0].TotalBytes)
	tokenPath := filepath.Join(root, "bootstrap")
	require.NoError(t, os.WriteFile(tokenPath, []byte("fixture-bootstrap"), 0o600))
	b.SetBootstrapCredential("fixture-bootstrap")
	controlRecorder := newDrainAckRecorder(NewControlHandler(b))
	control := httptest.NewServer(controlRecorder)
	defer control.Close()
	generations, secrets := map[string]string{}, map[string]string{}
	for i, item := range items {
		intent := Intent{
			Key: fmt.Sprintf("concurrent-intent-%d", i), Namespace: item.Namespace, Digest: item.Digest, TotalBytes: item.TotalBytes,
			Grant: grant(item.Namespace, 1<<20), WorkerID: "source-pod", NodeName: "node", IntentUID: fmt.Sprintf("prefetch-%d", i),
			IntentGeneration: 1, IntentName: "intent", ArtifactName: item.ArtifactName, BindingUID: "binding-uid-1", BindingName: "binding",
			ReservationID: fmt.Sprintf("concurrent-reservation-%d", i), Registrations: item.Registrations, ArtifactUID: item.ArtifactUID,
			SourceGeneration: item.SourceGeneration, ManifestSHA256: item.ManifestSHA256, ManifestJSON: item.ManifestJSON,
			TreePath: item.TreePath, Backend: "nixl",
		}
		assignment, perr := b.AdmitIntentFill(t.Context(), intent)
		require.Nil(t, perr)
		generations[item.Namespace] = assignment.Generation
		secrets[item.Namespace] = assignment.WorkerSecret
	}
	generationA, generationB := generations[items[0].Namespace], generations[items[1].Namespace]
	authority := b.reservationAuthorities[types.UID("binding-uid-1")]
	ledgerID := b.ledger.identity

	source := startPythonFixture(t)
	var ready struct {
		Event          string `json:"event"`
		PrivateAddress string `json:"privateAddress"`
	}
	instance := strings.Repeat("a", 32)
	source.call(t, map[string]any{
		"mode": "source", "bootstrapFile": tokenPath, "controlAddress": strings.TrimPrefix(control.URL, "http://"),
		"workerID": "source-pod", "workerInstanceID": instance,
	}, &ready)
	require.Equal(t, "ready", ready.Event)
	private := &countedPrivateSource{PrivateSource: &httpPrivateSource{
		address: "http://" + ready.PrivateAddress,
		token:   "fixture-bootstrap", client: &http.Client{Timeout: 5 * time.Second},
	}, calls: map[string]int{}}
	b.SetPrivateSource(private)
	require.Eventually(t, func() bool {
		entries := b.Snapshot()
		return len(entries) == 2 &&
			entries[0].State == StateServing && entries[1].State == StateServing
	}, 15*time.Second, 50*time.Millisecond)

	auth := fixtureAuth{fixtures: map[string]pythonFixture{}}
	for _, item := range items {
		auth.fixtures[item.Namespace] = item
	}
	tlsServer := httptest.NewTLSServer(NewSessionHandler(b, auth))
	defer tlsServer.Close()
	cert := tlsServer.Certificate()
	ca := filepath.Join(root, "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600))
	serverName := "127.0.0.1"
	if len(cert.DNSNames) > 0 {
		serverName = cert.DNSNames[0]
	}
	endpoint := map[string]any{"version": 1, "endpoints": []any{map[string]string{
		"manufacturer": "nvidia", "address": strings.TrimPrefix(tlsServer.URL, "https://"),
		"serverName": serverName, "caFile": ca, "podUID": "source-pod", "podName": "source", "podNamespace": "operator", "nodeName": "node",
	}}}
	config, err := json.Marshal(endpoint)
	require.NoError(t, err)
	configPath := filepath.Join(root, "config.json")
	require.NoError(t, os.WriteFile(configPath, config, 0o600))

	engineA := admitEngineSession(t, root, items[0], configPath)
	engineB := admitEngineSession(t, root, items[1], configPath)
	sessions, operations, records := concurrentBrokerState(b)
	// Each engine's probe opens and fully releases one metadata session
	// before start, so the journal of sessions holds two released
	// metadata phases plus exactly the two live admissions.
	admitted := map[string]string{}
	for id, session := range sessions {
		if session.Phase == "admitted" {
			admitted[id] = session.Generation
		} else {
			require.Equal(t, "released", session.Phase)
		}
	}
	require.Len(t, sessions, 4)
	require.Empty(t, operations)
	require.Equal(t, map[string]string{engineA.sessionID: generationA, engineB.sessionID: generationB}, admitted)
	require.Equal(t, int32(1), records[generationA].ActiveSessions)
	require.Equal(t, int32(1), records[generationB].ActiveSessions)

	// A refused submission is proven before the native POST exists: the
	// real session handler answers after full owner and incarnation
	// validation, and neither the private source call counter nor the
	// durable journal moved.
	refused := map[string]any{
		"requestID": strings.Repeat("e", 32), "sessionID": engineA.sessionID,
		"podNamespace": items[0].Namespace, "podName": "engine", "podUID": "uid-" + items[0].Namespace,
		"targetInstanceID": engineA.targetInstanceID, "sourcePodUID": "source-pod", "workerInstanceID": instance,
		"generation": generationA, "registrationID": strings.Repeat("1", 32), "windowID": strings.Repeat("2", 32),
		"tensor": first.Name, "tensorOffset": first.DataEnd - first.DataBegin + 1, "length": 1,
		"target": map[string]any{"backend": "nixl", "address": 4096, "length": 16, "metadata": "YQ==", "endpoint": ""},
	}
	raw, err := json.Marshal(refused)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, tlsServer.URL+"/v1/transfers", bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer token-"+items[0].Namespace)
	response, err := tlsServer.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.Zero(t, private.count("/v1/operations"))
	_, operations, _ = concurrentBrokerState(b)
	require.Empty(t, operations)

	var answer map[string]any
	engineA.proc.call(t, map[string]any{"op": "fill", "tensorIndex": 0}, &answer)
	require.Equal(t, true, answer["ok"], "fill A: %v", answer)
	require.Equal(t, items[0].Expected, answer["sha256"])

	// The active-session drain hold: draining a generation is allowed, but
	// its native release acknowledgement is refused while its session is
	// still admitted, and the charge stays in the ledger. The refused
	// drain-ack below is the only one the test itself sends; it is the
	// negative boundary, never release evidence. The real worker holds
	// too: its heartbeat arms the drain, but its own release gate reads
	// the broker's authoritative session count, so it keeps its
	// registration and never acknowledges while the session is charged.
	require.Nil(t, b.Drain(generationA))
	_, _, records = concurrentBrokerState(b)
	require.Equal(t, StateDraining, records[generationA].State)
	require.False(t, records[generationA].NativeReleased)
	held := controlPost(t, control.URL, "/v1/drain-ack", map[string]any{
		"workerID": "source-pod", "workerInstanceID": instance, "generation": generationA,
		"leaseID": records[generationA].LeaseID, "secret": secrets[items[0].Namespace], "nativeReleased": true,
	})
	require.Equal(t, http.StatusConflict, held.StatusCode)
	heldKind := map[string]string{}
	require.NoError(t, json.NewDecoder(held.Body).Decode(&heldKind))
	require.Equal(t, "operation-state", heldKind["kind"])
	// The worker sets its own state to draining before it reads the active
	// sessions, so the hold shows as draining with one registration still
	// held. Waiting for serving would pass before the worker processed the
	// drain at all, which proves nothing about the hold.
	require.Eventually(t, func() bool {
		state, registrations, found, ok := sourceWorkerStatus(source, generationA)
		return drainHoldObserved(state, registrations, found, ok)
	}, 15*time.Second, 50*time.Millisecond)
	require.False(t, controlRecorder.acknowledged(generationA))
	sessionsB, _, recordsB := concurrentBrokerState(b)
	require.Equal(t, "admitted", sessionsB[engineB.sessionID].Phase)
	require.Equal(t, StateServing, recordsB[generationB].State)
	require.Equal(t, int32(1), recordsB[generationB].ActiveSessions)
	reserved, err := authority.Adopt(context.Background(), b.scope, ledgerID)
	require.NoError(t, err)
	require.Len(t, reserved, 2)

	// B transfers while A retires: A is draining and its real worker is
	// holding, yet B's live session still completes a fresh verified
	// transfer, which also proves the refusal above was measured with a
	// working submission counter.
	engineB.proc.call(t, map[string]any{"op": "fill", "tensorIndex": 0}, &answer)
	require.Equal(t, true, answer["ok"], "fill B: %v", answer)
	require.Equal(t, items[1].Expected, answer["sha256"])
	require.Positive(t, private.count("/v1/operations"))

	// Retiring A is safe and independent. The real two-phase close fences
	// the source and releases the session phase and Binding slot durably;
	// the real source worker then reads zero authoritative sessions from
	// its own heartbeat, disconnects and revokes its registrations, and
	// posts its own native-release acknowledgement through the real
	// control handler. Release is asserted as the agreement of that
	// observed acknowledgement, the broker's durable record, the
	// Binding authority, and the worker's redacted source-side state; a
	// reaped worker counts only after its acknowledgement was observed,
	// because the supervisor reaps no worker before it reaches a
	// terminal drained or stopped state.
	resumeAck := controlRecorder.holdNativeAck(generationA)
	defer resumeAck()
	engineA.proc.call(t, map[string]any{"op": "close"}, &answer)
	require.Equal(t, true, answer["ok"], "close A: %v", answer)
	require.Equal(t, true, answer["closed"])
	sessions, _, records = concurrentBrokerState(b)
	require.Equal(t, "released", sessions[engineA.sessionID].Phase)
	require.Equal(t, "admitted", sessions[engineB.sessionID].Phase)
	require.Equal(t, int32(0), records[generationA].ActiveSessions)
	require.Equal(t, int32(1), records[generationB].ActiveSessions)
	reserved, err = authority.Adopt(context.Background(), b.scope, ledgerID)
	require.NoError(t, err)
	require.Len(t, reserved, 2)
	byReservation := map[string]ReservationEntry{}
	for _, entry := range reserved {
		byReservation[entry.ReservationID] = entry
	}
	require.Empty(t, byReservation["concurrent-reservation-0"].Sessions)
	require.Len(t, byReservation["concurrent-reservation-1"].Sessions, 1)
	resumeAck()
	require.Eventually(t, func() bool {
		if !controlRecorder.acknowledged(generationA) {
			return false
		}
		state, registrations, found, ok := sourceWorkerStatus(source, generationA)
		if !ok || (found && (state != "drained" || registrations != 0)) {
			return false
		}
		_, _, records = concurrentBrokerState(b)
		return records[generationA].State == StateReleased && records[generationA].NativeReleased
	}, 15*time.Second, 50*time.Millisecond)
	reserved, err = authority.Adopt(context.Background(), b.scope, ledgerID)
	require.NoError(t, err)
	require.Len(t, reserved, 1)
	require.Equal(t, "concurrent-reservation-1", reserved[0].ReservationID)
	entries := b.Snapshot()
	require.Len(t, entries, 1)
	require.Equal(t, generationB, entries[0].Generation)
	require.Equal(t, int32(1), entries[0].ActiveSessions)

	// Both engines' verified transfers are durably acknowledged, one per
	// live session, after A's full retirement.
	_, operations, _ = concurrentBrokerState(b)
	sawA, sawB := false, false
	for _, operation := range operations {
		require.Equal(t, "acknowledged", operation.State, "operation %s", operation.Request.RequestID)
		switch operation.Request.SessionID {
		case engineA.sessionID:
			sawA = true
		case engineB.sessionID:
			sawB = true
		}
	}
	require.True(t, sawA)
	require.True(t, sawB)

	engineB.proc.call(t, map[string]any{"op": "close"}, &answer)
	require.Equal(t, true, answer["ok"], "close B: %v", answer)
	require.Equal(t, true, answer["closed"])
	require.Nil(t, b.Drain(generationB))
	require.Eventually(t, func() bool {
		if !controlRecorder.acknowledged(generationB) {
			return false
		}
		state, registrations, found, ok := sourceWorkerStatus(source, generationB)
		if !ok || (found && (state != "drained" || registrations != 0)) {
			return false
		}
		_, _, records = concurrentBrokerState(b)
		return records[generationB].State == StateReleased && records[generationB].NativeReleased
	}, 15*time.Second, 50*time.Millisecond)
	reserved, err = authority.Adopt(context.Background(), b.scope, ledgerID)
	require.NoError(t, err)
	require.Empty(t, reserved)
	require.Empty(t, b.Snapshot())
	// With every generation and session durably released, the broker's
	// own drain gate opens once the controller bookkeeping flags stand in.
	b.mu.Lock()
	b.assignmentsFrozen, b.inventoryRequired, b.inventoryVerified = true, true, true
	b.mu.Unlock()
	require.True(t, b.ReadyForDrain())

	for _, engine := range []*liveEngine{engineA, engineB} {
		engine.proc.call(t, map[string]any{"op": "exit"}, &answer)
		require.Equal(t, true, answer["ok"], "exit: %v", answer)
	}
}
