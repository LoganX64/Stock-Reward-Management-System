# Stocky Rewards API - Edge Cases and Scaling

## 1. Duplicate Reward Events and Replay Attacks

- **Problem:** A client may retry a request or accidentally send the same reward more than once.
- **Handling:**
  - `idempotency_key` is optional and client-provided.
  - Empty idempotency keys are stored as SQL `NULL`.
  - Rewards are constrained by `(user_id, stock_symbol, reward_date)` to prevent multiple same-day rewards for the same user and stock.
  - A case-insensitive unique index also protects `(user_id, UPPER(stock_symbol), reward_date)`.
  - If a duplicate insert occurs with a matching `idempotency_key`, the API fetches the existing reward and returns an idempotent replay response.
  - If the same `idempotency_key` is reused with a different payload, the API returns a conflict.

## 2. Stock Splits, Mergers, and Delisting

- **Problem:** Corporate actions can change stock quantities, symbols, and portfolio visibility.
- **Handling:**
  - `stock_events` tracks splits, bonus issues, mergers, and delisting events.
  - `historical_rewards` and `user_portfolio` apply cumulative multipliers for split/bonus/merge-style events.
  - `user_portfolio` excludes delisted stocks whose delisting effective date is on or before the current date.
  - `today_rewards` currently shows today's rewards from the reward and price tables and does not independently filter delisted stocks.

## 3. Missing Historical Prices

- **Problem:** Historical INR values depend on `stock_price_history`.
- **Handling:**
  - `historical_rewards` joins rewards to `stock_price_history` by reward date.
  - If there is no history row for a reward's date and symbol, that reward will not appear in historical results.
  - The background price service inserts or updates daily history rows when it updates prices.
  - Deployments should ensure daily history exists for dates that need historical reporting.

## 4. Rounding Errors in INR Valuation

- **Problem:** Floating-point calculations can introduce small precision differences.
- **Handling:**
  - `RoundQuantity` rounds quantities to 6 decimal places.
  - `RoundAmount` rounds INR amounts to 4 decimal places.
  - Handlers round quantities, prices, INR values, and ledger amounts before returning or inserting values.

## 5. Simulated Price Updates and Stale Data

- **Problem:** Stock prices can become stale, and the current implementation does not call a real external market-data API.
- **Handling:**
  - `stock_prices` stores the latest known price.
  - `stock_price_history` stores daily prices for historical reporting.
  - `PriceService.Start` runs a background update loop.
  - `RandomPriceFetcher` generates simulated price changes from the last stored price.
  - When `RandomPriceFetcher` simulates a fetch failure, it returns a dampened fallback price based on the last stored price.
  - Successful updates are written to `stock_prices` and `stock_price_history`.

## 6. Adjustments and Refunds

- **Problem:** Existing rewards may need reversals, fee refunds, or manual corrections.
- **Handling:**
  - `adjustments` stores `delta_quantity`, `delta_amount`, adjustment type, and reason.
  - The adjustment handler locks the reward row with `FOR UPDATE`.
  - It rejects changes that would make the reward quantity negative.
  - It updates the reward quantity and inserts adjustment ledger entries in a single transaction.

## 7. Pagination

- **Problem:** Historical and portfolio endpoints can grow large over time.
- **Handling:**
  - `GET /api/v1/historical-inr/:userId` supports `limit` and `offset`.
  - `GET /api/v1/portfolio/:userId` supports `limit` and `offset`.
  - `limit` defaults to `100` and is capped at `500`.
  - `offset` defaults to `0`.
  - Historical results use stable ordering: `reward_date DESC, reward_event_id DESC`.
  - Portfolio results are ordered by `stock_symbol`.

## Scaling Considerations

- **Database Indexes:**
  - Rewards are indexed by user and reward date.
  - Adjustments and ledger entries are indexed by reward ID.
  - Stock prices are indexed by stock symbol.
- **Views for Computation:**
  - Portfolio and historical calculations are centralized in database views.
  - Split and bonus multipliers use logarithmic product aggregation with `EXP(SUM(LN(ratio)))`.
- **Background Jobs:**
  - `PriceService` uses a worker pool for concurrent price updates.
  - A buffered-channel semaphore limits concurrent database writes.
  - Database writes have retry logic.
  - Workers recover from panics and respect context cancellation.

## Summary

This system handles:

1. Duplicate rewards and idempotent retries.
2. Stock events and delisting behavior.
3. Missing historical price data as an explicit reporting dependency.
4. Consistent rounding for quantities and INR values.
5. Simulated price update failures.
6. Transactional reward adjustments.
7. Paginated historical and portfolio reads.
