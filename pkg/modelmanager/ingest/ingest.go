// Package ingest serves authenticated local ingestion of verified ForgeSandbox emptyDir trees
// into NodeModelStore on the seed node.
package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	authenticationv1 "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelkernelkey"
	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/peer"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/utils/json"
)

// Hard limits enforced on every ingested ForgeSandbox output tree before reserving cache space.
// MaxTotalBytes is the default byte ceiling when outputSizeLimit is unset; configured values are
// clamped to [MinOutputSizeBytes, MaxOutputSizeBytes].
const (
	MaxFiles           = 1024
	MaxTotalBytes      = int64(2) << 30
	MinOutputSizeBytes = int64(64) << 20
	MaxOutputSizeBytes = int64(16) << 30
	MaxPathDepth       = 16
	MaxPathLength      = 256

	VerificationReportSchemaVersion = "gpustack-forge-verification/v1"
	DispatchManifestSchemaVersion   = "gpustack-forge-dispatch/v1"

	dispatchManifestFileName   = "dispatch-manifest.json"
	verificationReportFileName = "verification-report.json"
	forgeOutputVolumeName      = "forge-output"
	forgeVerifiedVolumeName    = "forge-verified"

	initContainerRunner   = "forge-runner"
	initContainerVerifier = "forge-verifier"

	maxRequestBytes      = 64 << 10
	maxJSONMetadataBytes = 4 << 20
	copyChunkBytes       = 64 << 10
)

var (
	safeUIDPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	safeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
)

// TokenReviewer is the Kubernetes TokenReview API surface needed to authenticate ingest requests.
type TokenReviewer interface {
	Create(ctx context.Context, review *authenticationv1.TokenReview, opts meta.CreateOptions) (*authenticationv1.TokenReview, error)
}

// NewTokenAdmit builds a bearer-token authenticator strictly bound to audience
// gpustack-model-ingest and username system:serviceaccount:<namespace>:<serviceAccount>.
// When serviceAccount is empty, local ingestion is disabled and every request is refused.
func NewTokenAdmit(namespace, serviceAccount string, reviewer TokenReviewer) (func(*http.Request) error, error) {
	ns := strings.TrimSpace(namespace)
	sa := strings.TrimSpace(serviceAccount)
	if ns == "" || sa == "" {
		return func(*http.Request) error {
			return &peer.AuthError{Msg: "local ingestion is not configured"}
		}, nil
	}
	return peer.AuthOptions{
		Mode:           peer.AuthToken,
		Audience:       modelstore.IngestAudience,
		Namespace:      ns,
		ServiceAccount: sa,
	}.Admit(reviewer)
}

// Server handles POST /ingest/v1/trees on model-manager's secure port.
type Server struct {
	NodeID      string
	KubeletDir  string
	Store       *store.Store
	APIReader   ctrlcli.Reader
	Admit       func(*http.Request) error
	Ready       func() bool
	Reserve     func(ctx context.Context, hex string, sizeBytes int64, written func() int64) (func(), error)
	FlushReport func(ctx context.Context) error
	Now         func() time.Time

	publishMu sync.Mutex
}

type wireRequest struct {
	Namespace  string `json:"namespace"`
	ForgerName string `json:"forgerName"`
	Forger     string `json:"forger,omitempty"`
	ForgerUID  string `json:"forgerUID,omitempty"`
	PodName    string `json:"podName"`
	PodUID     string `json:"podUID"`
}

type verificationReport struct {
	SchemaVersion          string `json:"schemaVersion"`
	OverallStatus          string `json:"overallStatus"`
	Status                 string `json:"status"`
	DispatchManifestDigest string `json:"dispatchManifestDigest"`
	PassedCount            int    `json:"passedCount"`
	VerifiedCount          int    `json:"verifiedCount"`
	FailedCount            int    `json:"failedCount"`
	Entries                []struct {
		Passed     bool `json:"passed"`
		Evaluation *struct {
			Status string `json:"status"`
		} `json:"evaluation"`
	} `json:"entries"`
}

func (r verificationReport) verifiedEntryCount() int {
	return max(r.PassedCount, r.VerifiedCount)
}

type dispatchManifest struct {
	SchemaVersion string `json:"schemaVersion"`
	Compatibility struct {
		Engine        string `json:"engine"`
		EngineVersion string `json:"engineVersion"`
		Manufacturer  string `json:"manufacturer"`
		Architecture  string `json:"architecture"`
	} `json:"compatibility"`
	Entries []struct {
		LocalKernelKey string `json:"localKernelKey"`
		LibraryPath    string `json:"libraryPath"`
	} `json:"entries"`
}

type httpError struct {
	code   int
	reason string
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// Handler returns an http.Handler serving POST /ingest/v1/trees.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		refuseJSON(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only POST is supported")
		return
	}
	if s.Admit == nil {
		refuseJSON(w, http.StatusUnauthorized, "Unauthorized", "local ingestion authentication is not configured")
		return
	}
	if err := s.Admit(r); err != nil {
		var authErr *peer.AuthError
		if errors.As(err, &authErr) {
			refuseJSON(w, http.StatusUnauthorized, "Unauthorized", authErr.Msg)
			return
		}
		klog.ErrorS(err, "review ingest token")
		refuseJSON(w, http.StatusInternalServerError, "InternalError", "token review failed")
		return
	}
	if s.Ready != nil && !s.Ready() {
		refuseJSON(w, http.StatusServiceUnavailable, "NotReady", "the plugin is starting: its caches have not synced yet")
		return
	}

	req, err := decodeIngestRequest(r.Body)
	if err != nil {
		refuseJSON(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}

	ctx := r.Context()
	maxTotalBytes, code, reason, msg := s.verifySandboxOwnership(ctx, req)
	if code != 0 {
		refuseJSON(w, code, reason, msg)
		return
	}

	verifiedDir, outputDir, err := s.resolveSandboxDirs(req.PodUID)
	if err != nil {
		klog.V(1).InfoS("sandbox emptyDir paths failed validation", "node", s.NodeID, "error", err.Error())
		refuseJSON(w, http.StatusUnprocessableEntity, "InvalidSandboxOutput", "sandbox staging directories are unavailable or invalid")
		return
	}

	vReport, err := readAndValidateVerificationReport(verifiedDir)
	if err != nil {
		klog.V(1).InfoS("sandbox verification report rejected", "node", s.NodeID, "error", err.Error())
		refuseJSON(w, http.StatusUnprocessableEntity, "VerificationFailed", "sandbox verification report failed validation")
		return
	}
	dispatchHex := store.HexOf(vReport.DispatchManifestDigest)
	wantVerifiedEntries := vReport.verifiedEntryCount()

	totalBytes, err := preScanForgeOutput(ctx, outputDir, vReport.DispatchManifestDigest, wantVerifiedEntries, maxTotalBytes)
	if err != nil {
		klog.V(1).InfoS("sandbox output pre-scan rejected", "node", s.NodeID, "error", err.Error())
		refuseJSON(w, http.StatusUnprocessableEntity, "InvalidSandboxOutput", "sandbox output failed integrity or size checks")
		return
	}

	var copiedBytes atomic.Int64
	if s.Reserve != nil {
		release, err := s.Reserve(ctx, dispatchHex, totalBytes, copiedBytes.Load)
		if err != nil {
			if download.ReasonOf(err) == download.ReasonInsufficientCapacity {
				refuseJSON(w, http.StatusInsufficientStorage, download.ReasonInsufficientCapacity, "the node cache has insufficient capacity")
				return
			}
			klog.ErrorS(err, "reserve cache space for ingest")
			refuseJSON(w, http.StatusServiceUnavailable, "ReserveFailed", "cache capacity reservation failed")
			return
		}
		defer release()
	}

	resp, err := s.copyAndPublish(ctx, outputDir, vReport.DispatchManifestDigest, wantVerifiedEntries, maxTotalBytes, &copiedBytes)
	if err != nil {
		var statusErr *httpError
		if errors.As(err, &statusErr) {
			refuseJSON(w, statusErr.code, statusErr.reason, statusErr.msg)
			return
		}
		klog.ErrorS(err, "copy and publish ingested kernel tree")
		refuseJSON(w, http.StatusInternalServerError, "PublishFailed", "publishing the kernel tree failed")
		return
	}

	httpx.PureJSON(w, http.StatusOK, resp)
}

func refuseJSON(w http.ResponseWriter, code int, reason, msg string) {
	httpx.PureJSON(w, code, map[string]string{
		"reason": reason,
		"error":  msg,
	})
}

func decodeIngestRequest(body io.Reader) (modelstore.IngestTreeRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxRequestBytes+1))
	if err != nil {
		return modelstore.IngestTreeRequest{}, errors.New("read request body failed")
	}
	if len(raw) == 0 || len(raw) > maxRequestBytes {
		return modelstore.IngestTreeRequest{}, errors.New("request body is empty or exceeds size limit")
	}
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		return modelstore.IngestTreeRequest{}, errors.New("request body is not valid JSON")
	}
	forgerName := strings.TrimSpace(w.ForgerName)
	if forgerName == "" {
		forgerName = strings.TrimSpace(w.Forger)
	}
	req := modelstore.IngestTreeRequest{
		Namespace:  strings.TrimSpace(w.Namespace),
		ForgerName: forgerName,
		ForgerUID:  strings.TrimSpace(w.ForgerUID),
		PodName:    strings.TrimSpace(w.PodName),
		PodUID:     strings.TrimSpace(w.PodUID),
	}
	if !safeNamePattern.MatchString(req.Namespace) ||
		!safeNamePattern.MatchString(req.ForgerName) ||
		!safeNamePattern.MatchString(req.PodName) ||
		!safeUIDPattern.MatchString(req.PodUID) {
		return modelstore.IngestTreeRequest{}, errors.New("namespace, forgerName, podName, and podUID must be valid identifiers")
	}
	if req.ForgerUID != "" && !safeUIDPattern.MatchString(req.ForgerUID) {
		return modelstore.IngestTreeRequest{}, errors.New("forgerUID is not a valid identifier")
	}
	return req, nil
}

func resolveOutputSizeLimit(limit *resource.Quantity) int64 {
	if limit == nil || limit.Sign() <= 0 {
		return MaxTotalBytes
	}
	size := limit.Value()
	if size < MinOutputSizeBytes {
		return MinOutputSizeBytes
	}
	if size > MaxOutputSizeBytes {
		return MaxOutputSizeBytes
	}
	return size
}

// verifySandboxOwnership checks live API state before reading a single byte from kubelet's disk.
func (s *Server) verifySandboxOwnership(ctx context.Context, req modelstore.IngestTreeRequest) (int64, int, string, string) {
	if s.APIReader == nil {
		return 0, http.StatusServiceUnavailable, "Unavailable", "live API reader is unavailable"
	}

	forger := new(workercore.ModelDeploymentForger)
	if err := s.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: req.Namespace, Name: req.ForgerName}, forger); err != nil {
		if kerrors.IsNotFound(err) {
			return 0, http.StatusForbidden, "ForgerNotFound", "the owning ModelDeploymentForger does not exist"
		}
		klog.ErrorS(err, "read ModelDeploymentForger for ingest")
		return 0, http.StatusServiceUnavailable, "Unavailable", "failed reading ModelDeploymentForger"
	}
	if forger.UID == "" || (req.ForgerUID != "" && string(forger.UID) != req.ForgerUID) {
		return 0, http.StatusForbidden, "ForgerUIDMismatch", "the ModelDeploymentForger UID does not match"
	}

	sandboxMatched := false
	for i := range forger.Status.Roles {
		sb := forger.Status.Roles[i].ActiveSandbox
		if sb == nil || sb.PodUID != req.PodUID {
			continue
		}
		if sb.PodName != "" && sb.PodName != req.PodName {
			return 0, http.StatusForbidden, "SandboxMismatch", "activeSandbox podName does not match request"
		}
		if sb.NodeName != "" && sb.NodeName != s.NodeID {
			return 0, http.StatusForbidden, "SandboxNodeMismatch", "activeSandbox nodeName does not match this node"
		}
		sandboxMatched = true
		break
	}
	if !sandboxMatched {
		return 0, http.StatusForbidden, "SandboxNotActive", "the Pod UID is not recorded as an activeSandbox on ModelDeploymentForger"
	}

	pod := new(core.Pod)
	if err := s.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: req.Namespace, Name: req.PodName}, pod); err != nil {
		if kerrors.IsNotFound(err) {
			return 0, http.StatusForbidden, "PodNotFound", "the ForgeSandbox Pod does not exist"
		}
		klog.ErrorS(err, "read ForgeSandbox Pod for ingest")
		return 0, http.StatusServiceUnavailable, "Unavailable", "failed reading ForgeSandbox Pod"
	}
	if string(pod.UID) != req.PodUID {
		return 0, http.StatusForbidden, "PodUIDMismatch", "the ForgeSandbox Pod UID does not match"
	}
	if s.NodeID == "" || pod.Spec.NodeName != s.NodeID {
		return 0, http.StatusForbidden, "PodNodeMismatch", "the ForgeSandbox Pod is not scheduled on this node"
	}
	owner := meta.GetControllerOf(pod)
	if owner == nil || owner.UID != forger.UID || owner.Kind != "ModelDeploymentForger" || owner.Name != forger.Name {
		return 0, http.StatusForbidden, "PodOwnerMismatch", "the ForgeSandbox Pod is not controlled by the owning ModelDeploymentForger"
	}

	for _, initName := range []string{initContainerRunner, initContainerVerifier} {
		if !initContainerSucceeded(pod.Status.InitContainerStatuses, initName) {
			return 0, http.StatusConflict, "SandboxNotTerminated",
				fmt.Sprintf("initContainer %s has not terminated with exit code 0", initName)
		}
	}

	return resolveOutputSizeLimit(forger.Spec.Sandbox.OutputSizeLimit), 0, "", ""
}

func initContainerSucceeded(statuses []core.ContainerStatus, wantName string) bool {
	for i := range statuses {
		cs := &statuses[i]
		if cs.Name != wantName {
			continue
		}
		return cs.State.Terminated != nil &&
			cs.State.Terminated.ExitCode == 0 &&
			cs.State.Running == nil &&
			cs.State.Waiting == nil
	}
	return false
}

func (s *Server) resolveSandboxDirs(podUID string) (verifiedDir, outputDir string, err error) {
	podsDir := filepath.Join(s.KubeletDir, "pods")
	podsRoot, err := os.OpenRoot(podsDir)
	if err != nil {
		return "", "", fmt.Errorf("open kubelet pods root: %w", err)
	}
	defer func() { _ = podsRoot.Close() }()

	relVerified := filepath.ToSlash(filepath.Join(podUID, "volumes", "kubernetes.io~empty-dir", forgeVerifiedVolumeName))
	relOutput := filepath.ToSlash(filepath.Join(podUID, "volumes", "kubernetes.io~empty-dir", forgeOutputVolumeName))
	if err := ensureDirWithinRoot(podsRoot, relVerified); err != nil {
		return "", "", fmt.Errorf("verify forge-verified path: %w", err)
	}
	if err := ensureDirWithinRoot(podsRoot, relOutput); err != nil {
		return "", "", fmt.Errorf("verify forge-output path: %w", err)
	}

	return filepath.Join(podsDir, filepath.FromSlash(relVerified)),
		filepath.Join(podsDir, filepath.FromSlash(relOutput)),
		nil
}

func ensureDirWithinRoot(root *os.Root, relSlash string) error {
	for i := range len(relSlash) {
		if relSlash[i] != '/' {
			continue
		}
		info, err := root.Lstat(relSlash[:i])
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%q is not a plain directory", relSlash[:i])
		}
	}
	info, err := root.Lstat(relSlash)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a plain directory", relSlash)
	}
	return nil
}

func readAndValidateVerificationReport(verifiedDir string) (verificationReport, error) {
	var out verificationReport
	root, err := os.OpenRoot(verifiedDir)
	if err != nil {
		return out, err
	}
	defer func() { _ = root.Close() }()

	info, err := root.Lstat(verificationReportFileName)
	if err != nil {
		return out, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return out, errors.New("verification-report.json is not a regular file")
	}

	dirFile, err := openDirNoFollow(verifiedDir)
	if err != nil {
		return out, err
	}
	defer func() { _ = dirFile.Close() }()

	f, size, err := openRegularSingleLinkAt(dirFile, verificationReportFileName)
	if err != nil {
		return out, err
	}
	defer func() { _ = f.Close() }()
	if size <= 0 || size > maxJSONMetadataBytes {
		return out, fmt.Errorf("verification-report.json size %d is out of bounds", size)
	}

	raw, err := io.ReadAll(io.LimitReader(f, maxJSONMetadataBytes+1))
	if err != nil || int64(len(raw)) != size {
		return out, errors.New("read verification-report.json failed")
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("parse verification-report.json: %w", err)
	}
	if out.SchemaVersion != VerificationReportSchemaVersion {
		return out, fmt.Errorf("unsupported schemaVersion %q", out.SchemaVersion)
	}

	status := strings.TrimSpace(out.OverallStatus)
	altStatus := strings.TrimSpace(out.Status)
	if status != "" && altStatus != "" && status != altStatus {
		return out, errors.New("conflicting overallStatus and status fields")
	}
	if status == "" {
		status = altStatus
	}
	if status != "PASSED" {
		return out, fmt.Errorf("verification status is %q, want PASSED", status)
	}

	if out.PassedCount < 0 || out.VerifiedCount < 0 || out.FailedCount < 0 {
		return out, errors.New("negative verification counts")
	}
	if out.FailedCount > 0 {
		return out, fmt.Errorf("verification report failedCount is %d, want 0", out.FailedCount)
	}
	if out.PassedCount > 0 && out.VerifiedCount > 0 && out.PassedCount != out.VerifiedCount {
		return out, errors.New("conflicting passedCount and verifiedCount")
	}
	verifiedCount := out.verifiedEntryCount()
	if verifiedCount < 1 {
		return out, errors.New("verified count must be at least 1")
	}
	if len(out.Entries) > 0 {
		if len(out.Entries) != verifiedCount {
			return out, fmt.Errorf("verification report entries length %d does not match verified count %d",
				len(out.Entries), verifiedCount)
		}
		for i, e := range out.Entries {
			if !e.Passed {
				return out, fmt.Errorf("verification report entry %d did not pass", i)
			}
			if e.Evaluation != nil && strings.TrimSpace(e.Evaluation.Status) != "PASSED" {
				return out, fmt.Errorf("verification report entry %d evaluation status is %q, want PASSED",
					i, e.Evaluation.Status)
			}
		}
	}
	if store.HexOf(out.DispatchManifestDigest) == "" {
		return out, fmt.Errorf("invalid dispatchManifestDigest %q", out.DispatchManifestDigest)
	}

	return out, nil
}

func validateDispatchManifest(raw []byte, wantVerifiedEntries int) ([]string, error) {
	var dm dispatchManifest
	if err := json.Unmarshal(raw, &dm); err != nil {
		return nil, fmt.Errorf("parse dispatch-manifest.json: %w", err)
	}
	if dm.SchemaVersion != DispatchManifestSchemaVersion {
		return nil, fmt.Errorf("unsupported dispatch-manifest schemaVersion %q", dm.SchemaVersion)
	}
	if len(dm.Entries) == 0 {
		return nil, errors.New("dispatch-manifest.json contains no entries")
	}
	if wantVerifiedEntries > 0 && len(dm.Entries) != wantVerifiedEntries {
		return nil, fmt.Errorf("verified entry count %d does not cover all %d entries in dispatch-manifest.json",
			wantVerifiedEntries, len(dm.Entries))
	}

	target := modelkernelkey.CompatibilityTarget{
		Engine:        dm.Compatibility.Engine,
		EngineVersion: dm.Compatibility.EngineVersion,
		Manufacturer:  dm.Compatibility.Manufacturer,
		Arch:          dm.Compatibility.Architecture,
	}
	if _, err := modelkernelkey.NormalizeEngine(target.Engine); err != nil {
		return nil, fmt.Errorf("compatibility.engine: %w", err)
	}
	if _, err := modelkernelkey.NormalizeEngineVersion(target.EngineVersion); err != nil {
		return nil, fmt.Errorf("compatibility.engineVersion: %w", err)
	}
	if _, err := modelkernelkey.NormalizeManufacturer(target.Manufacturer); err != nil {
		return nil, fmt.Errorf("compatibility.manufacturer: %w", err)
	}
	if _, err := modelkernelkey.NormalizeArch(target.Manufacturer, target.Arch); err != nil {
		return nil, fmt.Errorf("compatibility.architecture: %w", err)
	}

	seen := make(map[string]struct{}, len(dm.Entries))
	keys := make([]string, 0, len(dm.Entries))
	for i, e := range dm.Entries {
		rawKey := strings.TrimSpace(e.LocalKernelKey)
		parsed, err := modelkernelkey.Parse(rawKey)
		if err != nil {
			return nil, fmt.Errorf("entry %d localKernelKey: %w", i, err)
		}
		if excluded, reason := modelkernelkey.IsExcluded(parsed.OpFamily); excluded {
			return nil, fmt.Errorf("entry %d localKernelKey excluded (%s)", i, reason)
		}
		if err := modelkernelkey.MatchCompatibility(parsed, target); err != nil {
			return nil, fmt.Errorf("entry %d localKernelKey compatibility: %w", i, err)
		}
		if strings.TrimSpace(e.LibraryPath) == "" {
			return nil, fmt.Errorf("entry %d libraryPath is empty", i)
		}
		if _, dup := seen[rawKey]; !dup {
			seen[rawKey] = struct{}{}
			keys = append(keys, rawKey)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

// walkForgeOutput walks outputDir via os.OpenRoot and openat(O_NOFOLLOW|O_NONBLOCK), enforcing
// single-link regular files and file/byte/depth/path caps at every step.
func walkForgeOutput(
	ctx context.Context,
	outputDir string,
	maxTotalBytes int64,
	onDir func(relPath string) error,
	onFile func(relPath string, f *os.File, size int64) error,
) (int64, error) {
	if maxTotalBytes <= 0 {
		maxTotalBytes = MaxTotalBytes
	}
	root, err := os.OpenRoot(outputDir)
	if err != nil {
		return 0, &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: err.Error()}
	}
	defer func() { _ = root.Close() }()

	dirFile, err := openDirNoFollow(outputDir)
	if err != nil {
		return 0, &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: err.Error()}
	}
	defer func() { _ = dirFile.Close() }()

	var (
		fileCount  int
		totalBytes int64
	)
	var walk func(parent *os.File, prefix string, depth int) error
	walk = func(parent *os.File, prefix string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > MaxPathDepth {
			return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: "path depth exceeds cap"}
		}
		names, err := readSortedDirNames(parent)
		if err != nil {
			return err
		}
		for _, name := range names {
			relPath := name
			if prefix != "" {
				relPath = prefix + "/" + name
			}
			if err := validateRelativePath(relPath, depth); err != nil {
				return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: err.Error()}
			}
			info, err := root.Lstat(relPath)
			if err != nil {
				return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: err.Error()}
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: "symlinks are forbidden in forge-output"}
			}
			if info.IsDir() {
				if onDir != nil {
					if err := onDir(relPath); err != nil {
						return err
					}
				}
				subDir, err := openSubdirNoFollow(parent, name)
				if err != nil {
					return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: err.Error()}
				}
				walkErr := walk(subDir, relPath, depth+1)
				_ = subDir.Close()
				if walkErr != nil {
					return walkErr
				}
				continue
			}
			if !info.Mode().IsRegular() {
				return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: "non-regular files are forbidden in forge-output"}
			}

			fileCount++
			if fileCount > MaxFiles {
				return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: "file count exceeds cap"}
			}
			f, size, err := openRegularSingleLinkAt(parent, name)
			if err != nil {
				return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: err.Error()}
			}
			if size > maxTotalBytes-totalBytes {
				_ = f.Close()
				return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: "total size exceeds cap"}
			}
			totalBytes += size
			ferr := onFile(relPath, f, size)
			_ = f.Close()
			if ferr != nil {
				return ferr
			}
		}
		return nil
	}

	if err := walk(dirFile, "", 1); err != nil {
		return 0, err
	}
	return totalBytes, nil
}

// preScanForgeOutput validates caps, file types, and verification-report digest agreement before
// gc.Collector.Reserve is called.
func preScanForgeOutput(
	ctx context.Context, outputDir, wantDispatchDigest string, wantVerifiedEntries int, maxTotalBytes int64,
) (int64, error) {
	seenDispatch := false
	totalBytes, err := walkForgeOutput(ctx, outputDir, maxTotalBytes, nil, func(relPath string, f *os.File, size int64) error {
		if relPath != dispatchManifestFileName {
			return nil
		}
		if size <= 0 || size > maxJSONMetadataBytes {
			return errors.New("dispatch-manifest.json size is out of bounds")
		}
		raw, rerr := io.ReadAll(io.LimitReader(f, maxJSONMetadataBytes+1))
		if rerr != nil || int64(len(raw)) != size {
			return errors.New("read dispatch-manifest.json failed")
		}
		sum := sha256.Sum256(raw)
		gotDigest := "sha256:" + hex.EncodeToString(sum[:])
		if gotDigest != wantDispatchDigest {
			return fmt.Errorf("dispatch-manifest.json digest %s does not match verification report %s",
				gotDigest, wantDispatchDigest)
		}
		if _, err := validateDispatchManifest(raw, wantVerifiedEntries); err != nil {
			return err
		}
		seenDispatch = true
		return nil
	})
	if err != nil {
		return 0, err
	}
	if !seenDispatch {
		return 0, errors.New("dispatch-manifest.json is missing from forge-output")
	}
	return totalBytes, nil
}

// copyAndPublish copies forge-output into a staging directory under <cacheRoot>/partial/ while
// hashing every copied byte in a single pass, verifies dispatch-manifest.json against the
// verification report, writes manifest.json (0600, Kind: "Kernel"), publishes atomically with
// Source: "Forge" and Kind: "Kernel", stamps LastUsedTime, and flushes NodeModelStore status.
func (s *Server) copyAndPublish(
	ctx context.Context,
	outputDir, wantDispatchDigest string,
	wantVerifiedEntries int,
	maxTotalBytes int64,
	copiedBytes *atomic.Int64,
) (modelstore.IngestTreeResponse, error) {
	var zero modelstore.IngestTreeResponse
	if maxTotalBytes <= 0 {
		maxTotalBytes = MaxTotalBytes
	}

	partialParent := filepath.Join(s.Store.Root(), "partial")
	stagingDir, err := os.MkdirTemp(partialParent, ".ingest-")
	if err != nil {
		return zero, fmt.Errorf("create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()

	stagedTreeDir := filepath.Join(stagingDir, "tree")
	if err := os.MkdirAll(stagedTreeDir, 0o755); err != nil {
		return zero, err
	}

	var (
		entries              []modelartifact.ManifestEntry
		totalCopied          int64
		copiedDispatchDigest string
		copiedKernelKeys     []string
		buf                  = make([]byte, copyChunkBytes)
	)
	_, err = walkForgeOutput(ctx, outputDir, maxTotalBytes,
		func(relPath string) error {
			return os.MkdirAll(filepath.Join(stagedTreeDir, filepath.FromSlash(relPath)), 0o755)
		},
		func(relPath string, srcFile *os.File, expectedSize int64) error {
			destPath := filepath.Join(stagedTreeDir, filepath.FromSlash(relPath))
			if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
				return err
			}
			dstFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}

			hasher := sha256.New()
			var dispatchBuf bytes.Buffer
			writers := []io.Writer{dstFile, hasher}
			if relPath == dispatchManifestFileName {
				writers = append(writers, &dispatchBuf)
			}
			mw := io.MultiWriter(writers...)

			var writtenFile int64
			for {
				n, rerr := srcFile.Read(buf)
				if n > 0 {
					if int64(n) > maxTotalBytes-totalCopied {
						_ = dstFile.Close()
						return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: "total size exceeds cap"}
					}
					if relPath == dispatchManifestFileName && int64(dispatchBuf.Len()+n) > maxJSONMetadataBytes {
						_ = dstFile.Close()
						return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: "dispatch-manifest.json exceeds size cap"}
					}
					if _, werr := mw.Write(buf[:n]); werr != nil {
						_ = dstFile.Close()
						return werr
					}
					writtenFile += int64(n)
					totalCopied += int64(n)
					copiedBytes.Add(int64(n))
				}
				if rerr != nil {
					if errors.Is(rerr, io.EOF) {
						break
					}
					_ = dstFile.Close()
					return rerr
				}
			}
			if err := dstFile.Sync(); err != nil {
				_ = dstFile.Close()
				return err
			}
			if err := dstFile.Close(); err != nil {
				return err
			}
			if writtenFile != expectedSize {
				return &httpError{code: http.StatusUnprocessableEntity, reason: "InvalidSandboxOutput", msg: "file size changed during copy"}
			}

			fileDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
			if relPath == dispatchManifestFileName {
				if fileDigest != wantDispatchDigest {
					return &httpError{
						code:   http.StatusUnprocessableEntity,
						reason: "VerificationMismatch",
						msg:    "copied dispatch-manifest.json digest does not match verification report",
					}
				}
				verifiedKeys, err := validateDispatchManifest(dispatchBuf.Bytes(), wantVerifiedEntries)
				if err != nil {
					return &httpError{
						code:   http.StatusUnprocessableEntity,
						reason: "InvalidDispatchManifest",
						msg:    "dispatch-manifest.json failed schema validation",
					}
				}
				copiedDispatchDigest = fileDigest
				copiedKernelKeys = verifiedKeys
			}

			entries = append(entries, modelartifact.ManifestEntry{
				Path:   relPath,
				Size:   writtenFile,
				Digest: fileDigest,
			})
			return nil
		},
	)
	if err != nil {
		return zero, err
	}
	if copiedDispatchDigest == "" {
		return zero, &httpError{
			code:   http.StatusUnprocessableEntity,
			reason: "InvalidSandboxOutput",
			msg:    "dispatch-manifest.json is missing from forge-output",
		}
	}

	manifest, err := modelartifact.NewManifest(entries)
	if err != nil {
		return zero, &httpError{
			code:   http.StatusUnprocessableEntity,
			reason: "InvalidManifest",
			msg:    "failed encoding canonical manifest",
		}
	}
	treeHex := store.HexOf(manifest.Digest)
	now := s.now()

	s.publishMu.Lock()
	err = s.publishAttemptLocked(treeHex, stagedTreeDir, manifest, now)
	s.publishMu.Unlock()
	if err != nil {
		return zero, err
	}

	if err := s.Store.UpdateDigest(treeHex, func(rec *store.DigestRecord) {
		rec.Failures, rec.Reason, rec.Message, rec.RetryTime = 0, "", "", time.Time{}
		rec.LastUsedTime = now
	}); err != nil {
		return zero, fmt.Errorf("record digest lastUsedTime: %w", err)
	}

	if s.FlushReport != nil {
		if err := s.FlushReport(ctx); err != nil {
			klog.ErrorS(err, "flush NodeModelStore status after ingest", "digest", manifest.Digest)
			return zero, &httpError{
				code:   http.StatusServiceUnavailable,
				reason: "ReportFlushFailed",
				msg:    "published tree locally, waiting to flush NodeModelStore status",
			}
		}
	}

	return modelstore.IngestTreeResponse{
		ManifestDigest:         manifest.Digest,
		DispatchManifestDigest: copiedDispatchDigest,
		SizeBytes:              manifest.SizeBytes,
		FileCount:              manifest.FileCount,
		SeedNode:               s.NodeID,
		KernelKeys:             copiedKernelKeys,
	}, nil
}

func (s *Server) publishAttemptLocked(treeHex, stagedTreeDir string, manifest modelartifact.Manifest, now time.Time) error {
	unlock, ok := s.Store.TryLockAttempt(treeHex)
	if !ok {
		return &httpError{
			code:   http.StatusServiceUnavailable,
			reason: "AttemptInProgress",
			msg:    "another store operation is active for this digest; retry",
		}
	}
	defer unlock()

	if s.Store.IsPublished(treeHex) {
		marker, err := s.Store.ReadMarker(treeHex)
		if err != nil {
			return err
		}
		if marker.Kind != store.MarkerKindKernel {
			return &httpError{
				code:   http.StatusConflict,
				reason: "ContentClassConflict",
				msg:    "digest is already published with a non-kernel content class",
			}
		}
		return nil
	}

	attempt, err := s.Store.NewAttempt(treeHex)
	if err != nil {
		return err
	}
	attemptTree := filepath.Join(attempt.Dir(), "tree")
	if err := os.RemoveAll(attemptTree); err != nil {
		return err
	}
	if err := os.Rename(stagedTreeDir, attemptTree); err != nil {
		return err
	}

	listing := peer.Listing{
		Digest:  manifest.Digest,
		Kind:    store.MarkerKindKernel,
		Entries: make([]peer.ManifestFile, 0, len(manifest.Entries)),
	}
	for _, e := range manifest.Entries {
		listing.Entries = append(listing.Entries, peer.ManifestFile{
			Path:   e.Path,
			Size:   e.Size,
			Digest: e.Digest,
		})
	}
	if err := writeManifestFile0600(filepath.Join(attempt.Dir(), "manifest.json"), listing); err != nil {
		return fmt.Errorf("write published manifest.json: %w", err)
	}

	return attempt.Publish(store.Marker{
		Digest:         manifest.Digest,
		ManifestFormat: modelartifact.ManifestHeader,
		SizeBytes:      manifest.SizeBytes,
		FileCount:      manifest.FileCount,
		PublishedTime:  now,
		Source:         store.SourceForge,
		Kind:           store.MarkerKindKernel,
	})
}

func writeManifestFile0600(dest string, listing peer.Listing) error {
	b, err := json.Marshal(listing)
	if err != nil {
		return err
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func validateRelativePath(relPath string, depth int) error {
	if relPath == "" || len(relPath) > MaxPathLength || depth > MaxPathDepth || !utf8.ValidString(relPath) {
		return fmt.Errorf("invalid relative path %q", relPath)
	}
	for _, r := range relPath {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return fmt.Errorf("control character or backslash in path %q", relPath)
		}
	}
	segments := 0
	for seg := range strings.SplitSeq(relPath, "/") {
		segments++
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("invalid path segment in %q", relPath)
		}
	}
	if segments != depth || segments > MaxPathDepth {
		return fmt.Errorf("path depth %d exceeds %d", segments, MaxPathDepth)
	}
	return nil
}

func readSortedDirNames(dir *os.File) ([]string, error) {
	if _, err := dir.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names, nil
}

func openDirNoFollow(absPath string) (*os.File, error) {
	fd, err := openatRetryEINTR(unix.AT_FDCWD, absPath, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), absPath), nil
}

func openSubdirNoFollow(parent *os.File, name string) (*os.File, error) {
	if strings.ContainsRune(name, '/') || name == "" || name == "." || name == ".." {
		return nil, fs.ErrInvalid
	}
	fd, err := openatRetryEINTR(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func openRegularSingleLinkAt(parent *os.File, name string) (*os.File, int64, error) {
	if strings.ContainsRune(name, '/') || name == "" || name == "." || name == ".." {
		return nil, 0, fs.ErrInvalid
	}
	fd, err := openatRetryEINTR(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC)
	if err != nil {
		return nil, 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, 0, errors.New("not a regular file")
	}
	if st.Nlink != 1 {
		_ = unix.Close(fd)
		return nil, 0, fmt.Errorf("hardlink count %d != 1", st.Nlink)
	}
	return os.NewFile(uintptr(fd), name), st.Size, nil
}

func openatRetryEINTR(dirfd int, path string, flags int) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, path, flags, 0)
		if !errors.Is(err, unix.EINTR) {
			return fd, err
		}
	}
}
