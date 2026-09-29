package modelartifact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"gpustack.ai/gpustack/pkg/utils/httpx"
)

// ModelScope resolves ModelScope repositories.
//
// Every method takes the token rather than holding one, so a client is shared across namespaces
// without ever carrying a namespace's credential beyond one call.
type ModelScope struct {
	// Endpoint is the hub's base URL, e.g. "https://www.modelscope.cn".
	Endpoint string
	// Client sends the requests; see NewHTTPClient.
	Client *http.Client
	// MaxEntries bounds the tree entries of one resolution. Zero uses
	// DefaultHuggingFaceMaxEntries, the bound the two hubs share.
	MaxEntries int

	// lsRemote asks the repository's git endpoint for its refs, keyed by ref name. Tests replace
	// it; the default speaks the HTTP smart protocol itself.
	lsRemote func(ctx context.Context, gitURL, token string) (map[string]string, error)
}

// modelScopeCommitPattern is the shape of a full commit, which is used as is and cross-checked
// against git only when the revision is a branch or a tag.
var modelScopeCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// resolveRevision resolves a branch or a tag to the commit both the hub's index and its git name,
// and takes a full commit as is.
//
// The commits endpoint is undocumented and answers master when its Ref parameter is misspelled,
// so its answer is never trusted alone: for a branch it must equal the commit git names for the
// same ref, and for an annotated tag the peeled entry git names for the tag. A disagreement
// refuses the resolution: a drift breaks toward refusal, never toward wrong content.
func (m *ModelScope) resolveRevision(ctx context.Context, repository, revision, token string) (string, error) {
	if modelScopeCommitPattern.MatchString(revision) {
		return revision, nil
	}

	var answer struct {
		Commit []struct {
			Id string `json:"Id"`
		} `json:"Commit"`
	}
	target := m.apiURL("models", repository, "commits") + "?Ref=" + url.QueryEscape(revision) + "&PageSize=1"
	if err := m.getJSON(ctx, target, token, &answer); err != nil {
		return "", err
	}
	if len(answer.Commit) == 0 {
		return "", sourceErrorf(ReasonRevisionNotFound, "the repository has no branch or tag %q", revision)
	}
	commit := answer.Commit[0].Id
	if !modelScopeCommitPattern.MatchString(commit) {
		return "", sourceErrorf(ReasonSourceUnavailable,
			"the hub answered revision %q of %q with a commit %q that is not 40 hexadecimal digits",
			revision, repository, commit)
	}

	refs, err := m.runLsRemote(ctx, repository, token)
	if err != nil {
		return "", err
	}
	against := modelScopeRefCommit(refs, revision)
	switch {
	case against == "":
		return "", sourceErrorf(ReasonSourceUnavailable,
			"the hub's git names no ref for %q of %q: the commits endpoint answered %s, and the two must agree",
			revision, repository, commit)
	case against != commit:
		return "", sourceErrorf(ReasonSourceUnavailable,
			"the hub's index and its git disagree about %q of %q: the commits endpoint answers %s, git answers %s",
			revision, repository, commit, against)
	}

	return commit, nil
}

// modelScopeRefCommit picks the commit git names for a revision: the peeled entry of an
// annotated tag, the tag itself if it is lightweight, the branch's ref, and HEAD last — HEAD can
// only confirm a commit the index already named, so it cannot mask a disagreement.
func modelScopeRefCommit(refs map[string]string, revision string) string {
	for _, name := range []string{
		"refs/tags/" + revision + "^{}", "refs/tags/" + revision, "refs/heads/" + revision, "HEAD",
	} {
		if commit, ok := refs[name]; ok {
			return commit
		}
	}

	return ""
}

// modelScopePageBound is the listing size at which repo/files truncates silently (measured,
// PoC-D), whatever pagination the query names: a page of this many entries is a truncation, and
// only a walk over direct children can recover the tree.
const modelScopePageBound = 3000

// Resolve resolves revision to a commit and builds the manifest of the files at it that filter
// selects.
func (m *ModelScope) Resolve(ctx context.Context, repository, revision, token string, filter Filter) (Resolution, error) {
	commit, err := m.resolveRevision(ctx, repository, revision, token)
	if err != nil {
		return Resolution{}, err
	}

	manifest, err := m.ListManifest(ctx, repository, commit, token, filter)
	if err != nil {
		return Resolution{}, err
	}

	return Resolution{Commit: commit, Manifest: manifest}, nil
}

// ListManifest builds the manifest of the files at commit that filter selects, from the hub's
// file listing. A node recomputes an artifact's manifest this way, with the credential of the Pod
// that mounts it, and compares the digest with the one the controller published. Every file's
// digest is the hub's own sha256, so a ModelScope manifest has no gitsha1 lines.
func (m *ModelScope) ListManifest(ctx context.Context, repository, commit, token string, filter Filter) (Manifest, error) {
	entries, err := m.listRepoFiles(ctx, repository, commit, token)
	if err != nil {
		return Manifest{}, err
	}
	if len(entries) == 0 {
		return Manifest{}, sourceErrorf(ReasonEmptyManifest, "commit %s of %q holds no file", commit, repository)
	}
	entries = FilterEntries(entries, filter.Allow, filter.Ignore)
	if len(entries) == 0 {
		return Manifest{}, sourceErrorf(ReasonEmptyManifest,
			"no file of commit %s of %q matches the allow and ignore patterns", commit, repository)
	}
	manifest, err := NewManifest(entries)
	if err != nil {
		return Manifest{}, sourceErrorf(ReasonInvalidManifest, "commit %s of %q: %v", commit, repository, err)
	}

	return manifest, nil
}

// listRepoFiles lists every file at the commit, recovering from the API's silent truncation.
//
// A recursive page of exactly modelScopePageBound entries is a truncation: the walk lists that
// level's direct children instead and descends into every directory of it, and only subtrees
// that truncate are broken up further. A level whose direct children reach the bound cannot be
// enumerated at all, and a partial manifest would be a silent wrong answer, so it refuses.
func (m *ModelScope) listRepoFiles(ctx context.Context, repository, commit, token string) ([]ManifestEntry, error) {
	limit := m.MaxEntries
	if limit <= 0 {
		limit = DefaultHuggingFaceMaxEntries
	}

	var entries []ManifestEntry
	var walk func(dir string) error
	walk = func(dir string) error {
		page, truncated, err := m.listFiles(ctx, repository, commit, dir, true, token)
		if err != nil {
			return err
		}
		if !truncated {
			for _, e := range page {
				entry, ok, err := modelScopeManifestEntry(e)
				if err != nil {
					return err
				}
				if ok {
					entries = append(entries, entry)
				}
			}
			return nil
		}

		children, truncated, err := m.listFiles(ctx, repository, commit, dir, false, token)
		if err != nil {
			return err
		}
		if truncated {
			return sourceErrorf(ReasonSourceUnavailable,
				"directory %q of commit %s lists %d or more direct children, which the API cannot enumerate",
				dir, commit, modelScopePageBound)
		}
		for _, e := range children {
			if e.Type == modelScopeTypeTree {
				if err := walk(e.Path); err != nil {
					return err
				}
				continue
			}
			entry, ok, err := modelScopeManifestEntry(e)
			if err != nil {
				return err
			}
			if ok {
				entries = append(entries, entry)
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	if len(entries) > limit {
		return nil, sourceErrorf(ReasonManifestTooLarge,
			"commit %s of %q lists more than %d tree entries", commit, repository, limit)
	}

	return entries, nil
}

// modelScopeTypeTree is a directory entry's type; every file is a blob.
const modelScopeTypeTree = "tree"

// modelScopeFileEntry is one entry of a repo/files page.
type modelScopeFileEntry struct {
	Type   string `json:"Type"`
	Path   string `json:"Path"`
	Size   int64  `json:"Size"`
	Sha256 string `json:"Sha256"`
}

// modelScopeManifestEntry turns a listing entry into a manifest entry, and reports false for a
// directory. Every file must carry the hub's sha256: ModelScope gives no alternative digest, so
// an entry without one is a manifest the format cannot express.
func modelScopeManifestEntry(e modelScopeFileEntry) (ManifestEntry, bool, error) {
	switch e.Type {
	case modelScopeTypeTree:
		return ManifestEntry{}, false, nil
	case "blob":
	default:
		return ManifestEntry{}, false, sourceErrorf(ReasonInvalidManifest,
			"entry %q is of type %q, which is neither a blob nor a tree", e.Path, e.Type)
	}
	if len(e.Sha256) != 64 || strings.ContainsFunc(e.Sha256, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
	}) {
		return ManifestEntry{}, false, sourceErrorf(ReasonInvalidManifest,
			"entry %q names no usable Sha256, which every manifest line requires", e.Path)
	}

	return ManifestEntry{Path: e.Path, Size: e.Size, Digest: DigestSHA256 + ":" + e.Sha256}, true, nil
}

// listFiles lists one level: the whole subtree below root when recursive, root's direct children
// otherwise. A page at the bound is a truncation, whatever it holds.
func (m *ModelScope) listFiles(
	ctx context.Context, repository, commit, root string, recursive bool, token string,
) ([]modelScopeFileEntry, bool, error) {
	target := m.apiURL("models", repository, "repo", "files") + "?Revision=" + url.QueryEscape(commit)
	if root != "" {
		target += "&Root=" + url.QueryEscape(root)
	}
	if recursive {
		target += "&Recursive=true"
	}

	var answer struct {
		Files []modelScopeFileEntry `json:"Files"`
	}
	if err := m.getJSON(ctx, target, token, &answer); err != nil {
		return nil, false, err
	}

	return answer.Files, len(answer.Files) >= modelScopePageBound, nil
}

// Revalidate checks that token can still read the repository at commit: one HEAD of the
// repo endpoint for the first file, with redirects not followed.
//
// Measured against the hub: a HEAD of an accessible file answers 200 — an LFS file's too, where
// the GET would redirect to the CDN, so a redirect confirms as well — and a missing commit, path
// or repository and a gated repository without a grant all answer 404 with no envelope. The
// first file comes from one listing of the root, so the check costs two requests whatever the
// repository's size.
func (m *ModelScope) Revalidate(ctx context.Context, repository, commit, token string) error {
	first, err := m.firstRootFile(ctx, repository, commit, token)
	if err != nil {
		return err
	}

	target := m.fileURL(repository, commit, first)
	resp, err := m.doNoRedirect(ctx, http.MethodHead, target, token)
	if err != nil {
		return err
	}
	defer httpx.Close(resp)
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// A redirect names a CDN path the hub only grants for a readable repository.
		return nil
	default:
		return classifyModelScope(resp.StatusCode, 0, resp.Request.URL.Path)
	}
}

// firstRootFile names the file the revalidation HEADs: the first file of the root by path, from
// one direct-children listing; a root holding no file falls back to the recursive listing.
func (m *ModelScope) firstRootFile(ctx context.Context, repository, commit, token string) (string, error) {
	page, _, err := m.listFiles(ctx, repository, commit, "", false, token)
	if err != nil {
		return "", err
	}
	first := ""
	for _, e := range page {
		if e.Type != modelScopeTypeTree && (first == "" || e.Path < first) {
			first = e.Path
		}
	}
	if first != "" {
		return first, nil
	}

	page, _, err = m.listFiles(ctx, repository, commit, "", true, token)
	if err != nil {
		return "", err
	}
	for _, e := range page {
		if e.Type != modelScopeTypeTree && (first == "" || e.Path < first) {
			first = e.Path
		}
	}
	if first == "" {
		return "", sourceErrorf(ReasonEmptyManifest, "commit %s of %q holds no file to revalidate against", commit, repository)
	}

	return first, nil
}

// FileURL is where a file of repository at commit is downloaded from. The hub answers a small
// file with the bytes and an LFS file with a redirect to a signed, content-addressed CDN path
// that honors byte ranges.
func (m *ModelScope) FileURL(repository, commit, path string) string {
	return m.fileURL(repository, commit, path)
}

func (m *ModelScope) fileURL(repository, commit, path string) string {
	return m.apiURL("models", repository, "repo") +
		"?Revision=" + url.QueryEscape(commit) + "&FilePath=" + url.QueryEscape(path)
}

// ValidToken reports whether the hub accepts token at all: an answered users/me. A token the hub
// rejects is silently ignored on a public repository, so this is the only way such a mistake
// becomes visible.
func (m *ModelScope) ValidToken(ctx context.Context, token string) (bool, error) {
	target := strings.TrimSuffix(m.Endpoint, "/") + "/openapi/v1/users/me"
	resp, err := m.doNoRedirect(ctx, http.MethodGet, target, token)
	if err != nil {
		return false, err
	}
	defer httpx.Close(resp)
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return false, nil
	default:
		return false, sourceErrorf(ReasonSourceUnavailable,
			"the token check against %s: HTTP %d", target, resp.StatusCode)
	}
}

// gitURL is the repository's git endpoint, the hub's host serving the same id.
func (m *ModelScope) gitURL(repository string) (string, error) {
	base, err := url.Parse(m.Endpoint)
	if err != nil {
		return "", sourceErrorf(ReasonSourceUnavailable, "the endpoint %q does not parse: %v", m.Endpoint, err)
	}
	base.Path = strings.TrimSuffix(escapeRepository(repository), "/") + ".git"
	base.RawQuery, base.Fragment = "", ""

	return base.String(), nil
}

// runLsRemote runs the git cross-check for one repository.
func (m *ModelScope) runLsRemote(ctx context.Context, repository, token string) (map[string]string, error) {
	run := m.lsRemote
	if run == nil {
		run = m.lsRemoteOverHTTP
	}
	gitURL, err := m.gitURL(repository)
	if err != nil {
		return nil, err
	}
	refs, err := run(ctx, gitURL, token)
	if err != nil {
		return nil, sourceErrorf(ReasonSourceUnavailable, "the git cross-check of %s: %v", gitURL, err)
	}

	return refs, nil
}

// lsRemoteOverHTTP asks the repository's git endpoint for its refs over the HTTP smart protocol —
// the same request `git ls-remote` answers — so the cross-check needs no git binary in the image
// and no subprocess: the endpoint's client is the one the Settings configure, and a private
// repository authenticates as the user oauth2 with the token as the password, an HTTP header
// rather than anything another process could read.
func (m *ModelScope) lsRemoteOverHTTP(ctx context.Context, gitURL, token string) (map[string]string, error) {
	target := gitURL + "/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	// The endpoint answers a git client and refuses others with 421 (measured), so the request
	// names itself the protocol it speaks.
	req.Header.Set("User-Agent", "git/2.39")
	req.Header.Set("Accept", "application/x-git-upload-pack-advertisement")
	if token != "" {
		req.SetBasicAuth("oauth2", token)
	}

	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %v", http.MethodGet, target, unwrapURLError(err))
	}
	defer httpx.Close(resp)
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, sourceErrorf(ReasonAccessDenied,
			"%s: the repository does not exist or is not accessible with the credential (HTTP %d)",
			target, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-git-upload-pack-advertisement") {
		return nil, fmt.Errorf("the answer is %q, not a git advertisement", ct)
	}

	return parseGitAdvertisement(resp.Body)
}

// parseGitAdvertisement reads a smart protocol advertisement: pkt-lines of `<commit> <ref>`, the
// first carrying the server's capabilities after a NUL and an annotated tag's commit named by the
// peeled entry with a trailing `^{}`. A packet that does not parse is an error — a truncated
// answer must not shrink the cross-check.
func parseGitAdvertisement(r io.Reader) (map[string]string, error) {
	buf, err := io.ReadAll(io.LimitReader(r, modelScopeMaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(buf) > modelScopeMaxResponseBytes {
		return nil, fmt.Errorf("the advertisement exceeds %d bytes", modelScopeMaxResponseBytes)
	}

	refs := make(map[string]string)
	for i := 0; i < len(buf); {
		if len(buf)-i < 4 {
			return nil, fmt.Errorf("the advertisement ends inside a packet length")
		}
		n, err := strconv.ParseInt(string(buf[i:i+4]), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("unparsable packet length %q", string(buf[i:min(i+4, len(buf))]))
		}
		i += 4
		if n == 0 {
			continue // a flush packet
		}
		if n < 4 {
			return nil, fmt.Errorf("unparsable packet length %q", string(buf[i-4:i]))
		}
		if i+int(n)-4 > len(buf) {
			return nil, fmt.Errorf("the advertisement ends inside a packet")
		}
		line := strings.TrimSuffix(string(buf[i:i+int(n)-4]), "\n")
		i += int(n) - 4
		if strings.HasPrefix(line, "# service=") {
			continue
		}
		if z := strings.IndexByte(line, 0); z >= 0 {
			line = line[:z] // the first ref carries the server's capabilities after a NUL
		}
		commit, ref, ok := strings.Cut(line, " ")
		if !ok || !modelScopeCommitPattern.MatchString(commit) || ref == "" {
			return nil, fmt.Errorf("unparsable advertisement line %q", line)
		}
		refs[ref] = commit
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("the advertisement names no ref")
	}

	return refs, nil
}

// modelScopeMaxResponseBytes bounds one response body. A commits page of one entry is a few
// hundred bytes; a repo/files page of 3000 is a few hundred kilobytes.
const modelScopeMaxResponseBytes = 16 << 20

// modelScopeEnvelope is every answer's shape, errors included: the Code inside the body, not the
// HTTP status, is what distinguishes a missing revision from a forbidden one.
type modelScopeEnvelope struct {
	Code    int             `json:"Code"`
	Message string          `json:"Message"`
	Data    json.RawMessage `json:"Data"`
}

func (m *ModelScope) apiURL(segments ...string) string {
	escaped := make([]string, 0, len(segments)+1)
	escaped = append(escaped, strings.TrimSuffix(m.Endpoint, "/"), "api", "v1")
	for _, s := range segments {
		escaped = append(escaped, escapeRepository(s))
	}

	return strings.Join(escaped, "/")
}

func (m *ModelScope) getJSON(ctx context.Context, target, token string, into any) error {
	resp, err := m.do(ctx, http.MethodGet, target, token)
	if err != nil {
		return err
	}

	return decodeModelScope(resp, into)
}

// do sends one request. A transport failure is SourceUnavailable; the caller classifies the
// status code.
func (m *ModelScope) do(ctx context.Context, method, target, token string) (*http.Response, error) {
	return m.doRequest(ctx, method, target, token, false)
}

// doNoRedirect sends one request whose redirects stay unanswered: a redirect is an answer about
// access, not a page to follow.
func (m *ModelScope) doNoRedirect(ctx context.Context, method, target, token string) (*http.Response, error) {
	return m.doRequest(ctx, method, target, token, true)
}

func (m *ModelScope) doRequest(ctx context.Context, method, target, token string, noRedirect bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gpustack-operator")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := m.Client
	if noRedirect {
		c := *client
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &c
	}

	resp, err := client.Do(req)
	if err != nil {
		// The URL is the only part of a transport error that names the request, and it carries no
		// credential; the error is rebuilt so nothing else of the request can ride along.
		return nil, sourceErrorf(ReasonSourceUnavailable, "%s %s: %v", method, target, unwrapURLError(err))
	}

	return resp, nil
}

// decodeModelScope reads the envelope of one answer. A non-200 — or a 200 whose own Code is not
// 200 — is classified; only a clean answer's Data is decoded into.
func decodeModelScope(resp *http.Response, into any) error {
	defer httpx.Close(resp)
	body, err := io.ReadAll(io.LimitReader(resp.Body, modelScopeMaxResponseBytes+1))
	if err != nil {
		return sourceErrorf(ReasonSourceUnavailable, "read %s: %v", resp.Request.URL.Path, err)
	}
	if len(body) > modelScopeMaxResponseBytes {
		return sourceErrorf(ReasonSourceUnavailable, "%s answered more than %d bytes", resp.Request.URL.Path,
			modelScopeMaxResponseBytes)
	}

	var env modelScopeEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		if resp.StatusCode != http.StatusOK {
			return classifyModelScope(resp.StatusCode, 0, resp.Request.URL.Path)
		}
		return sourceErrorf(ReasonSourceUnavailable, "%s answered an unreadable body: %v", resp.Request.URL.Path, err)
	}
	if resp.StatusCode != http.StatusOK || env.Code != http.StatusOK {
		return classifyModelScope(resp.StatusCode, env.Code, resp.Request.URL.Path)
	}
	if into == nil || len(env.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Data, into); err != nil {
		return sourceErrorf(ReasonSourceUnavailable, "%s answered an unreadable body: %v", resp.Request.URL.Path, err)
	}

	return nil
}

// classifyModelScope maps a non-success answer onto a reason, as measured against ModelScope
// (PoC-D): every access failure is a 404, so the envelope Code classifies, with the HTTP status
// as the fallback.
//
//   - 10990101004 is a repository with no file tree at the revision: RevisionNotFound.
//   - 10010205001 (not found) and 10010200001 (no access — private without the token, or gated
//     without a grant) are AccessDenied, and so is every other 404, 401 and 403: the hub answers
//     a repository that does not exist and one the caller cannot read alike, so the message does
//     not guess which.
//   - Anything else, 5xx and 429 among it, is SourceUnavailable.
func classifyModelScope(status, code int, what string) error {
	switch {
	case code == 10990101004:
		return sourceErrorf(ReasonRevisionNotFound, "%s: the repository has no file tree at the revision", what)
	case code == 10010205001, code == 10010200001:
		return sourceErrorf(ReasonAccessDenied,
			"%s: the repository does not exist or is not accessible with the credential (HTTP %d, code %d)",
			what, status, code)
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusNotFound:
		return sourceErrorf(ReasonAccessDenied,
			"%s: the repository does not exist or is not accessible with the credential (HTTP %d, code %d)",
			what, status, code)
	default:
		return sourceErrorf(ReasonSourceUnavailable, "%s: HTTP %d, code %d", what, status, code)
	}
}
