package modelartifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const modelScopeCommit = "186d8559ad54c32cf47dc3a8225f993742c507b8"

// newFakeModelScope is a ModelScope client against a fake hub whose answers each case writes,
// with git replaced by refs the case supplies and an optional error standing in for its failure.
func newFakeModelScope(t *testing.T, routes map[string]http.HandlerFunc, refs map[string]string, gitErr error) *ModelScope {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, ok := routes[r.Method+" "+r.URL.EscapedPath()]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"Code":10010205001,"Message":"not found"}`))
			return
		}
		route(w, r)
	}))
	t.Cleanup(server.Close)

	m := &ModelScope{Endpoint: server.URL, Client: server.Client()}
	m.lsRemote = func(_ context.Context, _, _ string) (map[string]string, error) {
		if gitErr != nil {
			return nil, gitErr
		}
		return refs, nil
	}

	return m
}

// modelScopeCommitsRoute answers the commits endpoint the way the hub does: an envelope whose
// Data.Commit lists the commits, empty for a ref the repository does not have.
func modelScopeCommitsRoute(ids ...string) http.HandlerFunc {
	type commit struct {
		Id string `json:"Id"`
	}
	commits := make([]commit, 0, len(ids))
	for _, id := range ids {
		commits = append(commits, commit{Id: id})
	}
	return jsonAnswer(map[string]any{
		"Code":    200,
		"Message": "success",
		"Data":    map[string]any{"Commit": commits, "TotalCount": len(commits)},
	})
}

func modelScopeCommitsPath(repository string) string {
	return "/api/v1/models/" + repository + "/commits"
}

func TestModelScopeResolveRevision(t *testing.T) {
	const branchCommit = "13448952f850140b43c5e14d0e19f7e7f8cb3c47"

	t.Run("a commit is taken as is, without asking the hub", func(t *testing.T) {
		m := newFakeModelScope(t, nil, nil, nil)
		got, err := m.resolveRevision(context.Background(), "qwen/Qwen2.5-0.5B-Instruct", modelScopeCommit, "")
		require.NoError(t, err)
		assert.Equal(t, modelScopeCommit, got)
	})

	t.Run("a branch is resolved and cross-checked", func(t *testing.T) {
		m := newFakeModelScope(t,
			map[string]http.HandlerFunc{
				"GET " + modelScopeCommitsPath("qwen/Qwen2.5-0.5B-Instruct"): modelScopeCommitsRoute(branchCommit),
			},
			map[string]string{
				"HEAD":              branchCommit,
				"refs/heads/master": branchCommit,
			}, nil)
		got, err := m.resolveRevision(context.Background(), "qwen/Qwen2.5-0.5B-Instruct", "master", "")
		require.NoError(t, err)
		assert.Equal(t, branchCommit, got)
	})

	t.Run("a disagreement between the index and git refuses the resolution", func(t *testing.T) {
		other := strings.Repeat("f", 40)
		m := newFakeModelScope(t,
			map[string]http.HandlerFunc{
				"GET " + modelScopeCommitsPath("qwen/Qwen2.5-0.5B-Instruct"): modelScopeCommitsRoute(branchCommit),
			},
			map[string]string{"refs/heads/master": other}, nil)
		_, err := m.resolveRevision(context.Background(), "qwen/Qwen2.5-0.5B-Instruct", "master", "")
		require.Error(t, err)
		se := &SourceError{}
		require.True(t, errors.As(err, &se))
		assert.Equal(t, ReasonSourceUnavailable, se.Reason)
		assert.Contains(t, se.Message, "disagree")
		assert.Contains(t, se.Message, branchCommit)
		assert.Contains(t, se.Message, other)
	})

	t.Run("an annotated tag is cross-checked against the peeled entry", func(t *testing.T) {
		tagObject := strings.Repeat("b", 40)
		m := newFakeModelScope(t,
			map[string]http.HandlerFunc{
				"GET " + modelScopeCommitsPath("qwen/Qwen2.5-0.5B-Instruct"): modelScopeCommitsRoute(modelScopeCommit),
			},
			map[string]string{
				"refs/tags/v1.0":    tagObject,
				"refs/tags/v1.0^{}": modelScopeCommit,
			}, nil)
		got, err := m.resolveRevision(context.Background(), "qwen/Qwen2.5-0.5B-Instruct", "v1.0", "")
		require.NoError(t, err)
		assert.Equal(t, modelScopeCommit, got)
	})

	t.Run("a tag whose index names the tag object rather than the commit disagrees", func(t *testing.T) {
		tagObject := strings.Repeat("b", 40)
		m := newFakeModelScope(t,
			map[string]http.HandlerFunc{
				"GET " + modelScopeCommitsPath("qwen/Qwen2.5-0.5B-Instruct"): modelScopeCommitsRoute(tagObject),
			},
			map[string]string{
				"refs/tags/v1.0":    tagObject,
				"refs/tags/v1.0^{}": modelScopeCommit,
			}, nil)
		_, err := m.resolveRevision(context.Background(), "qwen/Qwen2.5-0.5B-Instruct", "v1.0", "")
		require.Error(t, err)
		assert.Equal(t, ReasonSourceUnavailable, ReasonOf(err))
	})

	t.Run("a branch git does not name refuses", func(t *testing.T) {
		m := newFakeModelScope(t,
			map[string]http.HandlerFunc{
				"GET " + modelScopeCommitsPath("qwen/Qwen2.5-0.5B-Instruct"): modelScopeCommitsRoute(branchCommit),
			},
			map[string]string{"refs/heads/other": branchCommit}, nil)
		_, err := m.resolveRevision(context.Background(), "qwen/Qwen2.5-0.5B-Instruct", "master", "")
		require.Error(t, err)
		assert.Equal(t, ReasonSourceUnavailable, ReasonOf(err))
		assert.Contains(t, ReasonOf(err), ReasonSourceUnavailable)
	})

	t.Run("git failing is SourceUnavailable, never a pass-through", func(t *testing.T) {
		m := newFakeModelScope(t,
			map[string]http.HandlerFunc{
				"GET " + modelScopeCommitsPath("qwen/Qwen2.5-0.5B-Instruct"): modelScopeCommitsRoute(branchCommit),
			},
			nil, errors.New("git: exit status 128"))
		_, err := m.resolveRevision(context.Background(), "qwen/Qwen2.5-0.5B-Instruct", "master", "")
		require.Error(t, err)
		assert.Equal(t, ReasonSourceUnavailable, ReasonOf(err))
		assert.Contains(t, ReasonOf(err), ReasonSourceUnavailable)
	})

	t.Run("a ref the index does not know is RevisionNotFound", func(t *testing.T) {
		m := newFakeModelScope(t,
			map[string]http.HandlerFunc{
				"GET " + modelScopeCommitsPath("qwen/Qwen2.5-0.5B-Instruct"): modelScopeCommitsRoute(),
			},
			nil, nil)
		_, err := m.resolveRevision(context.Background(), "qwen/Qwen2.5-0.5B-Instruct", "no-such", "")
		require.Error(t, err)
		assert.Equal(t, ReasonRevisionNotFound, ReasonOf(err))
	})

	t.Run("an index answer that is not a commit is SourceUnavailable", func(t *testing.T) {
		m := newFakeModelScope(t,
			map[string]http.HandlerFunc{
				"GET " + modelScopeCommitsPath("qwen/Qwen2.5-0.5B-Instruct"): modelScopeCommitsRoute("not-a-commit"),
			},
			map[string]string{"refs/heads/master": modelScopeCommit}, nil)
		_, err := m.resolveRevision(context.Background(), "qwen/Qwen2.5-0.5B-Instruct", "master", "")
		require.Error(t, err)
		assert.Equal(t, ReasonSourceUnavailable, ReasonOf(err))
	})
}

func TestModelScopeClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   int
		want   string
	}{
		{name: "a revision the listing cannot serve", status: 404, code: 10990101004, want: ReasonRevisionNotFound},
		{name: "a repository that does not exist", status: 404, code: 10010205001, want: ReasonAccessDenied},
		{name: "a repository without access", status: 404, code: 10010200001, want: ReasonAccessDenied},
		{name: "a 404 with an unknown code", status: 404, code: 99999999, want: ReasonAccessDenied},
		{name: "a 404 with no code", status: 404, code: 0, want: ReasonAccessDenied},
		{name: "an unauthorized", status: 401, want: ReasonAccessDenied},
		{name: "a forbidden", status: 403, want: ReasonAccessDenied},
		{name: "a server error", status: 500, want: ReasonSourceUnavailable},
		{name: "rate limited", status: 429, want: ReasonSourceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := classifyModelScope(c.status, c.code, "/api/v1/models/o/r/repo")
			require.Error(t, err)
			assert.Equal(t, c.want, ReasonOf(err))
		})
	}
}

func TestParseGitAdvertisement(t *testing.T) {
	pkt := func(payload string) string {
		return fmt.Sprintf("%04x%s", len(payload)+4, payload)
	}
	head := pkt("# service=git-upload-pack\n") + "0000"
	first := pkt("13448952f850140b43c5e14d0e19f7e7f8cb3c47 refs/heads/master\x00multi_ack thin-pack side-band")
	tag := pkt(strings.Repeat("b", 40) + " refs/tags/v1.0")
	peeled := pkt(modelScopeCommit + " refs/tags/v1.0^{}")
	advertisement := head + first + tag + peeled + "0000"

	refs, err := parseGitAdvertisement(strings.NewReader(advertisement))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"refs/heads/master": "13448952f850140b43c5e14d0e19f7e7f8cb3c47",
		"refs/tags/v1.0":    strings.Repeat("b", 40),
		"refs/tags/v1.0^{}": modelScopeCommit,
	}, refs)

	_, err = parseGitAdvertisement(strings.NewReader(head + pkt("short line") + "0000"))
	require.Error(t, err)

	_, err = parseGitAdvertisement(strings.NewReader("00"))
	require.Error(t, err)
}

// TestLsRemoteOverHTTP pins the request and its credential: the smart protocol answered by the
// operator itself, a private repository authenticating as oauth2 with an HTTP header.
func TestLsRemoteOverHTTP(t *testing.T) {
	pkt := func(payload string) string {
		return fmt.Sprintf("%04x%s", len(payload)+4, payload)
	}
	advertisement := pkt("# service=git-upload-pack\n") + "0000" +
		pkt(modelScopeCommit+" refs/heads/master\x00caps") + "0000"

	setup := func(t *testing.T, status int, body, contentType string) (*string, *string, *ModelScope) {
		gotAuth := ""
		gotUA := ""
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotUA = r.Header.Get("User-Agent")
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(server.Close)
		m := &ModelScope{Endpoint: server.URL, Client: server.Client()}
		return &gotAuth, &gotUA, m
	}

	t.Run("a public repository reads the refs", func(t *testing.T) {
		auth, ua, m := setup(t, http.StatusOK, advertisement, "application/x-git-upload-pack-advertisement")
		refs, err := m.lsRemoteOverHTTP(context.Background(), m.Endpoint+"/qwen/repo.git", "")
		require.NoError(t, err)
		assert.Equal(t, modelScopeCommit, refs["refs/heads/master"])
		assert.Empty(t, *auth, "no credential, no header")
		assert.Equal(t, "git/2.39", *ua, "the endpoint answers a git client and refuses others")
	})

	t.Run("a private repository authenticates as oauth2", func(t *testing.T) {
		auth, _, m := setup(t, http.StatusOK, advertisement, "application/x-git-upload-pack-advertisement")
		_, err := m.lsRemoteOverHTTP(context.Background(), m.Endpoint+"/qwen/repo.git", testToken)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(*auth, "Basic "), *auth)
		assert.NotContains(t, *auth, testToken, "the header carries the base64 pair, not the bare token")
	})

	t.Run("a refusal is AccessDenied", func(t *testing.T) {
		_, _, m := setup(t, http.StatusUnauthorized, "denied", "text/plain")
		_, err := m.lsRemoteOverHTTP(context.Background(), m.Endpoint+"/qwen/repo.git", "")
		require.Error(t, err)
		assert.Equal(t, ReasonAccessDenied, ReasonOf(err))
	})

	t.Run("a non-advertisement answer is refused", func(t *testing.T) {
		_, _, m := setup(t, http.StatusOK, "<html>", "text/html")
		_, err := m.lsRemoteOverHTTP(context.Background(), m.Endpoint+"/qwen/repo.git", "")
		require.Error(t, err)
		assert.Equal(t, ReasonSourceUnavailable, ReasonOf(err))
	})
}

// modelScopeFilesRoute answers the repo/files endpoint from a map of Root to entries, recording
// every query it served so a case can assert the descent.
func modelScopeFilesRoute(repository string, pages map[string][]map[string]any, served *[]url.Values) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if served != nil {
			*served = append(*served, r.URL.Query())
		}
		root := r.URL.Query().Get("Root")
		page, ok := pages[root]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"Code":10990101004,"Message":"no file tree"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Code":    200,
			"Message": "success",
			"Data":    map[string]any{"Files": page, "TotalCount": len(page)},
		})
	}
}

func modelScopeFilesPath(repository string) string {
	return "/api/v1/models/" + repository + "/repo/files"
}

func msFile(path string, size int64, sha string) map[string]any {
	return map[string]any{"Type": "blob", "Path": path, "Size": size, "Sha256": sha}
}

func msDir(path string) map[string]any {
	return map[string]any{"Type": "tree", "Path": path, "Size": 0}
}

func shaOf(s string) string {
	return strings.Repeat(s, 64)
}

func TestModelScopeListManifest(t *testing.T) {
	t.Run("a small tree lists whole, files only, and hashes as an independent v1 manifest", func(t *testing.T) {
		route := modelScopeFilesRoute("qwen/repo", map[string][]map[string]any{
			"": {
				msFile("README.md", 3, shaOf("a")),
				msDir("sub"),
				msFile("sub/tokenizer.json", 7, shaOf("b")),
			},
		}, nil)
		m := newFakeModelScope(t, map[string]http.HandlerFunc{"GET " + modelScopeFilesPath("qwen/repo"): route}, nil, nil)

		manifest, err := m.ListManifest(context.Background(), "qwen/repo", modelScopeCommit, "", Filter{})
		require.NoError(t, err)
		want, err := NewManifest([]ManifestEntry{
			{Path: "README.md", Size: 3, Digest: DigestSHA256 + ":" + shaOf("a")},
			{Path: "sub/tokenizer.json", Size: 7, Digest: DigestSHA256 + ":" + shaOf("b")},
		})
		require.NoError(t, err)
		assert.Equal(t, want, manifest)
	})

	t.Run("a truncated root listing is re-listed per directory", func(t *testing.T) {
		// The root's recursive page holds exactly 3000 entries, the API's silent truncation point.
		// Its content is dropped — a truncated page cannot be trusted — and the walk recovers the
		// tree from the direct children: two directories whose own pages are short, and one loose
		// file that is its own entry.
		root := make([]map[string]any, 0, modelScopePageBound)
		for i := 0; i < modelScopePageBound; i++ {
			root = append(root, msFile(fmt.Sprintf("gone/file-%04d", i), 1, shaOf("a")))
		}
		direct := []map[string]any{msDir("docs"), msDir("data"), msFile("loose.txt", 1, shaOf("e"))}
		docs := []map[string]any{msFile("docs/readme.md", 5, shaOf("b"))}
		for i := 0; i < 1498; i++ {
			docs = append(docs, msFile(fmt.Sprintf("docs/f-%04d", i), 1, shaOf("a")))
		}
		data := []map[string]any{msFile("data/set.bin", 9, shaOf("c"))}
		for i := 0; i < 1498; i++ {
			data = append(data, msFile(fmt.Sprintf("data/f-%04d", i), 1, shaOf("a")))
		}
		var served []url.Values
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served = append(served, r.URL.Query())
			page := root
			switch {
			case r.URL.Query().Get("Recursive") != "true":
				page = direct
			case r.URL.Query().Get("Root") == "docs":
				page = docs
			case r.URL.Query().Get("Root") == "data":
				page = data
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Code": 200, "Message": "success",
				"Data": map[string]any{"Files": page, "TotalCount": len(page)},
			})
		}))
		t.Cleanup(server.Close)
		m := &ModelScope{Endpoint: server.URL, Client: server.Client()}

		manifest, err := m.ListManifest(context.Background(), "qwen/repo", modelScopeCommit, "", Filter{})
		require.NoError(t, err)
		assert.Equal(t, int64(len(docs)+len(data)+1), manifest.FileCount)
		// Only a subtree that truncates is broken up further, so the walk touches exactly the
		// root and the two directories: the root once whole (recursive) and once for its direct
		// children, each directory whole.
		rootWhole, rootDirect := 0, 0
		children := make([]string, 0, 2)
		for _, q := range served {
			switch root := q.Get("Root"); root {
			case "":
				if q.Get("Recursive") == "true" {
					rootWhole++
				} else {
					rootDirect++
				}
			default:
				children = append(children, root)
				assert.Equal(t, "true", q.Get("Recursive"))
			}
		}
		assert.Equal(t, 1, rootWhole)
		assert.Equal(t, 1, rootDirect)
		assert.ElementsMatch(t, []string{"docs", "data"}, children)
	})

	t.Run("a directory with 3000 direct children refuses", func(t *testing.T) {
		root := make([]map[string]any, 0, modelScopePageBound)
		for i := 0; i < modelScopePageBound; i++ {
			root = append(root, msDir(fmt.Sprintf("d-%04d", i)))
		}
		route := modelScopeFilesRoute("qwen/repo", map[string][]map[string]any{"": root}, nil)
		m := newFakeModelScope(t, map[string]http.HandlerFunc{"GET " + modelScopeFilesPath("qwen/repo"): route}, nil, nil)

		_, err := m.ListManifest(context.Background(), "qwen/repo", modelScopeCommit, "", Filter{})
		require.Error(t, err)
		assert.Equal(t, ReasonSourceUnavailable, ReasonOf(err))
		se := &SourceError{}
		require.True(t, errors.As(err, &se))
		assert.Contains(t, se.Message, "3000")
	})

	t.Run("an entry without a usable Sha256 is an invalid manifest", func(t *testing.T) {
		route := modelScopeFilesRoute("qwen/repo", map[string][]map[string]any{
			"": {{"Type": "blob", "Path": "README.md", "Size": 3}},
		}, nil)
		m := newFakeModelScope(t, map[string]http.HandlerFunc{"GET " + modelScopeFilesPath("qwen/repo"): route}, nil, nil)

		_, err := m.ListManifest(context.Background(), "qwen/repo", modelScopeCommit, "", Filter{})
		require.Error(t, err)
		assert.Equal(t, ReasonInvalidManifest, ReasonOf(err))
	})

	t.Run("an unexpected entry type is an invalid manifest", func(t *testing.T) {
		route := modelScopeFilesRoute("qwen/repo", map[string][]map[string]any{
			"": {{"Type": "symlink", "Path": "link", "Size": 1, "Sha256": shaOf("a")}},
		}, nil)
		m := newFakeModelScope(t, map[string]http.HandlerFunc{"GET " + modelScopeFilesPath("qwen/repo"): route}, nil, nil)

		_, err := m.ListManifest(context.Background(), "qwen/repo", modelScopeCommit, "", Filter{})
		require.Error(t, err)
		assert.Equal(t, ReasonInvalidManifest, ReasonOf(err))
	})

	t.Run("an empty commit and a fully filtered one are EmptyManifest", func(t *testing.T) {
		route := modelScopeFilesRoute("qwen/repo", map[string][]map[string]any{
			"": {msDir("docs")},
		}, nil)
		m := newFakeModelScope(t, map[string]http.HandlerFunc{"GET " + modelScopeFilesPath("qwen/repo"): route}, nil, nil)

		_, err := m.ListManifest(context.Background(), "qwen/repo", modelScopeCommit, "", Filter{})
		require.Error(t, err)
		assert.Equal(t, ReasonEmptyManifest, ReasonOf(err))

		_, err = m.ListManifest(context.Background(), "qwen/repo", modelScopeCommit, "",
			Filter{Allow: []string{"*.safetensors"}})
		require.Error(t, err)
		assert.Equal(t, ReasonEmptyManifest, ReasonOf(err))
	})

	t.Run("the resolve path pins the commit the revision resolved to", func(t *testing.T) {
		const branchCommit = "13448952f850140b43c5e14d0e19f7e7f8cb3c47"
		commits := modelScopeCommitsRoute(branchCommit)
		var served []url.Values
		files := modelScopeFilesRoute("qwen/repo", map[string][]map[string]any{
			"": {msFile("README.md", 3, shaOf("a"))},
		}, &served)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.EscapedPath() == modelScopeCommitsPath("qwen/repo") {
				commits(w, r)
				return
			}
			files(w, r)
		}))
		t.Cleanup(server.Close)
		m := &ModelScope{Endpoint: server.URL, Client: server.Client()}
		m.lsRemote = func(_ context.Context, _, _ string) (map[string]string, error) {
			return map[string]string{"refs/heads/master": branchCommit}, nil
		}

		got, err := m.Resolve(context.Background(), "qwen/repo", "master", "", Filter{})
		require.NoError(t, err)
		assert.Equal(t, branchCommit, got.Commit)
		require.Len(t, served, 1)
		assert.Equal(t, branchCommit, served[0].Get("Revision"))
	})
}

func TestModelScopeRevalidate(t *testing.T) {
	const first = "README.md"

	setup := func(t *testing.T, root []map[string]any, status int) *ModelScope {
		route := modelScopeFilesRoute("qwen/repo", map[string][]map[string]any{"": root}, nil)
		headHit := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				headHit = true
				w.WriteHeader(status)
				return
			}
			route(w, r)
		}))
		t.Cleanup(server.Close)
		m := &ModelScope{Endpoint: server.URL, Client: server.Client()}
		t.Cleanup(func() { assert.True(t, headHit, "the revalidation must be one HEAD") })
		return m
	}

	t.Run("an accessible file confirms", func(t *testing.T) {
		m := setup(t, []map[string]any{msFile(first, 3, shaOf("a")), msDir("docs")}, http.StatusOK)
		require.NoError(t, m.Revalidate(context.Background(), "qwen/repo", modelScopeCommit, ""))
	})

	t.Run("an LFS file's redirect confirms too", func(t *testing.T) {
		m := setup(t, []map[string]any{msFile(first, 3, shaOf("a"))}, http.StatusFound)
		require.NoError(t, m.Revalidate(context.Background(), "qwen/repo", modelScopeCommit, ""))
	})

	t.Run("a 404 is an access refusal, the staircase's input", func(t *testing.T) {
		m := setup(t, []map[string]any{msFile(first, 3, shaOf("a"))}, http.StatusNotFound)
		err := m.Revalidate(context.Background(), "qwen/repo", modelScopeCommit, "")
		require.Error(t, err)
		assert.Equal(t, ReasonAccessDenied, ReasonOf(err))
	})

	t.Run("a server error is SourceUnavailable", func(t *testing.T) {
		m := setup(t, []map[string]any{msFile(first, 3, shaOf("a"))}, http.StatusInternalServerError)
		err := m.Revalidate(context.Background(), "qwen/repo", modelScopeCommit, "")
		require.Error(t, err)
		assert.Equal(t, ReasonSourceUnavailable, ReasonOf(err))
	})

	t.Run("a root with no file falls back to the recursive listing", func(t *testing.T) {
		// The root's direct children hold one directory; the recursive root page holds the file
		// inside it, so the two Root="" answers differ by the Recursive parameter.
		direct := []map[string]any{msDir("docs")}
		whole := []map[string]any{msDir("docs"), msFile("docs/readme.md", 5, shaOf("b"))}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			page := direct
			if r.URL.Query().Get("Recursive") == "true" {
				page = whole
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Code": 200, "Message": "success",
				"Data": map[string]any{"Files": page, "TotalCount": len(page)},
			})
		}))
		t.Cleanup(server.Close)
		m := &ModelScope{Endpoint: server.URL, Client: server.Client()}

		require.NoError(t, m.Revalidate(context.Background(), "qwen/repo", modelScopeCommit, ""))
	})
}

func TestModelScopeFileURL(t *testing.T) {
	m := &ModelScope{Endpoint: "https://www.modelscope.cn"}

	got := m.FileURL("qwen/Qwen2.5-0.5B-Instruct", modelScopeCommit, "sub/tokenizer.json")
	assert.Equal(t,
		"https://www.modelscope.cn/api/v1/models/qwen/Qwen2.5-0.5B-Instruct/repo"+
			"?Revision="+modelScopeCommit+"&FilePath=sub%2Ftokenizer.json", got)
}

func TestModelScopeValidToken(t *testing.T) {
	setup := func(t *testing.T, status int) *ModelScope {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/openapi/v1/users/me", r.URL.EscapedPath())
			assert.Equal(t, "Bearer "+testToken, r.Header.Get("Authorization"))
			w.WriteHeader(status)
		}))
		t.Cleanup(server.Close)
		return &ModelScope{Endpoint: server.URL, Client: server.Client()}
	}

	t.Run("an accepted token", func(t *testing.T) {
		ok, err := setup(t, http.StatusOK).ValidToken(context.Background(), testToken)
		require.NoError(t, err)
		assert.True(t, ok)
	})
	t.Run("a rejected token", func(t *testing.T) {
		ok, err := setup(t, http.StatusUnauthorized).ValidToken(context.Background(), testToken)
		require.NoError(t, err)
		assert.False(t, ok)
	})
	t.Run("an unreachable hub is an error, not a verdict", func(t *testing.T) {
		ok, err := setup(t, http.StatusInternalServerError).ValidToken(context.Background(), testToken)
		require.Error(t, err)
		assert.False(t, ok)
	})
}
