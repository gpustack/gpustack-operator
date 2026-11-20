package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PrivateSource is the bounded, authenticated source-process control channel.
type PrivateSource interface {
	Call(context.Context, string, any) (PrivateReply, error)
}

// PrivateReply is the source process's operation or session verdict.
type PrivateReply struct {
	RequestID string `json:"requestID,omitempty"`
	State     string `json:"state"`
	Verdict   string `json:"verdict,omitempty"`
}

// PrivateAddress is a validated local-only private control endpoint: either a
// Unix domain socket path or an explicit loopback TCP host:port.
type PrivateAddress struct {
	UnixPath string
	HostPort string
}

type httpPrivateSource struct {
	address string
	token   string
	client  *http.Client
	initErr error
}

// privateObservationError means the source's reply was not observed. The
// operation remains owned and may only be recovered through the same identity.
type privateObservationError struct{ reason string }

func (e *privateObservationError) Error() string {
	return "private source observation unavailable: " + e.reason
}

var errPrivateRedirect = errors.New("private redirect refused")

const unixAddrPrefix = "unix://"

// ValidatePrivateAddress verifies that addr names a local-only private
// endpoint: either unix://<abs-path> (absolute, non-empty, not "/", without
// query or fragment) or explicit loopback HTTP/TCP (127.0.0.1:<port>,
// [::1]:<port>, or localhost:<port>, optionally prefixed with http:// and
// without path, userinfo, query, or fragment).
func ValidatePrivateAddress(addr string) error {
	_, err := parsePrivateAddress(addr, false)
	return err
}

// ParsePrivateAddress parses and validates a local-only private endpoint. When
// allowEphemeralPort is true, TCP port 0 is accepted for listeners.
func ParsePrivateAddress(addr string, allowEphemeralPort bool) (PrivateAddress, error) {
	return parsePrivateAddress(addr, allowEphemeralPort)
}

func parsePrivateAddress(addr string, allowEphemeralPort bool) (PrivateAddress, error) {
	if addr == "" {
		return PrivateAddress{}, errors.New("private address must not be empty")
	}
	if strings.HasPrefix(addr, unixAddrPrefix) {
		sockPath := strings.TrimPrefix(addr, unixAddrPrefix)
		if strings.ContainsAny(sockPath, "?#\x00") {
			return PrivateAddress{}, fmt.Errorf("unix socket path %q must not contain query, fragment, or null bytes", addr)
		}
		if sockPath == "" || !filepath.IsAbs(sockPath) || filepath.Clean(sockPath) == "/" {
			return PrivateAddress{}, fmt.Errorf("unix socket path %q must be a non-empty absolute path", addr)
		}
		return PrivateAddress{UnixPath: filepath.Clean(sockPath)}, nil
	}
	if strings.ContainsAny(addr, "?#\x00") {
		return PrivateAddress{}, fmt.Errorf("private address %q must not contain query or fragment", addr)
	}
	rawURL := addr
	if strings.Contains(addr, "://") {
		if !strings.HasPrefix(addr, "http://") {
			return PrivateAddress{}, fmt.Errorf("private address %q must use http:// or unix:// scheme", addr)
		}
	} else {
		rawURL = "http://" + addr
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return PrivateAddress{}, fmt.Errorf("parse private address %q: %w", addr, err)
	}
	if u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return PrivateAddress{}, fmt.Errorf("private address %q must be a bare loopback host:port without path, userinfo, query, or fragment", addr)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil || host == "" || portStr == "" {
		return PrivateAddress{}, fmt.Errorf("private address %q must specify an explicit loopback host and port", addr)
	}
	if !isLoopbackHost(host) {
		return PrivateAddress{}, fmt.Errorf("private address %q must target a loopback host", addr)
	}
	minPort := 1
	if allowEphemeralPort {
		minPort = 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < minPort || port > 65535 || strconv.Itoa(port) != portStr {
		return PrivateAddress{}, fmt.Errorf("private address %q has an invalid port", addr)
	}
	return PrivateAddress{HostPort: net.JoinHostPort(host, portStr)}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Zone() == "" && ip.IsLoopback()
}

// NewPrivateHTTPClient builds the base URL and bounded HTTP client for a
// validated local-only private endpoint. Proxy resolution and HTTP redirects
// are disabled so bootstrap credentials never traverse proxies or redirects.
func NewPrivateHTTPClient(addr string) (string, *http.Client, error) {
	parsed, err := parsePrivateAddress(addr, false)
	if err != nil {
		return "", nil, err
	}
	transport := &http.Transport{Proxy: nil, MaxConnsPerHost: 64}
	base := "http://" + parsed.HostPort
	if parsed.UnixPath != "" {
		sockPath := parsed.UnixPath
		base = "http://unix"
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", sockPath)
		}
	}
	return base, &http.Client{
		Timeout:       5 * time.Second,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errPrivateRedirect },
	}, nil
}

func unavailableObservation(err error) error {
	var networkErr *net.OpError
	reason := "transport interrupted"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = "deadline exceeded"
	case errors.Is(err, context.Canceled):
		reason = "request canceled"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &networkErr):
	default:
		return errors.New("private response is invalid")
	}
	return &privateObservationError{reason: reason}
}

// NewPrivateSource constructs the production loopback transport.
func NewPrivateSource(token string) PrivateSource {
	return NewPrivateSourceWithAddress("http://127.0.0.1:9252", token)
}

// NewPrivateSourceWithAddress constructs a bounded local-only private-source
// client over either loopback TCP ("host:port" / "http://host:port") or a Unix
// domain socket ("unix://<abs-path>"). Invalid or non-local addresses return a
// source that fails closed on every call.
func NewPrivateSourceWithAddress(address, token string) PrivateSource {
	base, client, err := NewPrivateHTTPClient(address)
	if err != nil {
		return &httpPrivateSource{
			token:   token,
			initErr: err,
		}
	}
	return &httpPrivateSource{
		address: base,
		token:   token,
		client:  client,
	}
}

func (s *httpPrivateSource) Call(ctx context.Context, path string, payload any) (PrivateReply, error) {
	var reply PrivateReply
	switch path {
	case "/v1/operations", "/v1/operations/status", "/v1/operations/ack", "/v1/sessions/close":
	default:
		return reply, errors.New("private path refused")
	}
	if s.initErr != nil {
		return reply, s.initErr
	}
	if s.address == "" || s.token == "" || s.client == nil {
		return reply, errors.New("private source is unavailable")
	}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > maxControlBody {
		return reply, errors.New("private request is invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.address+path, bytes.NewReader(raw))
	if err != nil {
		return reply, err
	}
	req.Header.Set("Authorization", s.token)
	req.Header.Set("Content-Type", "application/json")
	if path == "/v1/operations" || path == "/v1/operations/ack" {
		req.Header.Set("X-GPUStack-Wait-Ms", "50")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, errPrivateRedirect) {
			return reply, errPrivateRedirect
		}
		return reply, unavailableObservation(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		return reply, &privateObservationError{reason: "source HTTP failure"}
	}
	if resp.StatusCode != http.StatusOK {
		return reply, errors.New("private source refused the request")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxControlBody+1))
	if err != nil {
		return reply, unavailableObservation(err)
	}
	if len(data) > maxControlBody || rejectDuplicateJSONKeys(string(data)) != nil {
		return reply, errors.New("private response is invalid")
	}
	if err := decodeStrictDocument(data, &reply); err != nil {
		return reply, errors.New("private response is invalid")
	}
	return reply, nil
}
