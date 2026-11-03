package jobs

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// ProjectionMismatch is one drift row returned by
// run_projection_reconciliation(). The expected/actual payloads are the full
// lifecycle/projection rows for the entity that disagreed.
type ProjectionMismatch struct {
	UserID       int             `json:"user_id"`
	MismatchType string          `json:"mismatch_type"`
	EntityKey    string          `json:"entity_key"`
	Expected     json.RawMessage `json:"expected"`
	Actual       json.RawMessage `json:"actual"`
}

// ReconciliationService samples projection users (plus every user touched by a
// corporate action) and compares their projections with the authoritative
// lifecycle baseline. It runs independently of the queue worker so drift caused
// by a missed trigger, a rebuild gap, or a rollover bug is detected even when no
// write is occurring.
type ReconciliationService struct {
	db            DB
	interval      time.Duration
	samplePercent float64
	maxUsers      int
	maxDetails    int
	timeout       time.Duration
	alerter       Alerter
	limiter       *alertLimiter
	started       int32
}

// ReconciliationServiceOptions controls sampling and alerting. Zero values use
// the defaults below.
type ReconciliationServiceOptions struct {
	Interval      time.Duration
	SamplePercent float64
	MaxUsers      int
	MaxDetails    int
	Timeout       time.Duration
	AlertCooldown time.Duration
	Alerter       Alerter
}

func NewReconciliationService(db DB) *ReconciliationService {
	return NewReconciliationServiceWithOptions(db, nil)
}

func NewReconciliationServiceWithOptions(db DB, opts *ReconciliationServiceOptions) *ReconciliationService {
	options := ReconciliationServiceOptions{
		Interval:      time.Hour,
		SamplePercent: 0.1,
		MaxUsers:      500,
		MaxDetails:    1000,
		Timeout:       5 * time.Minute,
		AlertCooldown: 15 * time.Minute,
		Alerter:       LogAlerter{},
	}
	if opts != nil {
		if opts.Interval > 0 {
			options.Interval = opts.Interval
		}
		if opts.SamplePercent > 0 {
			options.SamplePercent = opts.SamplePercent
		}
		if opts.MaxUsers > 0 {
			options.MaxUsers = opts.MaxUsers
		}
		if opts.MaxDetails > 0 {
			options.MaxDetails = opts.MaxDetails
		}
		if opts.Timeout > 0 {
			options.Timeout = opts.Timeout
		}
		if opts.AlertCooldown >= 0 {
			options.AlertCooldown = opts.AlertCooldown
		}
		if opts.Alerter != nil {
			options.Alerter = opts.Alerter
		}
	}

	return &ReconciliationService{
		db:            db,
		interval:      options.Interval,
		samplePercent: options.SamplePercent,
		maxUsers:      options.MaxUsers,
		maxDetails:    options.MaxDetails,
		timeout:       options.Timeout,
		alerter:       options.Alerter,
		limiter:       newAlertLimiter(options.AlertCooldown),
	}
}

func (s *ReconciliationService) Start(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		logrus.Warn("ReconciliationService.Start called more than once -- ignoring duplicate")
		return
	}

	logrus.WithFields(logrus.Fields{
		"interval":       s.interval,
		"sample_percent": s.samplePercent,
		"max_users":      s.maxUsers,
	}).Info("Projection reconciliation service started")

	s.process(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.process(ctx)
		case <-ctx.Done():
			logrus.Info("Projection reconciliation service stopped")
			return
		}
	}
}

func (s *ReconciliationService) process(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	started := time.Now()
	usersChecked, mismatchCount, mismatches, err := s.run(ctx)
	if err != nil {
		logrus.WithError(err).Error("Projection reconciliation failed")
		s.limiter.send(ctx, s.alerter, "reconciliation-error", "projection_reconciliation_error", logrus.Fields{
			"error": err.Error(),
		})
		return
	}

	fields := logrus.Fields{
		"users_checked":  usersChecked,
		"mismatch_count": mismatchCount,
		"duration_ms":    time.Since(started).Milliseconds(),
	}

	if mismatchCount == 0 {
		logrus.WithFields(fields).Info("Projection reconciliation found no drift")
		return
	}

	logrus.WithFields(fields).Error("Projection reconciliation found drift")
	for _, mismatch := range mismatches {
		logrus.WithFields(logrus.Fields{
			"user_id":       mismatch.UserID,
			"mismatch_type": mismatch.MismatchType,
			"entity_key":    mismatch.EntityKey,
			"expected":      string(mismatch.Expected),
			"actual":        string(mismatch.Actual),
		}).Error("Projection mismatch")
	}

	s.limiter.send(ctx, s.alerter, "reconciliation-drift", "projection_reconciliation_drift", fields)
}

func (s *ReconciliationService) run(ctx context.Context) (int, int, []ProjectionMismatch, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT users_checked, mismatch_count, mismatches
		FROM run_projection_reconciliation($1, $2, $3)
	`, s.samplePercent, s.maxUsers, s.maxDetails)

	var usersChecked, mismatchCount int
	var raw []byte
	if err := row.Scan(&usersChecked, &mismatchCount, &raw); err != nil {
		return 0, 0, nil, err
	}

	var mismatches []ProjectionMismatch
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &mismatches); err != nil {
			return usersChecked, mismatchCount, nil, err
		}
	}
	return usersChecked, mismatchCount, mismatches, nil
}
