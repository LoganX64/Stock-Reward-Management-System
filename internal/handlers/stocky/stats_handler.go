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

	rows, err := h.DB.Query(`
		SELECT stock_symbol, adjusted_quantity
		FROM today_rewards
		WHERE user_id = $1
	`, userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch today rewards")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	defer rows.Close()

	var todayRewards []models.TodayReward
	for rows.Next() {
		var tr models.TodayReward
		if err := rows.Scan(&tr.StockSymbol, &tr.TotalQuantity); err != nil {
			logger.WithError(err).Error("Failed to scan today reward")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
			return
		}

		tr.TotalQuantity = utils.RoundQuantity(tr.TotalQuantity)
		todayRewards = append(todayRewards, tr)
	}

	if err := rows.Err(); err != nil {
		logger.WithError(err).Error("Error iterating today rewards rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	var totalPortfolioValue float64
	err = h.DB.QueryRow(`
		SELECT COALESCE(SUM(inr_value), 0)
		FROM user_portfolio
		WHERE user_id = $1
	`, userID).Scan(&totalPortfolioValue)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch total portfolio value")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":              userID,
		"todayRewards":        utils.OrEmpty(todayRewards),
		"totalPortfolioValue": utils.RoundAmount(totalPortfolioValue),
	})
}
