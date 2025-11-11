package stocky

import (
	"net/http"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func StatsHandler(c *gin.Context) {
	userID, ok := parseUserID(c)
	if !ok {
		return
	}
	logger := logrus.WithFields(logrus.Fields{
		"request_id": requestID(c),
		"user_id":    userID,
	})

	rowsToday, err := db.Query(`
		SELECT stock_symbol, adjusted_quantity
		FROM today_rewards
		WHERE user_id = $1
	`, userID)
	if err != nil {
		logger.WithError(err).Error("today rewards query")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal error"))
		return
	}
	defer rowsToday.Close()

	var todayRewards []models.TodayReward
	for rowsToday.Next() {
		var tr models.TodayReward
		if err := rowsToday.Scan(&tr.StockSymbol, &tr.TotalQuantity); err != nil {
			logger.WithError(err).Error("today scan")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal error"))
			return
		}
		tr.TotalQuantity = utils.RoundQuantity(tr.TotalQuantity)
		todayRewards = append(todayRewards, tr)
	}

	var totalPortfolioValue float64
	err = db.QueryRow(`
		SELECT COALESCE(SUM(inr_value),0)
		FROM user_portfolio
		WHERE user_id = $1
	`, userID).Scan(&totalPortfolioValue)
	if err != nil {
		logger.WithError(err).Error("portfolio value query")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal error"))
		return
	}

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":              userID,
		"todayRewards":        todayRewards,
		"totalPortfolioValue": utils.RoundAmount(totalPortfolioValue),
	})
}
