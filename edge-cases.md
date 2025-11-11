# Stocky Rewards API - Edge Case Handling & Scaling

## 1. Duplicate Reward Events / Replay Attacks

- **Problem:** Users could accidentally receive the same reward multiple times.
- **Solution:**
  - Each reward has an **`idempotency_key`** generated at insertion.
  - Unique constraint: `(user_id, stock_symbol, reward_date)` prevents multiple rewards for the same stock on the same day.
  - API checks `pq.Error.Code == "23505"` to catch duplicates and return a proper error.

## 2. Stock Splits, Mergers, and Delisting

- **Problem:** Corporate actions change stock quantities and symbols, affecting reward valuation.
- **Solution:**
  - `stock_events` table tracks splits, bonus issues, mergers, and delists.
  - Views (`historical_rewards`, `today_rewards`, `user_portfolio`) apply cumulative multipliers for splits/bonus and adjust quantities for mergers.
  - Delisted stocks are automatically excluded from portfolio and today’s rewards.

## 3. Rounding Errors in INR Valuation

- **Problem:** Floating point calculations can introduce minor discrepancies.
- **Solution:**
  - Utility functions `RoundQuantity` and `RoundAmount` ensure quantities are rounded to 6 decimal places and amounts to 4 decimal places consistently.
  - All ledger entries and portfolio calculations use these rounding functions.

## 4. Price API Downtime or Stale Data

- **Problem:** Stock prices may be unavailable or outdated.
- **Solution:**
  - `stock_prices` table caches the latest stock prices.
  - `stock_price_history` maintains historical prices for reference.
  - APIs fetch from this database rather than live API.
  - If price is missing, API returns a `400` or appropriate error message.

## 5. Adjustments / Refunds of Previously Given Rewards

- **Problem:** Rewards may need manual correction or refunds.
- **Solution:**
  - `adjustments` table tracks all manual changes with `delta_quantity` and `delta_amount`.
  - Ledger entries are automatically created for each adjustment.
  - Transactional handling ensures atomic updates:
    - A **single transaction** updates rewards, adjustments, and ledger entries.
    - Rollback flag ensures the transaction is rolled back if any insert fails.

## Scaling Considerations

- **Database Indexes:**
  - Indexed columns: `user_id`, `reward_id`, `stock_symbol`.
- **Views for Computation:**
  - Heavy calculations (multipliers, cumulative adjustments) are done in database views to avoid repeated computation in API layer.
- **Asynchronous Jobs:**
  - Daily price updates and reward computations can run in background jobs (`jobs.StartPriceUpdater`) without blocking API requests.
- **Pagination:**
  - API endpoints returning historical rewards or portfolio items support pagination for large datasets.

## Summary

This system ensures:

1. Idempotency and prevention of duplicate rewards.
2. Accurate handling of corporate actions.
3. Precision in quantity and INR calculations.
4. Reliability even when external price data is unavailable.
5. Auditability and traceability via ledger and adjustment records.
6. Scalable architecture using DB-level computation, indexes, and background jobs.
