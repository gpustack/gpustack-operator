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

	t.Run("invalid unix socket paths are refused", func(t *testing.T) {
		for _, addr := range []string{"unix://", "unix://relative.sock", "unix:///"} {
			src := NewPrivateSourceWithAddress(addr, "token")
			_, err := src.Call(context.Background(), "/v1/operations/status", map[string]string{"requestID": reqID})
			require.Error(t, err, "address %q should fail", addr)
		}
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
