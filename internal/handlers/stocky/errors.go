package stocky

import (
	"context"
	"errors"
	"net/http"

	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/sirupsen/logrus"
)

// isTimeoutError reports whether an error came from a request deadline or a
// PostgreSQL statement_timeout. PostgreSQL cancels a statement that exceeds
// statement_timeout with SQLSTATE 57014 (query_canceled).
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "57014" {
		return true
	}
	return false
}

// writeReadError maps read failures to retryable 503s when a deadline expired
// and to the existing 500 response otherwise.
func writeReadError(c *gin.Context, logger *logrus.Entry, logMessage, publicMessage string, err error) {
	if isTimeoutError(err) {
		logger.WithError(err).Warn("Read request timed out")
		response.WriteJson(c.Writer, http.StatusServiceUnavailable, response.ErrorResponse("request timed out"))
		return
	}

	logger.WithError(err).Error(logMessage)
	response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse(publicMessage))
}
