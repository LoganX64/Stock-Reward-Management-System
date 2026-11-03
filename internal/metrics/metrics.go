// Package metrics exposes the small Prometheus surface used by the API and the
// projection jobs. A custom registry keeps tests isolated and avoids relying on
// process-global registration.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns the registry and collectors for one process.
type Metrics struct {
	registry *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	httpInFlight prometheus.Gauge

	queryDuration *prometheus.HistogramVec
	queryErrors   *prometheus.CounterVec

	projectionQueuePending       prometheus.Gauge
	projectionQueueReady         prometheus.Gauge
	projectionQueueFailed        prometheus.Gauge
	projectionQueueProcessed     prometheus.Gauge
	projectionQueueOldestPending prometheus.Gauge
	projectionQueueMaxAttempts   prometheus.Gauge

	projectionFactorTotal   prometheus.Gauge
	projectionFactorStale   prometheus.Gauge
	projectionFactorInvalid prometheus.Gauge

	reconciliationUsersChecked prometheus.Gauge
	reconciliationMismatches   prometheus.Gauge
	reconciliationDuration     prometheus.Gauge

	jobLastSuccess *prometheus.GaugeVec
}

// ProjectionQueueStats is the projection queue health snapshot exposed as
// gauges. It mirrors projection_queue_stats() without importing the jobs
// package.
type ProjectionQueueStats struct {
	Pending              int64
	Ready                int64
	Failed               int64
	Processed            int64
	OldestPendingSeconds int64
	MaxAttempts          int64
}

// ProjectionFactorStats is the instrument-factor cache health snapshot.
type ProjectionFactorStats struct {
	Total   int64
	Stale   int64
	Invalid int64
}

// New builds an isolated metrics registry and its collectors.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
	}

	m.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stocky_http_requests_total",
		Help: "Total HTTP requests handled by the API.",
	}, []string{"method", "route", "status"})

	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "stocky_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})

	m.httpInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_http_in_flight_requests",
		Help: "HTTP requests currently being served.",
	})

	m.queryDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "stocky_db_query_duration_seconds",
		Help:    "Named database query duration in seconds.",
		Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"query"})

	m.queryErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stocky_db_query_errors_total",
		Help: "Total errors returned by named database queries.",
	}, []string{"query"})

	m.projectionQueuePending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_queue_pending",
		Help: "Projection refresh queue rows that are not processed or dead-lettered.",
	})
	m.projectionQueueReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_queue_ready",
		Help: "Projection refresh queue rows ready to be claimed.",
	})
	m.projectionQueueFailed = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_queue_failed",
		Help: "Projection refresh queue dead letters.",
	})
	m.projectionQueueProcessed = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_queue_processed",
		Help: "Projection refresh queue rows processed and retained.",
	})
	m.projectionQueueOldestPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_queue_oldest_pending_seconds",
		Help: "Age in seconds of the oldest pending projection refresh row.",
	})
	m.projectionQueueMaxAttempts = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_queue_max_attempts",
		Help: "Maximum attempts reached by a projection refresh row.",
	})

	m.projectionFactorTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_factor_total",
		Help: "Cached instrument factor rows.",
	})
	m.projectionFactorStale = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_factor_stale",
		Help: "Instrument factor rows not stamped for the current market date.",
	})
	m.projectionFactorInvalid = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_factor_invalid",
		Help: "Instrument factor rows with a non-positive or null factor.",
	})

	m.reconciliationUsersChecked = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_reconciliation_users_checked",
		Help: "Users checked in the last projection reconciliation run.",
	})
	m.reconciliationMismatches = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_reconciliation_mismatches",
		Help: "Mismatches found in the last projection reconciliation run.",
	})
	m.reconciliationDuration = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stocky_projection_reconciliation_duration_seconds",
		Help: "Duration of the last projection reconciliation run in seconds.",
	})

	m.jobLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stocky_projection_job_last_success_timestamp_seconds",
		Help: "Unix timestamp of the last successful projection job run.",
	}, []string{"job"})

	m.registry.MustRegister(
		m.httpRequests,
		m.httpDuration,
		m.httpInFlight,
		m.queryDuration,
		m.queryErrors,
		m.projectionQueuePending,
		m.projectionQueueReady,
		m.projectionQueueFailed,
		m.projectionQueueProcessed,
		m.projectionQueueOldestPending,
		m.projectionQueueMaxAttempts,
		m.projectionFactorTotal,
		m.projectionFactorStale,
		m.projectionFactorInvalid,
		m.reconciliationUsersChecked,
		m.reconciliationMismatches,
		m.reconciliationDuration,
		m.jobLastSuccess,
	)

	return m
}

// Handler returns the Prometheus scrape handler for this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// GinMiddleware records request counts, durations, and in-flight requests.
// Route labels use Gin's route pattern, never the raw URL, so user IDs do not
// create unbounded cardinality.
func (m *Metrics) GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		m.httpInFlight.Inc()
		defer m.httpInFlight.Dec()

		c.Next()

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		if route == "/metrics" {
			return
		}

		status := strconv.Itoa(c.Writer.Status())
		m.httpRequests.WithLabelValues(c.Request.Method, route, status).Inc()
		m.httpDuration.WithLabelValues(c.Request.Method, route).Observe(time.Since(start).Seconds())
	}
}

// ObserveQuery records a named query's duration and error outcome.
func (m *Metrics) ObserveQuery(name string, start time.Time, err error) {
	if m == nil {
		return
	}
	m.queryDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())
	if err != nil {
		m.queryErrors.WithLabelValues(name).Inc()
	}
}

// ObserveProjectionQueue publishes the latest queue snapshot.
func (m *Metrics) ObserveProjectionQueue(stats ProjectionQueueStats) {
	if m == nil {
		return
	}
	m.projectionQueuePending.Set(float64(stats.Pending))
	m.projectionQueueReady.Set(float64(stats.Ready))
	m.projectionQueueFailed.Set(float64(stats.Failed))
	m.projectionQueueProcessed.Set(float64(stats.Processed))
	m.projectionQueueOldestPending.Set(float64(stats.OldestPendingSeconds))
	m.projectionQueueMaxAttempts.Set(float64(stats.MaxAttempts))
}

// ObserveProjectionFactors publishes the latest instrument-factor snapshot.
func (m *Metrics) ObserveProjectionFactors(stats ProjectionFactorStats) {
	if m == nil {
		return
	}
	m.projectionFactorTotal.Set(float64(stats.Total))
	m.projectionFactorStale.Set(float64(stats.Stale))
	m.projectionFactorInvalid.Set(float64(stats.Invalid))
}

// ObserveReconciliation publishes the result of one reconciliation run.
func (m *Metrics) ObserveReconciliation(usersChecked, mismatches int, duration time.Duration) {
	if m == nil {
		return
	}
	m.reconciliationUsersChecked.Set(float64(usersChecked))
	m.reconciliationMismatches.Set(float64(mismatches))
	m.reconciliationDuration.Set(duration.Seconds())
}

// MarkJobSuccess records a successful projection job run. The timestamp lets
// operators alert on a job that has stopped running, not just one that failed.
func (m *Metrics) MarkJobSuccess(job string, at time.Time) {
	if m == nil {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	m.jobLastSuccess.WithLabelValues(job).Set(float64(at.Unix()))
}
