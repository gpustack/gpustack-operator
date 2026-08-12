package manager

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	klog "k8s.io/klog/v2"
)

// newMetricsHandler serves the given gatherer in the Prometheus text exposition format.
//
// A collector whose source failed degrades the response to the metrics that were gathered,
// rather than discarding all of them: the endpoint carries several independent sources, and
// losing a whole node's metrics because one of them is unavailable is worse than reporting the
// rest and letting the failure show up as its own gauge. Only a gather that produced nothing
// at all still fails the request, since then there is nothing to serve.
//
// Only the device manager reaches this registration: the worker hands the shared manager a
// webserver.Null() (see option.go) and registers its own handler on its aggregated API server,
// and worker-gateway owns another. The blast radius of this handler is one binary.
func newMetricsHandler(gatherer prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		ErrorLog:      klog.NewStandardLogger("WARNING"),
		ErrorHandling: promhttp.ContinueOnError,
	})
}
