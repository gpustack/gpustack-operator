package gox

import "github.com/prometheus/client_golang/prometheus"

// NewStatsCollector returns a new prometheus.Collector for global gorountine pool.
func NewStatsCollector() prometheus.Collector {
	fqName := func(name string) string {
		return "gopool_" + name
	}

	return &statsCollector{
		maxWorkers: prometheus.NewDesc(
			fqName("max_workers"),
			"The maximum number of pooling goroutines.",
			nil, nil,
		),
		runningWorkers: prometheus.NewDesc(
			fqName("running_workers"),
			"The number of running pooling goroutines.",
			nil, nil,
		),
		maxTasks: prometheus.NewDesc(
			fqName("max_tasks"),
			"The maximum number of queuing tasks.",
			nil, nil,
		),
		submittedTasksTotal: prometheus.NewDesc(
			fqName("submitted_tasks_total"),
			"The total number of tasks submitted.",
			nil, nil,
		),
		succeededTasksTotal: prometheus.NewDesc(
			fqName("succeeded_tasks_total"),
			"The total number of tasks successful completed.",
			nil, nil,
		),
		failedTasksTotal: prometheus.NewDesc(
			fqName("failed_tasks_total"),
			"The total number of tasks unsuccessful completed.",
			nil, nil,
		),
		waitingTasks: prometheus.NewDesc(
			fqName("waiting_tasks"),
			"The number of tasks waiting for, which has not submitted yet.",
			nil, nil,
		),
		droppedTasks: prometheus.NewDesc(
			fqName("running_tasks"),
			"The number of tasks running, which has submitted but not completed yet.",
			nil, nil,
		),
	}
}

type statsCollector struct {
	maxWorkers     *prometheus.Desc
	runningWorkers *prometheus.Desc

	maxTasks            *prometheus.Desc
	waitingTasks        *prometheus.Desc
	submittedTasksTotal *prometheus.Desc
	succeededTasksTotal *prometheus.Desc
	failedTasksTotal    *prometheus.Desc
	droppedTasks        *prometheus.Desc
}

func (c *statsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.maxWorkers
	ch <- c.runningWorkers
	ch <- c.maxTasks
	ch <- c.waitingTasks
	ch <- c.submittedTasksTotal
	ch <- c.succeededTasksTotal
	ch <- c.failedTasksTotal
	ch <- c.droppedTasks
}

func (c *statsCollector) Collect(ch chan<- prometheus.Metric) {
	var (
		maxWorkers     = gp.MaxConcurrency()
		runningWorkers = gp.RunningWorkers()
		maxTasks       = gp.QueueSize()
		waitingTasks   = gp.WaitingTasks()
		submittedTasks = gp.SubmittedTasks()
		succeededTasks = gp.SuccessfulTasks()
		failedTasks    = gp.FailedTasks()
		droppedTasks   = gp.DroppedTasks()
	)

	ch <- prometheus.MustNewConstMetric(c.maxWorkers, prometheus.GaugeValue, float64(maxWorkers))
	ch <- prometheus.MustNewConstMetric(c.runningWorkers, prometheus.GaugeValue, float64(runningWorkers))
	ch <- prometheus.MustNewConstMetric(c.maxTasks, prometheus.GaugeValue, float64(maxTasks))
	ch <- prometheus.MustNewConstMetric(c.waitingTasks, prometheus.GaugeValue, float64(waitingTasks))
	ch <- prometheus.MustNewConstMetric(c.submittedTasksTotal, prometheus.CounterValue, float64(submittedTasks))
	ch <- prometheus.MustNewConstMetric(c.succeededTasksTotal, prometheus.CounterValue, float64(succeededTasks))
	ch <- prometheus.MustNewConstMetric(c.failedTasksTotal, prometheus.CounterValue, float64(failedTasks))
	ch <- prometheus.MustNewConstMetric(c.droppedTasks, prometheus.CounterValue, float64(droppedTasks))
}
