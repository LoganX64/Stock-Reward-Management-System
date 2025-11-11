package stocky

import (
	"net/http"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func GetTodayStocks(c *gin.Context) {
	userID, ok := parseUserID(c)
	if !ok {
		return
	}
	logger := logrus.WithFields(logrus.Fields{
		"request_id": requestID(c),
		"user_id":    userID,
	})

	rows, err := db.Query(`
		SELECT 
			reward_event_id,
			stock_symbol,
			adjusted_quantity,
			current_price,
			total_adjustment_amount,
			inr_value
		FROM today_rewards
		WHERE user_id = $1
		ORDER BY stock_symbol, reward_event_id
	`, userID)

	if err != nil {
		logger.WithError(err).Error("Failed to fetch today stocks")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	defer rows.Close()

	var stocks []models.TodayStock
	for rows.Next() {
		var s models.TodayStock
		if err := rows.Scan(
			&s.RewardID,
			&s.StockSymbol,
			&s.AdjustedQuantity,
			&s.CurrentPrice,
			&s.TotalAdjustmentAmount,
			&s.INRValue,
		); err != nil {
			logger.WithError(err).Error("scan error")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
			return
		}

		s.AdjustedQuantity = utils.RoundQuantity(s.AdjustedQuantity)
		s.CurrentPrice = utils.RoundAmount(s.CurrentPrice)
		s.TotalAdjustmentAmount = utils.RoundAmount(s.TotalAdjustmentAmount)
		s.INRValue = utils.RoundAmount(s.INRValue)

		stocks = append(stocks, s)
	}

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId": userID,
		"stocks": utils.OrEmpty(stocks),
	})
}
