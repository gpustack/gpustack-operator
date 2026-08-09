package devicemanager

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/json"
)

func TestMonitorSnapshotHandler(t *testing.T) {
	sample := &MonitorSnapshot{
		Timestamp:     time.Date(2026, 8, 7, 1, 2, 3, 0, time.UTC),
		PeriodSeconds: 15,
		Groups: device.MetricsGroupList{
			{
				Manufacturer: "nvidia",
				Timestamp:    time.Date(2026, 8, 7, 1, 2, 2, 0, time.UTC),
				Accelerators: []device.AcceleratorMetrics{
					{ID: "gpu-1", MemoryUsage: 1024},
					{ID: "gpu-0", MemoryUsage: 2048},
				},
			},
			{
				Manufacturer: "amd",
				Timestamp:    time.Date(2026, 8, 7, 1, 2, 2, 0, time.UTC),
				Accelerators: []device.AcceleratorMetrics{
					{ID: "gpu-2", MemoryUsage: 512},
				},
			},
		},
	}

	cases := []struct {
		name    string
		method  string
		sample  *MonitorSnapshot
		want    int
		wantRaw string
	}{
		{
			name:   "non-GET method is rejected",
			method: http.MethodPost,
			want:   http.StatusMethodNotAllowed,
		},
		{
			name:   "empty before the first tick",
			method: http.MethodGet,
			want:   http.StatusOK,
			wantRaw: `{"timestamp":"0001-01-01T00:00:00Z","periodSeconds":0,"groups":[]}
`,
		},
		{
			name:   "populated snapshot keeps shape and order",
			method: http.MethodGet,
			sample: sample,
			want:   http.StatusOK,
			wantRaw: `{"timestamp":"2026-08-07T01:02:03Z","periodSeconds":15,"groups":[` +
				`{"manufacturer":"nvidia","timestamp":"2026-08-07T01:02:02Z","accelerators":[` +
				`{"id":"gpu-1","memory":0,"memoryUsage":1024,"memoryUtilization":0,"coresUtilization":0,"temperature":0,"powerUsage":0,"unhealthy":false},` +
				`{"id":"gpu-0","memory":0,"memoryUsage":2048,"memoryUtilization":0,"coresUtilization":0,"temperature":0,"powerUsage":0,"unhealthy":false}]},` +
				`{"manufacturer":"amd","timestamp":"2026-08-07T01:02:02Z","accelerators":[` +
				`{"id":"gpu-2","memory":0,"memoryUsage":512,"memoryUtilization":0,"coresUtilization":0,"temperature":0,"powerUsage":0,"unhealthy":false}]}]}
`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newMonitorSnapshotHandler(func() *MonitorSnapshot {
				return c.sample
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(c.method, MonitorSnapshotPath, nil)
			h.ServeHTTP(rec, req)

			assert.Equal(t, c.want, rec.Code)
			if c.want != http.StatusOK {
				return
			}
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.JSONEq(t, c.wantRaw, rec.Body.String())

			// Decode again to pin the envelope fields explicitly.
			var got MonitorSnapshot
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			if c.sample == nil {
				assert.True(t, got.Timestamp.IsZero())
				assert.Empty(t, got.Groups)
				assert.NotNil(t, got.Groups, "groups marshals as [], not null")
				return
			}
			assert.Equal(t, c.sample.Timestamp, got.Timestamp)
			assert.Equal(t, c.sample.Groups, got.Groups)
		})
	}
}
