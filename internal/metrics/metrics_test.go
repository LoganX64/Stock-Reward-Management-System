package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	return string(body)
}

func TestGinMiddlewareRecordsRoutePattern(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := New()

	router := gin.New()
	router.Use(m.GinMiddleware())
	router.GET("/api/v1/portfolio/:userId", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/42", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	body := scrape(t, m)
	for _, want := range []string{
		`stocky_http_requests_total{method="GET",route="/api/v1/portfolio/:userId",status="200"} 1`,
		`stocky_http_request_duration_seconds_count{method="GET",route="/api/v1/portfolio/:userId"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n%s", want, body)
		}
	}
}

func TestProjectionAndReconciliationGauges(t *testing.T) {
	m := New()
	m.ObserveProjectionQueue(ProjectionQueueStats{
		Pending:              3,
		Ready:                2,
		Failed:               1,
		Processed:            9,
		OldestPendingSeconds: 120,
		MaxAttempts:          5,
	})
	m.ObserveProjectionFactors(ProjectionFactorStats{Total: 10, Stale: 2, Invalid: 1})
	m.ObserveReconciliation(25, 2, 1500*time.Millisecond)
	m.MarkJobSuccess("reconciliation", time.Unix(1700000000, 0))

	body := scrape(t, m)
	for _, want := range []string{
		"stocky_projection_queue_pending 3",
		"stocky_projection_queue_ready 2",
		"stocky_projection_queue_failed 1",
		"stocky_projection_queue_processed 9",
		"stocky_projection_queue_oldest_pending_seconds 120",
		"stocky_projection_factor_total 10",
		"stocky_projection_factor_stale 2",
		"stocky_projection_factor_invalid 1",
		"stocky_projection_reconciliation_users_checked 25",
		"stocky_projection_reconciliation_mismatches 2",
		"stocky_projection_reconciliation_duration_seconds 1.5",
		`stocky_projection_job_last_success_timestamp_seconds{job="reconciliation"} 1.7e+09`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n%s", want, body)
		}
	}
}

func TestDBPoolMetrics(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	m := New()
	m.RegisterDBPool("api", db)
	// Registering the same pool again must not panic.
	m.RegisterDBPool("api", db)

	body := scrape(t, m)
	for _, want := range []string{
		`stocky_db_pool_max_open_connections{pool="api"} 0`,
		`stocky_db_pool_open_connections{pool="api"}`,
		`stocky_db_pool_wait_count_total{pool="api"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n%s", want, body)
		}
	}
}

func TestObserveQueryRecordsDurationAndError(t *testing.T) {
	m := New()
	m.ObserveQuery("portfolio_page", time.Now().Add(-time.Millisecond), nil)
	m.ObserveQuery("portfolio_page", time.Now().Add(-time.Millisecond), io.EOF)

	body := scrape(t, m)
	for _, want := range []string{
		`stocky_db_query_duration_seconds_count{query="portfolio_page"} 2`,
		`stocky_db_query_errors_total{query="portfolio_page"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n%s", want, body)
		}
	}
}
