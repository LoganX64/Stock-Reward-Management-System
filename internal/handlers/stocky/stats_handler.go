package stocky

import (
	"net/http"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func (h *Handler) StatsHandler(c *gin.Context) {
	userID, ok := parseUserID(c)
	if !ok {
		return
	}

	logger := logrus.WithFields(logrus.Fields{
		"request_id": requestID(c),
		"user_id":    userID,
	})

	// Today's rewards and the portfolio totals must describe the same
	// valuation date and snapshot as each other.
	tx, err := h.beginReadSnapshot(c.Request.Context())
	if err != nil {
		logger.WithError(err).Error("Failed to start stats read snapshot")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT rs.terminal_symbol, rs.adjusted_quantity::NUMERIC
		FROM reward_state rs
		WHERE rs.user_id = $1
		  AND rs.reward_date = market_today()
		  AND rs.is_active
	`, userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch today rewards")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	var todayRewards []models.TodayReward
	for rows.Next() {
		var tr models.TodayReward
		if err := rows.Scan(&tr.StockSymbol, &tr.TotalQuantity); err != nil {
			rows.Close()
			logger.WithError(err).Error("Failed to scan today reward")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
			return
		}

		tr.TotalQuantity = utils.RoundQuantity(tr.TotalQuantity)
		todayRewards = append(todayRewards, tr)
	}

	if err := rows.Err(); err != nil {
		rows.Close()
		logger.WithError(err).Error("Error iterating today rewards rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	if err := rows.Close(); err != nil {
		logger.WithError(err).Error("Failed to close today rewards rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	summary, err := h.getPortfolioSummary(tx, userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch portfolio summary")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	if err := tx.Commit(); err != nil {
		logger.WithError(err).Error("Failed to commit stats read snapshot")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":              userID,
		"todayRewards":        utils.OrEmpty(todayRewards),
		"totalPortfolioValue": utils.RoundAmount(summary.TotalPortfolioValue),
		"totalPositions":      summary.TotalPositions,
		"valuationDate":       summary.ValuationDate,
		"valuationComplete":   len(summary.UnpricedSymbols) == 0,
		"unpricedPositions":   len(summary.UnpricedSymbols),
		"unpricedSymbols":     utils.OrEmpty(summary.UnpricedSymbols),
		"stalePositions":      summary.StalePositions,
		"staleSymbols":        utils.OrEmpty(summary.StaleSymbols),
		"pricesFresh":         summary.PricesFresh,
		"cashProceeds":        utils.RoundAmount(summary.CashProceeds),
		"cashInLieuProceeds":  utils.RoundAmount(summary.CashInLieuProceeds),
		"writtenOffQuantity":  utils.RoundQuantity(summary.WrittenOffQuantity),
	})
}
