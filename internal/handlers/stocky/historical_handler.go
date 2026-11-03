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
	"github.com/lib/pq"
	"github.com/shopspring/decimal"
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
		value := parsed.Format("2006-01-02")
		asOf = &value
	}

	logger := logrus.WithFields(logrus.Fields{
		"request_id": requestID(c),
		"user_id":    userID,
	})

	var asOfArg any
	valuationMode := "grant_date"
	if asOf != nil {
		asOfArg = *asOf
		valuationMode = "as_of"
	}

	// One statement returns the requested page plus the full-set unpriced
	// summary. The function applies the user and date filters before any
	// lifecycle work, so the page is bounded even when the user has a long
	// history. A summary-only row is returned when the page is empty.
	started := time.Now()
	rows, err := h.readDB().QueryContext(c.Request.Context(), `
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
			written_off_quantity,
			price_source,
			price_fetched_at,
			valuation_date,
			as_of_allowed,
			unpriced_rewards,
			unpriced_symbols
		FROM historical_rewards_for_user($1, $2::date, $3, $4)
	`, userID, asOfArg, limit, offset)
	h.Metrics.ObserveQuery("historical", started, err)
	if err != nil {
		writeReadError(c, logger, "Failed to fetch historical INR", "internal server error", err)
		return
	}
	defer rows.Close()

	var history []models.HistoricalINR
	var (
		valuationDate   time.Time
		asOfAllowed     bool
		unpricedRewards int64
		unpricedSymbols pq.StringArray
		sawSummary      bool
	)

	for rows.Next() {
		var (
			rewardDate            sql.NullTime
			rewardEventID         sql.NullInt64
			stockSymbol           sql.NullString
			adjustedQuantity      decimal.NullDecimal
			price                 decimal.NullDecimal
			totalAdjustmentAmount decimal.NullDecimal
			inrValue              decimal.NullDecimal
			priceStatus           sql.NullString
			priceAsOf             sql.NullTime
			outcomeType           sql.NullString
			isActive              sql.NullBool
			delistEffectiveDate   sql.NullTime
			cashProceeds          decimal.NullDecimal
			writtenOffQuantity    decimal.NullDecimal
			priceSource           sql.NullString
			priceFetchedAt        sql.NullTime
		)

		if err := rows.Scan(
			&rewardDate,
			&rewardEventID,
			&stockSymbol,
			&adjustedQuantity,
			&price,
			&totalAdjustmentAmount,
			&inrValue,
			&priceStatus,
			&priceAsOf,
			&outcomeType,
			&isActive,
			&delistEffectiveDate,
			&cashProceeds,
			&writtenOffQuantity,
			&priceSource,
			&priceFetchedAt,
			&valuationDate,
			&asOfAllowed,
			&unpricedRewards,
			&unpricedSymbols,
		); err != nil {
			writeReadError(c, logger, "Failed to scan historical INR record", "internal server error", err)
			return
		}
		sawSummary = true

		// The summary-only row has no reward fields; it exists so an empty
		// page still reports valuation completeness.
		if !rewardEventID.Valid {
			continue
		}

		rec := models.HistoricalINR{
			RewardDate:            rewardDate.Time.Format("2006-01-02"),
			RewardEventID:         int(rewardEventID.Int64),
			StockSymbol:           stockSymbol.String,
			AdjustedQuantity:      utils.RoundQuantity(adjustedQuantity.Decimal),
			TotalAdjustmentAmount: utils.RoundAmount(totalAdjustmentAmount.Decimal),
			PriceStatus:           priceStatus.String,
			OutcomeType:           outcomeType.String,
			IsActive:              isActive.Bool,
			CashProceeds:          utils.RoundAmount(cashProceeds.Decimal),
			WrittenOffQuantity:    utils.RoundQuantity(writtenOffQuantity.Decimal),
		}

		// Apply consistent rounding to available values and preserve NULL for
		// missing prices. A zero-quantity row reports not_required and no price.
		if price.Valid {
			rec.Price = decimal.NullDecimal{Decimal: utils.RoundAmount(price.Decimal), Valid: true}
		}
		if inrValue.Valid {
			rec.INRValue = decimal.NullDecimal{Decimal: utils.RoundAmount(inrValue.Decimal), Valid: true}
		}
		if priceAsOf.Valid {
			t := priceAsOf.Time.UTC()
			rec.PriceAsOf = &t
		}
		if priceSource.Valid {
			source := priceSource.String
			rec.PriceSource = &source
		}
		if priceFetchedAt.Valid {
			t := priceFetchedAt.Time.UTC()
			rec.PriceFetchedAt = &t
		}
		if delistEffectiveDate.Valid {
			d := delistEffectiveDate.Time.Format("2006-01-02")
			rec.DelistEffectiveDate = &d
		}

		history = append(history, rec)
	}
	if err := rows.Err(); err != nil {
		writeReadError(c, logger, "Error iterating historical INR rows", "internal server error", err)
		return
	}
	if !sawSummary {
		logger.Error("Historical INR query returned no summary row")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	if !asOfAllowed {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("asOf cannot be in the future"))
		return
	}

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":            userID,
		"limit":             limit,
		"offset":            offset,
		"valuationMode":     valuationMode,
		"valuationDate":     valuationDate.Format("2006-01-02"),
		"asOf":              asOf,
		"history":           utils.OrEmpty(history),
		"valuationComplete": unpricedRewards == 0,
		"unpricedRewards":   int(unpricedRewards),
		"unpricedSymbols":   utils.OrEmpty([]string(unpricedSymbols)),
	})
}
