package modelartifact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"gpustack.ai/gpustack/pkg/utils/httpx"
)

// DefaultHuggingFaceMaxEntries bounds the tree entries, files and directories together, one
// resolution reads. It is far above any model repository and keeps a pathological one from holding
// a reconcile and the Hub's rate limit for thousands of pages.
const DefaultHuggingFaceMaxEntries = 100000

// huggingFaceMaxResponseBytes bounds one response body. A tree page of a thousand entries is a few
// hundred kilobytes.
const huggingFaceMaxResponseBytes = 16 << 20

// huggingFaceCommitPattern is the shape of a full commit, which is what the revision endpoint
// answers for every revision it accepts.
var huggingFaceCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// huggingFaceMaskedOID is how a gated repository answers an LFS file's SHA-256 to a caller that
// has not been granted access, while answering the tree itself with 200.
var huggingFaceMaskedOID = strings.Repeat("*", 64)

// HuggingFace resolves Hugging Face repositories.
//
// Every method takes the token rather than holding one, so a client is shared across namespaces
// without ever carrying a namespace's credential beyond one call.
type HuggingFace struct {
	// Endpoint is the Hub's base URL, e.g. "https://huggingface.co".
	Endpoint string
	// Client sends the requests; see NewHTTPClient.
	Client *http.Client
	// MaxEntries bounds the tree entries of one resolution. Zero uses
	// DefaultHuggingFaceMaxEntries.
	MaxEntries int
}

// Resolution is what resolving a repository at a revision produced.
type Resolution struct {
	// Commit is the full 40-character commit the revision resolved to.
	Commit string
	// Manifest is the canonical manifest of every file at Commit.
	Manifest Manifest
}

// Filter is an artifact's allow and ignore patterns; see FilterEntries. The zero value selects
// every file.
type Filter struct {
	Allow  []string
	Ignore []string
}

// Resolve resolves revision to a commit and builds the manifest of the files at it that filter
// selects.
//
// It uses the revision endpoint rather than the refs endpoint because only the former peels an
// annotated tag to the commit it points at; the latter answers the tag object.
func (h *HuggingFace) Resolve(ctx context.Context, repository, revision, token string, filter Filter) (Resolution, error) {
	var info struct {
		SHA string `json:"sha"`
	}
	infoURL := h.apiURL("models", repository, "revision", revision)
	if err := h.getJSON(ctx, infoURL, token, &info); err != nil {
		return Resolution{}, err
	}
	if !huggingFaceCommitPattern.MatchString(info.SHA) {
		return Resolution{}, sourceErrorf(ReasonSourceUnavailable,
			"the Hub answered revision %q of %q with a commit %q that is not 40 hexadecimal digits",
			revision, repository, info.SHA)
	}

	manifest, err := h.ListManifest(ctx, repository, info.SHA, token, filter)
	if err != nil {
		return Resolution{}, err
	}

	return Resolution{Commit: info.SHA, Manifest: manifest}, nil
}

// ListManifest builds the manifest of the files at commit that filter selects, from every page of
// the tree listing. A node recomputes an artifact's manifest this way, with the credential of the
// Pod that mounts it, and compares the digest with the one the controller published.
func (h *HuggingFace) ListManifest(ctx context.Context, repository, commit, token string, filter Filter) (Manifest, error) {
	entries, err := h.listTree(ctx, repository, commit, token)
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

// FileURL is where a file of repository at commit is downloaded from. The Hub answers it with a
// redirect to a content host, which honors byte ranges.
func (h *HuggingFace) FileURL(repository, commit, path string) string {
	return h.resolveURL(repository, commit, path)
}

// huggingFaceTreeEntry is one entry of a tree listing.
type huggingFaceTreeEntry struct {
	Type string `json:"type"`
	OID  string `json:"oid"`
	Size int64  `json:"size"`
	Path string `json:"path"`
	LFS  *struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"lfs"`
}

// listTree reads every page of the recursive tree at commit. The model-info endpoint's siblings
// are not used: that list is not paginated, and huggingface_hub itself stops trusting it above a
// thousand files.
func (h *HuggingFace) listTree(ctx context.Context, repository, commit, token string) ([]ManifestEntry, error) {
	limit := h.MaxEntries
	if limit <= 0 {
		limit = DefaultHuggingFaceMaxEntries
	}

	var (
		entries []ManifestEntry
		seen    int
		next    = h.apiURL("models", repository, "tree", commit) + "?recursive=true"
	)
	for next != "" {
		var page []huggingFaceTreeEntry
		resp, err := h.do(ctx, http.MethodGet, next, token, false)
		if err != nil {
			return nil, err
		}
		link := resp.Header.Get("Link")
		err = decodeHuggingFaceBody(resp, &page)
		if err != nil {
			return nil, err
		}

		seen += len(page)
		if seen > limit {
			return nil, sourceErrorf(ReasonManifestTooLarge,
				"commit %s of %q lists more than %d tree entries", commit, repository, limit)
		}
		for _, e := range page {
			entry, isFile, err := huggingFaceManifestEntry(e)
			if err != nil {
				return nil, err
			}
			if isFile {
				entries = append(entries, entry)
			}
		}

		next, err = h.nextPage(link)
		if err != nil {
			return nil, err
		}
	}

	return entries, nil
}

// huggingFaceManifestEntry turns a tree entry into a manifest entry, and reports false for a
// directory.
func huggingFaceManifestEntry(e huggingFaceTreeEntry) (ManifestEntry, bool, error) {
	switch e.Type {
	case "directory":
		return ManifestEntry{}, false, nil
	case "file":
	default:
		return ManifestEntry{}, false, sourceErrorf(ReasonInvalidManifest,
			"tree entry %q has the unknown type %q", e.Path, e.Type)
	}

	if e.LFS == nil {
		return ManifestEntry{Path: e.Path, Size: e.Size, Digest: DigestGitSHA1 + ":" + e.OID}, true, nil
	}
	if e.LFS.OID == huggingFaceMaskedOID {
		return ManifestEntry{}, false, sourceErrorf(ReasonAccessDenied,
			"the repository is gated and the credential has not been granted access: the Hub masks "+
				"the digest of %q", e.Path)
	}
	if e.LFS.Size != e.Size {
		return ManifestEntry{}, false, sourceErrorf(ReasonInvalidManifest,
			"tree entry %q has size %d but LFS size %d", e.Path, e.Size, e.LFS.Size)
	}

	return ManifestEntry{Path: e.Path, Size: e.Size, Digest: DigestSHA256 + ":" + e.LFS.OID}, true, nil
}

// nextPage returns the URL a Link header names as rel="next", or "" when it names none.
//
// A next page on another host is refused rather than followed: the token travels with every page,
// and pagination is not a reason to send it anywhere but the configured endpoint.
func (h *HuggingFace) nextPage(link string) (string, error) {
	for part := range strings.SplitSeq(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(strings.ReplaceAll(params, " ", ""), `rel="next"`) {
			continue
		}
		target = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(target), "<"), ">")
		next, err := url.Parse(target)
		if err != nil {
			return "", sourceErrorf(ReasonSourceUnavailable, "the Hub answered an unreadable next page %q", target)
		}
		base, err := url.Parse(h.Endpoint)
		if err != nil {
			return "", err
		}
		next = base.ResolveReference(next)
		if next.Host != base.Host {
			return "", sourceErrorf(ReasonSourceUnavailable,
				"the Hub answered a next page on another host %q", next.Host)
		}

		return next.String(), nil
	}

	return "", nil
}

// ValidToken reports whether the Hub accepts token at all. A token the Hub rejects is silently
// ignored on a public repository, so this is the only way such a mistake becomes visible.
func (h *HuggingFace) ValidToken(ctx context.Context, token string) (bool, error) {
	resp, err := h.do(ctx, http.MethodGet, h.apiURL("whoami-v2"), token, false)
	if err != nil {
		return false, err
	}
	httpx.Close(resp)

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return false, nil
	case resp.StatusCode/100 == 2:
		return true, nil
	default:
		return false, classifyHuggingFace(resp, "the token check")
	}
}

// Revalidate checks that token can still read the repository at commit: a HEAD of one file, with
// redirects not followed. The revision and tree endpoints cannot answer this, because a gated
// repository answers both with 200 to a caller that has no access.
//
// The file is the first file by path at the commit's root, read from one page of the root
// listing, so the check costs two requests whatever the repository's size. A root holding no file
// falls back to the first file of the recursive listing.
func (h *HuggingFace) Revalidate(ctx context.Context, repository, commit, token string) error {
	var page []huggingFaceTreeEntry
	if err := h.getJSON(ctx, h.apiURL("models", repository, "tree", commit), token, &page); err != nil {
		return err
	}

	file := firstHuggingFaceFile(page)
	if file == "" {
		entries, err := h.listTree(ctx, repository, commit, token)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if file == "" || e.Path < file {
				file = e.Path
			}
		}
		if file == "" {
			return sourceErrorf(ReasonEmptyManifest, "commit %s of %q holds no file", commit, repository)
		}
	}

	resp, err := h.do(ctx, http.MethodHead, h.resolveURL(repository, commit, file), token, true)
	if err != nil {
		return err
	}
	httpx.Close(resp)
	if resp.StatusCode/100 == 2 || resp.StatusCode/100 == 3 {
		return nil
	}

	return classifyHuggingFace(resp, "the access check")
}

func firstHuggingFaceFile(page []huggingFaceTreeEntry) string {
	first := ""
	for _, e := range page {
		if e.Type == "file" && (first == "" || e.Path < first) {
			first = e.Path
		}
	}

	return first
}

func (h *HuggingFace) apiURL(segments ...string) string {
	escaped := make([]string, 0, len(segments)+1)
	escaped = append(escaped, strings.TrimSuffix(h.Endpoint, "/"), "api")
	for i, s := range segments {
		// A repository keeps its owner separator; every other segment, a revision above all, is
		// one path segment whatever it contains.
		if i == 1 && segments[0] == "models" {
			escaped = append(escaped, escapeRepository(s))
			continue
		}
		escaped = append(escaped, url.PathEscape(s))
	}

	return strings.Join(escaped, "/")
}

func (h *HuggingFace) resolveURL(repository, commit, file string) string {
	parts := strings.Split(file, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}

	return strings.Join([]string{
		strings.TrimSuffix(h.Endpoint, "/"), escapeRepository(repository), "resolve", commit, strings.Join(parts, "/"),
	}, "/")
}

func escapeRepository(repository string) string {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok {
		return url.PathEscape(repository)
	}

	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func (h *HuggingFace) getJSON(ctx context.Context, target, token string, into any) error {
	resp, err := h.do(ctx, http.MethodGet, target, token, false)
	if err != nil {
		return err
	}

	return decodeHuggingFaceBody(resp, into)
}

// do sends one request. A transport failure is SourceUnavailable; the caller classifies the
// status code.
func (h *HuggingFace) do(ctx context.Context, method, target, token string, noRedirect bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gpustack-operator")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := h.Client
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

func unwrapURLError(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}

	return err
}

func decodeHuggingFaceBody(resp *http.Response, into any) error {
	defer httpx.Close(resp)
	if resp.StatusCode != http.StatusOK {
		return classifyHuggingFace(resp, resp.Request.URL.Path)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, huggingFaceMaxResponseBytes+1))
	if err != nil {
		return sourceErrorf(ReasonSourceUnavailable, "read %s: %v", resp.Request.URL.Path, err)
	}
	if len(body) > huggingFaceMaxResponseBytes {
		return sourceErrorf(ReasonSourceUnavailable, "%s answered more than %d bytes", resp.Request.URL.Path,
			huggingFaceMaxResponseBytes)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return sourceErrorf(ReasonSourceUnavailable, "%s answered an unreadable body: %v", resp.Request.URL.Path, err)
	}

	return nil
}

// classifyHuggingFace maps a non-success answer onto a reason, as measured against the Hub:
//
//   - 404 with X-Error-Code RevisionNotFound is a revision the repository does not have.
//   - 401 (with or without an error code), 403, any other 404 and X-Error-Code GatedRepo are
//     AccessDenied. A repository that does not exist and a private repository the caller cannot
//     read answer the same 401 without a token, so the message does not guess which.
//   - Anything else, 5xx and 429 among it, is SourceUnavailable.
func classifyHuggingFace(resp *http.Response, what string) error {
	code := resp.Header.Get("X-Error-Code")
	switch {
	case resp.StatusCode == http.StatusNotFound && code == "RevisionNotFound":
		return sourceErrorf(ReasonRevisionNotFound, "%s: the repository has no such revision", what)
	case code == "GatedRepo":
		return sourceErrorf(ReasonAccessDenied,
			"%s: the repository is gated and the credential has not been granted access", what)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusNotFound:
		return sourceErrorf(ReasonAccessDenied,
			"%s: the repository does not exist or is not accessible with the credential (HTTP %d%s)",
			what, resp.StatusCode, errorCodeSuffix(code))
	default:
		return sourceErrorf(ReasonSourceUnavailable, "%s: HTTP %d%s", what, resp.StatusCode, errorCodeSuffix(code))
	}
}

func errorCodeSuffix(code string) string {
	if code == "" {
		return ""
	}

	return ", " + code
}
