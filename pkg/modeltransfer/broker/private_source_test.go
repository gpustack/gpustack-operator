package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func shortUnixSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if len(path) < 96 {
		return path
	}
	shortDir, err := os.MkdirTemp("/tmp", "mt-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
	return filepath.Join(shortDir, name)
}

func TestNewPrivateSourceWithAddress(t *testing.T) {
	reqID := strings.Repeat("a", 32)

	t.Run("default NewPrivateSource targets loopback 9252", func(t *testing.T) {
		src, ok := NewPrivateSource("secret-token").(*httpPrivateSource)
		require.True(t, ok)
		assert.Equal(t, "http://127.0.0.1:9252", src.address)
		assert.Equal(t, "secret-token", src.token)
		transport, ok := src.client.Transport.(*http.Transport)
		require.True(t, ok)
		assert.Nil(t, transport.Proxy)
		assert.Equal(t, 64, transport.MaxConnsPerHost)
	})

	t.Run("unix domain socket call succeeds", func(t *testing.T) {
		sockPath := shortUnixSocketPath(t, "worker.sock")
		ln, err := net.Listen("unix", sockPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		var gotPath, gotAuth, gotWait, gotHost string
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotWait = r.Header.Get("X-GPUStack-Wait-Ms")
				gotHost = r.Host
				_ = json.NewEncoder(w).Encode(PrivateReply{
					RequestID: reqID,
					State:     "submitted",
				})
			}),
		}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })

		src := NewPrivateSourceWithAddress("unix://"+sockPath, "uds-token")
		reply, err := src.Call(context.Background(), "/v1/operations", map[string]string{"requestID": reqID})
		require.NoError(t, err)
		assert.Equal(t, reqID, reply.RequestID)
		assert.Equal(t, "submitted", reply.State)
		assert.Equal(t, "/v1/operations", gotPath)
		assert.Equal(t, "uds-token", gotAuth)
		assert.Equal(t, "50", gotWait)
		assert.Equal(t, "unix", gotHost)
	})

	t.Run("tcp host:port and http://host:port both succeed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "tcp-token", r.Header.Get("Authorization"))
			assert.Equal(t, "/v1/operations/status", r.URL.Path)
			_ = json.NewEncoder(w).Encode(PrivateReply{
				RequestID: reqID,
				State:     "terminal",
				Verdict:   "succeeded",
			})
		}))
		t.Cleanup(srv.Close)

		hostPort := strings.TrimPrefix(srv.URL, "http://")
		for _, tc := range []struct {
			name string
			addr string
		}{
			{name: "bare host:port", addr: hostPort},
			{name: "http scheme", addr: srv.URL},
		} {
			t.Run(tc.name, func(t *testing.T) {
				src := NewPrivateSourceWithAddress(tc.addr, "tcp-token")
				reply, err := src.Call(context.Background(), "/v1/operations/status", map[string]string{"requestID": reqID})
				require.NoError(t, err)
				assert.Equal(t, "terminal", reply.State)
				assert.Equal(t, "succeeded", reply.Verdict)
			})
		}
	})

	t.Run("invalid and non-local addresses are refused", func(t *testing.T) {
		for _, addr := range []string{
			"",
			"unix://",
			"unix://relative.sock",
			"unix:///",
			"unix:///run/model-transfer/worker.sock?x=1",
			"unix:///run/model-transfer/worker.sock#frag",
			"http://10.0.0.2:9252",
			"example.com:9252",
			"https://127.0.0.1:9252",
			"0.0.0.0:9252",
			"[::]:9252",
			":9252",
			"127.0.0.1",
			"127.0.0.1:0",
			"127.0.0.1:70000",
			"http://user@127.0.0.1:9252",
			"http://127.0.0.1:9252/extra",
			"http://127.0.0.1:9252?a=1",
			"http://127.0.0.1:9252#frag",
		} {
			require.Error(t, ValidatePrivateAddress(addr), "address %q should fail validation", addr)
			src := NewPrivateSourceWithAddress(addr, "token")
			_, err := src.Call(context.Background(), "/v1/operations/status", map[string]string{"requestID": reqID})
			require.Error(t, err, "address %q should fail closed", addr)
		}
	})

	t.Run("valid local addresses pass validation", func(t *testing.T) {
		for _, addr := range []string{
			"127.0.0.1:9252",
			"http://127.0.0.1:9252",
			"[::1]:9252",
			"http://[::1]:9252",
			"localhost:9252",
			"http://localhost:9252",
			"unix:///run/model-transfer/worker.sock",
		} {
			require.NoError(t, ValidatePrivateAddress(addr), "address %q should be valid", addr)
		}
	})

	t.Run("http redirect on loopback tcp is refused", func(t *testing.T) {
		var redirected bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			redirected = true
			_ = json.NewEncoder(w).Encode(PrivateReply{RequestID: reqID, State: "submitted"})
		}))
		t.Cleanup(target.Close)

		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
		}))
		t.Cleanup(redirector.Close)

		src := NewPrivateSourceWithAddress(redirector.URL, "tcp-token")
		_, err := src.Call(context.Background(), "/v1/operations/status", map[string]string{"requestID": reqID})
		require.ErrorIs(t, err, errPrivateRedirect)
		assert.False(t, redirected)
	})

	t.Run("unreachable unix socket reports observation unavailable", func(t *testing.T) {
		missing := shortUnixSocketPath(t, "missing.sock")
		src := NewPrivateSourceWithAddress("unix://"+missing, "token")
		_, err := src.Call(context.Background(), "/v1/operations/status", map[string]string{"requestID": reqID})
		require.Error(t, err)
		var obsErr *privateObservationError
		assert.True(t, errors.As(err, &obsErr))
	})
}
