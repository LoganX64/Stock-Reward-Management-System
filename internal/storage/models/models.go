package models

type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	// Password  string `json:"password"`
	CreatedAt string `json:"created_at"`
}

type Stock_Prices struct {
	ID           int     `json:"id"`
	Stock_Symbol string  `json:"stock_symbol"`
	Price        float64 `json:"price"`
	Fetched_At   string  `json:"fetched_at"`
}

type Reward struct {
	ID             int     `json:"id"`
	User_ID        int     `json:"user_id"`
	Stock_Symbol   string  `json:"stock_symbol"`
	Quantity       float64 `json:"quantity"`
	IdempotencyKey string  `json:"idempotency_key"`
	CreatedAt      string  `json:"created_at"`
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
	ID           int     `json:"id"`
	Reward_ID    int     `json:"reward_id"`
	Entry_Type   string  `json:"entry_type"`
	Stock_Symbol string  `json:"stock_symbol"`
	Quantity     float64 `json:"quantity"`
	Amount       float64 `json:"amount"`
	CreatedAt    string  `json:"created_at"`
}

const (
	Reward_Reversal   = "reward_reversal"
	Fee_Refund        = "fee_refund"
	Manual_Correction = "manual_correction"
)

type Adjustment struct {
	ID             int     `json:"id"`
	RewardID       int     `json:"reward_id"`
	AdjustmentType string  `json:"adjustment_type"`
	DeltaQuantity  float64 `json:"delta_quantity"`
	DeltaAmount    float64 `json:"delta_amount"`
	Reason         string  `json:"reason"`
	CreatedAt      string  `json:"created_at"`
}

type HistoricalINR struct {
	RewardDate            string  `json:"rewardDate"`
	RewardEventID         int     `json:"rewardEventId"`
	StockSymbol           string  `json:"stockSymbol"`
	AdjustedQuantity      float64 `json:"adjustedQuantity"`
	Price                 float64 `json:"price"`
	TotalAdjustmentAmount float64 `json:"totalAdjustmentAmount"`
	INRValue              float64 `json:"inrValue"`
}

type TodayReward struct {
	StockSymbol   string  `json:"stockSymbol"`
	TotalQuantity float64 `json:"totalQuantity"`
}

type PortfolioItem struct {
	StockSymbol  string  `json:"stockSymbol"`
	Quantity     float64 `json:"quantity"`
	CurrentPrice float64 `json:"currentPrice"`
	INRValue     float64 `json:"inrValue"`
}

type TodayStock struct {
	RewardID              int64   `json:"rewardId"`
	StockSymbol           string  `json:"stockSymbol"`
	AdjustedQuantity      float64 `json:"adjustedQuantity"`
	CurrentPrice          float64 `json:"currentPrice"`
	TotalAdjustmentAmount float64 `json:"totalAdjustmentAmount"`
	INRValue              float64 `json:"inrValue"`
}

type CreateRewardRequest struct {
	UserID      int     `json:"user_id"`
	StockSymbol string  `json:"stock_symbol"`
	Quantity    float64 `json:"quantity"`
}
