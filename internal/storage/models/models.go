package models

import (
	"time"

	"github.com/shopspring/decimal"
)

type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	// Password  string `json:"password"`
	CreatedAt string `json:"created_at"`
}

type Stock_Prices struct {
	ID           int             `json:"id"`
	Stock_Symbol string          `json:"stock_symbol"`
	Price        decimal.Decimal `json:"price"`
	Fetched_At   string          `json:"fetched_at"`
}

type Reward struct {
	ID             int             `json:"id"`
	User_ID        int             `json:"user_id"`
	Stock_Symbol   string          `json:"stock_symbol"`
	Quantity       decimal.Decimal `json:"quantity"`
	IdempotencyKey string          `json:"idempotency_key"`
	CreatedAt      string          `json:"created_at"`
}

type Stock_Events struct {
	ID            int    `json:"id"`
	Stock_Symbol  string `json:"stock_symbol"`
	EventType     string `json:"event_type"`
	RatioNum      int    `json:"ratio_num"`
	RatioDen      int    `json:"ratio_den"`
	EffectiveDate string `json:"effective_date"`
	CreatedAt     string `json:"created_at"`
}

const (
	StockUnits   = "stock_units"
	INROutflow   = "inr_outflow"
	BrokerageFee = "brokerage_fee"
	STTFee       = "stt_fee"
	GSTFee       = "gst_fee"
)

type Ledger struct {
	ID           int             `json:"id"`
	Reward_ID    int             `json:"reward_id"`
	Entry_Type   string          `json:"entry_type"`
	Stock_Symbol string          `json:"stock_symbol"`
	Quantity     decimal.Decimal `json:"quantity"`
	Amount       decimal.Decimal `json:"amount"`
	CreatedAt    string          `json:"created_at"`
}

const (
	Reward_Reversal   = "reward_reversal"
	Fee_Refund        = "fee_refund"
	Manual_Correction = "manual_correction"
)

type Adjustment struct {
	ID             int             `json:"id"`
	RewardID       int             `json:"reward_id"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	AdjustmentType string          `json:"adjustment_type"`
	DeltaQuantity  decimal.Decimal `json:"delta_quantity"`
	DeltaAmount    decimal.Decimal `json:"delta_amount"`
	Reason         string          `json:"reason"`
	EffectiveDate  string          `json:"effective_date,omitempty"`
	CreatedAt      string          `json:"created_at"`
}

type HistoricalINR struct {
	RewardDate            string              `json:"rewardDate"`
	RewardEventID         int                 `json:"rewardEventId"`
	StockSymbol           string              `json:"stockSymbol"`
	AdjustedQuantity      decimal.Decimal     `json:"adjustedQuantity"`
	Price                 decimal.NullDecimal `json:"price"`
	TotalAdjustmentAmount decimal.Decimal     `json:"totalAdjustmentAmount"`
	INRValue              decimal.NullDecimal `json:"inrValue"`
	PriceStatus           string              `json:"priceStatus"`
	PriceAsOf             *time.Time          `json:"priceAsOf"`
	OutcomeType           string              `json:"outcomeType"`
	IsActive              bool                `json:"isActive"`
	DelistEffectiveDate   *string             `json:"delistEffectiveDate"`
	CashProceeds          decimal.Decimal     `json:"cashProceeds"`
	WrittenOffQuantity    decimal.Decimal     `json:"writtenOffQuantity"`
}

type TodayReward struct {
	StockSymbol   string          `json:"stockSymbol"`
	TotalQuantity decimal.Decimal `json:"totalQuantity"`
}

type PortfolioItem struct {
	StockSymbol  string              `json:"stockSymbol"`
	Quantity     decimal.Decimal     `json:"quantity"`
	CurrentPrice decimal.NullDecimal `json:"currentPrice"`
	INRValue     decimal.NullDecimal `json:"inrValue"`
	PriceStatus  string              `json:"priceStatus"`
	PriceAsOf    *time.Time          `json:"priceAsOf"`
}

type TodayStock struct {
	RewardID              int64               `json:"rewardId"`
	StockSymbol           string              `json:"stockSymbol"`
	AdjustedQuantity      decimal.Decimal     `json:"adjustedQuantity"`
	CurrentPrice          decimal.NullDecimal `json:"currentPrice"`
	TotalAdjustmentAmount decimal.Decimal     `json:"totalAdjustmentAmount"`
	INRValue              decimal.NullDecimal `json:"inrValue"`
	PriceStatus           string              `json:"priceStatus"`
	PriceAsOf             *time.Time          `json:"priceAsOf"`
}

type CreateRewardRequest struct {
	UserID         int             `json:"user_id"`
	StockSymbol    string          `json:"stock_symbol"`
	Quantity       decimal.Decimal `json:"quantity"`
	IdempotencyKey string          `json:"idempotency_key"`
}
