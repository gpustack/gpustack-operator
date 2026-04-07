package httpx

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

type TransportOption struct {
	transport *http.Transport
}

func TransportOptions() *TransportOption {
	transport := &http.Transport{
		Proxy: ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &TransportOption{
		transport: transport,
	}
}

// WithoutProxy disables the proxy.
func (o *TransportOption) WithoutProxy() *TransportOption {
	if o == nil || o.transport == nil {
		return o
	}
	o.transport.Proxy = nil
	return o
}

// WithoutKeepalive disables the keepalive.
func (o *TransportOption) WithoutKeepalive() *TransportOption {
	if o == nil || o.transport == nil {
		return o
	}
	o.transport.DialContext = (&net.Dialer{KeepAlive: -1}).DialContext
	o.transport.MaxIdleConns = 0
	o.transport.IdleConnTimeout = 0
	return o
}

// WithoutInsecureVerify skips the insecure verify.
func (o *TransportOption) WithoutInsecureVerify() *TransportOption {
	if o == nil || o.transport == nil {
		return o
	}
	o.transport.TLSClientConfig.InsecureSkipVerify = true
	return o
}

// WithTLSClientConfig sets the tls.Config.
func (o *TransportOption) WithTLSClientConfig(config *tls.Config) *TransportOption {
	if o == nil || o.transport == nil {
		return o
	}
	o.transport.TLSClientConfig = config
	return o
}

// WithDial sets the dial function.
func (o *TransportOption) WithDial(dial func(context.Context, string, string) (net.Conn, error)) *TransportOption {
	if o == nil || o.transport == nil {
		return o
	}
	o.transport.DialContext = dial
	return o
}
