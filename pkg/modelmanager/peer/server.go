package peer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
	certcache "gpustack.ai/gpustack/pkg/utils/certs/cache"
	"gpustack.ai/gpustack/pkg/utils/certs/fakecert"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// PathPrefix is where the peer endpoints live, under their own namespace on the port.
const PathPrefix = "/peer/v1/"

// The endpoints a peer serves.
const (
	// TreePath serves one published tree's listing.
	TreePath = PathPrefix + "trees/{hex}"
	// FilePath serves one file of one published tree, by byte range.
	FilePath = PathPrefix + "trees/{hex}/files/{path...}"
)

// Server serves the node's published trees to the plugins of other nodes: read-only, per file,
// with byte ranges, at most MaxStreams answers in flight.
type Server struct {
	Store *store.Store
	// Admit says whether a request may proceed; nil admits everything.
	Admit func(*http.Request) error
	// MaxStreams bounds the file answers served at once; further requests wait.
	MaxStreams int
	Now        func() time.Time

	streams chan struct{}
}

// listing is the answer at TreePath: a published tree's files, as the publish recorded them.
// The per-file digests are what lets a pulling side reassemble and re-bind the manifest to the
// artifact's resolved root digest before it trusts a byte.
// ManifestFile is one file of a stored manifest, as a listing names it.
type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// Listing is a stored manifest: the tree's digest and its files. The materializer stores it at
// publish, and the peer server answers a listing with it.
type Listing struct {
	Digest  string         `json:"digest"`
	Entries []ManifestFile `json:"entries"`
}

// listing is the served form of a stored manifest.
type listing = Listing

// Handler returns the peer endpoints. The authentication travels in the handler rather than the
// TLS layer alone, so a refused credential is an answer rather than a failed handshake.
func (s *Server) Handler() http.Handler {
	if s.MaxStreams > 0 {
		s.streams = make(chan struct{}, s.MaxStreams)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+TreePath, s.serveTree)
	mux.HandleFunc("GET "+FilePath, s.serveFile)

	return mux
}

// serveTree answers one published tree's manifest, the file the publish stored beside the
// marker. A tree published before manifests were stored is not a listing source.
func (s *Server) serveTree(w http.ResponseWriter, r *http.Request) {
	if err := s.admit(w, r); err != nil {
		return
	}
	hex, ok := HexOf("sha256:" + r.PathValue("hex"))
	if !ok {
		httpxRefused(w, http.StatusBadRequest, "not a manifest digest")

		return
	}
	m, err := s.Store.ReadMarker(hex)
	if err != nil {
		httpxRefused(w, http.StatusNotFound, "no such published tree")

		return
	}
	b, err := os.ReadFile(s.Store.PublishedManifestPath(hex))
	if err != nil {
		httpxRefused(w, http.StatusNotFound, "the tree carries no manifest")

		return
	}
	var l Listing
	if err := json.Unmarshal(b, &l); err != nil || l.Digest != m.Digest {
		klog.ErrorS(err, "a stored manifest does not parse or disagrees with its marker", "digest", m.Digest)
		httpxRefused(w, http.StatusInternalServerError, "the stored manifest is unreadable")

		return
	}
	httpx.PureJSON(w, http.StatusOK, l)
}

// serveFile answers one file of one published tree, whole or by range.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	if err := s.admit(w, r); err != nil {
		return
	}
	hex, ok := HexOf("sha256:" + r.PathValue("hex"))
	if !ok {
		httpxRefused(w, http.StatusBadRequest, "not a manifest digest")

		return
	}
	name, err := treeFile(s.Store.PublishedTree(hex), r.PathValue("path"))
	if err != nil {
		httpxRefused(w, http.StatusNotFound, "no such file in the tree")

		return
	}
	f, err := os.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			httpxRefused(w, http.StatusNotFound, "no such file in the tree")

			return
		}
		klog.ErrorS(err, "open a published file for a peer", "digest", hex)
		httpxRefused(w, http.StatusInternalServerError, "opening the file failed")

		return
	}
	defer func() { _ = f.Close() }()

	// One file answer at a time per slot: a stalled reader holds its slot, and the rest wait,
	// which is the serving side's own concurrency limit.
	if s.streams != nil {
		select {
		case s.streams <- struct{}{}:
			defer func() { <-s.streams }()
		case <-r.Context().Done():
			return
		}
	}
	now := s.now()
	http.ServeContent(w, r, path.Base(name), now, f)
}

// admit runs the authentication, answering the refusal.
func (s *Server) admit(w http.ResponseWriter, r *http.Request) error {
	if s.Admit == nil {
		return nil
	}
	if err := s.Admit(r); err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			httpxRefused(w, http.StatusUnauthorized, authErr.Msg)

			return err
		}
		klog.ErrorS(err, "admit a peer request")
		httpxRefused(w, http.StatusInternalServerError, "admission failed")

		return err
	}

	return nil
}

// treeFile resolves one tree-relative path to a file inside the tree, refusing anything that
// leaves it.
func treeFile(root, name string) (string, error) {
	cleaned := path.Clean("/" + filepath.ToSlash(name))
	if cleaned == "/" {
		return "", fmt.Errorf("%q is not a file", name)
	}
	full := filepath.Join(root, filepath.FromSlash(cleaned[1:]))
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%q leaves the tree", name)
	}

	return full, nil
}

// now is the handler's clock, replaceable so answers stay testable.
func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}

	return time.Now()
}

// Listen opens the peer server's TLS listener: the certificate is self-signed, as the plugin's
// secure port serves one, and the authentication may require a client certificate.
func Listen(port int, auth AuthOptions) (net.Listener, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("peer port %d is out of range", port)
	}
	certManager := &fakecert.DynamicManager{Cache: certcache.NewDirCache(osx.TempDir("peer-tls"))}
	tlsCfg, err := auth.ServerTLSConfig(certManager.GetCertificate)
	if err != nil {
		return nil, err
	}
	lis, err := tls.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port), tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("listen on the peer port %d: %w", port, err)
	}

	return lis, nil
}

// Serve answers peer requests on lis until ctx is done.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	gp := gox.GroupWithContextIn(ctx)
	gp.Go(func(ctx context.Context) error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		return srv.Shutdown(shutdownCtx)
	})
	gp.Go(func(_ context.Context) error {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}

		return nil
	})

	return gp.Wait()
}

// readFileLimited reads a small file, a token's, that another process may rewrite.
func readFileLimited(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// httpxRefused answers a refused request with the refusal in the body, which names no tenant.
func httpxRefused(w http.ResponseWriter, code int, msg string) {
	httpx.PureJSON(w, code, map[string]string{"error": msg})
}
