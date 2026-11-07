package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authentication "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

type sessionReviewer struct{}

func (sessionReviewer) Create(_ context.Context, review *authentication.TokenReview,
	_ meta.CreateOptions,
) (*authentication.TokenReview, error) {
	return &authentication.TokenReview{Status: authentication.TokenReviewStatus{
		Authenticated: review.Spec.Token == "projected-token",
		Audiences:     review.Spec.Audiences,
		User: authentication.UserInfo{
			Username: "system:serviceaccount:tenant-a:reader",
			Extra: map[string]authentication.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"engine-1"},
				"authentication.kubernetes.io/pod-uid":  {"pod-uid-tenant-a"},
			},
		},
	}}, nil
}

func TestSessionCleanupAuthenticatesPersistedOwner(t *testing.T) {
	cases := []struct {
		name        string
		artifactUID string
		digest      string
		bearer      string
		terminating bool
		wantStatus  int
	}{
		{name: "artifact absent", bearer: "Bearer projected-token", wantStatus: http.StatusOK},
		{name: "terminating Pod cleanup", bearer: "Bearer projected-token", terminating: true, wantStatus: http.StatusOK},
		{name: "foreign artifact", artifactUID: "foreign", bearer: "Bearer projected-token", wantStatus: http.StatusForbidden},
		{name: "foreign digest", digest: "sha256:" + strings.Repeat("b", 64), bearer: "Bearer projected-token", wantStatus: http.StatusForbidden},
		{name: "unreviewed token", bearer: "Bearer other-token", wantStatus: http.StatusForbidden},
		{name: "missing Bearer scheme", bearer: "projected-token", wantStatus: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _, digest := newBroker(t, defaultLimits())
			var generation, secret string
			admitServe(t, b, digest, &generation, &secret)
			owner := testOwner("tenant-a")
			owner.Digest = digest
			session, err := b.AdmitSession(t.Context(), owner, owner.Namespace, generation)
			require.Nil(t, err)
			pod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{Namespace: owner.Namespace, Name: "engine-1", UID: types.UID(owner.PodUID)},
				Spec:       core.PodSpec{ServiceAccountName: "reader"},
			}
			if tc.terminating {
				now := meta.Now()
				pod.DeletionTimestamp = &now
				pod.Finalizers = []string{"test.gpustack.ai/hold"}
			}
			auth := &authorization.Authorizer{
				Reader:   ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pod).Build(),
				Reviewer: sessionReviewer{},
			}
			artifactUID, requestedDigest := owner.ArtifactUID, owner.Digest
			if tc.artifactUID != "" {
				artifactUID = tc.artifactUID
			}
			if tc.digest != "" {
				requestedDigest = tc.digest
			}
			body, marshalErr := json.Marshal(map[string]string{
				"podNamespace": owner.Namespace, "podName": pod.Name, "podUID": owner.PodUID,
				"artifactUID": artifactUID, "manifestDigest": requestedDigest,
			})
			require.NoError(t, marshalErr)
			req := httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+session.ID, bytes.NewReader(body))
			req.Header.Set("Authorization", tc.bearer)
			recorder := httptest.NewRecorder()
			NewSessionHandler(b, auth).ServeHTTP(recorder, req)
			require.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			assert.NotContains(t, recorder.Body.String(), "projected-token")
			require.Len(t, b.Snapshot(), 1)
			if tc.wantStatus == http.StatusOK {
				assert.Zero(t, b.Snapshot()[0].ActiveSessions)
			} else {
				assert.EqualValues(t, 1, b.Snapshot()[0].ActiveSessions)
			}
		})
	}
}

func TestSessionCleanupRejectsMalformedBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "trailing object", body: "{} {}"},
		{name: "trailing text", body: "{} secret"},
		{name: "unknown field", body: `{"unexpected":true}`},
		{name: "oversized body", body: strings.Repeat(" ", maxControlBody) + "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodDelete, "/v1/sessions/opaque", strings.NewReader(tc.body))
			NewSessionHandler(nil, nil).ServeHTTP(recorder, req)
			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			assert.NotContains(t, recorder.Body.String(), "secret")
		})
	}
}

func newControl(t *testing.T) (*httptest.Server, *Broker, string, string) {
	t.Helper()
	b, _, digest := newBroker(t, defaultLimits())
	admission, err := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, err)
	server := httptest.NewServer(NewControlHandler(b))
	t.Cleanup(server.Close)
	return server, b, admission.Generation, admission.WorkerSecret
}

func post(t *testing.T, url, path string, body map[string]any) (int, map[string]any) {
	t.Helper()
	if _, ok := body["workerID"]; ok {
		body["workerInstanceID"] = "test-native-instance"
	}
	if path == "/v1/drain-ack" {
		body["nativeReleased"] = true
	}
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(url+path, "application/json", bytes.NewReader(encoded))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var decoded map[string]any
	// A plain-text refusal body (unknown path) is not JSON; read it as none.
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

func TestControlRegisterAndHeartbeat(t *testing.T) {
	server, b, generation, secret := newControl(t)

	// A foreign secret is refused and nothing of the secret is echoed.
	status, body := post(t, server.URL, "/v1/register", map[string]any{
		"workerID": "source-worker-a", "generation": generation, "secret": "wrong",
	})
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, string(protocol.KindAuthentication), body["kind"])
	assert.NotContains(t, body, "wrong")

	status, body = post(t, server.URL, "/v1/register", map[string]any{
		"workerID": "source-worker-a", "generation": generation, "secret": secret,
	})
	require.Equal(t, http.StatusOK, status)
	leaseID, _ := body["leaseID"].(string)
	require.NotEmpty(t, leaseID)
	status, _ = post(t, server.URL, "/v1/native-start", map[string]any{
		"workerID": "source-worker-a", "generation": generation, "leaseID": leaseID, "secret": secret,
	})
	require.Equal(t, http.StatusOK, status)

	// A count without descriptors claims memory nothing can address: the
	// wire now requires one native descriptor per reported registration.
	status, body = post(t, server.URL, "/v1/filled", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "registrations": 2,
	})
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, string(protocol.KindAuthentication), body["kind"])
	status, _ = post(t, server.URL, "/v1/filled", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "registrations": 2,
		"descriptors": []map[string]any{
			{
				"name": "model.norm.weight", "addr": 4096, "length": 6,
				"memoryType": "HOST", "backend": "nixl", "workerUID": "source-worker-a",
			},
			{
				"name": "model.bias", "addr": 8192, "length": 6,
				"memoryType": "HOST", "backend": "nixl", "workerUID": "source-worker-a",
			},
		},
	})
	require.Equal(t, http.StatusOK, status)

	// A heartbeat with a live lease answers no action, and a draining
	// generation answers drain.
	status, body = post(t, server.URL, "/v1/heartbeat", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret,
	})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, ActionNone, body["action"])

	// A reader is in flight while the generation drains.
	session, err := b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", generation)
	require.Nil(t, err)
	status, body = post(t, server.URL, "/v1/heartbeat", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "drainRequested": true,
	})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, ActionDrain, body["action"])

	// A drain ack before the sessions drain is a conflict the worker retries.
	status, body = post(t, server.URL, "/v1/drain-ack", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret,
	})
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, string(protocol.KindOperationState), body["kind"])

	require.Nil(t, b.ReleaseSession(t.Context(), testOwner("tenant-a"), session.ID))
	status, _ = post(t, server.URL, "/v1/drain-ack", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret,
	})
	assert.Equal(t, http.StatusOK, status)
}

func TestControlQuarantineAndUnknownPaths(t *testing.T) {
	server, b, generation, secret := newControl(t)
	status, body := post(t, server.URL, "/v1/register", map[string]any{
		"workerID": "source-worker-a", "generation": generation, "secret": secret,
	})
	require.Equal(t, http.StatusOK, status)
	leaseID := body["leaseID"].(string)
	require.Nil(t, b.FillVerified(t.Context(), generation, "source-worker-a", leaseID, secret, 1,
		[]RegistrationDescriptor{{
			Name: "model.norm.weight", Addr: 4096, Length: 6,
			MemoryType: "HOST", Backend: "nixl", WorkerUID: "source-worker-a",
		}}))

	status, _ = post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "reason": "unknown operation outcome",
	})
	require.Equal(t, http.StatusOK, status)
	entries := b.Snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, "Quarantined", string(entries[0].State))

	// Unknown paths are refused, not routed somewhere by accident.
	status, _ = post(t, server.URL, "/v1/session", map[string]any{})
	assert.Equal(t, http.StatusNotFound, status)

	// Malformed JSON is a client error.
	resp, err := http.Post(server.URL+"/v1/register", "application/json",
		strings.NewReader("{"))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestControlResponsesCarryNoSecrets(t *testing.T) {
	server, _, generation, secret := newControl(t)
	status, body := post(t, server.URL, "/v1/register", map[string]any{
		"workerID": "source-worker-a", "generation": generation, "secret": secret,
	})
	require.Equal(t, http.StatusOK, status)
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), secret)
	assert.NotContains(t, string(encoded), "tenant-a")
}

func TestControlNativeStartRequiresDurableInstance(t *testing.T) {
	server, b, generation, secret := newControl(t)
	message := func(instance string) map[string]any {
		return map[string]any{
			"workerID": "source-worker-a", "generation": generation,
			"secret": secret, "workerInstanceID": instance,
		}
	}
	// Use a direct request here so the normal test helper cannot fill the instance.
	send := func(path string, body map[string]any) int {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		resp, err := http.Post(server.URL+path, "application/json", bytes.NewReader(raw))
		require.NoError(t, err)
		defer resp.Body.Close()
		if path == "/v1/register" && resp.StatusCode == http.StatusOK {
			var answer map[string]any
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&answer))
			body["leaseID"] = answer["leaseID"]
		}
		return resp.StatusCode
	}
	missing := message("")
	assert.Equal(t, http.StatusForbidden, send("/v1/register", missing))
	first := message("instance-a")
	require.Equal(t, http.StatusOK, send("/v1/register", first))
	require.Equal(t, http.StatusOK, send("/v1/native-start", first))
	require.Equal(t, http.StatusOK, send("/v1/native-start", first))
	b.mu.Lock()
	record := *b.records[generation]
	b.mu.Unlock()
	assert.True(t, record.NativeStarted)
	assert.Equal(t, "instance-a", record.NativeInstance)
	assert.Equal(t, http.StatusConflict, send("/v1/register", message("instance-b")))
	b.mu.Lock()
	assert.Equal(t, StateQuarantined, b.records[generation].State)
	b.mu.Unlock()
}

func TestAssignmentPollRotatesWithinResponseBound(t *testing.T) {
	b := &Broker{records: map[string]*GenerationRecord{}, bootstrap: []byte("token"), assignmentSizes: map[string]int{}}
	for _, generation := range []string{"1", "2", "3"} {
		b.records[generation] = &GenerationRecord{
			Generation: generation, IntentKey: generation,
			State: StateFilling, WorkerSecret: "secret", ManifestJSON: "{}",
		}
		b.assignmentSizes[generation] = protocol.MaxManifestResponseBytes/2 - 1024
	}
	first, err := b.AssignmentsFor("token")
	require.Nil(t, err)
	second, err := b.AssignmentsFor("token")
	require.Nil(t, err)
	assert.Len(t, first, 2)
	assert.Len(t, second, 2)
	assert.Equal(t, "3", second[0].Generation)
}

func TestAssignmentPollCountAndOversizedRefusal(t *testing.T) {
	for _, tt := range []struct {
		name        string
		count       int
		encodedSize int
		want        int
		wantErr     bool
	}{
		{name: "sixteen", count: 16, encodedSize: 1024, want: 16},
		{name: "seventeen", count: 17, encodedSize: 1024, want: 16},
		{name: "full raw manifest escaped", count: 1, encodedSize: 6*protocol.MaxManifestBytes + 1024, want: 1},
		{name: "oversized", count: 1, encodedSize: protocol.MaxManifestResponseBytes, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := &Broker{records: map[string]*GenerationRecord{}, bootstrap: []byte("token"), assignmentSizes: map[string]int{}}
			for i := range tt.count {
				generation := fmt.Sprint(i)
				b.records[generation] = &GenerationRecord{Generation: generation, IntentKey: generation, State: StateFilling, WorkerSecret: "secret"}
				b.assignmentSizes[generation] = tt.encodedSize
			}
			got, err := b.AssignmentsFor("token")
			if tt.wantErr {
				require.NotNil(t, err)
				return
			}
			require.Nil(t, err)
			require.Len(t, got, tt.want)
		})
	}
}

func TestOversizedAssignmentDoesNotBlockOtherGenerations(t *testing.T) {
	b := &Broker{records: map[string]*GenerationRecord{}, bootstrap: []byte("token"), assignmentSizes: map[string]int{}}
	for i, size := range []int{protocol.MaxManifestResponseBytes, 1024} {
		generation := fmt.Sprint(i)
		b.records[generation] = &GenerationRecord{Generation: generation, IntentKey: generation, State: StateFilling, WorkerSecret: "secret"}
		b.assignmentSizes[generation] = size
	}
	_, perr := b.AssignmentsFor("token")
	require.NotNil(t, perr)
	assignments, perr := b.AssignmentsFor("token")
	require.Nil(t, perr)
	require.Len(t, assignments, 1)
	require.Equal(t, "1", assignments[0].Generation)
}
