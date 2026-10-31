package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/LoganX64/stocky-api/internal/marketdata"
)

// PriceStore persists validated quotes. Current price and daily history are
// written in one transaction so a quote is never half-recorded.
type PriceStore interface {
	SaveQuote(ctx context.Context, quote marketdata.Quote) (SaveResult, error)
}

// SaveResult reports whether the current price row and the history row were
// changed. A false value means the existing row won the precedence rules, for
// example because the incoming quote was older or because bhavcopy already owns
// the trading date.
type SaveResult struct {
	CurrentAccepted bool
	HistoryChanged  bool
}

// PriceStoreOptions configures the store. Now is injectable for deterministic
// quote validation in tests.
type PriceStoreOptions struct {
	Now func() time.Time
}

// SQLPriceStore is the PostgreSQL implementation of PriceStore.
type SQLPriceStore struct {
	db  *sql.DB
	now func() time.Time
}

var _ PriceStore = (*SQLPriceStore)(nil)

// NewPriceStore creates a price store. The caller owns db.
func NewPriceStore(db *sql.DB, opts *PriceStoreOptions) *SQLPriceStore {
	now := time.Now
	if opts != nil && opts.Now != nil {
		now = opts.Now
	}
	return &SQLPriceStore{db: db, now: now}
}

// upsertCurrentPriceSQL applies the A3 precedence rules to the single current
// price row per symbol. A non-bhavcopy quote cannot replace a bhavcopy row for
// the same market date, while bhavcopy may replace a slightly later Yahoo
// timestamp on that date. A quote from a later market date can still take over.
const upsertCurrentPriceSQL = `
INSERT INTO stock_prices
    (stock_symbol, price, updated_at, source, fetched_at,
     provider_symbol, exchange, currency)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'INR')
ON CONFLICT (stock_symbol) DO UPDATE SET
    price           = EXCLUDED.price,
    updated_at      = EXCLUDED.updated_at,
    source          = EXCLUDED.source,
    fetched_at      = EXCLUDED.fetched_at,
    provider_symbol = EXCLUDED.provider_symbol,
    exchange        = EXCLUDED.exchange,
    currency        = EXCLUDED.currency
WHERE
    (
        stock_prices.source IN ('legacy', 'seed')
        OR EXCLUDED.updated_at > stock_prices.updated_at
        OR (
            EXCLUDED.updated_at = stock_prices.updated_at
            AND market_data_source_rank(EXCLUDED.source)
                > market_data_source_rank(stock_prices.source)
        )
        OR (
            EXCLUDED.source = 'nse_bhavcopy'
            AND market_date(stock_prices.updated_at) = market_date(EXCLUDED.updated_at)
        )
    )
    AND NOT (
        EXCLUDED.source <> 'nse_bhavcopy'
        AND stock_prices.source = 'nse_bhavcopy'
        AND market_date(stock_prices.updated_at) = market_date(EXCLUDED.updated_at)
    )
`

// upsertHistorySQL keeps one history row per symbol and market date. Bhavcopy
// wins its date. For other sources a newer quote timestamp wins, with source
// rank breaking equal-timestamp ties. The history date comes from the quote
// time via market_date(), never from market_today(), so a late fetch cannot
// move a quote into the wrong market day.
const upsertHistorySQL = `
INSERT INTO stock_price_history
    (stock_symbol, price, date, created_at, source, fetched_at,
     provider_symbol, exchange, currency)
VALUES ($1, $2, market_date($3::TIMESTAMPTZ), $4, $5, $6, $7, $8, 'INR')
ON CONFLICT (stock_symbol, date) DO UPDATE SET
    price           = EXCLUDED.price,
    created_at      = EXCLUDED.created_at,
    source          = EXCLUDED.source,
    fetched_at      = EXCLUDED.fetched_at,
    provider_symbol = EXCLUDED.provider_symbol,
    exchange        = EXCLUDED.exchange,
    currency        = EXCLUDED.currency
WHERE
    (EXCLUDED.source = 'nse_bhavcopy'
     AND stock_price_history.source <> 'nse_bhavcopy')
    OR stock_price_history.source IN ('legacy', 'seed')
    OR (
        stock_price_history.source <> 'nse_bhavcopy'
        AND (
            EXCLUDED.created_at > stock_price_history.created_at
            OR (
                EXCLUDED.created_at = stock_price_history.created_at
                AND market_data_source_rank(EXCLUDED.source)
                    > market_data_source_rank(stock_price_history.source)
            )
        )
    )
`

// SaveQuote validates quote, then upserts the current price and the daily
// history row in one transaction. Both writes commit together or neither does.
func (s *SQLPriceStore) SaveQuote(ctx context.Context, quote marketdata.Quote) (SaveResult, error) {
	if err := quote.Validate(s.now()); err != nil {
		return SaveResult{}, fmt.Errorf("validate quote for %s: %w", quote.Symbol, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SaveResult{}, fmt.Errorf("begin price transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	currentResult, err := tx.ExecContext(
		ctx,
		upsertCurrentPriceSQL,
		quote.Symbol,
		quote.Price,
		quote.AsOf,
		quote.Source,
		quote.FetchedAt,
		quote.ProviderSymbol,
		quote.Exchange,
	)
	if err != nil {
		return SaveResult{}, fmt.Errorf("upsert current price for %s: %w", quote.Symbol, err)
	}

	currentAccepted, err := rowsChanged(currentResult)
	if err != nil {
		return SaveResult{}, fmt.Errorf("read current price result for %s: %w", quote.Symbol, err)
	}

	historyResult, err := tx.ExecContext(
		ctx,
		upsertHistorySQL,
		quote.Symbol,
		quote.Price,
		quote.AsOf,
		quote.AsOf,
		quote.Source,
		quote.FetchedAt,
		quote.ProviderSymbol,
		quote.Exchange,
	)
	if err != nil {
		return SaveResult{}, fmt.Errorf("upsert price history for %s: %w", quote.Symbol, err)
	}

	historyChanged, err := rowsChanged(historyResult)
	if err != nil {
		return SaveResult{}, fmt.Errorf("read price history result for %s: %w", quote.Symbol, err)
	}

	if err := tx.Commit(); err != nil {
		return SaveResult{}, fmt.Errorf("commit price transaction for %s: %w", quote.Symbol, err)
	}

	return SaveResult{
		CurrentAccepted: currentAccepted,
		HistoryChanged:  historyChanged,
	}, nil
}

func rowsChanged(result sql.Result) (bool, error) {
	if result == nil {
		return false, nil
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}
