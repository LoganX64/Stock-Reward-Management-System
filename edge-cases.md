# Stocky Rewards API - Edge Cases and Scaling

## 1. Duplicate Reward Events and Replay Attacks

- **Problem:** A client may retry a request or accidentally send the same reward more than once.
- **Handling:**
  - `idempotency_key` is optional and client-provided.
  - Empty idempotency keys are stored as SQL `NULL`.
  - Rewards are constrained by `(user_id, stock_symbol, reward_date)` to prevent multiple same-day rewards for the same user and stock. “Same day” means the market date in `Asia/Kolkata`, not the database session date.
  - A normalized unique index protects `(user_id, normalize_instrument_symbol(stock_symbol), reward_date)`; symbols are trimmed and uppercased by a trigger and a canonical-value check constraint.
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
  - The same canonical representation is enforced across `stock_prices`, `stock_price_history`, `rewards`, and `ledger` by migration `0023_case_normalized_instruments`. See “Case normalization across instruments” below.
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

### Case normalization across instruments

- **Problem:** PostgreSQL string uniqueness is case-sensitive. Without a shared canonical form, `TCS`, `tcs`, and ` TCS ` can coexist in prices, price history, rewards, or ledger rows. Joins using `UPPER(...)` can then multiply valuation rows, and a reward price lookup can choose an arbitrary duplicate.
- **Handling:**
  - `normalize_instrument_symbol(text)` defines the canonical form as `UPPER(BTRIM(text, E' \t\n\r'))`; Go uses the matching `utils.NormalizeStockSymbol`.
  - `stock_prices`, `stock_price_history`, `rewards`, `ledger`, and `stock_events` store canonical symbols. `BEFORE INSERT OR UPDATE` triggers normalize writes, and `CHECK` constraints reject non-canonical stored values if a trigger is bypassed.
  - Normalized functional unique indexes enforce one current price per symbol, one history row per symbol and date, and one reward per user/symbol/market date. `stock_events` keeps its normalized unique index.
  - Valuation, lineage, settlement, and reconciliation joins use exact canonical equality. Reward creation uses an exact canonical price lookup with a deterministic `updated_at DESC, id DESC` tie-break.
  - Existing `stock_prices` and `stock_price_history` variants are resolved by keeping the row with the latest `updated_at` or `created_at` (then the canonical row, then the highest id). The migration aborts if `rewards` or `stock_events` contain normalized duplicates; it never deletes financial/audit rows, and migration `0024` prevents event deletion from cascading into ledger postings.
  - The migration normalizes data one-way. The down migration restores the previous schema behavior but does not restore original casing or whitespace.

## 3. Missing Current and Historical Prices

### Current prices

- **Problem:** A reward or holding can lose its current price when `stock_prices` has no row for its terminal symbol, for example after a merger into a newly listed successor.
- **Handling:**
  - `today_rewards` and `user_portfolio` use a `LEFT JOIN` to `stock_prices_adjusted`.
  - An unpriced reward or holding remains visible with `price_status = 'missing'`, a null `current_price`, and a null `inr_value`.
  - The views expose `price_as_of` from `stock_prices.updated_at` (the provider quote time), plus `price_source` and `price_fetched_at`.
  - `/today-stocks` returns `valuationComplete`, `unpricedRewards`, `unpricedSymbols`, `staleRewards`, `staleSymbols`, and `pricesFresh`.
  - `/portfolio` and `/stats` return `valuationComplete`, `unpricedPositions`, `unpricedSymbols`, `stalePositions`, `staleSymbols`, and `pricesFresh`.
  - `totalPortfolioValue` in `/stats` sums only priced holdings. Clients must check `valuationComplete` before treating it as a complete value.
  - Missing prices are not stale prices. A missing row has null `priceFreshness` and is reported through `valuationComplete`; staleness applies only to `priceStatus = 'available'` rows.
  - Stale counts cover the user's full set, not just the current page. `pricesFresh` is true only when every available price is `fresh`; an `unknown` price makes it false without being counted as stale.

### Historical prices

- **Problem:** Historical INR values depend on `stock_price_history`, and a missing row used to hide the reward entirely.
- **Handling:**
  - `historical_rewards_as_of(as_of)` left-joins each reward to `stock_price_history_adjusted` using the reward date by default, or the requested `asOf` date.
  - A reward with no history row remains visible with `priceStatus: "missing"`, `price: null`, `inrValue: null`, and `priceAsOf: null`.
  - A zero effective quantity (terminal delist or full reversal) reports `priceStatus: "not_required"` with `inrValue: 0`; it is not counted as a missing-price gap.
  - `/historical-inr` returns `valuationComplete`, `unpricedRewards`, and `unpricedSymbols` for the user's full set at the same grant-date or `asOf` reference so clients can detect incomplete totals.
  - Priced historical rows expose `priceSource` and `priceFetchedAt` in addition to `priceAsOf`. They do not expose `priceFreshness`, because a historical close is not expected to be current.
  - The live quote scheduler inserts or updates a history row using the **quote's market date**, not the cycle date, so a late fetch cannot move a close into the wrong day.
  - Missing dates are imported with the `backfill-market-data` command from official NSE bhavcopy files. See “Historical backfill” below.
  - `SELECT backfill_price_history('YYYY-MM-DD');` still exists as a local/demo helper that copies the latest current price into missing rows. It does not provide real historical closes and should not be used when market history is required.

## 4. Rounding Errors in INR Valuation

- **Problem:** Floating-point calculations can introduce small precision differences, especially at half-way rounding boundaries and when PostgreSQL `NUMERIC` values are converted to `float64`.
- **Handling:**
  - Quantities are stored to 6 decimal places; INR amounts and prices are stored to 4 decimal places.
  - The reward write path uses `shopspring/decimal` for request decoding, database reads, notional and fee calculations, ledger inserts, and JSON output. Database `NUMERIC` values are not converted through `float64` before storage.
  - `RoundQuantity` rounds quantities to 6 decimal places and `RoundAmount` rounds INR amounts to 4 decimal places. Both use half-away-from-zero rounding, matching PostgreSQL `ROUND(numeric, scale)`.
  - Fee components are rounded at each step: notional first, then brokerage and STT, then GST on the rounded brokerage + STT, then the fee total. See the worked examples in `readme.md`.
  - Reward responses and ledger rows use the same rounded values. The notional is posted as `-notional` in `inr_outflow`; fees are negative `brokerage_fee`, `stt_fee`, and `gst_fee` rows.
  - The fee schedule is a simplified buy-side model: 0.5% brokerage, 0.1% STT, and 18% GST on brokerage + STT. It excludes sell-side STT, exchange/SEBI charges, stamp duty, DP charges, minimum brokerage, and the full statutory GST base. The user is assumed to bear the fees.
  - A `reward_reversal` or `fee_refund` does not automatically recompute the original fee schedule. Explicit cash-only corrections use the adjustment API and do not change the holding's valuation.
  - The simulator provider is the only float-based path. It rounds generated prices to 4 decimal places before writing them, and it is explicitly a local/demo data source rather than financial accounting.

## 5. Real Market Data, Provider Failures, and Stale Prices

### Sources and delay

- **Problem:** Current quotes and official closes have different trust levels and timing, and none of the current-quote sources is real-time.
- **Handling:**
  - Yahoo Finance supplies current quotes and is the default provider. Yahoo NSE/BSE quotes are unofficial and delayed, roughly by 15 minutes plus polling lag. The API must not be presented as a real-time tick feed.
  - NSE bhavcopy supplies the official end-of-day close. It is published after the session and is authoritative for its market date.
  - `MARKET_DATA_PROVIDER=simulator` supplies synthetic random-walk prices for offline development and tests. Simulator prices are never market data.
  - Every stored quote records `source`, `provider_symbol`, `exchange`, `currency`, `fetched_at`, and its provider quote time (`stock_prices.updated_at` / `stock_price_history.created_at`). `price_as_of` in API responses is the provider quote time.
  - Current quotes refresh provenance even when the price value is unchanged; history is upserted for the quote's market date.

### Source precedence

- **Problem:** Yahoo and bhavcopy can return different values for the same trading date, and a delayed Yahoo poll can arrive after the official close.
- **Handling:**
  - `nse_bhavcopy` is authoritative for its market date. A Yahoo or simulator quote for that date cannot replace it.
  - Bhavcopy may replace a Yahoo or simulator row for the same date, even if Yahoo's quote timestamp is later.
  - A quote from a later market date can take over the current price.
  - Equal timestamps are resolved by source rank: `nse_bhavcopy > yahoo > simulator > legacy/seed`.
  - History is keyed by `(stock_symbol, market_date(quote_time))`. A rejected quote does not update the cache and is logged as `quote_ignored_older`.
  - Legacy and seed prices rank below every live source, so the first real quote can replace them.

### Provider failure behavior

- **Problem:** A network error, malformed payload, or provider outage must not overwrite a good price or invent a fallback.
- **Handling:**
  - A failed fetch writes nothing. The last known price, history row, and cache entry remain unchanged.
  - A quote is rejected when it has a blank/non-canonical symbol, blank provider symbol, non-positive price, wrong currency, wrong exchange, missing timestamps, a source outside `yahoo`/`nse_bhavcopy`/`simulator`, or a timestamp more than five minutes in the future.
  - A live quote older than seven days is rejected; historical backfill uses a separate validation path that skips only the maximum-age rule and still rejects future timestamps.
  - A provider quote for a different symbol than requested is discarded.
  - The cache is updated only after a successful database save, so a failed save cannot make the API serve a price that was never persisted.
  - HTTP retries use capped exponential backoff with jitter and honor `Retry-After`. Only `408`, `429`, and `5xx` are retried; other `4xx` responses fail immediately. Full response bodies are not logged.
  - A failed cycle writes nothing; the stored price remains available and is classified as `fresh`, `stale`, or `unknown` by the normal freshness rules as time passes.

### Calendar and scheduling

- **Problem:** Polling outside NSE trading hours wastes requests, and weekend/holiday closes must not be reported as stale.
- **Handling:**
  - The market calendar uses `Asia/Kolkata`, weekdays, configured `MARKET_HOLIDAYS`, and the 09:15–15:30 session.
  - With `MARKET_DATA_MARKET_HOURS_ONLY=true`, the scheduler skips weekends/holidays, starts at the next session open, polls on `MARKET_DATA_TICK_INTERVAL`, and runs one final quote at 15:30 + `MARKET_DATA_FINAL_QUOTE_DELAY`.
  - At startup it catches up only when the latest quote is not fresh; it does not overwrite a fresh Friday close on a weekend.
  - With `MARKET_DATA_MARKET_HOURS_ONLY=false`, polling continues at the configured interval for manual testing.
  - Freshness is calendar-aware: during the session a quote must be at/after the open and no older than `MARKET_DATA_MAX_STALE`; after the close it must be at/after 15:30 minus `MARKET_DATA_CLOSE_GRACE`; before the open, on weekends, and on holidays the previous session close is fresh. A future-skewed quote is stale.
  - A row with no provenance or `legacy`/`seed` provenance is `unknown`, never `fresh`.

### Bhavcopy 404 and missing symbols

- **Problem:** NSE may publish the bhavcopy file late, omit a symbol, or return a 404 for a non-trading date.
- **Handling:**
  - A 404 is treated as “not published yet” and retried within the configured evening window. It never deletes existing prices.
  - A startup after the first attempt performs one catch-up for the current market date, including after the retry window; it does not recover previous sessions. Use the backfill command for earlier dates.
  - Symbols absent from the CSV are reported in the job log and skipped. Their existing prices remain untouched.
  - A save error causes the attempt to be retried; successful symbols are kept.
  - A file containing none of the tracked symbols is logged and does not erase anything.
  - Bhavcopy is NSE-only. `MARKET_DATA_BHAVCOPY_ENABLED=true` with a non-NSE exchange fails configuration loading. Simulator mode skips bhavcopy even when it is enabled.
  - The official close is stored as the raw close at 15:30 IST. Corporate actions are applied later by the adjusted views, never by the provider.

### Stale prices in API responses

- **Problem:** A client cannot tell whether a price is current, delayed, stale, or missing.
- **Handling:**
  - Current-price rows expose `priceSource`, `priceAsOf`, `priceFetchedAt`, and `priceFreshness` (`fresh`, `stale`, `unknown`; null for missing/not-required).
  - `/today-stocks` returns `staleRewards`, `staleSymbols`, and `pricesFresh`.
  - `/portfolio` and `/stats` return `stalePositions`, `staleSymbols`, and `pricesFresh`.
  - Stale counts cover the full user set, not only the requested page.
  - `pricesFresh` is true only when every available price is fresh. `unknown` makes it false; missing prices do not count as stale.
  - `priceStatus` and `valuationComplete` keep their previous meaning. A stale price is still `available`, and `valuationComplete` remains about missing prices only.
  - Historical rows expose provenance but no freshness flag.

### Simulator mode

- **Problem:** Development and tests must work without internet access or live providers.
- **Handling:**
  - `MARKET_DATA_PROVIDER=simulator` generates a random walk from the last stored price and writes through the same validation, precedence, history, and provenance path as a real provider.
  - Set `MARKET_DATA_BHAVCOPY_ENABLED=false` for a fully offline pipeline; simulator mode skips bhavcopy regardless.
  - Simulator prices are synthetic and must not be used as market data. Switching a production deployment to the simulator writes synthetic prices.

### Historical backfill

- **Problem:** Demo history may be flat, and gaps in `stock_price_history` produce `priceStatus: "missing"` in historical reports.
- **Handling:**
  - `go run ./cmd/backfill-market-data -from YYYY-MM-DD -to YYYY-MM-DD [-symbols TCS,INFY]` imports official NSE bhavcopy closes for an inclusive range.
  - It writes only `stock_price_history`; current prices are never changed.
  - It walks NSE trading days, skips weekends/holidays, and imports the raw close for each date.
  - Official `nse_bhavcopy` history is never overwritten by another source. Bhavcopy can replace a non-official row for its date.
  - Missing files are reported in `missingDates` and left as explicit gaps. No flat or synthetic price is fabricated.
  - Per-date and per-symbol failures are reported in `failed`; successful imports are kept and the command exits non-zero if any failure occurred.
  - Raw closes are stored, not adjusted closes. `stock_price_history_adjusted` applies splits/bonuses once; importing an adjusted vendor close would apply corporate actions twice.

### Rollback

- `MARKET_DATA_ENABLED=false` stops quote and EOD writes while the API continues to serve stored prices with provenance and freshness.
- `MARKET_DATA_BHAVCOPY_ENABLED=false` disables only official EOD ingestion; Yahoo current quotes continue.
- `MARKET_DATA_PROVIDER=simulator` plus `MARKET_DATA_BHAVCOPY_ENABLED=false` returns to a fully offline local pipeline. Do not use it against production data.
- Migration `0025` is additive. Its down migration drops the provenance columns; export provenance first if it is needed for audit.

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
  - `stock_events.effective_date`, `adjustments.effective_date`, and `stock_price_history.date` are market `DATE`s. A split, bonus, merger, or delist on a date is effective for that entire date. A history row's date is derived from the provider quote time with `market_date()`, not from the fetch time or `market_today()`, so a late fetch cannot move a close into the wrong day.
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
  - `PriceService` uses a worker pool for concurrent provider requests and a buffered-channel semaphore for database writes.
  - Provider HTTP requests use timeouts, rate limiting, retryable-status backoff, and response-size limits.
  - The bhavcopy EOD job retries within a configured evening window and never deletes prices for absent symbols.
  - The history backfill command walks NSE trading days and reports missing dates and failed imports without fabricating prices.
  - Workers recover from panics and respect context cancellation.

## 9. Retention and Deletion

- **Problem:** A direct `DELETE` on a user, reward, or stock event used to cascade into rewards, ledger postings, and adjustments. A single statement could silently erase the only record of a past transaction, making reconciliation impossible and changing historical reporting retroactively.
- **Handling:**
  - `users.deleted_at` is the supported offboarding path. A deactivated user keeps all financial history but cannot receive new rewards.
  - `rewards.user_id`, `ledger.reward_id`, `adjustments.reward_id`, and `ledger.stock_event_id` use `ON DELETE RESTRICT`. Hard deletion of a parent with financial children fails with SQLSTATE `23503` and leaves the history intact.
  - A reward or stock event with financial children cannot be hard-deleted. Correct it with an adjustment or an event update instead of deleting it.
  - There is no automatic purge job. Retention is indefinite by default; future archival, legal hold, or purge work must be an explicit, documented operational process.
  - Migration `0024_retention_and_delete_policy` implements the policy. Its down migration restores the old cascades for development rollback only.
  - `internal/database/retention_integration_test.go` proves that rejected deletes preserve history, deactivation keeps records readable, and the down migration restores the previous behavior.

## Summary

This system handles:

1. Duplicate rewards and idempotent retries.
2. Stock events and delisting behavior.
3. Missing historical price data as an explicit reporting status, with official NSE bhavcopy backfill for real closes.
4. Decimal-safe fee calculations and consistent rounding for quantities and INR values.
5. Provider failures, delayed data, source precedence, and calendar-aware stale-price reporting.
6. Transactional reward adjustments.
7. Explicit market-date and timezone semantics for rewards, corporate actions, price history, and historical snapshots.
8. Paginated historical and portfolio reads.
9. Financial-record retention with restrictive deletion and user deactivation.
