package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/LoganX64/stocky-api/internal/metrics"
	"github.com/sirupsen/logrus"
)

// ProjectionRefreshService drains the Phase 2 projection_refresh_queue. Each
// call processes one user in one transaction, so the service never holds a
// large set of per-user advisory locks. Multiple API instances can run the
// service safely because the claim uses FOR UPDATE SKIP LOCKED.
type ProjectionRefreshService struct {
	db                 DB
	interval           time.Duration
	batchSize          int
	maxAttempts        int
	processedRetention time.Duration
	failedRetention    time.Duration
	alertDepth         int
	alertLag           time.Duration
	alertCooldown      time.Duration
	syncUserLimit      int
	timeout            time.Duration
	alerter            Alerter
	limiter            *alertLimiter
	metrics            *metrics.Metrics
	started            int32
	configured         int32
	lastPrune          time.Time
	lastStaleFactors   int64
}

// ProjectionRefreshServiceOptions controls the queue worker. Zero values use
// the defaults below.
type ProjectionRefreshServiceOptions struct {
	Interval           time.Duration
	BatchSize          int
	MaxAttempts        int
	ProcessedRetention time.Duration
	FailedRetention    time.Duration
	AlertDepth         int
	AlertLag           time.Duration
	AlertCooldown      time.Duration
	SyncUserLimit      int
	Timeout            time.Duration
	Alerter            Alerter
	Metrics            *metrics.Metrics
}

func NewProjectionRefreshService(db DB) *ProjectionRefreshService {
	return NewProjectionRefreshServiceWithOptions(db, nil)
}

func NewProjectionRefreshServiceWithOptions(db DB, opts *ProjectionRefreshServiceOptions) *ProjectionRefreshService {
	options := ProjectionRefreshServiceOptions{
		Interval:           30 * time.Second,
		BatchSize:          200,
		MaxAttempts:        5,
		ProcessedRetention: 7 * 24 * time.Hour,
		FailedRetention:    30 * 24 * time.Hour,
		AlertDepth:         1000,
		AlertLag:           15 * time.Minute,
		AlertCooldown:      15 * time.Minute,
		SyncUserLimit:      50,
		Timeout:            2 * time.Minute,
		Alerter:            LogAlerter{},
	}
	if opts != nil {
		if opts.Interval > 0 {
			options.Interval = opts.Interval
		}
		if opts.BatchSize > 0 {
			options.BatchSize = opts.BatchSize
		}
		if opts.MaxAttempts > 0 {
			options.MaxAttempts = opts.MaxAttempts
		}
		if opts.ProcessedRetention > 0 {
			options.ProcessedRetention = opts.ProcessedRetention
		}
		if opts.FailedRetention > 0 {
			options.FailedRetention = opts.FailedRetention
		}
		if opts.AlertDepth > 0 {
			options.AlertDepth = opts.AlertDepth
		}
		if opts.AlertLag > 0 {
			options.AlertLag = opts.AlertLag
		}
		if opts.AlertCooldown >= 0 {
			options.AlertCooldown = opts.AlertCooldown
		}
		if opts.SyncUserLimit >= 0 {
			options.SyncUserLimit = opts.SyncUserLimit
		}
		if opts.Timeout > 0 {
			options.Timeout = opts.Timeout
		}
		if opts.Alerter != nil {
			options.Alerter = opts.Alerter
		}
	}

	return &ProjectionRefreshService{
		db:                 db,
		interval:           options.Interval,
		batchSize:          options.BatchSize,
		maxAttempts:        options.MaxAttempts,
		processedRetention: options.ProcessedRetention,
		failedRetention:    options.FailedRetention,
		alertDepth:         options.AlertDepth,
		alertLag:           options.AlertLag,
		alertCooldown:      options.AlertCooldown,
		syncUserLimit:      options.SyncUserLimit,
		timeout:            options.Timeout,
		alerter:            options.Alerter,
		limiter:            newAlertLimiter(options.AlertCooldown),
		metrics:            options.Metrics,
	}
}

// Start runs one pass immediately, then once per interval until the context is
// cancelled.
func (s *ProjectionRefreshService) Start(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		logrus.Warn("ProjectionRefreshService.Start called more than once -- ignoring duplicate")
		return
	}

	logrus.WithFields(logrus.Fields{
		"interval":     s.interval,
		"batch":        s.batchSize,
		"max_attempts": s.maxAttempts,
	}).Info("Projection refresh worker started")

	s.process(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.process(ctx)
		case <-ctx.Done():
			logrus.Info("Projection refresh worker stopped")
			return
		}
	}
}

func (s *ProjectionRefreshService) process(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if atomic.CompareAndSwapInt32(&s.configured, 0, 1) {
		if err := s.configureSyncLimit(ctx); err != nil {
			logrus.WithError(err).Error("Failed to configure projection queue sync limit")
		}
	}

	processed := 0
	drainOK := true
drain:
	for processed < s.batchSize {
		outcome, userID, attempts, lastErr, err := s.processOne(ctx)
		if err != nil {
			logrus.WithError(err).Error("Failed to process projection refresh queue")
			drainOK = false
			break
		}

		switch outcome {
		case "idle":
			break drain
		case "processed":
			processed++
			logrus.WithFields(logrus.Fields{
				"user_id":  userID,
				"attempts": attempts,
			}).Debug("Projection refresh queue row processed")
		case "retry":
			processed++
			logrus.WithFields(logrus.Fields{
				"user_id":  userID,
				"attempts": attempts,
				"error":    lastErr,
			}).Warn("Projection refresh queue row scheduled for retry")
		case "failed":
			processed++
			s.limiter.send(ctx, s.alerter, fmt.Sprintf("queue-failed-%d", userID),
				"projection_queue_dead_letter", logrus.Fields{
					"user_id":  userID,
					"attempts": attempts,
					"error":    lastErr,
				})
		default:
			logrus.WithField("outcome", outcome).Warn("Unknown projection queue outcome")
			break drain
		}
	}

	if processed >= s.batchSize {
		logrus.WithField("batch", s.batchSize).Warn("Projection refresh batch limit reached; continuing next tick")
	}

	s.reportHealth(ctx)
	if drainOK {
		s.metrics.MarkJobSuccess("projection_refresh", time.Now())
	}

	if time.Since(s.lastPrune) >= time.Hour {
		if pruned, err := s.prune(ctx); err != nil {
			logrus.WithError(err).Warn("Failed to prune projection refresh queue")
		} else if pruned > 0 {
			logrus.WithField("rows", pruned).Info("Pruned projection refresh queue")
			s.lastPrune = time.Now()
		} else {
			s.lastPrune = time.Now()
		}
	}
}

func (s *ProjectionRefreshService) configureSyncLimit(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projection_meta (key, value, updated_at)
		VALUES ('queue_sync_user_limit', $1, NOW())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value,
		    updated_at = NOW()
	`, fmt.Sprintf("%d", s.syncUserLimit))
	if err != nil {
		return err
	}
	logrus.WithField("sync_user_limit", s.syncUserLimit).
		Info("Projection queue sync limit configured")
	return nil
}

func (s *ProjectionRefreshService) processOne(ctx context.Context) (string, int, int, string, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT outcome, user_id, attempts, available_at, last_error
		FROM process_projection_refresh_queue($1)
	`, s.maxAttempts)

	var outcome string
	var userID, attempts sql.NullInt64
	var availableAt sql.NullTime
	var lastErr sql.NullString
	if err := row.Scan(&outcome, &userID, &attempts, &availableAt, &lastErr); err != nil {
		return "", 0, 0, "", err
	}

	return outcome, int(userID.Int64), int(attempts.Int64), lastErr.String, nil
}

type projectionQueueStats struct {
	pending              int64
	ready                int64
	failed               int64
	processed            int64
	oldestPendingSeconds int64
	maxAttempts          int64
}

func (s *ProjectionRefreshService) queueStats(ctx context.Context) (projectionQueueStats, error) {
	var stats projectionQueueStats
	err := s.db.QueryRowContext(ctx, `
		SELECT pending, ready, failed, processed, oldest_pending_seconds, max_attempts
		FROM projection_queue_stats()
	`).Scan(
		&stats.pending,
		&stats.ready,
		&stats.failed,
		&stats.processed,
		&stats.oldestPendingSeconds,
		&stats.maxAttempts,
	)
	return stats, err
}

func (s *ProjectionRefreshService) reportHealth(ctx context.Context) {
	stats, err := s.queueStats(ctx)
	if err != nil {
		logrus.WithError(err).Warn("Failed to read projection queue statistics")
		return
	}

	if stats.pending > 0 || stats.failed > 0 {
		logrus.WithFields(logrus.Fields{
			"pending":                stats.pending,
			"ready":                  stats.ready,
			"failed":                 stats.failed,
			"oldest_pending_seconds": stats.oldestPendingSeconds,
		}).Debug("Projection refresh queue statistics")
	}

	s.metrics.ObserveProjectionQueue(metrics.ProjectionQueueStats{
		Pending:              stats.pending,
		Ready:                stats.ready,
		Failed:               stats.failed,
		Processed:            stats.processed,
		OldestPendingSeconds: stats.oldestPendingSeconds,
		MaxAttempts:          stats.maxAttempts,
	})

	if stats.failed > 0 {
		s.limiter.send(ctx, s.alerter, "queue-failed", "projection_queue_failed", logrus.Fields{
			"failed":                 stats.failed,
			"oldest_pending_seconds": stats.oldestPendingSeconds,
		})
	}
	if stats.pending > int64(s.alertDepth) {
		s.limiter.send(ctx, s.alerter, "queue-depth", "projection_queue_depth", logrus.Fields{
			"pending":                stats.pending,
			"threshold":              s.alertDepth,
			"oldest_pending_seconds": stats.oldestPendingSeconds,
		})
	}
	if stats.oldestPendingSeconds > int64(s.alertLag.Seconds()) {
		s.limiter.send(ctx, s.alerter, "queue-lag", "projection_queue_lag", logrus.Fields{
			"oldest_pending_seconds": stats.oldestPendingSeconds,
			"threshold_seconds":      int64(s.alertLag.Seconds()),
			"pending":                stats.pending,
		})
	}

	var total, stale, invalid int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT total, stale, invalid FROM projection_factor_status()
	`).Scan(&total, &stale, &invalid); err != nil {
		logrus.WithError(err).Warn("Failed to read projection factor status")
		return
	}

	s.metrics.ObserveProjectionFactors(metrics.ProjectionFactorStats{
		Total:   total,
		Stale:   stale,
		Invalid: invalid,
	})
	if stale > 0 || invalid > 0 {
		// Only re-alert when the stale count changes; otherwise a long weekend
		// or a paused rollover would emit the same error every tick.
		if stale != s.lastStaleFactors || invalid > 0 {
			s.limiter.send(ctx, s.alerter, "factor-status", "projection_factor_stale", logrus.Fields{
				"total":   total,
				"stale":   stale,
				"invalid": invalid,
			})
		}
	}
	s.lastStaleFactors = stale
}

func (s *ProjectionRefreshService) prune(ctx context.Context) (int, error) {
	var pruned int
	err := s.db.QueryRowContext(ctx, `
		SELECT prune_projection_refresh_queue(
			make_interval(secs => $1::DOUBLE PRECISION),
			make_interval(secs => $2::DOUBLE PRECISION)
		)
	`, s.processedRetention.Seconds(), s.failedRetention.Seconds()).Scan(&pruned)
	return pruned, err
}
