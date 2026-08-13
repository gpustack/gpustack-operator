package manager

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// _BrokenCollector stands in for a collector whose source failed at scrape time: it describes a
// metric and then reports that it cannot produce it, which is what makes prometheus.Gather
// return an error while still returning every other collector's families.
type _BrokenCollector struct {
	desc *prometheus.Desc
}

func (c _BrokenCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c _BrokenCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(c.desc, errors.New("source unavailable"))
}

func newBrokenCollector() prometheus.Collector {
	return _BrokenCollector{desc: prometheus.NewDesc("broken_metric", "Never produced.", nil, nil)}
}

func newHealthyCollector() prometheus.Collector {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "healthy_metric", Help: "Always produced."})
	g.Set(1)
	return g
}

func TestNewMetricsHandler(t *testing.T) {
	// The endpoint carries several independent sources, and a scrape that drops a whole node's
	// metrics because one of them failed is worse than a scrape that reports the rest.
	testCases := []struct {
		name string

		collectors []prometheus.Collector

		wantStatus int
		wantBody   string
	}{
		{
			name:       "serves every metric when nothing failed",
			collectors: []prometheus.Collector{newHealthyCollector()},
			wantStatus: http.StatusOK,
			wantBody:   "healthy_metric 1",
		},
		{
			name:       "serves the surviving metrics when a collector failed",
			collectors: []prometheus.Collector{newHealthyCollector(), newBrokenCollector()},
			wantStatus: http.StatusOK,
			wantBody:   "healthy_metric 1",
		},
		{
			name:       "fails only when the gather produced nothing at all",
			collectors: []prometheus.Collector{newBrokenCollector()},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			for _, c := range tc.collectors {
				require.NoError(t, reg.Register(c))
			}

			rec := httptest.NewRecorder()
			newMetricsHandler(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

			assert.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantBody != "" {
				assert.Contains(t, rec.Body.String(), tc.wantBody)
			}
		})
	}
}
