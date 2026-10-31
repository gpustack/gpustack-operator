package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

type countedPrivateSource struct {
	PrivateSource
	mu    sync.Mutex
	calls map[string]int
}

func (s *countedPrivateSource) Call(ctx context.Context, path string, payload any) (PrivateReply, error) {
	s.mu.Lock()
	s.calls[path]++
	s.mu.Unlock()
	return s.PrivateSource.Call(ctx, path, payload)
}

func (s *countedPrivateSource) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path]
}

func TestPythonUnknownTransferHoldsAcrossRestarts(t *testing.T) {
	root := t.TempDir()
	prepare := startPythonFixture(t)
	var prepared struct {
		Fixtures []pythonFixture `json:"fixtures"`
	}
	prepare.call(t, map[string]any{"mode": "prepare", "root": filepath.Join(root, "fixture")}, &prepared)
	require.Len(t, prepared.Fixtures, 2)
	item := prepared.Fixtures[0]

	b, st, _ := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	publishTree(t, st, store.HexOf(item.Digest), "fixture", item.TotalBytes)
	tokenPath := filepath.Join(root, "bootstrap")
	require.NoError(t, os.WriteFile(tokenPath, []byte("fixture-bootstrap"), 0o600))
	b.SetBootstrapCredential("fixture-bootstrap")
	control := httptest.NewServer(NewControlHandler(b))
	defer control.Close()
	intent := Intent{
		Key: "unknown-intent", Namespace: item.Namespace, Digest: item.Digest, TotalBytes: item.TotalBytes,
		Grant: grant(item.Namespace, 1<<20), WorkerID: "source-pod", NodeName: "node", IntentUID: "prefetch",
		IntentGeneration: 1, IntentName: "intent", ArtifactName: item.ArtifactName, BindingUID: "binding-uid-1", BindingName: "binding",
		ReservationID: "unknown-reservation", Registrations: item.Registrations, ArtifactUID: item.ArtifactUID,
		SourceGeneration: item.SourceGeneration, ManifestSHA256: item.ManifestSHA256, ManifestJSON: item.ManifestJSON, TreePath: item.TreePath, Backend: "nixl",
	}
	assignment, perr := b.AdmitIntentFill(intent)
	require.Nil(t, perr)
	authority := b.reservationAuthorities[types.UID(intent.BindingUID)]
	ledgerID := b.ledger.identity

	source := startPythonFixture(t)
	var ready struct {
		Event          string `json:"event"`
		PrivateAddress string `json:"privateAddress"`
	}
	instance := strings.Repeat("a", 32)
	source.call(t, map[string]any{
		"mode": "source", "bootstrapFile": tokenPath, "controlAddress": strings.TrimPrefix(control.URL, "http://"),
		"workerID": "source-pod", "workerInstanceID": instance, "postMode": "ambiguous",
	}, &ready)
	require.Equal(t, "ready", ready.Event)
	private := &countedPrivateSource{PrivateSource: &httpPrivateSource{
		address: "http://" + ready.PrivateAddress,
		token:   "fixture-bootstrap", client: &http.Client{Timeout: 5 * time.Second},
	}, calls: map[string]int{}}
	b.SetPrivateSource(private)
	require.Eventually(t, func() bool {
		entries := b.Snapshot()
		return len(entries) == 1 && entries[0].State == StateServing
	}, 15*time.Second, 50*time.Millisecond)

	auth := fixtureAuth{fixtures: map[string]pythonFixture{item.Namespace: item}}
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
	targetToken := filepath.Join(root, "token")
	require.NoError(t, os.WriteFile(targetToken, []byte("token-"+item.Namespace), 0o600))
	env := map[string]string{
		"GPUSTACK_MODEL_TRANSFER_BACKEND": "nixl", "GPUSTACK_MODEL_TRANSFER_ARTIFACT": item.ArtifactName,
		"GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID": item.ArtifactUID, "GPUSTACK_MODEL_TRANSFER_MANIFEST_DIGEST": item.Digest,
		"GPUSTACK_MODEL_TRANSFER_MODEL_PATH": item.ModelPath, "GPUSTACK_MODEL_TRANSFER_TOKEN": targetToken, "GPUSTACK_MODEL_TRANSFER_CONFIG": configPath,
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
	engine.call(t, map[string]any{"op": "fill"}, &answer)
	require.Equal(t, false, answer["ok"])
	require.Equal(t, "SourceUnavailableError", answer["kind"])
	engine.call(t, map[string]any{"op": "inspect"}, &answer)
	require.Equal(t, true, answer["unknown"])
	require.Equal(t, true, answer["inflight"])
	require.Equal(t, false, answer["windowClosed"])
	require.Equal(t, 1, private.count("/v1/operations"))

	b.mu.Lock()
	var operation OperationRecord
	for _, op := range b.operations {
		operation = *op
	}
	b.mu.Unlock()
	require.NotEmpty(t, operation.Request.RequestID)
	require.Equal(t, "unknown", operation.State)
	reserved, err := authority.Adopt(context.Background(), b.scope, ledgerID)
	require.NoError(t, err)
	require.Len(t, reserved, 1)

	// The old source process and its in-memory job are lost, not released.
	require.NoError(t, source.cmd.Process.Kill())
	_ = source.cmd.Wait()
	control.Close()
	require.NoError(t, b.Close())
	reopened, err := Open(b.root, b.scope, st, b.limits, func() time.Time { return testNow })
	require.NoError(t, err)
	defer reopened.Close()
	require.NoError(t, reopened.EnableOperationJournal())
	reopened.SetBootstrapCredential("fixture-bootstrap")
	newInstance := strings.Repeat("b", 32)
	require.False(t, reopened.CanBindWorkerInstance(newInstance))
	newControl := httptest.NewServer(NewControlHandler(reopened))
	defer newControl.Close()
	newSource := startPythonFixture(t)
	newSource.call(t, map[string]any{
		"mode": "source", "bootstrapFile": tokenPath, "controlAddress": strings.TrimPrefix(newControl.URL, "http://"),
		"workerID": "source-pod", "workerInstanceID": newInstance,
	}, &ready)
	require.Equal(t, "ready", ready.Event)
	newPrivate := &countedPrivateSource{PrivateSource: &httpPrivateSource{
		address: "http://" + ready.PrivateAddress,
		token:   "fixture-bootstrap", client: &http.Client{Timeout: 5 * time.Second},
	}, calls: map[string]int{}}
	reopened.SetPrivateSource(newPrivate)
	restartedTLS := httptest.NewTLSServer(NewSessionHandler(reopened, auth))
	defer restartedTLS.Close()
	closeBody, err := json.Marshal(map[string]any{
		"podNamespace": item.Namespace, "podName": "engine", "podUID": operation.Request.PodUID,
		"artifactUID": item.ArtifactUID, "manifestDigest": item.Digest,
		"targetInstanceID": operation.Request.TargetInstanceID, "requestID": strings.Repeat("c", 32), "targetReleased": false,
	})
	require.NoError(t, err)
	closeRequest, err := http.NewRequest(http.MethodPost,
		restartedTLS.URL+"/v1/sessions/"+operation.Request.SessionID+"/closing", bytes.NewReader(closeBody))
	require.NoError(t, err)
	closeRequest.Header.Set("Authorization", "Bearer token-"+item.Namespace)
	closeResponse, err := restartedTLS.Client().Do(closeRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, closeResponse.StatusCode)
	require.NoError(t, closeResponse.Body.Close())

	query := url.Values{
		"sessionID": {operation.Request.SessionID}, "targetInstanceID": {operation.Request.TargetInstanceID},
		"podUID": {operation.Request.PodUID}, "podName": {"engine"}, "podNamespace": {item.Namespace},
	}
	for range 2 {
		request, err := http.NewRequest(http.MethodGet,
			restartedTLS.URL+"/v1/transfers/"+operation.Request.RequestID+"?"+query.Encode(), nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer token-"+item.Namespace)
		response, err := restartedTLS.Client().Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		var status map[string]string
		require.NoError(t, json.NewDecoder(response.Body).Decode(&status))
		require.NoError(t, response.Body.Close())
		require.Equal(t, operation.Request.RequestID, status["requestID"])
		require.Equal(t, "unknown", status["state"])
	}
	require.Zero(t, newPrivate.count("/v1/operations"))
	require.Equal(t, 2, newPrivate.count("/v1/operations/status"))
	reopened.mu.Lock()
	reopened.assignmentsFrozen, reopened.inventoryRequired, reopened.inventoryVerified = true, true, true
	record, found := reopened.records[assignment.Generation]
	var snapshot GenerationRecord
	if found {
		snapshot = *record
	}
	reopened.mu.Unlock()
	require.False(t, reopened.ReadyForDrain())
	require.True(t, found)
	require.NotEqual(t, StateReleased, snapshot.State)
	require.Positive(t, snapshot.ActiveSessions)
	require.False(t, snapshot.NativeReleased)
	stillReserved, err := authority.Adopt(context.Background(), reopened.scope, ledgerID)
	require.NoError(t, err)
	require.Equal(t, reserved, stillReserved)
}
