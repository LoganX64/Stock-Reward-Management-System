package jobs

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/sirupsen/logrus"
)

type recordingAlerter struct {
	events []string
	fields []logrus.Fields
}

func (r *recordingAlerter) Alert(_ context.Context, event string, fields logrus.Fields) {
	r.events = append(r.events, event)
	r.fields = append(r.fields, fields)
}

func projectionRefreshServiceForTest(t *testing.T, db DB, alerter Alerter) *ProjectionRefreshService {
	t.Helper()
	return NewProjectionRefreshServiceWithOptions(db, &ProjectionRefreshServiceOptions{
		BatchSize:     10,
		MaxAttempts:   3,
		AlertDepth:    100,
		AlertLag:      time.Minute,
		AlertCooldown: 0,
		SyncUserLimit: 50,
		Timeout:       time.Second,
		Alerter:       alerter,
	})
}

func expectProjectionHealth(mock sqlmock.Sqlmock, pending int64, failed int64, stale int64) {
	mock.ExpectQuery("FROM projection_queue_stats").
		WillReturnRows(sqlmock.NewRows([]string{
			"pending", "ready", "failed", "processed", "oldest_pending_seconds", "max_attempts",
		}).AddRow(pending, pending, failed, 0, 0, 0))
	mock.ExpectQuery("FROM projection_factor_status").
		WillReturnRows(sqlmock.NewRows([]string{"total", "stale", "invalid"}).
			AddRow(3000, stale, 0))
	mock.ExpectQuery("SELECT prune_projection_refresh_queue").
		WillReturnRows(sqlmock.NewRows([]string{"prune_projection_refresh_queue"}).AddRow(0))
}

func TestProjectionRefreshService_Idle(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("INSERT INTO projection_meta").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM process_projection_refresh_queue").
		WillReturnRows(sqlmock.NewRows([]string{
			"outcome", "user_id", "attempts", "available_at", "last_error",
		}).AddRow("idle", nil, nil, nil, nil))
	expectProjectionHealth(mock, 0, 0, 0)

	alerter := &recordingAlerter{}
	service := projectionRefreshServiceForTest(t, db, alerter)
	service.process(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
	if len(alerter.events) != 0 {
		t.Fatalf("unexpected alerts: %v", alerter.events)
	}
}

func TestProjectionRefreshService_ProcessesAndRetries(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("INSERT INTO projection_meta").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM process_projection_refresh_queue").
		WillReturnRows(sqlmock.NewRows([]string{
			"outcome", "user_id", "attempts", "available_at", "last_error",
		}).AddRow("processed", 7, 0, nil, nil))
	mock.ExpectQuery("FROM process_projection_refresh_queue").
		WillReturnRows(sqlmock.NewRows([]string{
			"outcome", "user_id", "attempts", "available_at", "last_error",
		}).AddRow("retry", 8, 1, nil, "lock timeout"))
	mock.ExpectQuery("FROM process_projection_refresh_queue").
		WillReturnRows(sqlmock.NewRows([]string{
			"outcome", "user_id", "attempts", "available_at", "last_error",
		}).AddRow("idle", nil, nil, nil, nil))
	expectProjectionHealth(mock, 0, 0, 0)

	alerter := &recordingAlerter{}
	service := projectionRefreshServiceForTest(t, db, alerter)
	service.process(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
	if len(alerter.events) != 0 {
		t.Fatalf("unexpected alerts: %v", alerter.events)
	}
}

func TestProjectionRefreshService_AlertsOnDeadLetterAndBacklog(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("INSERT INTO projection_meta").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM process_projection_refresh_queue").
		WillReturnRows(sqlmock.NewRows([]string{
			"outcome", "user_id", "attempts", "available_at", "last_error",
		}).AddRow("failed", 9, 3, nil, "permission denied"))
	mock.ExpectQuery("FROM process_projection_refresh_queue").
		WillReturnRows(sqlmock.NewRows([]string{
			"outcome", "user_id", "attempts", "available_at", "last_error",
		}).AddRow("idle", nil, nil, nil, nil))
	expectProjectionHealth(mock, 250, 1, 2)

	alerter := &recordingAlerter{}
	service := projectionRefreshServiceForTest(t, db, alerter)
	service.process(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}

	want := map[string]bool{
		"projection_queue_dead_letter": false,
		"projection_queue_failed":      false,
		"projection_queue_depth":       false,
		"projection_factor_stale":      false,
	}
	for _, event := range alerter.events {
		if _, ok := want[event]; ok {
			want[event] = true
		}
	}
	for event, seen := range want {
		if !seen {
			t.Errorf("missing alert %q; got %v", event, alerter.events)
		}
	}
}

func TestProjectionRefreshService_ConfigureWritesSyncLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("queue_sync_user_limit")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM process_projection_refresh_queue").
		WillReturnRows(sqlmock.NewRows([]string{
			"outcome", "user_id", "attempts", "available_at", "last_error",
		}).AddRow("idle", nil, nil, nil, nil))
	expectProjectionHealth(mock, 0, 0, 0)

	service := projectionRefreshServiceForTest(t, db, &recordingAlerter{})
	service.process(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}

func TestReconciliationService_NoDrift(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM run_projection_reconciliation").
		WithArgs(0.1, 500, 1000).
		WillReturnRows(sqlmock.NewRows([]string{
			"users_checked", "mismatch_count", "mismatches",
		}).AddRow(25, 0, []byte("[]")))

	alerter := &recordingAlerter{}
	service := NewReconciliationServiceWithOptions(db, &ReconciliationServiceOptions{
		SamplePercent: 0.1,
		MaxUsers:      500,
		AlertCooldown: 0,
		Alerter:       alerter,
	})
	service.process(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
	if len(alerter.events) != 0 {
		t.Fatalf("unexpected alerts: %v", alerter.events)
	}
}

func TestReconciliationService_ReportsDrift(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	payload := []byte(`[{"user_id":42,"mismatch_type":"portfolio_diff","entity_key":"42:ALPHA","expected":{"quantity":10},"actual":{"quantity":9}}]`)
	mock.ExpectQuery("FROM run_projection_reconciliation").
		WillReturnRows(sqlmock.NewRows([]string{
			"users_checked", "mismatch_count", "mismatches",
		}).AddRow(10, 1, payload))

	alerter := &recordingAlerter{}
	service := NewReconciliationServiceWithOptions(db, &ReconciliationServiceOptions{
		SamplePercent: 0.1,
		MaxUsers:      500,
		AlertCooldown: 0,
		Alerter:       alerter,
	})
	service.process(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
	if len(alerter.events) != 1 || alerter.events[0] != "projection_reconciliation_drift" {
		t.Fatalf("alerts = %v, want projection_reconciliation_drift", alerter.events)
	}
}

func TestReconciliationService_ReportsError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM run_projection_reconciliation").
		WillReturnError(context.DeadlineExceeded)

	alerter := &recordingAlerter{}
	service := NewReconciliationServiceWithOptions(db, &ReconciliationServiceOptions{
		SamplePercent: 0.1,
		MaxUsers:      500,
		AlertCooldown: 0,
		Alerter:       alerter,
	})
	service.process(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
	if len(alerter.events) != 1 || alerter.events[0] != "projection_reconciliation_error" {
		t.Fatalf("alerts = %v, want projection_reconciliation_error", alerter.events)
	}
}
