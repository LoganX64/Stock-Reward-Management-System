package jobs

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// CorporateActionService keeps the ledger's corporate-action cash postings and
// the read projections in sync with stock_events. Stock-event changes are
// posted immediately by a database trigger; this service catches events that
// become effective later (for example a cash-in-lieu split dated in the future)
// and refreshes date-sensitive projection state at every market-date rollover.
type CorporateActionService struct {
	db       DB
	interval time.Duration
	timeout  time.Duration
	started  int32
}

// NewCorporateActionService creates a service that runs hourly. Running hourly
// rather than every 24 hours means a process started after midnight still
// refreshes instrument_factor and effective corporate actions on the same day.
func NewCorporateActionService(db DB) *CorporateActionService {
	return &CorporateActionService{
		db:       db,
		interval: time.Hour,
		timeout:  2 * time.Minute,
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
	// state is refreshed, and the cash ledger is part of the base state.
	s.runCount(ctx, "corporate action cash", "postings", `
		SELECT process_corporate_action_cash(market_today())
	`)
	s.runCount(ctx, "instrument factors", "symbols", `
		SELECT refresh_instrument_factors(market_today())
	`)
	s.runCount(ctx, "effective corporate actions", "rewards", `
		SELECT refresh_effective_corporate_actions(market_today())
	`)
}

func (s *CorporateActionService) runCount(ctx context.Context, label, field, query string) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		logrus.WithError(err).Errorf("%s refresh failed", label)
		return
	}
	defer rows.Close()

	var count int
	if rows.Next() {
		if err := rows.Scan(&count); err != nil {
			logrus.WithError(err).Errorf("Failed to read %s result", label)
			return
		}
	}
	if err := rows.Err(); err != nil {
		logrus.WithError(err).Errorf("%s refresh failed", label)
		return
	}

	logrus.WithField(field, count).Debugf("%s refresh completed", label)
}
