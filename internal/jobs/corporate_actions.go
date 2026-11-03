package jobs

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/LoganX64/stocky-api/internal/metrics"
	"github.com/sirupsen/logrus"
)

// CorporateActionService keeps the ledger's corporate-action cash postings and
// the read projections in sync with stock_events. Stock-event changes are
// posted immediately by a database trigger; this service catches events that
// become effective later (for example a cash-in-lieu split dated in the future)
// and refreshes date-sensitive projection state at every market-date rollover.
//
// Phase 2 runs the service frequently but the SQL wrappers perform the
// expensive work at most once per market date.
type CorporateActionService struct {
	db       DB
	interval time.Duration
	timeout  time.Duration
	metrics  *metrics.Metrics
	started  int32
}

// CorporateActionServiceOptions overrides the rollover cadence and timeout.
type CorporateActionServiceOptions struct {
	Interval time.Duration
	Timeout  time.Duration
	Metrics  *metrics.Metrics
}

// NewCorporateActionService creates a service that runs every 15 minutes. The
// rollover wrappers are due-checked, so the frequent tick is cheap and a
// process started after midnight still catches the new market date.
func NewCorporateActionService(db DB) *CorporateActionService {
	return NewCorporateActionServiceWithOptions(db, nil)
}

func NewCorporateActionServiceWithOptions(db DB, opts *CorporateActionServiceOptions) *CorporateActionService {
	options := CorporateActionServiceOptions{
		Interval: 15 * time.Minute,
		Timeout:  2 * time.Minute,
	}
	if opts != nil {
		if opts.Interval > 0 {
			options.Interval = opts.Interval
		}
		if opts.Timeout > 0 {
			options.Timeout = opts.Timeout
		}
	}

	return &CorporateActionService{
		db:       db,
		interval: options.Interval,
		timeout:  options.Timeout,
		metrics:  options.Metrics,
	}
}

// Start runs one processing pass immediately and then once per interval until
// the context is cancelled.
func (s *CorporateActionService) Start(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		logrus.Warn("CorporateActionService.Start called more than once -- ignoring duplicate")
		return
	}

	logrus.Info("Corporate action cash and projection processor started")
	s.process(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.process(ctx)
		case <-ctx.Done():
			logrus.Info("Corporate action cash and projection processor stopped")
			return
		}
	}
}

func (s *CorporateActionService) process(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	// The order matters: factors must be current before date-sensitive reward
	// state is refreshed, and the cash ledger is part of the base state. The
	// *_if_due wrappers do the expensive work at most once per market date and
	// serialise multiple API instances with advisory locks.
	ok := true
	if err := s.runCount(ctx, "corporate action cash", "postings", `
		SELECT process_corporate_action_cash_if_due(market_today())
	`); err != nil {
		ok = false
	}
	if err := s.runCount(ctx, "instrument factors", "symbols", `
		SELECT refresh_instrument_factors_if_due(market_today())
	`); err != nil {
		ok = false
	}
	if err := s.runCount(ctx, "effective corporate actions", "rewards", `
		SELECT refresh_effective_corporate_actions_if_due(market_today())
	`); err != nil {
		ok = false
	}
	if ok {
		s.metrics.MarkJobSuccess("corporate_actions", time.Now())
	}
}

func (s *CorporateActionService) runCount(ctx context.Context, label, field, query string) error {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		logrus.WithError(err).Errorf("%s refresh failed", label)
		return err
	}
	defer rows.Close()

	var count int
	if rows.Next() {
		if err := rows.Scan(&count); err != nil {
			logrus.WithError(err).Errorf("Failed to read %s result", label)
			return err
		}
	}
	if err := rows.Err(); err != nil {
		logrus.WithError(err).Errorf("%s refresh failed", label)
		return err
	}

	logrus.WithField(field, count).Debugf("%s refresh completed", label)
	return nil
}
