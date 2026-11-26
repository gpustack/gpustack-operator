package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	core "k8s.io/api/core/v1"

	"gpustack.ai/gpustack/pkg/utils/httpx"
)

// The Mooncake store families vLLM's store connector exposes, read from upstream vLLM at v0.25.1 and
// v0.30.0 (`vllm/distributed/kv_transfer/kv_connector/v1/mooncake/store/metrics.py`; main moved the
// directory to `mooncake_store/` and kept the families). Every series carries `operation` in
// {save_exists, save_put, load_get, lookup_exists, lookup_lease} and `status` in {ok, error,
// partial_failure}.
//
// THESE ARE vLLM'S FAMILIES. SGLang and vLLM-Ascend store metrics are not read and read Unknown
// until their exposition is measured.
//
// THREE COUNTERS DECIDE THE VERDICT, and the status label alone cannot: for the reason, read
// modelDeploymentCacheAccount before changing what counts as success.
const (
	modelDeploymentCacheMetricOperations = "vllm:mooncake_store_operation_total"
	modelDeploymentCacheMetricKeys       = "vllm:mooncake_store_operation_keys_total"
	modelDeploymentCacheMetricFailedKeys = "vllm:mooncake_store_operation_failed_keys_total"
	modelDeploymentCacheLabelOperation   = "operation"
	modelDeploymentCacheLabelStatus      = "status"

	modelDeploymentCacheStatusOK      = "ok"
	modelDeploymentCacheStatusError   = "error"
	modelDeploymentCacheStatusPartial = "partial_failure"

	modelDeploymentCacheOpSavePut = "save_put"
	modelDeploymentCacheOpLoadGet = "load_get"
)

// defaultModelDeploymentCacheHTTPClient asks an engine's own metrics endpoint. It follows no
// redirect, because an engine that sends the request elsewhere is answering a question nobody
// asked, and it does not verify the certificate: the certificate is whatever the role passed to
// `--ssl-certfile`, which no cluster authority signed. That is the trust the runtime-control
// dial already extends to the same `Pod`s.
var defaultModelDeploymentCacheHTTPClient = &http.Client{
	Transport: httpx.Transport(httpx.TransportOptions().WithoutProxy().WithoutInsecureVerify()),
	Timeout:   samplerPodTimeout,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// defaultModelDeploymentCacheScraper is the scraper a reconciler built without one reads through,
// the way a nil groupForwardFetch is the production transport.
var defaultModelDeploymentCacheScraper ModelDeploymentCacheScraper = newModelDeploymentMetricsCacheScraper()

// modelDeploymentMetricsCacheScraper reads one replica's account off its engine's Prometheus
// exposition.
//
// IT READS vLLM'S FAMILIES ALONE. SGLang and vLLM-Ascend store metrics are not read and read
// Unknown until their exposition is measured.
type modelDeploymentMetricsCacheScraper struct{}

func newModelDeploymentMetricsCacheScraper() *modelDeploymentMetricsCacheScraper {
	return &modelDeploymentMetricsCacheScraper{}
}

// ScrapeCache reads the replica's /metrics at the address its render stamped on it, and returns the
// account the store families in it support.
//
// The URL is built the way the group forward probe builds the address it asks the same engine: the
// scheme and port are the render's own annotations. A replica carrying neither, or no address yet,
// is an error rather than a guess: inventing a port would read whatever listens there.
func (s *modelDeploymentMetricsCacheScraper) ScrapeCache(
	ctx context.Context, pod *core.Pod,
) (ModelDeploymentCacheAccount, error) {
	url, err := modelDeploymentGroupForwardURL(pod, "/metrics")
	if err != nil {
		return ModelDeploymentCacheAccount{}, fmt.Errorf("cache scrape address: %w", err)
	}

	body, err := s.fetch(ctx, url)
	if err != nil {
		return ModelDeploymentCacheAccount{}, err
	}

	return modelDeploymentCacheAccount(body)
}

// fetch performs one GET and bounds the answer. The bound and the client are the scaler's, because
// both read the same exposition from the same engines.
func (s *modelDeploymentMetricsCacheScraper) fetch(ctx context.Context, url string) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, samplerPodTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build the cache scrape request: %w", err)
	}
	response, err := defaultModelDeploymentCacheHTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read the cache scrape endpoint: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the cache scrape endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, samplerMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the cache scrape body: %w", err)
	}
	if len(body) > samplerMaxBodyBytes {
		return nil, fmt.Errorf("the cache scrape body exceeds %d bytes", samplerMaxBodyBytes)
	}

	return body, nil
}

// modelDeploymentCacheAccount reads one replica's account out of a Prometheus text exposition.
//
// TRANSFER OPERATIONS ALONE ARE COUNTED, and they are save_put and load_get. save_exists and the two
// lookups travel the master's TCP RPC, so on the failure this account exists to report they answer
// ok while every transfer is refused; counting them would read a broken replica as healthy.
//
// THE VERDICT IS KEY-LEVEL BECAUSE THE STATUS LABEL DOES NOT MEAN WHAT IT LOOKS LIKE. On the images
// this operator pins, the NORMAL return of save_put and load_get is recorded as
// `status="partial_failure" if failed else "ok"` with `num_failed_keys=len(failed)` (vLLM v0.25.1 and
// v0.30.0, `vllm/distributed/kv_transfer/kv_connector/v1/mooncake/store/worker.py`), so a transfer
// whose every key was refused is partial_failure; the issue's own `batch_put failed: 2/2 keys failed`
// line is logged beside that record. `error` is the exception path alone, with
// `num_failed_keys=len(keys)`. main (`vllm/distributed/kv_transfer/kv_connector/v1/mooncake_store/
// worker.py`) keeps both transfer operations on that same shape; what it changed is save_exists and
// lookup_exists, which now record error when every key fails, and which this reader does not count.
// A reader that counts partial_failure as success therefore reports the pinned image's total failure
// as active. The key counters hold under every shape: a replica is active when at least one key
// succeeded or an operation completed with no failed key, and failing when no key ever did.
//
// Counters are cumulative since the process started, which is what keeps the verdict level-based:
// a replica that failed once and saved since reads active, and one that has only ever failed reads
// failing. A scrape that carries no transfer series is Unreadable and NOT active, because an
// attached idle engine and an engine whose client was never invoked look the same here.
func modelDeploymentCacheAccount(body []byte) (ModelDeploymentCacheAccount, error) {
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return ModelDeploymentCacheAccount{}, fmt.Errorf("parse the cache scrape exposition: %w", err)
	}

	account := ModelDeploymentCacheAccount{Reading: ModelDeploymentCacheUnreadable}
	operations, ok := modelDeploymentCacheFamily(families, modelDeploymentCacheMetricOperations)
	if !ok {
		return account, nil
	}
	keys := modelDeploymentCacheSeriesSum(families, modelDeploymentCacheMetricKeys)
	failedKeys := modelDeploymentCacheSeriesSum(families, modelDeploymentCacheMetricFailedKeys)

	var clean, unmeasured bool
	for _, metric := range operations.GetMetric() {
		series := modelDeploymentCacheSeries{
			operation: modelDeploymentCacheLabel(metric, modelDeploymentCacheLabelOperation),
			status:    modelDeploymentCacheLabel(metric, modelDeploymentCacheLabelStatus),
		}
		if !modelDeploymentCacheTransferOperation(series.operation) {
			continue
		}
		calls := metric.GetCounter().GetValue()
		if math.IsNaN(calls) || math.IsInf(calls, 0) {
			continue
		}
		account.Attempted += calls
		switch series.status {
		case modelDeploymentCacheStatusError, modelDeploymentCacheStatusPartial:
		case modelDeploymentCacheStatusOK:
			clean = true
		default:
			// A status outside the measured vocabulary counts as attempted and decides nothing: the
			// replica then gave an account this reader cannot classify, which is Unreadable rather
			// than a verdict in either direction.
			unmeasured = true

			continue
		}
		account.Keys += keys[series]
		account.Failed += failedKeys[series]
	}

	switch {
	case account.Attempted == 0 || unmeasured:
		// Unreadable: nothing was transferred, or the account cannot be classified.
	case failedKeys == nil:
		// The verdict rests on three counters. Transfer operations beside no failed-key counter are
		// an account this reader cannot classify, and reading it as active is the one wrong answer
		// that would hide the failure this condition exists to report.
	case account.Failed < account.Keys || clean:
		account.Reading = ModelDeploymentCacheActive
	default:
		account.Reading = ModelDeploymentCacheFailing
	}

	return account, nil
}

// modelDeploymentCacheSeries names one labeled child of the store families.
type modelDeploymentCacheSeries struct {
	operation string
	status    string
}

// modelDeploymentCacheTransferOperation reports whether an operation moves bytes between the engine
// and the store.
func modelDeploymentCacheTransferOperation(operation string) bool {
	return operation == modelDeploymentCacheOpSavePut || operation == modelDeploymentCacheOpLoadGet
}

// modelDeploymentCacheFamily returns one counter family, and false when the exposition does not
// carry it as a counter.
func modelDeploymentCacheFamily(
	families map[string]*dto.MetricFamily, name string,
) (*dto.MetricFamily, bool) {
	family, ok := families[name]
	if !ok || family.GetType() != dto.MetricType_COUNTER {
		return nil, false
	}

	return family, true
}

// modelDeploymentCacheSeriesSum sums each labeled child of one counter family by series. nil when
// the exposition does not carry the family as a counter.
func modelDeploymentCacheSeriesSum(
	families map[string]*dto.MetricFamily, name string,
) map[modelDeploymentCacheSeries]float64 {
	family, ok := modelDeploymentCacheFamily(families, name)
	if !ok {
		return nil
	}
	sums := make(map[modelDeploymentCacheSeries]float64, len(family.GetMetric()))
	for _, metric := range family.GetMetric() {
		value := metric.GetCounter().GetValue()
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		sums[modelDeploymentCacheSeries{
			operation: modelDeploymentCacheLabel(metric, modelDeploymentCacheLabelOperation),
			status:    modelDeploymentCacheLabel(metric, modelDeploymentCacheLabelStatus),
		}] += value
	}

	return sums
}

func modelDeploymentCacheLabel(metric *dto.Metric, name string) string {
	for _, pair := range metric.GetLabel() {
		if pair.GetName() == name {
			return pair.GetValue()
		}
	}

	return ""
}
