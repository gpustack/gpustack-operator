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

// The Mooncake store families vLLM's store connector exposes, read from upstream vLLM
// (`vllm/distributed/kv_transfer/kv_connector/v1/mooncake_store/metrics.py`). Every series carries
// `operation` in {save_exists, save_put, load_get, lookup_exists, lookup_lease} and `status` in
// {ok, error, partial_failure}.
//
// THE VERDICT READS THE OPERATION COUNTER ALONE. Keys, bytes and time describe transfers that
// happened; whether they happened is the counter's question, and a second family asked the same
// way would be a second opinion about one fact.
const (
	modelDeploymentCacheMetricOperation = "vllm:mooncake_store_operation_total"
	modelDeploymentCacheLabelOperation  = "operation"
	modelDeploymentCacheLabelStatus     = "status"

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
// TRANSFER OPERATIONS ALONE DECIDE, and they are save_put and load_get. save_exists and the two
// lookups travel the master's TCP RPC, so on the failure this account exists to report they answer
// ok while every transfer is refused; counting them would read a broken replica as healthy.
//
// Counters are cumulative since the process started, which is what keeps the verdict level-based:
// a replica that failed once and succeeded since reads active, and one that has only ever failed
// reads failing. A scrape that carries no transfer series is Unreadable and NOT active, because an
// attached idle engine and an engine whose client was never invoked look the same here.
func modelDeploymentCacheAccount(body []byte) (ModelDeploymentCacheAccount, error) {
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return ModelDeploymentCacheAccount{}, fmt.Errorf("parse the cache scrape exposition: %w", err)
	}

	account := ModelDeploymentCacheAccount{Reading: ModelDeploymentCacheUnreadable}
	family, ok := families[modelDeploymentCacheMetricOperation]
	if !ok || family.GetType() != dto.MetricType_COUNTER {
		return account, nil
	}
	var succeeded bool
	for _, metric := range family.GetMetric() {
		operation, status := modelDeploymentCacheLabel(metric, modelDeploymentCacheLabelOperation),
			modelDeploymentCacheLabel(metric, modelDeploymentCacheLabelStatus)
		if operation != modelDeploymentCacheOpSavePut && operation != modelDeploymentCacheOpLoadGet {
			continue
		}
		calls := metric.GetCounter().GetValue()
		if math.IsNaN(calls) || math.IsInf(calls, 0) {
			continue
		}
		account.Attempted += calls
		switch status {
		case modelDeploymentCacheStatusError:
			account.Failed += calls
		case modelDeploymentCacheStatusOK, modelDeploymentCacheStatusPartial:
			succeeded = true
		}
	}
	// A status outside the measured vocabulary counts as attempted and decides nothing: the
	// replica then gave an account this reader cannot classify, which is Unreadable rather than a
	// verdict in either direction.
	switch {
	case succeeded:
		account.Reading = ModelDeploymentCacheActive
	case account.Attempted > 0 && account.Failed == account.Attempted:
		account.Reading = ModelDeploymentCacheFailing
	}

	return account, nil
}

func modelDeploymentCacheLabel(metric *dto.Metric, name string) string {
	for _, pair := range metric.GetLabel() {
		if pair.GetName() == name {
			return pair.GetValue()
		}
	}

	return ""
}
