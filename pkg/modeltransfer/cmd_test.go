package modeltransfer

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func shortSocketPath(t *testing.T, name string) string {
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

func TestCanceledLeaseMonitorDoesNotReportSuccessAsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failures := make(chan error, 1)
	cfg := &Config{}
	cfg.sweepLeases(ctx, failures)
	select {
	case err := <-failures:
		t.Fatalf("cancellation must not race the server shutdown with a monitor result: %v", err)
	default:
	}
}

func TestListenControlUnixAndTCP(t *testing.T) {
	t.Run("unix reclaims stale socket sets 0600 serves HTTP and removes on close", func(t *testing.T) {
		sockPath := shortSocketPath(t, "control.sock")
		stale, err := net.Listen("unix", sockPath)
		require.NoError(t, err)
		stale.(*net.UnixListener).SetUnlinkOnClose(false)
		require.NoError(t, stale.Close())

		ln, err := listenControl("unix://" + sockPath)
		require.NoError(t, err)

		info, err := os.Stat(sockPath)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("ok"))
			}),
		}
		go func() { _ = srv.Serve(ln) }()

		client := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
				},
			},
		}
		resp, err := client.Get("http://unix/ping")
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		assert.Equal(t, "ok", string(body))

		require.NoError(t, srv.Close())
		_ = ln.Close()
		_, err = os.Stat(sockPath)
		assert.True(t, os.IsNotExist(err), "closing unix control listener must remove socket file")
	})

	t.Run("unix refuses existing regular file and leaves it untouched", func(t *testing.T) {
		filePath := shortSocketPath(t, "regular-file")
		require.NoError(t, os.WriteFile(filePath, []byte("keep-me"), 0o600))

		ln, err := listenControl("unix://" + filePath)
		require.Error(t, err)
		assert.Nil(t, ln)

		got, err := os.ReadFile(filePath)
		require.NoError(t, err)
		assert.Equal(t, "keep-me", string(got))
	})

	t.Run("unix refuses active live listener without detaching it", func(t *testing.T) {
		sockPath := shortSocketPath(t, "live.sock")
		live, err := net.Listen("unix", sockPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = live.Close() })

		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("still-alive"))
			}),
		}
		go func() { _ = srv.Serve(live) }()
		t.Cleanup(func() { _ = srv.Close() })

		ln, err := listenControl("unix://" + sockPath)
		require.Error(t, err)
		assert.Nil(t, ln)

		client := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
				},
			},
		}
		resp, err := client.Get("http://unix/health")
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		assert.Equal(t, "still-alive", string(body))
	})

	t.Run("unix close preserves replacement path with different inode", func(t *testing.T) {
		sockPath := shortSocketPath(t, "replace.sock")
		ln, err := listenControl("unix://" + sockPath)
		require.NoError(t, err)

		require.NoError(t, os.Remove(sockPath))
		require.NoError(t, os.WriteFile(sockPath, []byte("replacement"), 0o600))

		require.NoError(t, ln.Close())
		got, err := os.ReadFile(sockPath)
		require.NoError(t, err)
		assert.Equal(t, "replacement", string(got))
	})

	t.Run("unix rejects invalid paths and non-loopback tcp addresses", func(t *testing.T) {
		for _, addr := range []string{
			"unix://",
			"unix://relative.sock",
			"unix:///",
			"unix:///tmp/control.sock?x=1",
			"unix:///tmp/control.sock#frag",
			"0.0.0.0:9250",
			"10.0.0.2:9250",
			"example.com:9250",
			"https://127.0.0.1:9250",
		} {
			ln, err := listenControl(addr)
			require.Error(t, err, "address %q should fail", addr)
			assert.Nil(t, ln)
		}
	})

	t.Run("unix preserves existing socket when probe fails without ECONNREFUSED", func(t *testing.T) {
		sockPath := shortSocketPath(t, "noaccess.sock")
		live, err := net.Listen("unix", sockPath)
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = os.Chmod(sockPath, 0o600)
			_ = live.Close()
		})
		require.NoError(t, os.Chmod(sockPath, 0o000))

		ln, err := listenControl("unix://" + sockPath)
		require.Error(t, err)
		assert.Nil(t, ln)

		fi, statErr := os.Lstat(sockPath)
		require.NoError(t, statErr, "non-refused socket must remain intact")
		assert.NotZero(t, fi.Mode()&os.ModeSocket)
	})

	t.Run("tcp preserves default TCP listen behavior", func(t *testing.T) {
		ln, err := listenControl("127.0.0.1:0")
		require.NoError(t, err)
		defer ln.Close()
		assert.Equal(t, "tcp", ln.Addr().Network())
	})
}
