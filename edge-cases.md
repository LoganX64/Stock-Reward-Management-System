# Stocky Rewards API - Edge Cases and Scaling

## 1. Duplicate Reward Events and Replay Attacks

- **Problem:** A client may retry a request or accidentally send the same reward more than once.
- **Handling:**
  - `idempotency_key` is optional and client-provided.
  - Empty idempotency keys are stored as SQL `NULL`.
  - Rewards are constrained by `(user_id, stock_symbol, reward_date)` to prevent multiple same-day rewards for the same user and stock. “Same day” means the market date in `Asia/Kolkata`, not the database session date.
  - A case-insensitive unique index also protects `(user_id, UPPER(stock_symbol), reward_date)`.
  - If a duplicate insert occurs with a matching `idempotency_key`, the API fetches the existing reward and returns an idempotent replay response.
  - If the same `idempotency_key` is reused with a different payload, the API returns a conflict.

## 2. Stock Splits, Mergers, and Delisting

- **Problem:** Corporate actions can change stock quantities, symbols, and portfolio visibility.
- **Handling:**
  - `stock_events` tracks splits, bonus issues, mergers, and delisting events with normalized symbols, explicit market-date effective dates, and ratios. All event types become effective at 00:00 IST and use the same inclusive date boundary.
  - Prices stay raw in `stock_prices` and `stock_price_history`; `stock_prices_adjusted` and `stock_price_history_adjusted` apply split/bonus factors once at read time so raw values are never scaled twice.
  - A 2:1 split or 1:1 bonus is stored as `2/1`; a 1:5 reverse split is stored as `1/5`.
  - `instrument_lineage` exposes normalized merger edges. A merger chain such as A→B→C is resolved recursively, composing ratios and effective dates.
  - Split/bonus events on B or C apply only while the holding is in that instrument. An event after the holding has moved to the next successor is ignored.
  - Merger events require a distinct successor and positive ratios. A trigger rejects lineage cycles, and each predecessor may have only one merge event.
  - Corporate-action symbols are trimmed and uppercased before storage. A unique index on normalized symbol, event type, and effective date rejects case, whitespace, and same-day duplicate events.
  - Ratios are validated: split ratios are positive and not 1:1; bonus ratios are greater than 1:1; merger ratios are positive; delist ratios are normalized to 1:1.
  - Event-type fields are constrained: split/bonus events cannot carry merger/delist fields; mergers require a non-blank successor and no delist outcome; delists require an outcome and no successor.
  - There is no corporate-action write API yet; validation is enforced by database constraints and triggers for every write path.
  - `reward_quantity_movements` records the reward's original quantity and each effective-dated adjustment. Only corporate actions after each movement's date are applied, so a post-split or post-merger correction is not multiplied by prior events.
  - `user_portfolio` and `today_rewards` use movement-derived current balances, the terminal successor symbol, adjusted prices, and `is_active` from the shared outcome view. Handlers must not add their own delist predicates.
  - `historical_rewards_as_of(as_of)` keeps every historical row. The default grant-date value is never changed by a later delist. With `?asOf=YYYY-MM-DD`, effective quantity, terminal symbol, adjustments, delist status, and price are all evaluated on that date.
  - A terminal-delisted lot has zero effective quantity in as-of history and `priceStatus: "not_required"`; its `cashProceeds` or `writtenOffQuantity` is still returned. `not_required` is a closed position, not a missing-price gap.
  - Delist filtering resolves the terminal symbol first. A delist on a predecessor after a merger does not remove the successor holding; a delist on the terminal instrument does.
  - A merger may include `cash_per_unit` cash consideration per predecessor share. Cash is calculated when the holding crosses that merger edge and is included in `cashProceeds`.
  - A terminal delist must set `delist_outcome`:
    - `cash_exit` requires positive `cash_per_unit`, removes the holding from the portfolio, and reports `quantity × cash_per_unit` as cash proceeds.
    - `write_off` requires no cash, removes the holding, and reports its quantity as `writtenOffQuantity`.
  - A cash-only acquisition is a `delist` with `cash_exit` and no successor symbol.
  - Legacy delist events are backfilled to `write_off`, matching the previous behavior of removing the holding without cash.
  - Fractional quantities are supported to 6 decimal places and cash to 4 decimal places. `stock_events.fraction_policy` is per event:
    - `retain` (default) keeps the fractional post-event quantity at 6 decimal places and pays no cash.
    - `cash_in_lieu` floors the post-event quantity to whole units and pays the removed fraction at the event's positive `fraction_cash_per_unit`, rounded to 4 decimal places. The price is per post-event (successor/current-instrument) share. A reverse split of 3 shares at 1:5 therefore settles at 0 shares plus 0.6 units of cash; a 10-share 42:25 merger settles at 16 successor shares plus 0.8 units of cash.
  - Fraction settlement is applied per reward lot, not to an aggregated user position. This is a documented limitation of the reward-scoped ledger.
  - Merger `cash_per_unit` proceeds use the pre-merger quantity and are not reduced by cash-in-lieu flooring. A delist cannot use `cash_in_lieu`; a cash-only exit is a `cash_exit` delist.
  - Same-date actions are applied in the order split → bonus → merger → adjustment.
  - Corporate-action cash is posted to `ledger` as positive `inr_outflow` rows tagged with `stock_event_id`; the unique key `(reward_id, stock_event_id)` prevents duplicates. A `stock_events` trigger posts immediately, and `CorporateActionService` processes future-dated events at startup and daily.
  - `reward_corporate_action_outcomes` is the shared per-reward outcome view used by `today_rewards` and `user_portfolio`, so `/portfolio` and `/stats` report the same `cashProceeds`, `cashInLieuProceeds`, and `writtenOffQuantity`. `totalPortfolioValue` remains stock-only.
  - `/portfolio` and `/stats` read inside one read-only `REPEATABLE READ` transaction. `totalPortfolioValue`, `totalPositions`, and `valuationDate` are full-set aggregates independent of pagination and describe the same snapshot as the returned page.
  - Reconciliation keeps corporate-action cash separate from adjustment cash and compares derived cash with tagged ledger postings through `corporate_action_cash_matches`.
  - Validation rejects merges without successors, negative cash, cash exits without cash, write-offs with cash, delists before mergers, duplicate delists, blank symbols, non-positive ratios, no-op 1:1 splits, non-forward bonuses, non-1:1 delists, and case/whitespace/same-day duplicate events.

## 3. Missing Current and Historical Prices

### Current prices

- **Problem:** A reward or holding can lose its current price when `stock_prices` has no row for its terminal symbol, for example after a merger into a newly listed successor.
- **Handling:**
  - `today_rewards` and `user_portfolio` use a `LEFT JOIN` to `stock_prices_adjusted`.
  - An unpriced reward or holding remains visible with `price_status = 'missing'`, a null `current_price`, and a null `inr_value`.
  - The views expose `price_as_of` from `stock_prices.updated_at` for priced rows.
  - `/today-stocks` returns `valuationComplete`, `unpricedRewards`, and `unpricedSymbols`.
  - `/portfolio` and `/stats` return `valuationComplete`, `unpricedPositions`, and `unpricedSymbols`.
  - `totalPortfolioValue` in `/stats` sums only priced holdings. Clients must check `valuationComplete` before treating it as a complete value.
  - Staleness classification is not part of this behavior; `price_as_of` is returned so clients can evaluate freshness.

### Historical prices

- **Problem:** Historical INR values depend on `stock_price_history`, and a missing row used to hide the reward entirely.
- **Handling:**
  - `historical_rewards_as_of(as_of)` left-joins each reward to `stock_price_history_adjusted` using the reward date by default, or the requested `asOf` date.
  - A reward with no history row remains visible with `priceStatus: "missing"`, `price: null`, `inrValue: null`, and `priceAsOf: null`.
  - A zero effective quantity (terminal delist or full reversal) reports `priceStatus: "not_required"` with `inrValue: 0`; it is not counted as a missing-price gap.
  - `/historical-inr` returns `valuationComplete`, `unpricedRewards`, and `unpricedSymbols` for the user's full set at the same grant-date or `asOf` reference so clients can detect incomplete totals.
  - The background price service inserts or updates a daily history row on every cycle, including when the price is unchanged. The row's `date` is the market date in `Asia/Kolkata`.
  - Missing dates can be seeded through `SELECT backfill_price_history('YYYY-MM-DD');`, which inserts missing rows from the latest `stock_prices` values.
  - Real historical prices should be imported from an authoritative source; the backfill function is a local/demo convenience and does not fabricate multiple days automatically.

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
  - Successful fetch cycles update `stock_prices` when the price changed and always upsert today's `stock_price_history` row, including unchanged prices.

## 6. Adjustments and Refunds

- **Problem:** Existing rewards may need reversals, fee refunds, or manual corrections.
- **Handling:**
  - `adjustments` stores `delta_quantity`, `delta_amount`, adjustment type, reason, and an `effective_date`.
  - The adjustment handler locks the reward row with `FOR UPDATE`.
  - Deltas are denominated in units on the effective date. They are normalized to the reward-date-equivalent balance before the non-negative check.
  - It rejects changes that would make the reward quantity negative.
  - It updates the normalized reward balance and inserts adjustment ledger entries in a single transaction.

## 7. Business Dates and Timezones

- **Problem:** Timestamp/date comparisons are ambiguous when the database session timezone differs from the market timezone, especially around midnight.
- **Handling:**
  - The market timezone is fixed at `Asia/Kolkata` (IST, UTC+05:30, no daylight saving). It is exposed by `market_timezone()` and is not configurable per connection.
  - `market_today()` is the current IST calendar date and replaces `CURRENT_DATE` in business logic. `market_date(timestamptz)` converts an absolute instant to its IST date.
  - `rewards.reward_date` is an explicit `DATE` derived from `rewards.created_at` by a trigger. Legacy reward dates were preserved when the former generated column was converted.
  - `stock_events.effective_date`, `adjustments.effective_date`, and `stock_price_history.date` are market `DATE`s. A split, bonus, merger, or delist on a date is effective for that entire date.
  - Audit timestamps such as `created_at` and `updated_at` are `TIMESTAMPTZ`. API responses normalize `priceAsOf` and adjustment `created_at` values to UTC.
  - `/historical-inr?asOf=` rejects dates after `market_today()` inside the same read snapshot, so the application's local timezone cannot change the boundary.
  - Migration `0022_business_date_timezone_semantics` interprets legacy `TIMESTAMP` values as IST wall-clock time. The down migration cannot restore sub-day precision for event dates, which are already normalized to `DATE`.
  - `internal/database/timezone_integration_test.go` exercises midnight reward boundaries, same-day uniqueness, split/delist inclusivity, price-history dates, `asOf` valuation, corporate-action cash, UTC timestamp normalization, and the legacy migration path under four session timezones.

## 8. Pagination

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
  - Split/bonus and quantity factors use logarithmic product aggregation with `EXP(SUM(LN(ratio)))`.
  - Effective-dated quantity movements let reporting apply only corporate actions that occur after each movement.
- **Background Jobs:**
  - `PriceService` uses a worker pool for concurrent price updates.
  - A buffered-channel semaphore limits concurrent database writes.
  - Database writes have retry logic.
  - Workers recover from panics and respect context cancellation.

## Summary

This system handles:

1. Duplicate rewards and idempotent retries.
2. Stock events and delisting behavior.
3. Missing historical price data as an explicit reporting status with supported backfill.
4. Consistent rounding for quantities and INR values.
5. Simulated price update failures.
6. Transactional reward adjustments.
7. Explicit market-date and timezone semantics for rewards, corporate actions, price history, and historical snapshots.
8. Paginated historical and portfolio reads.
