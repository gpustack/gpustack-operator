package peer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	authentication "k8s.io/api/authentication/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
)

// AuthMode names how two plugins authenticate each other.
type AuthMode string

const (
	// AuthNone serves and pulls without authenticating, for a baseline measurement only.
	AuthNone AuthMode = "none"
	// AuthToken carries the pulling plugin's projected ServiceAccount token, which the serving
	// plugin checks with a TokenReview bound to the feature audience.
	AuthToken AuthMode = "token"
	// AuthMTLS has the operator's CA vouch for every plugin's client certificate, checked
	// during the TLS handshake.
	AuthMTLS AuthMode = "mtls"
)

const (
	// tokenFile is where the Pod's projected ServiceAccount token for this feature is mounted,
	// with the PeerAudience audience and a short lifetime; the chart declares the volume.
	tokenFile = "/var/run/secrets/gpustack-model-peer/token"
	// PeerAudience is the audience every peer token is minted for and every review checks: one
	// shared audience, since a server cannot review against an audience list it does not know.
	PeerAudience = "gpustack-model-peer"
)

// tokenExpiry reads the expiry a bearer token carries in its own claims, without verifying it:
// verification is the API server's job in the TokenReview; the expiry only bounds how long this
// token's review may be cached. A token without a readable expiry has none.
func tokenExpiry(token string) (time.Time, bool) {
	payload, rest, ok := strings.Cut(token, ".")
	if !ok || rest == "" {
		return time.Time{}, false
	}
	payload, _, _ = strings.Cut(payload, ".")
	claims, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return time.Time{}, false
	}
	var parsed struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(claims, &parsed) != nil || parsed.Exp <= 0 {
		return time.Time{}, false
	}

	return time.Unix(parsed.Exp, 0), true
}

// AuthOptions is what both sides of a peer connection build their authentication from.
type AuthOptions struct {
	// Mode is how peers authenticate each other.
	Mode AuthMode
	// Audience is the audience peer tokens are minted for and reviews check. Token mode needs
	// it: a username check alone would admit any token of the account minted for the API
	// server.
	Audience string
	// Namespace is the namespace the plugins run in.
	Namespace string
	// ServiceAccount is the plugins' ServiceAccount name, without its namespace. Token mode
	// admits only the bearer tokens of this account.
	ServiceAccount string
	// ClientCAPEM is the certificate of the CA client certificates carry, in PEM. Empty
	// outside mtls mode.
	ClientCAPEM []byte
	// ClientCertPEM and ClientKeyPEM are the pulling side's own certificate and key, in PEM.
	ClientCertPEM, ClientKeyPEM []byte
}

// Validate reports whether the mode is one this package serves. Which side needs which material
// is checked where the material is used.
func (a AuthOptions) Validate() error {
	switch a.Mode {
	case AuthNone, AuthToken, AuthMTLS:
		return nil
	default:
		return fmt.Errorf("peer auth %q is none of %q, %q or %q", a.Mode, AuthNone, AuthToken, AuthMTLS)
	}
}

// RoundTripper wraps transport so every request carries the pulling side's credential. The
// transport is returned as it is outside token mode, whose credential goes in a header rather
// than the TLS configuration.
func (a AuthOptions) RoundTripper(transport http.RoundTripper) http.RoundTripper {
	if a.Mode != AuthToken {
		return transport
	}

	return &tokenTransport{base: transport}
}

// tokenTransport adds the Pod's projected ServiceAccount token to every request. The file's
// current content is read per request, so a rotation reaches the next one.
type tokenTransport struct {
	base http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b, err := readFileLimited(tokenFile)
	if err == nil && len(b) > 0 {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	}

	return t.base.RoundTrip(req)
}

// reviewToken asks the API server who the bearer token belongs to. A positive answer is cached
// until the earlier of the cache's own lifetime and the token's expiry; a refusal is never
// cached, so a freshly rolled Pod is not locked out by one bad token it presented before its
// projected volume rotated.
type reviewToken func(ctx context.Context, token string) (string, error)

func (a AuthOptions) reviewer(cli tokenClient) reviewToken {
	const cacheTTL = 2 * time.Minute

	var (
		mu     sync.Mutex
		cached = map[string]reviewed{}
	)
	return func(ctx context.Context, token string) (string, error) {
		mu.Lock()
		if r, ok := cached[token]; ok && r.validUntil.After(time.Now()) {
			mu.Unlock()

			return r.username, nil
		}
		mu.Unlock()

		username, err := a.review(ctx, cli, token)
		if err != nil {
			return "", err
		}
		validUntil := time.Now().Add(cacheTTL)
		if exp, ok := tokenExpiry(token); ok && exp.Before(validUntil) {
			validUntil = exp
		}
		mu.Lock()
		cached[token] = reviewed{username: username, validUntil: validUntil}
		mu.Unlock()

		return username, nil
	}
}

type reviewed struct {
	username   string
	validUntil time.Time
}

// tokenClient is the API server surface a TokenReview needs.
type tokenClient interface {
	Create(ctx context.Context, review *authentication.TokenReview, opts meta.CreateOptions) (*authentication.TokenReview, error)
}

// review sends the token to the API server, bound to the feature audience, and says whether it
// is one of this chart's plugins.
func (a AuthOptions) review(ctx context.Context, cli tokenClient, token string) (string, error) {
	tr, err := cli.Create(ctx, &authentication.TokenReview{
		Spec: authentication.TokenReviewSpec{Token: token, Audiences: []string{a.Audience}},
	}, meta.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("token review: %w", err)
	}
	if !tr.Status.Authenticated {
		return "", fmt.Errorf("the peer request's token did not authenticate: %s", tr.Status.Error)
	}
	if !slicesContain(tr.Status.Audiences, a.Audience) {
		return "", fmt.Errorf("the peer request's token carries audiences %q, not %q",
			strings.Join(tr.Status.Audiences, ","), a.Audience)
	}

	want := "system:serviceaccount:" + a.Namespace + ":" + a.ServiceAccount
	if tr.Status.User.Username != want {
		return "", fmt.Errorf("the peer request's token belongs to %q, not a plugin", tr.Status.User.Username)
	}

	return tr.Status.User.Username, nil
}

func slicesContain(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}

	return false
}

// AuthError is a refused credential.
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// Admit builds the serving side's check of one request, or nil in none mode, where everything
// is admitted. mtls mode checks nothing here: its handshake has already verified the client
// certificate against the CA.
func (a AuthOptions) Admit(cli tokenClient) (func(*http.Request) error, error) {
	switch a.Mode {
	case AuthNone:
		return nil, nil
	case AuthMTLS:
		return func(r *http.Request) error {
			if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
				return &AuthError{Msg: "no verified client certificate"}
			}

			return nil
		}, nil
	case AuthToken:
		if a.Audience == "" {
			return nil, fmt.Errorf("peer auth %q needs the token audience", a.Mode)
		}
		if a.Namespace == "" || a.ServiceAccount == "" {
			return nil, fmt.Errorf("peer auth %q needs the plugins' namespace and ServiceAccount name", a.Mode)
		}
		if cli == nil {
			return nil, fmt.Errorf("peer auth %q needs the API server to review tokens with", a.Mode)
		}
		review := a.reviewer(cli)

		return func(r *http.Request) error {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || strings.TrimSpace(token) == "" {
				return &AuthError{Msg: "no bearer token"}
			}
			if _, err := review(r.Context(), strings.TrimSpace(token)); err != nil {
				klog.V(1).InfoS("a peer request's token was refused", "error", err.Error())

				return &AuthError{Msg: "the bearer token is not a plugin's"}
			}

			return nil
		}, nil
	default:
		return nil, a.Validate()
	}
}

// ClientTLSConfig is the TLS configuration of the pulling side: the server's certificate is
// self-signed, as the plugin's secure port serves one, and in mtls mode the request carries the
// plugin's client certificate.
func (a AuthOptions) ClientTLSConfig() (*tls.Config, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		// The peers' server certificates are self-signed, as the plugin's secure port's are,
		// inside the same trust domain.
		InsecureSkipVerify: true, // nolint: gosec
		MinVersion:         tls.VersionTLS12,
	}
	if a.Mode != AuthMTLS {
		return cfg, nil
	}
	if len(a.ClientCertPEM) == 0 || len(a.ClientKeyPEM) == 0 {
		return nil, fmt.Errorf("peer auth %q needs a client certificate and key", a.Mode)
	}
	cert, err := tls.X509KeyPair(a.ClientCertPEM, a.ClientKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("read the peer client certificate: %w", err)
	}
	cfg.Certificates = []tls.Certificate{cert}

	return cfg, nil
}

// ServerTLSConfig is the TLS configuration of the serving side: getCertificate serves the
// listener's certificate, self-signed unless told otherwise, and in mtls mode a client
// certificate the CA vouches for is required.
func (a AuthOptions) ServerTLSConfig(getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)) (*tls.Config, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: getCertificate,
	}
	if a.Mode != AuthMTLS {
		return cfg, nil
	}
	if len(a.ClientCAPEM) == 0 {
		return nil, fmt.Errorf("peer auth %q needs the client certificate authority", a.Mode)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(a.ClientCAPEM) {
		return nil, fmt.Errorf("the peer client CA carries no certificate")
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.RequireAndVerifyClientCert

	return cfg, nil
}
