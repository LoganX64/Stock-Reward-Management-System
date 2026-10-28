// historical_handler.go
package stocky

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// GetHistoricalINR returns historical reward valuations.
//
// By default each reward is valued on its grant date: original quantity times
// the adjusted reward-date price. The lifecycle fields describe the current
// state, so a later delist remains visible without removing the row.
//
// With ?asOf=YYYY-MM-DD the whole row is point-in-time: effective quantity,
// terminal symbol, adjustments, delist status, and price are evaluated on that
// date. A lot delisted on or before asOf has quantity 0, priceStatus
// "not_required", and reports its cash/write-off outcome.
func (h *Handler) GetHistoricalINR(c *gin.Context) {
	userID, ok := parseUserID(c)
	if !ok {
		return
	}
	limit, offset, ok := parsePagination(c)
	if !ok {
		return
	}

	var asOf *string
	rawAsOf := c.Query("asOf")
	if rawAsOf != "" {
		parsed, err := time.Parse("2006-01-02", rawAsOf)
		if err != nil {
			response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("asOf must be a date in YYYY-MM-DD format"))
			return
		}
		if rawAsOf > time.Now().UTC().Format("2006-01-02") {
			response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("asOf cannot be in the future"))
			return
		}
		value := parsed.Format("2006-01-02")
		asOf = &value
	}

	logger := logrus.WithFields(logrus.Fields{
		"request_id": requestID(c),
		"user_id":    userID,
	})

	tx, err := h.beginReadSnapshot(c.Request.Context())
	if err != nil {
		logger.WithError(err).Error("Failed to start historical read snapshot")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	defer tx.Rollback()

	var asOfArg any
	valuationMode := "grant_date"
	if asOf != nil {
		asOfArg = *asOf
		valuationMode = "as_of"
	}

	rows, err := tx.Query(`
		SELECT
			reward_date,
			reward_event_id,
			stock_symbol,
			adjusted_quantity,
			price,
			total_adjustment_amount,
			inr_value,
			price_status,
			price_as_of,
			outcome_type,
			is_active,
			delist_effective_date,
			cash_proceeds,
			written_off_quantity
		FROM historical_rewards_as_of($2::date)
		WHERE user_id = $1
		  AND reward_date <= COALESCE($2::date, CURRENT_DATE - 1)
		ORDER BY reward_date DESC, reward_event_id DESC
		LIMIT $3 OFFSET $4
	`, userID, asOfArg, limit, offset)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch historical INR")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	var history []models.HistoricalINR
	for rows.Next() {
		var rec models.HistoricalINR
		var price sql.NullFloat64
		var inrValue sql.NullFloat64
		var priceAsOf sql.NullTime
		var delistEffectiveDate sql.NullTime
		if err := rows.Scan(
			&rec.RewardDate,
			&rec.RewardEventID,
			&rec.StockSymbol,
			&rec.AdjustedQuantity,
			&price,
			&rec.TotalAdjustmentAmount,
			&inrValue,
			&rec.PriceStatus,
			&priceAsOf,
			&rec.OutcomeType,
			&rec.IsActive,
			&delistEffectiveDate,
			&rec.CashProceeds,
			&rec.WrittenOffQuantity,
		); err != nil {
			rows.Close()
			logger.WithError(err).Error("Failed to scan historical INR record")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
			return
		}

		// Apply consistent rounding to available values and preserve NULL for
		// missing prices. A zero-quantity row reports not_required and no price.
		rec.AdjustedQuantity = utils.RoundQuantity(rec.AdjustedQuantity)
		rec.TotalAdjustmentAmount = utils.RoundAmount(rec.TotalAdjustmentAmount)
		rec.CashProceeds = utils.RoundAmount(rec.CashProceeds)
		rec.WrittenOffQuantity = utils.RoundQuantity(rec.WrittenOffQuantity)
		if price.Valid {
			v := utils.RoundAmount(price.Float64)
			rec.Price = &v
		}
		if inrValue.Valid {
			v := utils.RoundAmount(inrValue.Float64)
			rec.INRValue = &v
		}
		if priceAsOf.Valid {
			t := priceAsOf.Time
			rec.PriceAsOf = &t
		}
		if delistEffectiveDate.Valid {
			d := delistEffectiveDate.Time.Format("2006-01-02")
			rec.DelistEffectiveDate = &d
		}

		history = append(history, rec)
	}

	if err := rows.Err(); err != nil {
		rows.Close()
		logger.WithError(err).Error("Error iterating historical INR rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	if err := rows.Close(); err != nil {
		logger.WithError(err).Error("Failed to close historical INR rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	valuationDate := ""
	if asOf != nil {
		valuationDate = *asOf
	} else {
		valuationDate, err = h.getCurrentDate(tx)
		if err != nil {
			logger.WithError(err).Error("Failed to fetch valuation date")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
			return
		}
	}

	unpricedRewards, unpricedSymbols, err := h.getUnpricedHistoricalRewards(tx, userID, asOf)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch unpriced historical rewards")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	if err := tx.Commit(); err != nil {
		logger.WithError(err).Error("Failed to commit historical read snapshot")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":            userID,
		"limit":             limit,
		"offset":            offset,
		"valuationMode":     valuationMode,
		"valuationDate":     valuationDate,
		"asOf":              asOf,
		"history":           utils.OrEmpty(history),
		"valuationComplete": unpricedRewards == 0,
		"unpricedRewards":   unpricedRewards,
		"unpricedSymbols":   utils.OrEmpty(unpricedSymbols),
	})
}
