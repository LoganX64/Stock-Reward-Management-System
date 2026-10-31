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
		SELECT stock_symbol, adjusted_quantity
		FROM today_rewards
		WHERE user_id = $1
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

	totalPortfolioValue, totalPositions, valuationDate, err := h.getPortfolioTotals(tx, userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch total portfolio value")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	freshness, err := h.getPortfolioFreshness(tx, userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch portfolio freshness")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	unpricedSymbols, err := h.getUnpricedPortfolioSymbols(tx, userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch unpriced portfolio symbols")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	cashProceeds, writtenOffQuantity, cashInLieuProceeds, err := h.getUserCorporateActionCash(tx, userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch corporate action cash outcomes")
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
		"totalPortfolioValue": utils.RoundAmount(totalPortfolioValue),
		"totalPositions":      totalPositions,
		"valuationDate":       valuationDate,
		"valuationComplete":   len(unpricedSymbols) == 0,
		"unpricedPositions":   len(unpricedSymbols),
		"unpricedSymbols":     utils.OrEmpty(unpricedSymbols),
		"stalePositions":      freshness.StalePositions,
		"staleSymbols":        utils.OrEmpty(freshness.StaleSymbols),
		"pricesFresh":         freshness.PricesFresh,
		"cashProceeds":        utils.RoundAmount(cashProceeds),
		"cashInLieuProceeds":  utils.RoundAmount(cashInLieuProceeds),
		"writtenOffQuantity":  utils.RoundQuantity(writtenOffQuantity),
	})
}
