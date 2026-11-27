package modelforger

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/utils/json"
)

func hostPortFromServer(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return host, port
}

func TestIngestClient_SuccessTokenRotationAndClusterCA(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("projected-token-v1\n"), 0o600))

	var seenAuth []string
	manifestDigest := "sha256:" + strings.Repeat("1", 64)
	dispatchDigest := "sha256:" + strings.Repeat("2", 64)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, modelstore.IngestTreesPath, r.URL.Path)
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))

		var req modelstore.IngestTreeRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Equal(t, "team-a", req.Namespace)
		require.Equal(t, "pod-uid-1", req.PodUID)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(modelstore.IngestTreeResponse{
			ManifestDigest:         manifestDigest,
			DispatchManifestDigest: dispatchDigest,
			SizeBytes:              2048,
			FileCount:              4,
			SeedNode:               "node-1",
		})
	}))
	defer srv.Close()

	// Write server certificate as cluster CA PEM file to verify strict CA validation.
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(caPath, certPEM, 0o600))

	client, err := NewIngestClient(
		WithIngestTokenFile(tokenPath),
		WithIngestCAFile(caPath),
		WithIngestTimeout(5*time.Second),
	)
	require.NoError(t, err)

	host, port := hostPortFromServer(t, srv)
	req := modelstore.IngestTreeRequest{
		Namespace:  "team-a",
		ForgerName: "qwen-forger",
		ForgerUID:  "forger-uid-1",
		PodName:    "qwen-forger-sb-1",
		PodUID:     "pod-uid-1",
	}

	resp, err := client.IngestTree(context.Background(), host, port, req)
	require.NoError(t, err)
	require.Equal(t, manifestDigest, resp.ManifestDigest)
	require.Equal(t, dispatchDigest, resp.DispatchManifestDigest)
	require.Equal(t, "node-1", resp.SeedNode)

	// Rotate projected token on disk and confirm next call reads it freshly.
	require.NoError(t, os.WriteFile(tokenPath, []byte("projected-token-v2\n"), 0o600))
	_, err = client.IngestTree(context.Background(), host, port, req)
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer projected-token-v1", "Bearer projected-token-v2"}, seenAuth)

	// Also verify WithIngestRootCAs path.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	clientPool, err := NewIngestClient(WithIngestTokenFile(tokenPath), WithIngestRootCAs(pool))
	require.NoError(t, err)
	_, err = clientPool.IngestTree(context.Background(), host, port, req)
	require.NoError(t, err)
}

func TestIngestClient_ErrorClassification(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("tok-1"), 0o600))

	cases := []struct {
		name       string
		status     int
		reason     string
		msg        string
		wantTarget error
	}{
		{name: "400 bad request", status: http.StatusBadRequest, reason: "InvalidRequest", msg: "bad fields", wantTarget: ErrIngestBadRequest},
		{name: "401 unauthorized", status: http.StatusUnauthorized, reason: "Unauthorized", msg: "bad token", wantTarget: ErrIngestUnauthorized},
		{name: "403 forbidden", status: http.StatusForbidden, reason: "SandboxNotActive", msg: "pod not active", wantTarget: ErrIngestForbidden},
		{name: "409 conflict", status: http.StatusConflict, reason: "SandboxNotTerminated", msg: "init still running", wantTarget: ErrIngestConflict},
		{name: "422 unprocessable", status: http.StatusUnprocessableEntity, reason: "VerificationFailed", msg: "report invalid", wantTarget: ErrIngestInvalidOutput},
		{name: "507 insufficient storage", status: http.StatusInsufficientStorage, reason: "InsufficientCapacity", msg: "disk full", wantTarget: ErrIngestInsufficientStorage},
		{name: "503 service unavailable", status: http.StatusServiceUnavailable, reason: "NotReady", msg: "starting", wantTarget: ErrIngestServerError},
	}

	req := modelstore.IngestTreeRequest{
		Namespace:  "team-a",
		ForgerName: "qwen-forger",
		PodName:    "sb-1",
		PodUID:     "uid-1",
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"reason": tc.reason,
					"error":  tc.msg,
				})
			}))
			defer srv.Close()

			client, err := NewIngestClient(
				WithIngestTokenFile(tokenPath),
				WithIngestInsecureSkipVerify(true),
			)
			require.NoError(t, err)

			host, port := hostPortFromServer(t, srv)
			_, err = client.IngestTree(context.Background(), host, port, req)
			require.Error(t, err)
			require.ErrorIs(t, err, tc.wantTarget)

			var ingestErr *IngestError
			require.ErrorAs(t, err, &ingestErr)
			require.Equal(t, tc.status, ingestErr.StatusCode)
			require.Equal(t, tc.reason, ingestErr.Reason)
			require.Contains(t, ingestErr.Error(), tc.reason)
		})
	}
}

func TestIngestClient_ValidationMissingTokenAndRedirectRefusal(t *testing.T) {
	t.Parallel()

	// Bad CA file fails NewIngestClient.
	_, err := NewIngestClient(WithIngestCAFile(filepath.Join(t.TempDir(), "missing.crt")))
	require.Error(t, err)

	badCAPath := filepath.Join(t.TempDir(), "bad.crt")
	require.NoError(t, os.WriteFile(badCAPath, []byte("not pem"), 0o600))
	_, err = NewIngestClient(WithIngestCAFile(badCAPath))
	require.Error(t, err)

	// Missing token file fails call.
	client, err := NewIngestClient(
		WithIngestTokenFile(filepath.Join(t.TempDir(), "no-token")),
		WithIngestInsecureSkipVerify(true),
	)
	require.NoError(t, err)

	_, err = client.IngestTree(context.Background(), "", 32444, modelstore.IngestTreeRequest{})
	require.ErrorIs(t, err, ErrIngestBadRequest)

	_, err = client.IngestTree(context.Background(), "127.0.0.1", 32444, modelstore.IngestTreeRequest{
		Namespace:  "ns",
		ForgerName: "fg",
		PodName:    "pod",
		PodUID:     "uid",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "projected ingest token")

	// Refuses HTTP redirects.
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("tok"), 0o600))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	redirClient, err := NewIngestClient(
		WithIngestTokenFile(tokenPath),
		WithIngestInsecureSkipVerify(true),
	)
	require.NoError(t, err)
	host, port := hostPortFromServer(t, srv)
	_, err = redirClient.IngestTree(context.Background(), host, port, modelstore.IngestTreeRequest{
		Namespace:  "ns",
		ForgerName: "fg",
		PodName:    "pod",
		PodUID:     "uid",
	})
	require.ErrorIs(t, err, ErrIngestRedirectRefused)

	// Empty token file, invalid port, oversized response, and invalid response digests.
	emptyTok := filepath.Join(t.TempDir(), "empty-tok")
	require.NoError(t, os.WriteFile(emptyTok, []byte("   \n"), 0o600))
	emptyTokClient, err := NewIngestClient(WithIngestTokenFile(emptyTok), WithIngestInsecureSkipVerify(true))
	require.NoError(t, err)
	_, err = emptyTokClient.IngestTree(context.Background(), host, port, modelstore.IngestTreeRequest{
		Namespace: "ns", ForgerName: "fg", PodName: "pod", PodUID: "uid",
	})
	require.Error(t, err)

	_, err = redirClient.IngestTree(context.Background(), host, 70000, modelstore.IngestTreeRequest{
		Namespace: "ns", ForgerName: "fg", PodName: "pod", PodUID: "uid",
	})
	require.ErrorIs(t, err, ErrIngestBadRequest)

	bigSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 512)))
	}))
	defer bigSrv.Close()
	bigHost, bigPort := hostPortFromServer(t, bigSrv)
	smallClient, err := NewIngestClient(
		WithIngestTokenFile(tokenPath),
		WithIngestInsecureSkipVerify(true),
		WithIngestMaxResponseBytes(64),
	)
	require.NoError(t, err)
	_, err = smallClient.IngestTree(context.Background(), bigHost, bigPort, modelstore.IngestTreeRequest{
		Namespace: "ns", ForgerName: "fg", PodName: "pod", PodUID: "uid",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeded")

	// IngestError.Error() formatting variants.
	require.Contains(t, (&IngestError{StatusCode: 500, Message: "boom"}).Error(), "boom")
	require.Contains(t, (&IngestError{StatusCode: 500, Reason: "Internal"}).Error(), "Internal")
	require.Contains(t, (&IngestError{StatusCode: 500}).Error(), "500")
}
