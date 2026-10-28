package jobs

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// CorporateActionService keeps the ledger's corporate-action cash postings in
// sync with stock_events. Stock-event changes are posted immediately by a
// database trigger; this service catches events that become effective later
// (for example a cash-in-lieu split dated in the future).
type CorporateActionService struct {
	db       DB
	interval time.Duration
	timeout  time.Duration
	started  int32
}

// NewCorporateActionService creates a service with a daily processing interval.
func NewCorporateActionService(db DB) *CorporateActionService {
	return &CorporateActionService{
		db:       db,
		interval: 24 * time.Hour,
		timeout:  30 * time.Second,
	}
}

// Start runs one processing pass immediately and then once per interval until
// the context is cancelled.
func (s *CorporateActionService) Start(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		logrus.Warn("CorporateActionService.Start called more than once -- ignoring duplicate")
		return
	}

	logrus.Info("Corporate action cash processor started")
	s.process(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.process(ctx)
		case <-ctx.Done():
			logrus.Info("Corporate action cash processor stopped")
			return
		}
	}
}

func (s *CorporateActionService) process(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `
		SELECT process_corporate_action_cash(CURRENT_DATE)
	`)
	if err != nil {
		logrus.WithError(err).Error("Corporate action cash processing failed")
		return
	}
	defer rows.Close()

	var postings int
	if rows.Next() {
		if err := rows.Scan(&postings); err != nil {
			logrus.WithError(err).Error("Failed to read corporate action cash result")
			return
		}
	}
	if err := rows.Err(); err != nil {
		logrus.WithError(err).Error("Corporate action cash processing failed")
		return
	}

	logrus.WithField("postings", postings).Debug("Corporate action cash processing completed")
}
