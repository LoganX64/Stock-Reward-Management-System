package stocky

import (
	"net/http"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func GetHistoricalINR(c *gin.Context) {
	userID, ok := parseUserID(c)
	if !ok {
		return
	}
	logger := logrus.WithFields(logrus.Fields{
		"request_id": requestID(c),
		"user_id":    userID,
	})

	rows, err := db.Query(`
			SELECT reward_date, reward_event_id, stock_symbol,
			adjusted_quantity, price, total_adjustment_amount, inr_value
			FROM historical_rewards
			WHERE user_id = $1
			AND reward_date < CURRENT_DATE
			ORDER BY reward_date DESC
	`, userID)

	if err != nil {
		logger.WithError(err).Error("Failed to fetch historical INR")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	defer rows.Close()

	var history []models.HistoricalINR
	for rows.Next() {
		var rec models.HistoricalINR
		if err := rows.Scan(&rec.RewardDate, &rec.RewardEventID, &rec.StockSymbol,
			&rec.AdjustedQuantity, &rec.Price, &rec.TotalAdjustmentAmount, &rec.INRValue); err != nil {
			logger.WithError(err).Error("scan error")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
			return
		}
		rec.AdjustedQuantity = utils.RoundQuantity(rec.AdjustedQuantity)
		rec.Price = utils.RoundAmount(rec.Price)
		rec.TotalAdjustmentAmount = utils.RoundAmount(rec.TotalAdjustmentAmount)
		rec.INRValue = utils.RoundAmount(rec.INRValue)
		history = append(history, rec)
	}

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":  userID,
		"history": utils.OrEmpty(history),
	})
}
