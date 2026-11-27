package forger

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/utils/json"
)

const (
	// DefaultIngestTimeout bounds one POST /ingest/v1/trees call to model-manager.
	DefaultIngestTimeout = 2 * time.Minute
	// DefaultIngestMaxResponseBytes bounds the JSON response read from model-manager.
	DefaultIngestMaxResponseBytes int64 = 1 << 20
	maxIngestTokenBytes           int64 = 64 << 10
)

var (
	// ErrIngestBadRequest reports HTTP 400 from model-manager /ingest/v1/trees.
	ErrIngestBadRequest = errors.New("ingest bad request")
	// ErrIngestUnauthorized reports HTTP 401 when the projected ingest token is rejected.
	ErrIngestUnauthorized = errors.New("ingest unauthorized")
	// ErrIngestForbidden reports HTTP 403 when sandbox ownership or forger status checks fail.
	ErrIngestForbidden = errors.New("ingest forbidden")
	// ErrIngestConflict reports HTTP 409 (e.g. init containers not terminated or content-class conflict).
	ErrIngestConflict = errors.New("ingest conflict")
	// ErrIngestInvalidOutput reports HTTP 422 when verification report or output tree checks fail.
	ErrIngestInvalidOutput = errors.New("ingest invalid sandbox output")
	// ErrIngestInsufficientStorage reports HTTP 507 when node cache capacity is exhausted.
	ErrIngestInsufficientStorage = errors.New("ingest insufficient storage")
	// ErrIngestServerError reports 5xx errors from model-manager /ingest/v1/trees.
	ErrIngestServerError = errors.New("ingest server error")
	// ErrIngestRedirectRefused reports that model-manager returned an HTTP redirect.
	ErrIngestRedirectRefused = errors.New("ingest redirect refused")
)

// IngestError classifies an HTTP error returned by model-manager POST /ingest/v1/trees.
type IngestError struct {
	StatusCode int
	Reason     string
	Message    string
	Err        error
}

func (e *IngestError) Error() string {
	switch {
	case e.Reason != "" && e.Message != "":
		return fmt.Sprintf("ingest returned status %d (%s): %s", e.StatusCode, e.Reason, e.Message)
	case e.Message != "":
		return fmt.Sprintf("ingest returned status %d: %s", e.StatusCode, e.Message)
	case e.Reason != "":
		return fmt.Sprintf("ingest returned status %d (%s)", e.StatusCode, e.Reason)
	default:
		return fmt.Sprintf("ingest returned status %d", e.StatusCode)
	}
}

func (e *IngestError) Unwrap() error {
	return e.Err
}

// IngestClientOption configures an IngestClient.
type IngestClientOption func(*IngestClient)

// WithIngestTokenFile overrides the projected bearer token file path.
func WithIngestTokenFile(path string) IngestClientOption {
	return func(c *IngestClient) {
		if strings.TrimSpace(path) != "" {
			c.tokenFile = strings.TrimSpace(path)
		}
	}
}

// WithIngestTimeout overrides the per-call HTTP timeout when positive.
func WithIngestTimeout(d time.Duration) IngestClientOption {
	return func(c *IngestClient) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithIngestMaxResponseBytes overrides the maximum response body size read from model-manager.
func WithIngestMaxResponseBytes(n int64) IngestClientOption {
	return func(c *IngestClient) {
		if n > 0 {
			c.maxResponseBytes = n
		}
	}
}

// WithIngestRootCAs configures the root CA pool used to verify model-manager's HTTPS certificate.
func WithIngestRootCAs(pool *x509.CertPool) IngestClientOption {
	return func(c *IngestClient) {
		c.rootCAs = pool
	}
}

// WithIngestCAFile loads a PEM CA bundle from path for verifying model-manager's HTTPS certificate.
func WithIngestCAFile(path string) IngestClientOption {
	return func(c *IngestClient) {
		c.caFile = strings.TrimSpace(path)
	}
}

// WithIngestInsecureSkipVerify explicitly disables TLS certificate verification for self-signed
// cert-manager-disabled deployments.
func WithIngestInsecureSkipVerify(insecure bool) IngestClientOption {
	return func(c *IngestClient) {
		c.insecureSkipVerify = insecure
	}
}

// IngestClient calls model-manager's POST /ingest/v1/trees endpoint on the seed node using a
// projected ServiceAccount token read freshly on every request.
type IngestClient struct {
	tokenFile          string
	timeout            time.Duration
	maxResponseBytes   int64
	rootCAs            *x509.CertPool
	caFile             string
	insecureSkipVerify bool
	httpClient         *http.Client
}

// NewIngestClient constructs an IngestClient with safe TLS and no-proxy defaults.
func NewIngestClient(opts ...IngestClientOption) (*IngestClient, error) {
	c := &IngestClient{
		tokenFile:        modelstore.IngestTokenFile,
		timeout:          DefaultIngestTimeout,
		maxResponseBytes: DefaultIngestMaxResponseBytes,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if c.caFile != "" {
		pemBytes, err := os.ReadFile(c.caFile)
		if err != nil {
			return nil, fmt.Errorf("read ingest CA file %q: %w", c.caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("ingest CA file %q contains no valid PEM certificates", c.caFile)
		}
		tlsCfg.RootCAs = pool
	} else if c.rootCAs != nil {
		tlsCfg.RootCAs = c.rootCAs
	}
	if c.insecureSkipVerify {
		// #nosec G402 -- explicit opt-in for cluster-internal self-signed model-manager TLS.
		tlsCfg.InsecureSkipVerify = true
	}

	transport := httpx.Transport(httpx.TransportOptions().WithoutProxy().WithTLSClientConfig(tlsCfg))
	c.httpClient = &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return c, nil
}

// IngestTree sends POST https://<host>:<port>/ingest/v1/trees with a freshly read projected bearer
// token and returns the published NodeModelStore tree metadata.
func (c *IngestClient) IngestTree(
	ctx context.Context, seedNodeHost string, port int, req modelstore.IngestTreeRequest,
) (*modelstore.IngestTreeResponse, error) {
	host := strings.TrimSpace(seedNodeHost)
	if host == "" {
		return nil, fmt.Errorf("%w: seedNodeHost must not be empty", ErrIngestBadRequest)
	}
	if port <= 0 {
		port = modelstore.DefaultPluginSecurePort
	}
	if port > 65535 {
		return nil, fmt.Errorf("%w: port %d is out of range", ErrIngestBadRequest, port)
	}
	if strings.TrimSpace(req.Namespace) == "" ||
		strings.TrimSpace(req.ForgerName) == "" ||
		strings.TrimSpace(req.PodName) == "" ||
		strings.TrimSpace(req.PodUID) == "" {
		return nil, fmt.Errorf("%w: namespace, forgerName, podName, and podUID are required", ErrIngestBadRequest)
	}

	token, err := readProjectedToken(c.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read projected ingest token %q: %w", c.tokenFile, err)
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode ingest request: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	endpoint := "https://" + net.JoinHostPort(host, strconv.Itoa(port)) + modelstore.IngestTreesPath
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build ingest request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		return nil, fmt.Errorf("%w: status %d", ErrIngestRedirectRefused, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read ingest response: %w", err)
	}
	if int64(len(body)) > c.maxResponseBytes {
		return nil, fmt.Errorf("ingest response exceeded %d bytes", c.maxResponseBytes)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, classifyIngestError(resp.StatusCode, body)
	}

	var out modelstore.IngestTreeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode ingest response: %w", err)
	}
	if err := ValidateSHA256Digest(out.ManifestDigest); err != nil {
		return nil, fmt.Errorf("ingest response manifestDigest: %w", err)
	}
	if err := ValidateSHA256Digest(out.DispatchManifestDigest); err != nil {
		return nil, fmt.Errorf("ingest response dispatchManifestDigest: %w", err)
	}
	return &out, nil
}

func readProjectedToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(io.LimitReader(f, maxIngestTokenBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(raw)) > maxIngestTokenBytes {
		return "", fmt.Errorf("token exceeds %d bytes", maxIngestTokenBytes)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("projected token file is empty")
	}
	return token, nil
}

func classifyIngestError(statusCode int, body []byte) error {
	var env struct {
		Reason string `json:"reason"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(body, &env)

	var sentinel error
	switch {
	case statusCode == http.StatusBadRequest:
		sentinel = ErrIngestBadRequest
	case statusCode == http.StatusUnauthorized:
		sentinel = ErrIngestUnauthorized
	case statusCode == http.StatusForbidden:
		sentinel = ErrIngestForbidden
	case statusCode == http.StatusConflict:
		sentinel = ErrIngestConflict
	case statusCode == http.StatusUnprocessableEntity:
		sentinel = ErrIngestInvalidOutput
	case statusCode == http.StatusInsufficientStorage:
		sentinel = ErrIngestInsufficientStorage
	case statusCode >= http.StatusInternalServerError:
		sentinel = ErrIngestServerError
	}

	return &IngestError{
		StatusCode: statusCode,
		Reason:     env.Reason,
		Message:    env.Error,
		Err:        sentinel,
	}
}
