package worker

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	forgerProbeResultSucceeded = "succeeded"
	forgerProbeResultCooldown  = "cooldown"
	forgerProbeResultError     = "error"

	forgerSandboxResultSucceeded          = "succeeded"
	forgerSandboxResultFailed             = "failed"
	forgerSandboxResultVerificationFailed = "verification_failed"
	forgerSandboxResultTimeout            = "timeout"

	forgerPromotionOutcomePromoted   = "promoted"
	forgerPromotionOutcomeRolledBack = "rolled_back"
	forgerPromotionOutcomeFailed     = "failed"
	forgerPromotionOutcomeSkipped    = "skipped"
)

var (
	forgerMetricsOnce sync.Once

	// ForgerProbesTotal counts short-window sentinel probes initiated by ModelDeploymentForger.
	ForgerProbesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gpustack_forger_probes_total",
		Help: "Total short-window sentinel probes executed by ModelDeploymentForger, by engine and result.",
	}, []string{"engine", "result"})

	// ForgerProbeDurationSeconds observes short-window sentinel probe wall-clock durations.
	ForgerProbeDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gpustack_forger_probe_duration_seconds",
		Help:    "Wall-clock duration in seconds of short-window sentinel probes, by engine.",
		Buckets: prometheus.DefBuckets,
	}, []string{"engine"})

	// ForgerSandboxRunsTotal counts ForgeSandbox executions by runner mode and terminal result.
	ForgerSandboxRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gpustack_forger_sandbox_runs_total",
		Help: "Total ForgeSandbox Pod runs completed by ModelDeploymentForger, by runner mode and result.",
	}, []string{"mode", "result"})

	// ForgerPromotionsTotal counts forge generation terminal outcomes.
	ForgerPromotionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gpustack_forger_promotions_total",
		Help: "Total ModelDeploymentForger generation outcomes (promoted, rolled_back, failed, skipped).",
	}, []string{"outcome"})

	// ForgerRollbacksTotal counts automatic canary rollbacks by machine-readable reason.
	ForgerRollbacksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gpustack_forger_rollbacks_total",
		Help: "Total automatic kernel artifact rollbacks triggered by ModelDeploymentForger, by reason.",
	}, []string{"reason"})
)

// ForgerMetricCollectors returns all Prometheus collectors owned by ModelDeploymentForgerReconciler.
func ForgerMetricCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		ForgerProbesTotal,
		ForgerProbeDurationSeconds,
		ForgerSandboxRunsTotal,
		ForgerPromotionsTotal,
		ForgerRollbacksTotal,
	}
}

func ensureForgerMetricsRegistered() {
	forgerMetricsOnce.Do(func() {
		if ctrlmetrics.Registry == nil {
			return
		}
		for _, c := range ForgerMetricCollectors() {
			if err := ctrlmetrics.Registry.Register(c); err != nil {
				var are prometheus.AlreadyRegisteredError
				if !errors.As(err, &are) {
					ctrl.Log.Error(err, "register forger metric collector")
				}
			}
		}
	})
}

func recordForgerProbeMetric(engine, result string, durationSeconds float64) {
	ensureForgerMetricsRegistered()
	if engine == "" {
		engine = "unknown"
	}
	ForgerProbesTotal.WithLabelValues(engine, result).Inc()
	if durationSeconds > 0 {
		ForgerProbeDurationSeconds.WithLabelValues(engine).Observe(durationSeconds)
	}
}

func recordForgerSandboxMetric(mode, result string) {
	ensureForgerMetricsRegistered()
	if mode == "" {
		mode = "External"
	}
	ForgerSandboxRunsTotal.WithLabelValues(mode, result).Inc()
}

func recordForgerPromotionMetric(outcome string) {
	ensureForgerMetricsRegistered()
	ForgerPromotionsTotal.WithLabelValues(outcome).Inc()
}

func recordForgerRollbackMetric(reason string) {
	ensureForgerMetricsRegistered()
	if reason == "" {
		reason = "Unknown"
	}
	ForgerRollbacksTotal.WithLabelValues(reason).Inc()
}
