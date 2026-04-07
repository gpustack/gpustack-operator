package loopback

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/mux"
	"k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/client-go/transport"

	"gpustack.ai/gpustack/pkg/extensionroute/openapi"
	"gpustack.ai/gpustack/pkg/server/extensionroutes/identify"
	"gpustack.ai/gpustack/pkg/server/extensionroutes/ui"
	"gpustack.ai/gpustack/pkg/utils/httpx"
)

func Route(r *mux.Route) openapi.Extender {
	p, _ := r.GetPathTemplate()
	r.Handler(http.StripPrefix(p, index()))
	return getOpenapiDecorate(p)
}

func index() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get kube config.
		_, _, cliCfg := identify.GetSubjectKubeConfig(r)
		if cliCfg == nil {
			ui.ResponseErrorWithCode(w, http.StatusUnauthorized, fmt.Errorf("unauthorized"))
			return
		}

		// Get kube target.
		var target *url.URL
		{
			host := cliCfg.Host
			if !strings.HasSuffix(host, "/") {
				host += "/"
			}
			var err error
			target, err = url.Parse(host)
			if err != nil {
				ui.ResponseErrorWithCode(w, http.StatusInternalServerError, fmt.Errorf("parse loopback kubernetes host: %w", err))
				return
			}
		}

		// Get kube transport config.
		tCfg, err := cliCfg.TransportConfig()
		if err != nil {
			ui.ResponseErrorWithCode(w, http.StatusInternalServerError, fmt.Errorf("get loopback kubernetes transport config: %w", err))
			return
		}

		// Get downstream kube transport.
		var dt proxy.UpgradeRequestRoundTripper
		{
			tlsCfg, err := transport.TLSConfigFor(tCfg)
			if err != nil {
				ui.ResponseErrorWithCode(w, http.StatusInternalServerError, fmt.Errorf("get loopback kubernetes tls config: %w", err))
				return
			}
			wrapper, err := transport.HTTPWrappersForConfig(tCfg, proxy.MirrorRequest)
			if err != nil {
				ui.ResponseErrorWithCode(w, http.StatusInternalServerError, fmt.Errorf("get loopback kubernetes http wrappers: %w", err))
				return
			}
			dt = proxy.NewUpgradeRequestRoundTripper(
				httpx.Transport(httpx.TransportOptions().WithTLSClientConfig(tlsCfg)), wrapper)
		}

		// Get upstream kube transport.
		ut, err := transport.New(tCfg)
		if err != nil {
			ui.ResponseErrorWithCode(w, http.StatusInternalServerError, fmt.Errorf("get loopback kubernetes transport: %w", err))
			return
		}

		// Serve proxy.
		p := &proxy.UpgradeAwareHandler{
			Location:           target,
			UpgradeTransport:   dt,
			Transport:          ut,
			Responder:          errResponder{},
			WrapTransport:      true,
			UseRequestLocation: true,
			UseLocationHost:    true,
		}
		p.ServeHTTP(w, r)
	})
}

type errResponder struct{}

func (r errResponder) Error(w http.ResponseWriter, _ *http.Request, err error) {
	ui.ResponseError(w, fmt.Errorf("proxy loopback kubernetes: %w", err))
}
