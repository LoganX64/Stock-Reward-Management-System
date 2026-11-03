package jobs

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"
)

// Alerter is the small alerting seam for projection health. The repository does
// not run a metrics stack yet, so the default implementation emits structured
// log entries that a log-based alert rule can match. A future metrics backend
// can implement this interface without changing the jobs.
type Alerter interface {
	Alert(ctx context.Context, event string, fields logrus.Fields)
}

// LogAlerter emits one structured error line per alert. Event names are stable
// so log alert rules can key on `event` rather than the message text.
type LogAlerter struct{}

func (LogAlerter) Alert(_ context.Context, event string, fields logrus.Fields) {
	if fields == nil {
		fields = logrus.Fields{}
	}
	fields["alert"] = true
	fields["event"] = event
	logrus.WithFields(fields).Error("projection alert")
}

// NopAlerter is useful in tests and for deployments that intentionally disable
// alerting while still running the jobs.
type NopAlerter struct{}

func (NopAlerter) Alert(context.Context, string, logrus.Fields) {}

// alertLimiter suppresses repeated alerts for the same condition. Queue depth
// and stale-factor alerts can persist for many ticks; without a cooldown they
// would flood the error log and hide new failures.
type alertLimiter struct {
	cooldown time.Duration
	last     map[string]time.Time
}

func newAlertLimiter(cooldown time.Duration) *alertLimiter {
	return &alertLimiter{
		cooldown: cooldown,
		last:     make(map[string]time.Time),
	}
}

func (l *alertLimiter) send(
	ctx context.Context,
	alerter Alerter,
	key, event string,
	fields logrus.Fields,
) {
	if alerter == nil {
		return
	}
	if l.cooldown > 0 {
		now := time.Now()
		if previous, ok := l.last[key]; ok && now.Sub(previous) < l.cooldown {
			return
		}
		l.last[key] = now
	}
	alerter.Alert(ctx, event, fields)
}
