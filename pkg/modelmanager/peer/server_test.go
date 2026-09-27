package peer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authentication "k8s.io/api/authentication/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

// writableAgain undoes Publish's read-only sealing, so the test's temp dir can be removed.
func writableAgain(root string) func() {
	return func() {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, _ error) error {
			if d.IsDir() {
				_ = os.Chmod(p, 0o755)
			} else {
				_ = os.Chmod(p, 0o644)
			}

			return nil
		})
	}
}

// publishTree publishes a tree of the named files, contents and all, and returns its hex. The
// hex derives from the files, so two different trees never share one. The publish also stores
// the tree's manifest, as the real publish does, with each file's real digest.
func publishTree(t *testing.T, s *store.Store, files map[string]string) string {
	t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	t.Cleanup(writableAgain(s.Root()))
	entries := make([]modelartifact.ManifestEntry, 0, len(files))
	for _, name := range names {
		body := files[name]
		fileSum := sha256.Sum256([]byte(body))
		entries = append(entries, modelartifact.ManifestEntry{
			Path: name, Size: int64(len(body)), Digest: "sha256:" + hex.EncodeToString(fileSum[:]),
		})
	}
	manifest, err := modelartifact.NewManifest(entries)
	require.NoError(t, err)
	treeHex := strings.TrimPrefix(manifest.Digest, "sha256:")
	a, err := s.NewAttempt(treeHex)
	require.NoError(t, err)
	for _, e := range manifest.Entries {
		dest, err := a.FilePath(e.Path)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
		require.NoError(t, os.WriteFile(dest, []byte(files[e.Path]), 0o644))
	}
	require.NoError(t, a.Publish(store.Marker{
		Digest: manifest.Digest, SizeBytes: manifest.SizeBytes, FileCount: manifest.FileCount,
	}))
	full := listing{Digest: manifest.Digest}
	for _, e := range manifest.Entries {
		full.Entries = append(full.Entries, ManifestFile{Path: e.Path, Size: e.Size, Digest: e.Digest})
	}
	stored, err := json.Marshal(full)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(s.PublishedManifestPath(treeHex), stored, 0o644))

	return treeHex
}

func serve(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	return ts
}

func TestServerServesPublishedTreesOnly(t *testing.T) {
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	hex := publishTree(t, st, map[string]string{"config.json": "{}", "sub/weights.bin": "0123456789"})
	ts := serve(t, &Server{Store: st})

	t.Run("a listing answers the tree's files", func(t *testing.T) {
		resp, err := ts.Client().Get(ts.URL + PathPrefix + "trees/" + hex)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var l listing
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&l))
		assert.Equal(t, "sha256:"+hex, l.Digest)
		assert.Len(t, l.Entries, 2)
	})

	t.Run("a whole file answers its bytes", func(t *testing.T) {
		resp, err := ts.Client().Get(ts.URL + PathPrefix + "trees/" + hex + "/files/sub/weights.bin")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "0123456789", string(body))
	})

	t.Run("a range answers its slice", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, ts.URL+PathPrefix+"trees/"+hex+"/files/sub/weights.bin", nil)
		require.NoError(t, err)
		req.Header.Set("Range", "bytes=2-5")
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusPartialContent, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "2345", string(body))
	})

	t.Run("a tree that is not published is not found", func(t *testing.T) {
		other := publishTree(t, st, map[string]string{"x": "x"})
		require.NoError(t, st.RemovePublished(other))
		resp, err := ts.Client().Get(ts.URL + PathPrefix + "trees/" + other)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("a path that leaves the tree is refused", func(t *testing.T) {
		resp, err := ts.Client().Get(ts.URL + PathPrefix + "trees/" + hex + "/files/../../marker")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		// The mux refuses a path with .. before the handler sees it; either way the escape
		// answers nothing from the tree.
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("a digest that is not hex is refused", func(t *testing.T) {
		resp, err := ts.Client().Get(ts.URL + PathPrefix + "trees/not-hex")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

// fakeReviews answers a TokenReview the way the test wants.
type fakeReviews struct {
	username  string
	audiences []string
	err       error
	creates   int
}

func (f *fakeReviews) Create(
	_ context.Context, review *authentication.TokenReview, _ meta.CreateOptions,
) (*authentication.TokenReview, error) {
	f.creates++
	out := &authentication.TokenReview{}
	if f.err != nil {
		return out, f.err
	}
	if f.username == "" {
		out.Status.Error = "the test says no"

		return out, nil
	}
	out.Status.Authenticated = true
	out.Status.User.Username = f.username
	out.Status.Audiences = f.audiences

	return out, nil
}

func authRequest(ts *httptest.Server, hex, token string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, ts.URL+PathPrefix+"trees/"+hex, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	return http.DefaultClient.Do(req)
}

func TestServerTokenAuthAdmitsPluginsOnly(t *testing.T) {
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	hex := publishTree(t, st, map[string]string{"config.json": "{}"})
	reviews := &fakeReviews{
		username: "system:serviceaccount:gpustack-system:gpustack-model-manager", audiences: []string{PeerAudience},
	}
	auth := AuthOptions{
		Mode: AuthToken, Audience: PeerAudience, Namespace: "gpustack-system",
		ServiceAccount: "gpustack-model-manager",
	}
	admit, err := auth.Admit(reviews)
	require.NoError(t, err)
	ts := serve(t, &Server{Store: st, Admit: admit})

	t.Run("a plugin's token is admitted", func(t *testing.T) {
		resp, err := authRequest(ts, hex, "token-a")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("the same token's review is reused", func(t *testing.T) {
		created := reviews.creates
		resp, err := authRequest(ts, hex, "token-a")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, created, reviews.creates, "a second request of the same token reviews once")
	})

	t.Run("no token is refused", func(t *testing.T) {
		resp, err := authRequest(ts, hex, "")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("a token of another account is refused", func(t *testing.T) {
		reviews.username = "system:serviceaccount:team-a:someone-else"
		resp, err := authRequest(ts, hex, "token-b")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

func TestServerMTLSAuthRequiresVerifiedChains(t *testing.T) {
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	auth := AuthOptions{Mode: AuthMTLS}
	admit, err := auth.Admit(nil)
	require.NoError(t, err)
	ts := serve(t, &Server{Store: st, Admit: admit})

	req, err := http.NewRequest(http.MethodGet, ts.URL+PathPrefix+"trees/"+strings.Repeat("a", 64), nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// A plain HTTP request carries no TLS state at all, which is exactly what the check refuses.
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAuthTLSConfigsCarryTheMaterial(t *testing.T) {
	t.Run("token mode sends no client certificate", func(t *testing.T) {
		cfg, err := AuthOptions{Mode: AuthToken}.ClientTLSConfig()
		require.NoError(t, err)
		assert.Empty(t, cfg.Certificates)
		assert.True(t, cfg.InsecureSkipVerify, "the peers' server certificates are self-signed")
	})
	t.Run("mtls mode wants a verified client certificate", func(t *testing.T) {
		auth := AuthOptions{Mode: AuthMTLS, ClientCAPEM: caPEM(t)}
		cfg, err := auth.ServerTLSConfig(nil)
		require.NoError(t, err)
		assert.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)
	})
	t.Run("mtls mode refuses a CA that is no PEM", func(t *testing.T) {
		auth := AuthOptions{Mode: AuthMTLS, ClientCAPEM: []byte("not pem"), ClientCertPEM: []byte("x"), ClientKeyPEM: []byte("y")}
		_, err := auth.ServerTLSConfig(nil)
		assert.Error(t, err)
	})
	t.Run("an unknown mode is refused", func(t *testing.T) {
		_, err := AuthOptions{Mode: "magic"}.ClientTLSConfig()
		assert.Error(t, err)
	})
}

// caPEM mints a one-off CA, PEM-encoded, for the server-side TLS configuration.
func caPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "peer test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
