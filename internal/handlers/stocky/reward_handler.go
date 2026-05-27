package stocky

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/sirupsen/logrus"
)

// CreateRewardRequest is the minimal request structure used by the insert helper.
type CreateRewardRequest struct {
	UserID         int64     `json:"user_id"`
	StockID        int64     `json:"stock_id"`
	RewardDate     time.Time `json:"reward_date"`
	IdempotencyKey string    `json:"idempotency_key"`
	Quantity       int64     `json:"quantity"`
}

// InsertReward inserts a reward row, ensuring an empty idempotency key is stored as SQL NULL.
func InsertReward(ctx context.Context, db *sql.DB, req CreateRewardRequest) (sql.Result, error) {
	// Use sql.NullString so the driver writes SQL NULL when the key is empty.
	var idemp sql.NullString
	if req.IdempotencyKey != "" {
		idemp = sql.NullString{String: req.IdempotencyKey, Valid: true}
	} else {
		idemp = sql.NullString{Valid: false}
	}

	insertSQL := `INSERT INTO rewards (user_id, stock_id, reward_date, idempotency_key, quantity, created_at)
        VALUES ($1, $2, $3, NULLIF($4, ''), $5, now())`

	return db.ExecContext(ctx, insertSQL, req.UserID, req.StockID, req.RewardDate, idemp, req.Quantity)
}

func (h *Handler) CreateReward(c *gin.Context) {
	logger := logrus.WithField("request_id", requestID(c))
	var req models.CreateRewardRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		logger.WithError(err).Warn("Invalid request payload")
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("Invalid request payload"))
		return
	}

	// Basic validation
	if req.Quantity == 0 {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("quantity cannot be zero"))
		return
	}
	if req.StockSymbol == "" {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("stock_symbol is required"))
		return
	}

	req.Quantity = utils.RoundQuantity(req.Quantity)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := h.DB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		logger.WithError(err).Error("Failed to begin transaction")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	// Ensure transaction is rolled back if not committed
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Check user exists
	var userExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`,
		req.UserID).Scan(&userExists); err != nil {
		logger.WithError(err).Error("User existence check failed")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	if !userExists {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("User does not exist"))
		return
	}

	// Fetch current price
	var currentPrice float64
	if err := tx.QueryRowContext(ctx, `SELECT price FROM stock_prices WHERE UPPER(stock_symbol) = UPPER($1)`, req.StockSymbol).Scan(&currentPrice); err != nil {
		if err == sql.ErrNoRows {
			response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("Stock symbol not found"))
			return
		}
		logger.WithError(err).Error("Failed to fetch stock price")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	// Insert reward
	var reward models.Reward
	idempotencyParam := sql.NullString{String: req.IdempotencyKey, Valid: req.IdempotencyKey != ""}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO rewards (user_id, stock_symbol, quantity, idempotency_key, created_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), NOW())
		RETURNING id, user_id, stock_symbol, quantity, idempotency_key, created_at`,
		req.UserID,
		req.StockSymbol,
		req.Quantity,
		idempotencyParam,
	).Scan(
		&reward.ID,
		&reward.User_ID,
		&reward.Stock_Symbol,
		&reward.Quantity,
		&reward.IdempotencyKey,
		&reward.CreatedAt,
	)

	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			switch pqErr.Constraint {
			case "unique_user_stock_date":
				response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("reward already given today"))
				return
			case "rewards_idempotency_key_key":
				fallthrough
			default:
				if strings.Contains(pqErr.Constraint, "idempotency") {
					// Fetch the existing reward for this idempotency key
					err = tx.QueryRowContext(ctx, `
						SELECT id, user_id, stock_symbol, quantity, idempotency_key, created_at
						FROM rewards
						WHERE idempotency_key = $1
					`, req.IdempotencyKey).Scan(
						&reward.ID,
						&reward.User_ID,
						&reward.Stock_Symbol,
						&reward.Quantity,
						&reward.IdempotencyKey,
						&reward.CreatedAt,
					)
					if err != nil {
						logger.WithError(err).Error("Failed to fetch existing reward")
						response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
						return
					}

					if reward.User_ID != req.UserID || reward.Stock_Symbol != req.StockSymbol || reward.Quantity != req.Quantity {
						response.WriteJson(c.Writer, http.StatusConflict, response.ErrorResponse("idempotency key already used with different payload"))
						return
					}

					if err := tx.Commit(); err != nil {
						logger.WithError(err).Error("Failed to commit transaction")
						response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
						return
					}
					committed = true
					response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
						"message":           "Reward already created (idempotent retry)",
						"rewardId":          reward.ID,
						"idempotent_replay": true,
					})
					return
				}
				response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("reward already given today"))
				return
			}
		}
		logger.WithError(err).Error("Failed to insert reward")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	// Calculate financials
	absQty := math.Abs(req.Quantity)
	shareValue := utils.RoundAmount(currentPrice * absQty)
	isReversal := req.Quantity < 0

	// Cash flow: negative = outflow (normal reward), positive = inflow (reversal)
	cashFlow := -shareValue
	if isReversal {
		cashFlow = shareValue
	}

	brokerage := 0.0
	stt := 0.0
	gst := 0.0
	if !isReversal {
		brokerage = utils.RoundAmount(shareValue * 0.005)
		stt = utils.RoundAmount(shareValue * 0.001)
		gst = utils.RoundAmount((brokerage + stt) * 0.18)
	}

	totalFees := utils.RoundAmount(brokerage + stt + gst)

	// Prepare ledger entries
	ledgerEntries := []models.Ledger{
		{
			Reward_ID:    reward.ID,
			Entry_Type:   models.StockUnits,
			Stock_Symbol: req.StockSymbol,
			Quantity:     req.Quantity,
			Amount:       0,
		},
		{
			Reward_ID:    reward.ID,
			Entry_Type:   models.INROutflow,
			Stock_Symbol: "",
			Quantity:     0,
			Amount:       cashFlow,
		},
	}
	if !isReversal {
		ledgerEntries = append(ledgerEntries,
			models.Ledger{
				Reward_ID:  reward.ID,
				Entry_Type: models.BrokerageFee,
				Amount:     -brokerage},
			models.Ledger{
				Reward_ID:  reward.ID,
				Entry_Type: models.STTFee,
				Amount:     -stt},
			models.Ledger{
				Reward_ID:  reward.ID,
				Entry_Type: models.GSTFee,
				Amount:     -gst},
		)
	}

	// Insert all ledger entries
	for _, entry := range ledgerEntries {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ledger (reward_id, entry_type, stock_symbol, quantity, amount, created_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
		`,
			entry.Reward_ID,
			entry.Entry_Type,
			entry.Stock_Symbol,
			utils.RoundQuantity(entry.Quantity),
			utils.RoundAmount(entry.Amount)); err != nil {
			logger.WithError(err).Error("Failed to insert ledger entry")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
			return
		}
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		logger.WithError(err).Error("Failed to commit transaction")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	committed = true

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"message":    "Reward created successfully",
		"rewardId":   reward.ID,
		"amount_inr": shareValue,
		"fees": map[string]interface{}{
			"brokerage": brokerage,
			"stt":       stt,
			"gst":       gst,
			"total":     totalFees,
		},
		"is_reversal": isReversal,
	})
}
