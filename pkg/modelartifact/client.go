package modelartifact

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http/httpproxy"

	"gpustack.ai/gpustack/pkg/utils/httpx"
)

// The reasons a source refuses or fails a resolution. They are the Resolved condition's reasons
// verbatim, so a caller maps nothing.
const (
	// ReasonRevisionNotFound is a branch, tag or commit the repository does not have.
	ReasonRevisionNotFound = "RevisionNotFound"
	// ReasonAccessDenied is a repository the credential cannot read, including one that does not
	// exist: the Hub answers the two alike to a caller without access.
	ReasonAccessDenied = "AccessDenied"
	// ReasonSourceUnavailable is a failure to get an answer at all, which says nothing about the
	// artifact and is retried.
	ReasonSourceUnavailable = "SourceUnavailable"
	// ReasonInvalidManifest is an answer that violates the manifest format.
	ReasonInvalidManifest = "InvalidManifest"
	// ReasonEmptyManifest is a commit with no files.
	ReasonEmptyManifest = "EmptyManifest"
	// ReasonManifestTooLarge is a tree above the client's entry bound.
	ReasonManifestTooLarge = "ManifestTooLarge"
)

// SourceError is a resolution failure with the reason it maps to.
//
// Its message is built from status codes and the source's error codes alone, never from a
// response body or a request header, so no credential can reach it.
type SourceError struct {
	Reason  string
	Message string
}

func (e *SourceError) Error() string {
	return e.Reason + ": " + e.Message
}

func sourceErrorf(reason, format string, args ...any) *SourceError {
	return &SourceError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// ReasonOf returns the reason of a SourceError anywhere in err's chain, and
// ReasonSourceUnavailable for any other error: an error that is not a verdict of the source is a
// failure to reach one.
func ReasonOf(err error) string {
	if se, ok := errors.AsType[*SourceError](err); ok {
		return se.Reason
	}

	return ReasonSourceUnavailable
}

// HTTPClientOptions is how the resolution client reaches a source. Every field is administrator
// configuration; nothing a tenant writes reaches it.
type HTTPClientOptions struct {
	// HTTPSProxy is the proxy for HTTPS requests. Empty keeps the worker's own environment.
	HTTPSProxy string
	// NoProxy is the comma-separated list of hosts that bypass HTTPSProxy.
	NoProxy string
	// CABundle is PEM certificates trusted in addition to the system pool. Empty trusts the system
	// pool alone.
	CABundle []byte
	// Timeout bounds one request. Zero uses DefaultRequestTimeout.
	Timeout time.Duration
}

// DefaultRequestTimeout bounds one request to a source when the options name none. A tree page of
// a thousand entries answers in well under a second; the bound is for a source that hangs.
const DefaultRequestTimeout = 30 * time.Second

// ValidateProxy checks a proxy URL before it is dialed or rendered: an http or https URL with a
// host and no credentials. The value is also rendered into tenant Pods as HTTPS_PROXY, where
// anyone who can read a Pod reads it.
//
// It is checked where the value is used, not only where it is written: a Setting's default comes
// from the worker's environment, and that path runs no admission.
func ValidateProxy(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid HTTPS proxy: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid HTTPS proxy: scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("invalid HTTPS proxy: no host")
	}
	if u.User != nil {
		return errors.New("invalid HTTPS proxy: it must not carry credentials")
	}

	return nil
}

// NewHTTPClient returns the client resolution requests are sent with.
func NewHTTPClient(opts HTTPClientOptions) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(opts.CABundle) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(opts.CABundle) {
			return nil, errors.New("the CA bundle holds no PEM certificate")
		}
		tlsConfig.RootCAs = pool
	}

	transport := httpx.Transport(httpx.TransportOptions().WithTLSClientConfig(tlsConfig))
	if opts.HTTPSProxy != "" {
		if err := ValidateProxy(opts.HTTPSProxy); err != nil {
			return nil, err
		}
		proxy := (&httpproxy.Config{HTTPSProxy: opts.HTTPSProxy, NoProxy: opts.NoProxy}).ProxyFunc()
		transport.Proxy = func(req *http.Request) (*url.URL, error) { return proxy(req.URL) }
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}

	return &http.Client{Transport: transport, Timeout: timeout}, nil
}
