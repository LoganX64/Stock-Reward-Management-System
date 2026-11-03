package metrics

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
)

// RegisterDBPool exposes database/sql pool statistics for one named pool.
// Registration is idempotent per pool name so tests and repeated wiring cannot
// panic the process.
func (m *Metrics) RegisterDBPool(name string, db *sql.DB) {
	if m == nil || db == nil {
		return
	}

	collector := newDBPoolCollector(name, db)
	if err := m.registry.Register(collector); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			panic(err)
		}
	}
}

type dbPoolCollector struct {
	name string
	db   *sql.DB

	maxOpen           *prometheus.Desc
	open              *prometheus.Desc
	inUse             *prometheus.Desc
	idle              *prometheus.Desc
	waitCount         *prometheus.Desc
	waitDuration      *prometheus.Desc
	maxIdleClosed     *prometheus.Desc
	maxIdleTimeClosed *prometheus.Desc
	maxLifetimeClosed *prometheus.Desc
}

func newDBPoolCollector(name string, db *sql.DB) *dbPoolCollector {
	labels := []string{"pool"}
	return &dbPoolCollector{
		name: name,
		db:   db,
		maxOpen: prometheus.NewDesc(
			"stocky_db_pool_max_open_connections",
			"Maximum number of open connections configured for the pool.",
			labels, nil,
		),
		open: prometheus.NewDesc(
			"stocky_db_pool_open_connections",
			"Number of open connections in the pool.",
			labels, nil,
		),
		inUse: prometheus.NewDesc(
			"stocky_db_pool_in_use_connections",
			"Number of connections currently in use.",
			labels, nil,
		),
		idle: prometheus.NewDesc(
			"stocky_db_pool_idle_connections",
			"Number of idle connections in the pool.",
			labels, nil,
		),
		waitCount: prometheus.NewDesc(
			"stocky_db_pool_wait_count_total",
			"Total number of waits for a database connection.",
			labels, nil,
		),
		waitDuration: prometheus.NewDesc(
			"stocky_db_pool_wait_duration_seconds_total",
			"Total time spent waiting for a database connection in seconds.",
			labels, nil,
		),
		maxIdleClosed: prometheus.NewDesc(
			"stocky_db_pool_max_idle_closed_total",
			"Total connections closed because the idle pool was full.",
			labels, nil,
		),
		maxIdleTimeClosed: prometheus.NewDesc(
			"stocky_db_pool_max_idle_time_closed_total",
			"Total connections closed because they exceeded the idle time.",
			labels, nil,
		),
		maxLifetimeClosed: prometheus.NewDesc(
			"stocky_db_pool_max_lifetime_closed_total",
			"Total connections closed because they exceeded their lifetime.",
			labels, nil,
		),
	}
}

func (c *dbPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.maxOpen
	ch <- c.open
	ch <- c.inUse
	ch <- c.idle
	ch <- c.waitCount
	ch <- c.waitDuration
	ch <- c.maxIdleClosed
	ch <- c.maxIdleTimeClosed
	ch <- c.maxLifetimeClosed
}

func (c *dbPoolCollector) Collect(ch chan<- prometheus.Metric) {
	stats := c.db.Stats()

	ch <- prometheus.MustNewConstMetric(c.maxOpen, prometheus.GaugeValue, float64(stats.MaxOpenConnections), c.name)
	ch <- prometheus.MustNewConstMetric(c.open, prometheus.GaugeValue, float64(stats.OpenConnections), c.name)
	ch <- prometheus.MustNewConstMetric(c.inUse, prometheus.GaugeValue, float64(stats.InUse), c.name)
	ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(stats.Idle), c.name)
	ch <- prometheus.MustNewConstMetric(c.waitCount, prometheus.CounterValue, float64(stats.WaitCount), c.name)
	ch <- prometheus.MustNewConstMetric(c.waitDuration, prometheus.CounterValue, stats.WaitDuration.Seconds(), c.name)
	ch <- prometheus.MustNewConstMetric(c.maxIdleClosed, prometheus.CounterValue, float64(stats.MaxIdleClosed), c.name)
	ch <- prometheus.MustNewConstMetric(c.maxIdleTimeClosed, prometheus.CounterValue, float64(stats.MaxIdleTimeClosed), c.name)
	ch <- prometheus.MustNewConstMetric(c.maxLifetimeClosed, prometheus.CounterValue, float64(stats.MaxLifetimeClosed), c.name)
}
