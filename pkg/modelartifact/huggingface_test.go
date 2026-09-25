package modelartifact

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testCommit = "0123456789abcdef0123456789abcdef01234567"
	testToken  = "test-token-value-never-in-errors"
)

// fakeHub is a Hub whose answers each case writes as plain handlers, keyed by method and path.
type fakeHub struct {
	server *httptest.Server
	routes map[string]http.HandlerFunc
	// auth records the Authorization header of every request, by path.
	auth map[string]string
	hits atomic.Int64
}

func newFakeHub(t *testing.T, routes map[string]http.HandlerFunc) *fakeHub {
	t.Helper()
	h := &fakeHub{routes: routes, auth: map[string]string{}}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		h.auth[r.URL.EscapedPath()] = r.Header.Get("Authorization")
		route, ok := h.routes[r.Method+" "+r.URL.EscapedPath()]
		if !ok {
			w.Header().Set("X-Error-Code", "EntryNotFound")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		route(w, r)
	}))
	t.Cleanup(h.server.Close)

	return h
}

func (h *fakeHub) client() *HuggingFace {
	return &HuggingFace{Endpoint: h.server.URL, Client: h.server.Client()}
}

func jsonAnswer(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func statusAnswer(status int, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if code != "" {
			w.Header().Set("X-Error-Code", code)
		}
		w.WriteHeader(status)
	}
}

func treeFile(path string, size int64, oid string) map[string]any {
	return map[string]any{"type": "file", "path": path, "size": size, "oid": oid}
}

func treeLFSFile(path string, size int64, sha string) map[string]any {
	e := treeFile(path, size, strings.Repeat("9", 40))
	e["lfs"] = map[string]any{"oid": sha, "size": size, "pointerSize": 134}
	return e
}

var (
	testConfigFile = treeFile("config.json", 659, strings.Repeat("b", 40))
	testWeightFile = treeLFSFile("model.safetensors", 100, strings.Repeat("a", 64))
	testTokenFile  = treeFile("sub/tokenizer.json", 7, strings.Repeat("c", 40))
	testSubDir     = map[string]any{"type": "directory", "path": "sub", "oid": strings.Repeat("d", 40), "size": 0}
)

const (
	revisionPath = "/api/models/owner/repo/revision/"
	treePath     = "/api/models/owner/repo/tree/" + testCommit
)

func revisionRoute(revision string) string {
	return "GET " + revisionPath + revision
}

func TestHuggingFaceResolve(t *testing.T) {
	want, err := NewManifest([]ManifestEntry{
		{Path: "config.json", Size: 659, Digest: DigestGitSHA1 + ":" + strings.Repeat("b", 40)},
		{Path: "model.safetensors", Size: 100, Digest: DigestSHA256 + ":" + strings.Repeat("a", 64)},
		{Path: "sub/tokenizer.json", Size: 7, Digest: DigestGitSHA1 + ":" + strings.Repeat("c", 40)},
	})
	require.NoError(t, err)

	cases := []struct {
		name     string
		revision string
		routes   func(hub *fakeHub) map[string]http.HandlerFunc
	}{
		{
			name:     "a branch over one page",
			revision: "main",
			routes: func(*fakeHub) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					revisionRoute("main"): jsonAnswer(map[string]string{"sha": testCommit}),
					"GET " + treePath:     jsonAnswer([]any{testConfigFile, testWeightFile, testSubDir, testTokenFile}),
				}
			},
		},
		{
			name:     "a revision holding a slash is one path segment",
			revision: "refs/pr/1",
			routes: func(*fakeHub) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					revisionRoute("refs%2Fpr%2F1"): jsonAnswer(map[string]string{"sha": testCommit}),
					"GET " + treePath:              jsonAnswer([]any{testConfigFile, testWeightFile, testTokenFile}),
				}
			},
		},
		{
			name:     "every page the Link header names, in any order",
			revision: "main",
			routes: func(hub *fakeHub) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					revisionRoute("main"): jsonAnswer(map[string]string{"sha": testCommit}),
					"GET " + treePath: func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Query().Get("cursor") {
						case "":
							w.Header().Set("Link", fmt.Sprintf(`<%s%s?recursive=true&cursor=p2>; rel="next"`, hub.server.URL, treePath))
							_ = json.NewEncoder(w).Encode([]any{testTokenFile, testSubDir})
						case "p2":
							w.Header().Set("Link", `<`+treePath+`?recursive=true&cursor=p3>; rel="next"`)
							_ = json.NewEncoder(w).Encode([]any{testWeightFile})
						default:
							_ = json.NewEncoder(w).Encode([]any{testConfigFile})
						}
					},
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := newFakeHub(t, nil)
			hub.routes = c.routes(hub)

			got, err := hub.client().Resolve(context.Background(), "owner/repo", c.revision, testToken)
			require.NoError(t, err)
			assert.Equal(t, testCommit, got.Commit)
			assert.Equal(t, want, got.Manifest)
			for path, auth := range hub.auth {
				assert.Equal(t, "Bearer "+testToken, auth, path)
			}
		})
	}
}

func TestHuggingFaceResolveWithoutToken(t *testing.T) {
	hub := newFakeHub(t, map[string]http.HandlerFunc{
		revisionRoute("main"): jsonAnswer(map[string]string{"sha": testCommit}),
		"GET " + treePath:     jsonAnswer([]any{testConfigFile}),
	})

	_, err := hub.client().Resolve(context.Background(), "owner/repo", "main", "")
	require.NoError(t, err)
	for path, auth := range hub.auth {
		assert.Empty(t, auth, path)
	}
}

func TestHuggingFaceResolveReasons(t *testing.T) {
	good := jsonAnswer(map[string]string{"sha": testCommit})
	cases := []struct {
		name       string
		revision   http.HandlerFunc
		tree       http.HandlerFunc
		maxEntries int
		wantReason string
	}{
		{name: "an unknown revision", revision: statusAnswer(http.StatusNotFound, "RevisionNotFound"), wantReason: ReasonRevisionNotFound},
		{name: "401 without an error code", revision: statusAnswer(http.StatusUnauthorized, ""), wantReason: ReasonAccessDenied},
		{name: "403", revision: statusAnswer(http.StatusForbidden, ""), wantReason: ReasonAccessDenied},
		{name: "404 RepoNotFound", revision: statusAnswer(http.StatusNotFound, "RepoNotFound"), wantReason: ReasonAccessDenied},
		{name: "GatedRepo", revision: statusAnswer(http.StatusUnauthorized, "GatedRepo"), wantReason: ReasonAccessDenied},
		{
			name:     "a tree masking an LFS digest",
			revision: good,
			tree: jsonAnswer([]any{
				testConfigFile,
				treeLFSFile("model.safetensors", 100, strings.Repeat("*", 64)),
			}),
			wantReason: ReasonAccessDenied,
		},
		{name: "a server error", revision: statusAnswer(http.StatusBadGateway, ""), wantReason: ReasonSourceUnavailable},
		{name: "a rate limit", revision: statusAnswer(http.StatusTooManyRequests, ""), wantReason: ReasonSourceUnavailable},
		{
			name: "an unreadable body",
			revision: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("<html>"))
			},
			wantReason: ReasonSourceUnavailable,
		},
		{name: "a short commit answered", revision: jsonAnswer(map[string]string{"sha": "0123456"}), wantReason: ReasonSourceUnavailable},
		{name: "an empty commit", revision: good, tree: jsonAnswer([]any{testSubDir}), wantReason: ReasonEmptyManifest},
		{
			name:       "an unknown entry type",
			revision:   good,
			tree:       jsonAnswer([]any{map[string]any{"type": "submodule", "path": "x"}}),
			wantReason: ReasonInvalidManifest,
		},
		{
			name:     "an LFS size disagreeing with the file size",
			revision: good,
			tree: jsonAnswer([]any{map[string]any{
				"type": "file", "path": "w", "size": 1, "oid": strings.Repeat("9", 40),
				"lfs": map[string]any{"oid": strings.Repeat("a", 64), "size": 2},
			}}),
			wantReason: ReasonInvalidManifest,
		},
		{
			name:       "a path escaping the repository",
			revision:   good,
			tree:       jsonAnswer([]any{treeFile("../x", 1, strings.Repeat("b", 40))}),
			wantReason: ReasonInvalidManifest,
		},
		{
			name:       "more entries than the bound",
			revision:   good,
			tree:       jsonAnswer([]any{testConfigFile, testSubDir, testTokenFile}),
			maxEntries: 2,
			wantReason: ReasonManifestTooLarge,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			routes := map[string]http.HandlerFunc{revisionRoute("main"): c.revision}
			if c.tree != nil {
				routes["GET "+treePath] = c.tree
			}
			hub := newFakeHub(t, routes)
			client := hub.client()
			client.MaxEntries = c.maxEntries

			_, err := client.Resolve(context.Background(), "owner/repo", "main", testToken)
			require.Error(t, err)
			assert.Equal(t, c.wantReason, ReasonOf(err))
			assert.NotContains(t, err.Error(), testToken)
		})
	}
}

func TestHuggingFaceResolveRefusesANextPageOnAnotherHost(t *testing.T) {
	hub := newFakeHub(t, map[string]http.HandlerFunc{
		revisionRoute("main"): jsonAnswer(map[string]string{"sha": testCommit}),
		"GET " + treePath: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Link", `<https://elsewhere.invalid/next>; rel="next"`)
			_ = json.NewEncoder(w).Encode([]any{testConfigFile})
		},
	})

	_, err := hub.client().Resolve(context.Background(), "owner/repo", "main", testToken)
	require.Error(t, err)
	assert.Equal(t, ReasonSourceUnavailable, ReasonOf(err))
	assert.Contains(t, err.Error(), "another host")
}

func TestHuggingFaceValidToken(t *testing.T) {
	cases := []struct {
		name       string
		answer     http.HandlerFunc
		want       bool
		wantReason string
	}{
		{name: "accepted", answer: jsonAnswer(map[string]string{"name": "someone"}), want: true},
		{name: "rejected", answer: statusAnswer(http.StatusUnauthorized, "")},
		{name: "unavailable", answer: statusAnswer(http.StatusServiceUnavailable, ""), wantReason: ReasonSourceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := newFakeHub(t, map[string]http.HandlerFunc{"GET /api/whoami-v2": c.answer})

			got, err := hub.client().ValidToken(context.Background(), testToken)
			if c.wantReason != "" {
				assert.Equal(t, c.wantReason, ReasonOf(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestHuggingFaceRevalidate(t *testing.T) {
	resolvePath := "/owner/repo/resolve/" + testCommit + "/config.json"
	rootListing := jsonAnswer([]any{testWeightFile, testSubDir, testConfigFile})
	cases := []struct {
		name       string
		routes     map[string]http.HandlerFunc
		wantReason string
		wantHead   string
	}{
		{
			name: "a redirect passes without being followed",
			routes: map[string]http.HandlerFunc{
				"GET " + treePath: rootListing,
				"HEAD " + resolvePath: func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, "/cdn/never-followed", http.StatusFound)
				},
				"HEAD /cdn/never-followed": statusAnswer(http.StatusInternalServerError, ""),
			},
			wantHead: resolvePath,
		},
		{
			name: "a gated file",
			routes: map[string]http.HandlerFunc{
				"GET " + treePath:     rootListing,
				"HEAD " + resolvePath: statusAnswer(http.StatusForbidden, "GatedRepo"),
			},
			wantReason: ReasonAccessDenied,
		},
		{
			name: "a repository turned private",
			routes: map[string]http.HandlerFunc{
				"GET " + treePath: statusAnswer(http.StatusUnauthorized, ""),
			},
			wantReason: ReasonAccessDenied,
		},
		{
			name: "an outage",
			routes: map[string]http.HandlerFunc{
				"GET " + treePath:     rootListing,
				"HEAD " + resolvePath: statusAnswer(http.StatusServiceUnavailable, ""),
			},
			wantReason: ReasonSourceUnavailable,
		},
		{
			name: "a root with no file falls back to the recursive listing",
			routes: map[string]http.HandlerFunc{
				"GET " + treePath: func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("recursive") == "" {
						_ = json.NewEncoder(w).Encode([]any{testSubDir})
						return
					}
					_ = json.NewEncoder(w).Encode([]any{testSubDir, testTokenFile})
				},
				"HEAD /owner/repo/resolve/" + testCommit + "/sub/tokenizer.json": statusAnswer(http.StatusOK, ""),
			},
			wantHead: "/owner/repo/resolve/" + testCommit + "/sub/tokenizer.json",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := newFakeHub(t, c.routes)

			err := hub.client().Revalidate(context.Background(), "owner/repo", testCommit, testToken)
			if c.wantReason != "" {
				require.Error(t, err)
				assert.Equal(t, c.wantReason, ReasonOf(err))
				assert.NotContains(t, err.Error(), testToken)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "Bearer "+testToken, hub.auth[c.wantHead])
			_, followed := hub.auth["/cdn/never-followed"]
			assert.False(t, followed)
		})
	}
}

func TestClientTransport(t *testing.T) {
	t.Run("a proxy is used for HTTPS and bypassed for a no-proxy host", func(t *testing.T) {
		client, err := NewHTTPClient(HTTPClientOptions{HTTPSProxy: "http://proxy.invalid:3128", NoProxy: "mirror.internal"})
		require.NoError(t, err)
		proxy := client.Transport.(*http.Transport).Proxy

		for _, c := range []struct {
			target string
			want   string
		}{
			{target: "https://huggingface.co/api/whoami-v2", want: "http://proxy.invalid:3128"},
			{target: "https://mirror.internal/api/whoami-v2"},
		} {
			req, err := http.NewRequest(http.MethodGet, c.target, nil)
			require.NoError(t, err)
			got, err := proxy(req)
			require.NoError(t, err)
			if c.want == "" {
				assert.Nil(t, got, c.target)
				continue
			}
			assert.Equal(t, c.want, got.String(), c.target)
		}
	})

	t.Run("a CA bundle is trusted", func(t *testing.T) {
		server := httptest.NewTLSServer(jsonAnswer(map[string]string{"sha": testCommit}))
		t.Cleanup(server.Close)
		bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})

		untrusted, err := NewHTTPClient(HTTPClientOptions{})
		require.NoError(t, err)
		_, err = untrusted.Get(server.URL)
		require.Error(t, err)

		trusted, err := NewHTTPClient(HTTPClientOptions{CABundle: bundle})
		require.NoError(t, err)
		resp, err := trusted.Get(server.URL)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("a CA bundle without a certificate is refused", func(t *testing.T) {
		_, err := NewHTTPClient(HTTPClientOptions{CABundle: []byte("not a certificate")})
		require.Error(t, err)
	})

	t.Run("a proxy it cannot use safely is refused", func(t *testing.T) {
		_, err := NewHTTPClient(HTTPClientOptions{HTTPSProxy: "http://user:secret@proxy.invalid:3128"})
		require.Error(t, err)
	})
}

func TestValidateProxy(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "an http proxy", value: "http://proxy.invalid:3128"},
		{name: "an https proxy", value: "https://proxy.invalid"},
		{name: "a password", value: "http://user:secret@proxy.invalid:3128", wantErr: true},
		{name: "a user alone", value: "http://user@proxy.invalid:3128", wantErr: true},
		{name: "a socks proxy", value: "socks5://proxy.invalid:1080", wantErr: true},
		{name: "credentials without a scheme", value: "user:secret@proxy.invalid", wantErr: true},
		{name: "no host", value: "http://", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateProxy(c.value)
			assert.Equal(t, c.wantErr, err != nil, "%v", err)
		})
	}
}
