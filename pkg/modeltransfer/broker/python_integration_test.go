package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

type pythonFixture struct {
	Namespace        string `json:"namespace"`
	ArtifactName     string `json:"artifactName"`
	ArtifactUID      string `json:"artifactUID"`
	SourceGeneration int64  `json:"sourceGeneration"`
	Digest           string `json:"digest"`
	ManifestSHA256   string `json:"manifestSHA256"`
	ManifestJSON     string `json:"manifestJSON"`
	TreePath         string `json:"treePath"`
	ModelPath        string `json:"modelPath"`
	TotalBytes       int64  `json:"totalBytes"`
	Registrations    int32  `json:"registrations"`
	Expected         string `json:"expectedFirstTensorSHA256"`
}

type pythonProcess struct {
	cmd   *exec.Cmd
	in    *json.Encoder
	lines chan []byte
}

func startPythonFixture(t *testing.T) *pythonProcess {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	require.NoError(t, err)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the Go–Python integration test")
	}
	pythonRoot := filepath.Join(root, "model-transfer", "python")
	cmd := exec.Command(python, filepath.Join(pythonRoot, "tests/client/go_fixture.py"))
	cmd.Dir = pythonRoot
	cmd.Env = append(os.Environ(), "PYTHONPATH="+pythonRoot+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	p := &pythonProcess{cmd: cmd, in: json.NewEncoder(stdin), lines: make(chan []byte, 16)}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			p.lines <- append([]byte(nil), scanner.Bytes()...)
		}
		close(p.lines)
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return p
}

func (p *pythonProcess) call(t *testing.T, request, response any) {
	t.Helper()
	require.NoError(t, p.in.Encode(request))
	select {
	case line, ok := <-p.lines:
		require.True(t, ok, "fixture exited")
		require.NoError(t, json.Unmarshal(line, response))
	case <-time.After(40 * time.Second):
		t.Fatal("fixture response timed out")
	}
}

type fixtureAuth struct{ fixtures map[string]pythonFixture }

func (a fixtureAuth) AuthenticatePod(_ context.Context, req authorization.Request) (authorization.PodIdentity, *protocol.Error) {
	if req.Token != "token-"+req.PodNamespace || req.PodName != "engine" || req.PodUID != "uid-"+req.PodNamespace {
		return authorization.PodIdentity{}, protocol.New(protocol.KindAuthentication, "fixture", "owner mismatch")
	}
	return authorization.PodIdentity{Namespace: req.PodNamespace, Name: req.PodName, UID: req.PodUID, IP: "127.0.0.1"}, nil
}

func (a fixtureAuth) AuthorizeSession(ctx context.Context, req authorization.Request) (authorization.Result, *protocol.Error) {
	pod, err := a.AuthenticatePod(ctx, req)
	if err != nil {
		return authorization.Result{}, err
	}
	fixture, ok := a.fixtures[req.PodNamespace]
	if !ok || req.ArtifactName != fixture.ArtifactName || req.ArtifactUID != fixture.ArtifactUID || req.ManifestDigest != fixture.Digest || (req.SourceGeneration != 0 && req.SourceGeneration != fixture.SourceGeneration) {
		return authorization.Result{}, protocol.New(protocol.KindDeniedContent, "fixture", "artifact mismatch")
	}
	return authorization.Result{Pod: pod, DigestHex: store.HexOf(fixture.Digest), SourceGeneration: fixture.SourceGeneration}, nil
}

type loseResponses struct {
	handler http.Handler
	mu      sync.Mutex
	paths   map[string]bool
}

func (l *loseResponses) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	recorder := httptest.NewRecorder()
	l.handler.ServeHTTP(recorder, r)
	key := ""
	if r.Method == http.MethodPost && r.URL.Path == "/v1/transfers" {
		key = "post"
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/transfers/") {
		key = "status"
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ack") {
		key = "ack"
	}
	l.mu.Lock()
	lose := key != "" && !l.paths[key]
	if lose {
		l.paths[key] = true
	}
	l.mu.Unlock()
	if lose {
		http.Error(w, "injected lost response", http.StatusServiceUnavailable)
		return
	}
	for k, values := range recorder.Header() {
		for _, value := range values {
			w.Header().Add(k, value)
		}
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

func TestPythonEngineSourceOverRealBrokerTLS(t *testing.T) {
	root := t.TempDir()
	prepare := startPythonFixture(t)
	var prepared struct {
		Fixtures []pythonFixture `json:"fixtures"`
	}
	prepare.call(t, map[string]any{"mode": "prepare", "root": filepath.Join(root, "fixture")}, &prepared)
	require.Len(t, prepared.Fixtures, 2)
	b, st, _ := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.SetIntentValidator(func(context.Context, LiveIntent) error { return nil })
	tokenPath := filepath.Join(root, "bootstrap")
	require.NoError(t, os.WriteFile(tokenPath, []byte("fixture-bootstrap"), 0o600))
	b.SetBootstrapCredential("fixture-bootstrap")
	controlHandler := NewControlHandler(b)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		controlHandler.ServeHTTP(recorder, r)
		if recorder.Code >= 400 {
			t.Logf("control %s: %d %s", r.URL.Path, recorder.Code, recorder.Body.String())
		}
		for key, values := range recorder.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	defer control.Close()
	for i, item := range prepared.Fixtures {
		if i == 0 {
			publishTree(t, st, store.HexOf(item.Digest), "fixture", item.TotalBytes)
		}
		intent := Intent{
			Key: fmt.Sprintf("intent-%d", i), Namespace: item.Namespace, Digest: item.Digest, TotalBytes: item.TotalBytes,
			Grant: grant(item.Namespace, 1<<20), WorkerID: "source-pod", NodeName: "node", IntentUID: fmt.Sprintf("intent-%d", i),
			IntentGeneration: 1, IntentName: "intent", ArtifactName: item.ArtifactName, BindingUID: "binding-uid-1", BindingName: "binding",
			ReservationID: fmt.Sprintf("reservation-%d", i), Registrations: item.Registrations, ArtifactUID: item.ArtifactUID,
			SourceGeneration: item.SourceGeneration, ManifestSHA256: item.ManifestSHA256, ManifestJSON: item.ManifestJSON, TreePath: item.TreePath, Backend: "nixl",
		}
		_, perr := b.AdmitIntentFill(intent)
		require.Nil(t, perr)
	}
	source := startPythonFixture(t)
	var ready struct {
		Event          string `json:"event"`
		PrivateAddress string `json:"privateAddress"`
	}
	instance := strings.Repeat("a", 32)
	source.call(t, map[string]any{"mode": "source", "bootstrapFile": tokenPath, "controlAddress": strings.TrimPrefix(control.URL, "http://"), "workerID": "source-pod", "workerInstanceID": instance}, &ready)
	require.Equal(t, "ready", ready.Event)
	b.SetPrivateSource(&httpPrivateSource{address: "http://" + ready.PrivateAddress, token: "fixture-bootstrap", client: &http.Client{Timeout: 5 * time.Second}})
	require.Eventually(t, func() bool {
		entries := b.Snapshot()
		return len(entries) == 2 && string(entries[0].State) == "Serving" && string(entries[1].State) == "Serving"
	}, 15*time.Second, 50*time.Millisecond)
	auth := fixtureAuth{fixtures: map[string]pythonFixture{}}
	for _, item := range prepared.Fixtures {
		auth.fixtures[item.Namespace] = item
	}
	loss := &loseResponses{handler: NewSessionHandler(b, auth), paths: map[string]bool{}}
	tlsServer := httptest.NewTLSServer(loss)
	defer tlsServer.Close()
	cert := tlsServer.Certificate()
	ca := filepath.Join(root, "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600))
	serverName := "127.0.0.1"
	if len(cert.DNSNames) > 0 {
		serverName = cert.DNSNames[0]
	}
	endpoint := map[string]any{"version": 1, "endpoints": []any{map[string]string{"manufacturer": "nvidia", "address": strings.TrimPrefix(tlsServer.URL, "https://"), "serverName": serverName, "caFile": ca, "podUID": "source-pod", "podName": "source", "podNamespace": "operator", "nodeName": "node"}}}
	config, err := json.Marshal(endpoint)
	require.NoError(t, err)
	configPath := filepath.Join(root, "config.json")
	require.NoError(t, os.WriteFile(configPath, config, 0o600))
	for _, item := range prepared.Fixtures {
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
		for _, op := range []string{"probe", "start", "fill", "close", "exit"} {
			engine.call(t, map[string]any{"op": op}, &answer)
			require.Equal(t, true, answer["ok"], "operation %s: %v", op, answer)
			if op == "fill" {
				require.Equal(t, item.Expected, answer["sha256"])
			}
		}
	}
	require.True(t, loss.paths["post"])
	require.True(t, loss.paths["status"])
	require.True(t, loss.paths["ack"])
}

func (a fixtureAuth) AuthenticateActivePod(ctx context.Context, req authorization.Request) (authorization.PodIdentity, *protocol.Error) {
	return a.AuthenticatePod(ctx, req)
}
