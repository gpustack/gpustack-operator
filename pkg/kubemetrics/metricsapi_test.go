package kubemetrics

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// podMetricsPayload renders a metrics.k8s.io PodMetrics answer for the test pod, measured at
// the given time.
func podMetricsPayload(measuredAt time.Time, cpu, memory string) string {
	return `{"kind":"PodMetrics","timestamp":"` + measuredAt.UTC().Format(time.RFC3339) +
		`","containers":[{"name":"main","usage":{"cpu":"` + cpu + `","memory":"` + memory + `"}}]}`
}

// serveMetricsAPI answers the test pod's metrics.k8s.io route, and nothing else — so a case
// that reaches for the kubelet instead fails rather than silently passing.
func serveMetricsAPI(t *testing.T, body string, status int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/namespaces/default/pods/inst",
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		})
	serveAPI(t, mux)
}

func TestFetchPodUsageFromMetricsAPI(t *testing.T) {
	measuredAt := time.Now().Truncate(time.Second)

	t.Run("reads the pod's CPU and memory", func(t *testing.T) {
		serveMetricsAPI(t, podMetricsPayload(measuredAt, "250m", "512Mi"), http.StatusOK)

		cpu, memory, ts, err := fetchPodUsageFromMetricsAPI(context.Background(), testPod())
		require.NoError(t, err)
		assert.Equal(t, uint64(250_000_000), *cpu, "nanocores, as the kubelet reports them")
		assert.Equal(t, uint64(512<<20), *memory, "bytes, as the kubelet reports them")
		require.NotNil(t, ts)
		assert.Equal(t, measuredAt.UTC(), ts.UTC())
	})

	t.Run("reports an unserved API as absence rather than failure", func(t *testing.T) {
		// A cluster with no metrics adapter answers 404. That is not an error the caller
		// should report as a failure — it has to tell it apart from a broken adapter.
		serveMetricsAPI(t, "", http.StatusNotFound)

		cpu, memory, ts, err := fetchPodUsageFromMetricsAPI(context.Background(), testPod())
		require.NoError(t, err)
		assert.Nil(t, cpu)
		assert.Nil(t, memory)
		assert.Nil(t, ts)
	})

	t.Run("errors when the adapter fails", func(t *testing.T) {
		serveMetricsAPI(t, "", http.StatusInternalServerError)

		_, _, _, err := fetchPodUsageFromMetricsAPI(context.Background(), testPod())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "default/inst", "the error must name the pod it failed on")
	})

	t.Run("errors on an undecodable body", func(t *testing.T) {
		serveMetricsAPI(t, "{not-json", http.StatusOK)

		_, _, _, err := fetchPodUsageFromMetricsAPI(context.Background(), testPod())
		require.Error(t, err)
	})
}

func TestParsePodMetricsUsage(t *testing.T) {
	t.Run("sums the containers' usage", func(t *testing.T) {
		raw := []byte(`{"kind":"PodMetrics","timestamp":"2026-08-07T01:02:03Z","containers":[
			{"name":"main","usage":{"cpu":"250m","memory":"512Mi"}},
			{"name":"sshd","usage":{"cpu":"2500000n","memory":"65536Ki"}}]}`)

		cpu, memory, ts, err := parsePodMetricsUsage(raw)
		require.NoError(t, err)
		// 250m + 2500000n = 252500000 nanocores.
		assert.Equal(t, uint64(252_500_000), *cpu)
		// 512Mi + 64Mi, in bytes — the caller scales to MiB.
		assert.Equal(t, uint64(576<<20), *memory)
		assert.NotNil(t, ts)
	})

	t.Run("clamps a negative quantity instead of wrapping it", func(t *testing.T) {
		// Any adapter may serve metrics.k8s.io; an unchecked conversion would turn
		// -250m into 1.8e19 nanocores of "current usage".
		raw := []byte(`{"kind":"PodMetrics","timestamp":"2026-08-07T01:02:03Z","containers":[
			{"name":"main","usage":{"cpu":"-250m","memory":"-1Gi"}}]}`)

		cpu, memory, _, err := parsePodMetricsUsage(raw)
		require.NoError(t, err)
		assert.Equal(t, uint64(0), *cpu)
		assert.Equal(t, uint64(0), *memory)
	})

	t.Run("reports an empty container list as unserved", func(t *testing.T) {
		cpu, memory, ts, err := parsePodMetricsUsage(
			[]byte(`{"kind":"PodMetrics","timestamp":"2026-08-07T01:02:03Z","containers":[]}`))
		require.NoError(t, err)
		assert.Nil(t, cpu, "no containers means no measurement, not a genuine zero")
		assert.Nil(t, memory)
		assert.Nil(t, ts)
	})

	t.Run("reports a measurement without a timestamp as untimed", func(t *testing.T) {
		// The caller rejects an entry that predates the pod, so it has to tell an untimed
		// measurement apart from one it can date.
		_, _, ts, err := parsePodMetricsUsage(
			[]byte(`{"kind":"PodMetrics","containers":[{"name":"main","usage":{"cpu":"1"}}]}`))
		require.NoError(t, err)
		assert.Nil(t, ts)
	})
}
