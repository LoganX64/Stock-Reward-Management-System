# Stock Reward Management System (Golang)

## Project Overview

Stocky is a stock rewards management API built in **Golang** using the **Gin** framework and **PostgreSQL**.
It tracks stock rewards, adjustments, ledger entries, and INR portfolio values.

The system maintains a ledger for stock units, INR outflows, brokerage, STT, and GST. It also supports stock events such as splits, bonus issues, mergers, and delisting through SQL views.

### Technologies Used

- **Go** - Backend programming language
- **Gin** - HTTP routing and middleware
- **PostgreSQL** - Relational database
- **golang-migrate** - Database migration tool
- **logrus** - Structured logging
- **lib/pq** - PostgreSQL driver
- **shopspring/decimal** - Exact decimal arithmetic for quantities, money, and fees
- **go-sqlmock** - Handler/unit test database mocking

### Key Features

- Record stock rewards with optional idempotency keys.
- Prevent duplicate same-day rewards per user and stock.
- Normalize stock symbols before storing rewards.
- Calculate reward INR value and fees for positive rewards.
- Track adjustments and refunds for previous rewards.
- Maintain ledger entries for stock units, cash flows, and fees.
- Calculate current portfolio and historical INR values.
- Keep unpriced holdings visible with explicit missing-price status instead of silently dropping them.
- Keep historical rewards visible with explicit missing-history status instead of silently dropping them.
- Support stock splits, bonus issues, mergers, and delisting.
- Adjust split/bonus prices and quantities exactly once while preserving raw prices.
- Apply effective-dated quantity movements so post-split corrections are not scaled twice.
- Paginate historical INR and portfolio endpoints.
- Track request IDs for logs.
- Fetch delayed current quotes from Yahoo Finance and official end-of-day closes from NSE bhavcopy.
- Store and expose price provenance (`priceSource`, `priceFetchedAt`) and calendar-aware freshness (`priceFreshness`, `staleSymbols`, `pricesFresh`).
- Keep the simulator available for offline development and testing.

---

## API Endpoints

| Method | Endpoint                         | Description                                |
| ------ | -------------------------------- | ------------------------------------------ |
| GET    | `/health`                        | Health check endpoint.                     |
| POST   | `/api/v1/reward`                 | Create a reward entry.                     |
| POST   | `/api/v1/adjustments/:id`        | Apply adjustment to a reward.              |
| GET    | `/api/v1/today-stocks/:userId`   | Fetch today's rewards with adjustments.    |
| GET    | `/api/v1/historical-inr/:userId` | Get historical INR valuation; supports `?asOf=YYYY-MM-DD`. |
| GET    | `/api/v1/stats/:userId`          | Get today's rewards and portfolio value.   |
| GET    | `/api/v1/portfolio/:userId`      | Get portfolio details per stock.           |

### Pagination

The historical INR and portfolio endpoints support `limit` and `offset` query parameters:

```text
GET /api/v1/historical-inr/:userId?limit=100&offset=0
GET /api/v1/historical-inr/:userId?asOf=2026-10-01&limit=100&offset=0
GET /api/v1/portfolio/:userId?limit=100&offset=0
```

- `limit` defaults to `100`.
- `limit` is capped at `500`.
- `offset` defaults to `0`.
- `limit` must be a positive integer.
- `offset` must be zero or a positive integer.
- `asOf` is optional and must be an ISO date (`YYYY-MM-DD`). Future dates are rejected.

`/historical-inr` defaults to **grant-date** valuation: each reward uses its original quantity and the adjusted reward-date price. With `asOf`, the same rows are revalued **as of that date**: effective-dated quantity, terminal successor symbol, adjustments, delist status, and price history are all evaluated on `asOf`. A lot delisted on or before `asOf` has quantity `0`, `priceStatus: "not_required"`, and reports its `cashProceeds` or `writtenOffQuantity`. A later delist never removes a historical row or changes its grant-date value.

Historical INR results are returned newest first with stable ordering:

```sql
ORDER BY reward_date DESC, reward_event_id DESC
```

Example response:

```json
{
  "userId": 1,
  "limit": 100,
  "offset": 0,
  "valuationMode": "grant_date",
  "valuationDate": "2026-10-28",
  "asOf": null,
  "history": [
    {
      "rewardDate": "2026-10-27",
      "rewardEventId": 2,
      "stockSymbol": "NEWCO",
      "adjustedQuantity": 5,
      "price": null,
      "totalAdjustmentAmount": 0,
      "inrValue": null,
      "priceStatus": "missing",
      "priceAsOf": null,
      "priceSource": null,
      "priceFetchedAt": null,
      "outcomeType": "active",
      "isActive": true,
      "delistEffectiveDate": null,
      "cashProceeds": 0,
      "writtenOffQuantity": 0
    }
  ],
  "valuationComplete": false,
  "unpricedRewards": 1,
  "unpricedSymbols": ["NEWCO"]
}
```

`valuationMode` is `"grant_date"` or `"as_of"`. `valuationDate` is the database date used for current lifecycle annotations in grant-date mode, or the requested `asOf` date in as-of mode. `outcomeType` is `active`, `cash_exit`, or `write_off`; `isActive` says whether the lot is still held; `delistEffectiveDate`, `cashProceeds`, and `writtenOffQuantity` expose the terminal outcome without hiding the historical row.

Portfolio responses use the same pagination metadata:

```json
{
  "userId": 1,
  "limit": 100,
  "offset": 0,
  "portfolio": [],
  "totalPortfolioValue": 0,
  "totalPositions": 0,
  "valuationDate": "2026-10-28",
  "valuationComplete": true,
  "unpricedPositions": 0,
  "unpricedSymbols": [],
  "stalePositions": 0,
  "staleSymbols": [],
  "pricesFresh": true,
  "cashProceeds": 0,
  "cashInLieuProceeds": 0,
  "writtenOffQuantity": 0
}
```

`totalPortfolioValue` is the full-set stock value and sums only holdings with an available current price. `totalPositions` counts every active holding, including unpriced ones. `valuationDate` is the market (IST) date used by the shared reporting views. `/portfolio` and `/stats` read the same `user_portfolio` view inside one read-only `REPEATABLE READ` snapshot, so their totals and position counts describe the same valuation date and MVCC state.

`cashProceeds` is the total cash received from merger consideration, cash-in-lieu fraction settlement, and cash-exit delistings. `cashInLieuProceeds` is the subset produced by fractional-share settlement. `writtenOffQuantity` is the quantity removed by write-off delistings. All three are zero when the user has no terminal corporate-action outcomes.

A holding is never dropped because its current price is missing. When no `stock_prices` row exists for a holding's terminal symbol, the row is still returned with `priceStatus: "missing"`, `currentPrice: null`, `inrValue: null`, and `priceAsOf: null`. The response then reports `valuationComplete: false`, `unpricedPositions`, and the affected `unpricedSymbols`. For priced holdings, `priceStatus` is `"available"`, and `priceAsOf` contains the provider quote time normalized to UTC. `priceSource` identifies the provider (`yahoo`, `nse_bhavcopy`, `simulator`, `legacy`, or `seed`), `priceFetchedAt` is when Stocky received the quote, and `priceFreshness` is `fresh`, `stale`, or `unknown` (null for `missing`/`not_required`). `stalePositions` and `staleSymbols` cover the user's full portfolio, not just the requested page. `pricesFresh` is true only when every available price is fresh; an `unknown` price makes it false, while missing prices are reported separately through `valuationComplete`.

`/today-stocks` applies the same rule per reward: an unpriced reward remains in `stocks` with a null price/value and is counted in `unpricedRewards` and `unpricedSymbols`. It also returns `staleRewards`, `staleSymbols`, and `pricesFresh` for the full reward set.

Historical rewards use the same explicit-gap rule. In grant-date mode, when `stock_price_history` has no row for a reward's date and terminal symbol, the reward remains in `history` with `priceStatus: "missing"`, `price: null`, `inrValue: null`, and `priceAsOf: null`. With `asOf`, the price is looked up on that date instead. `priceStatus` is `"not_required"` when the effective quantity is zero (for example after a terminal delist or a full reversal); that is a closed position, not a valuation gap. The `/historical-inr` response returns `valuationComplete`, `unpricedRewards`, and `unpricedSymbols` for the user's complete set at the same grant-date or `asOf` reference, not just the requested page. For available prices, `priceStatus` is `"available"`, `priceAsOf` is the provider quote time normalized to UTC, and `priceSource`/`priceFetchedAt` identify where and when the close was obtained. Historical rows do not expose `priceFreshness`, because a historical close is not expected to be current. An `asOf` date is validated against `market_today()` inside the same read snapshot, so it cannot be in the future in any server timezone.

`/stats` returns `todayRewards`, the stock-only `totalPortfolioValue`, the full-set `totalPositions`, `valuationDate`, the same `stalePositions`, `staleSymbols`, and `pricesFresh` fields as `/portfolio`, and the same `cashProceeds`, `cashInLieuProceeds`, and `writtenOffQuantity` totals as `/portfolio`. Its reads run in the same read-only snapshot, so `/stats` and `/portfolio` agree on the active symbol set, total value, position count, valuation date, and staleness summary. `totalPortfolioValue` sums only holdings with an available current price; check `valuationComplete`, `unpricedPositions`, and `unpricedSymbols` before treating it as a complete portfolio value, and check `pricesFresh`/`staleSymbols` before treating it as current.

---

## Database Schema

The project uses PostgreSQL with the following main tables and views:

- `users`: User information, including optional deactivation via `deleted_at`.
- `rewards`: Reward events with optional idempotency keys.
- `ledger`: Stock units, INR outflows, fee entries, and corporate-action cash postings tagged with `stock_event_id`.
- `stock_prices`: Latest stock prices.
- `stock_price_history`: Daily stock price history.
- `stock_events`: Splits, bonus issues, mergers, and delisting events with normalized symbols, optional cash consideration, delist outcome, and fraction policy.
- `adjustments`: Manual corrections, fee refunds, and reward reversals with effective dates.
- `instrument_lineage` (VIEW): Normalized merger edges derived from `stock_events`, used to resolve A→B→C chains.
- `stock_prices_adjusted` (VIEW): Raw latest prices with split/bonus factors applied.
- `stock_price_history_adjusted` (VIEW): Raw historical prices with point-in-time split/bonus factors applied.
- `reward_quantity_movements` (VIEW): Original and adjustment quantity movements normalized to reward-date units.
- `reward_current_balances` (VIEW): Movement-derived current settled quantity per reward.
- `reward_corporate_action_outcomes` (VIEW): Per-reward terminal outcome, total cash proceeds, cash-in-lieu proceeds, fraction quantity, and written-off quantity, evaluated at `market_today()`.
- `reward_corporate_action_outcome` (FUNCTION): Parameterized `(reward_id, as_of)` lifecycle/outcome for historical snapshots.
- `user_corporate_action_cash` (VIEW): Per-user totals of corporate-action cash, cash-in-lieu proceeds, and written-off quantity.
- `today_rewards` (VIEW): Today's reward values with explicit current-price status; terminal-delisted rewards are excluded.
- `historical_rewards_as_of` (FUNCTION): Grant-date or as-of historical valuation with lifecycle annotations.
- `historical_rewards` (VIEW): Backward-compatible grant-date mode of `historical_rewards_as_of`.
- `user_portfolio` (VIEW): Aggregated current active holdings, including holdings whose current price is missing; terminal-delisted holdings are excluded.
- `reward_ledger_reconciliation` (VIEW): Per-reward stock, cash, adjustment, and fee reconciliation.

### Key Relationships

- `users` -> `rewards` (`user_id`, `ON DELETE RESTRICT`)
- `rewards` -> `ledger` (`reward_id`, `ON DELETE RESTRICT`)
- `rewards` -> `adjustments` (`reward_id`, `ON DELETE RESTRICT`)
- `stock_events` -> `ledger` (`stock_event_id`, `ON DELETE RESTRICT`)
- `stock_events` -> `stock_prices` (`stock_symbol`)

### Retention and deletion

Financial records are append-only with respect to deletion. Deleting a user, reward, or stock event must not silently remove the ledger, adjustment, or cash-posting history that explains a past transaction.

- Users are deactivated by setting `users.deleted_at`; all of their rewards, ledger entries, adjustments, and corporate-action cash remain readable. Deactivated users cannot receive new rewards. Example: `UPDATE users SET deleted_at = NOW() WHERE id = $1;`.
- A user with rewards cannot be hard-deleted. A reward with ledger postings or adjustments cannot be hard-deleted. A stock event with tagged corporate-action cash postings cannot be hard-deleted.
- The foreign keys use `ON DELETE RESTRICT`, not `ON DELETE CASCADE`. PostgreSQL checks `RESTRICT` immediately, so the failing statement is rejected instead of deleting dependent financial rows.
- There is no automatic purge or archival job. Financial records are retained indefinitely by default; any future retention or legal-hold process must be explicit, documented, and outside the normal request path.
- Corrections should be made by posting compensating adjustments or updating the stock event, not by deleting the original record.

Migration `0024_retention_and_delete_policy` replaces the original cascades and adds `users.deleted_at`. Its down migration restores the development-era cascades and should not be used as a production deletion procedure.

### Business dates and timezones

Stocky is an Indian-equity system, so every business date is a market date in **Asia/Kolkata (IST, UTC+05:30, no daylight saving)**. The timezone is fixed in the database and is not read from the server or connection session:

- `market_timezone()` returns the fixed zone name.
- `market_date(timestamptz)` returns the IST calendar date of an absolute instant.
- `market_today()` returns the current IST date and replaces `CURRENT_DATE` in all business logic.

Timestamp and date columns are deliberately typed:

- `rewards.reward_date` is an explicit `DATE` derived from `rewards.created_at` by the `trg_set_reward_date` trigger. Existing reward dates were preserved when the former generated column was converted. Same-day uniqueness and indexes continue to use `reward_date`.
- `rewards.created_at`, `ledger.created_at`, `adjustments.created_at`, `users.created_at`, `users.deleted_at`, `stock_prices.updated_at`, and `stock_price_history.created_at` are `TIMESTAMPTZ`. API output converts them to UTC.
- `stock_events.effective_date`, `adjustments.effective_date`, and `stock_price_history.date` are `DATE`. Corporate actions become effective at 00:00 IST on their date, and every event type — including delists — uses the same inclusive boundary.

Migration `0022_business_date_timezone_semantics` interprets legacy `TIMESTAMP` values as IST wall-clock time (`AT TIME ZONE 'Asia/Kolkata'`), so existing business dates do not move. The boundary behavior is covered by `internal/database/timezone_integration_test.go`, which runs the migrations against a disposable database under `UTC`, `Asia/Kolkata`, `America/Los_Angeles`, and `Pacific/Kiritimati` when `TEST_DATABASE_URL` is set.

### Instrument symbol normalization

Migration `0023_case_normalized_instruments` makes instrument symbols canonical across the whole valuation path. `normalize_instrument_symbol(text)` returns `UPPER(BTRIM(text, E' \t\n\r'))`, and the same rule is implemented by `utils.NormalizeStockSymbol` in Go.

- `stock_prices`, `stock_price_history`, `rewards`, `ledger`, and `stock_events` store only canonical symbols. `BEFORE INSERT OR UPDATE` triggers normalize new writes, and `CHECK` constraints reject non-canonical values if a trigger is bypassed.
- Functional unique indexes enforce one current price, one daily history row per date, and one reward per user/symbol/market date after normalization. `stock_events` keeps its existing normalized unique index.
- Valuation, lineage, settlement, and reconciliation joins use exact canonical equality, so `AAPL` and `aapl` cannot multiply rows. Reward creation also looks up the price with an exact symbol match and a deterministic `updated_at DESC, id DESC` tie-break.
- Migration `0023` resolves existing `stock_prices` / `stock_price_history` case variants by keeping the latest row (`updated_at` / `created_at`). It aborts instead of deleting if `rewards` or `stock_events` contain normalized duplicates, because those are financial/audit records and event deletion must not erase ledger postings.
- The data normalization is one-way; the down migration restores the previous schema behavior but cannot restore original casing.

### Fee assumptions, precision, and rounding

Stocky uses exact decimal arithmetic for the financial write path. Requests, PostgreSQL `NUMERIC` reads, notional and fee calculations, ledger values, and JSON responses all use `shopspring/decimal`; quantities and amounts are not converted through `float64` before they are stored. The simulator provider is the only exception: its random walk uses `float64`, but the resulting price is rounded to 4 decimal places before storage.

**Storage precision**

- Quantities: `NUMERIC(18,6)` — 6 decimal places.
- INR amounts and stored prices: `NUMERIC(18,4)` — 4 decimal places.
- Split/bonus-adjusted prices can carry PostgreSQL division precision at read time; the resulting notional is rounded once to 4 decimal places.
- Rounding mode is **half away from zero**, matching PostgreSQL `ROUND(numeric, scale)`.

**Rounding points and fee formulas**

1. Round the requested quantity to 6 decimal places and reject it if it is not positive.
2. Calculate the notional and round it to 4 decimal places:
   `notional = ROUND4(adjusted_price × quantity)`
3. Round each fee component at its own step:
   - `brokerage = ROUND4(notional × 0.005)` — 0.5% of notional.
   - `stt = ROUND4(notional × 0.001)` — 0.1% of notional.
   - `gst = ROUND4((brokerage + stt) × 0.18)` — 18% of the **already-rounded** brokerage and STT.
   - `fee_total = ROUND4(brokerage + stt + gst)`.
4. Persist and return the same rounded values. The `inr_outflow` ledger row is `-notional`; fee rows are `-brokerage`, `-stt`, and `-gst`. A `fee_refund` posts a positive `inr_outflow` adjustment and does not change the holding quantity or its valuation; a `manual_correction` posts its signed quantity and cash deltas.

**Fee model assumptions and exclusions**

- The fee schedule is a simplified **buy-side** example for a reward treated as a market purchase: 0.5% brokerage, 0.1% STT, and 18% GST on brokerage + STT.
- The user is assumed to bear the fees. A free grant or company-borne transaction may have no buy-side fees, but Stocky does not model that distinction.
- The model does **not** include sell-side STT, exchange transaction charges, SEBI turnover fees, stamp duty, DP charges, minimum brokerage, or a configurable fee schedule.
- GST is intentionally calculated on brokerage + STT in this MVP. That is not the full statutory Indian GST base.
- Reversals do not automatically refund fees. Use a `fee_refund` adjustment for an explicit fee refund.
- Adjustment deltas are rounded to 6 quantity / 4 amount decimal places before validation. Quantity deltas are normalized through corporate-action factors in PostgreSQL `NUMERIC` arithmetic.

**Worked examples**

| Stored price | Quantity | Raw notional | Rounded notional | Brokerage | STT | GST | Fee total |
|---|---|---|---|---|---|---|---|
| `100.0000` | `2.000000` | `200` | `200.0000` | `1.0000` | `0.2000` | `0.2160` | `1.4160` |
| `0.0003` | `0.500000` | `0.00015` | `0.0002` | `0.0000` | `0.0000` | `0.0000` | `0.0000` |
| `4.0001` | `0.500000` | `2.00005` | `2.0001` | `0.0100` | `0.0020` | `0.0022` | `0.0142` |

The second row is the boundary case that binary floating point can round down. Stocky uses half-away-from-zero decimal rounding, so `0.00015` becomes `0.0002` in both the response and the persisted ledger.

### Corporate actions and adjustment timing

- Raw prices remain in `stock_prices` and `stock_price_history`. Split/bonus factors are applied by the adjusted views, never written back.
- A 2:1 forward split or 1:1 bonus uses ratio `2/1`; a 1:5 reverse split uses `1/5`. Only `split` and `bonus` events affect prices.
- Merger lineage is resolved recursively. A reward in A follows A→B→C, composing every merger ratio and applying split/bonus events only while the holding is in each successor instrument.
- Merger events must have a distinct successor and positive ratios. A database trigger rejects lineage cycles, and each predecessor may have only one merge event.
- Instrument and corporate-action symbols are trimmed and uppercased before storage. A unique index on normalized symbol, event type, and effective date rejects case, whitespace, and same-day duplicate events. Migration `0023` applies the same canonical representation and uniqueness to current prices, price history, and rewards.
- Event ratios are validated: split ratios must be positive and not 1:1; bonus ratios must be greater than 1:1; merger ratios must be positive; delist ratios are normalized to 1:1. Event-type fields are constrained so split/bonus events cannot carry merger/delist fields, mergers require a non-blank successor and cannot carry a delist outcome, and delists require an outcome and cannot carry a successor.
- There is no corporate-action write API yet; these rules are enforced by database constraints and triggers for every write path.
- Delist filtering resolves the terminal successor first. A predecessor delisting after a merger does not remove the valid successor holding; a delisting of the terminal instrument does.
- A merger can carry optional `cash_per_unit` consideration per predecessor share. The cash is calculated when the holding crosses that merger edge and is included in `cashProceeds`.
- A terminal delist must specify `delist_outcome`:
  - `cash_exit` requires positive `cash_per_unit`; the holding leaves the portfolio and `quantity × cash_per_unit` is reported as cash proceeds.
  - `write_off` requires no cash; the holding leaves the portfolio and its quantity is reported as `writtenOffQuantity`.
- A cash-only acquisition is represented as a `delist` with `cash_exit` and no successor.
- Legacy delist rows are backfilled to `write_off`, matching the previous portfolio-removal behavior.
- **Fractional shares.** Quantities are stored to 6 decimal places; cash is stored to 4 decimal places. `stock_events.fraction_policy` supports two per-event policies:
  - `retain` (default): the post-event fractional quantity is retained at 6 decimal places. No cash is paid.
  - `cash_in_lieu`: after each split, bonus, or merger, the post-event quantity is rounded to 6 decimal places and floored to whole units. The removed fraction is paid at the event's positive `fraction_cash_per_unit`, which is expressed per post-event (successor/current-instrument) share and rounded to 4 decimal places. The holding continues in the successor instrument with the whole-unit quantity.

  For example:

  | Scenario | `retain` | `cash_in_lieu` |
  |---|---|---|
  | 3 shares, 1:5 reverse split | 0.6 shares | 0 shares + `0.6 × fraction_cash_per_unit` |
  | 10 shares, 42:25 merger | 16.8 successor shares | 16 successor shares + `0.8 × fraction_cash_per_unit` |

  Settlement is applied per reward lot, not across an aggregated user position, matching the reward-scoped ledger. Merger `cash_per_unit` consideration is calculated on the pre-merger quantity and is not reduced by cash-in-lieu flooring. A delist cannot use `cash_in_lieu`; use `cash_exit` for a cash-only exit. Same-date actions are applied in the order split → bonus → merger → adjustment.
- Corporate-action cash is posted physically to `ledger` as positive `inr_outflow` rows tagged with `ledger.stock_event_id`. The unique key `(reward_id, stock_event_id)` makes posting idempotent. An `AFTER` statement trigger on `stock_events` posts immediately, and the background `CorporateActionService` runs at startup and daily to post future-dated events when they become effective. `process_corporate_action_cash(as_of)` updates or removes stale postings when events change.
- `reward_ledger_reconciliation` excludes corporate-action cash from adjustment-cash reconciliation and compares derived corporate-action cash with the tagged ledger postings through `corporate_action_cash_matches`.
- `reward_corporate_action_outcomes` is the single successor-aware outcome source for `today_rewards`, `user_portfolio`, and `user_corporate_action_cash`. A terminal delist removes the holding from active reporting; a predecessor delist after a merger does not. Handlers must not add their own delist predicates, so `/portfolio`, `/stats`, and `/today-stocks` cannot drift apart. `/portfolio` and `/stats` therefore return the same cash and write-off totals, total value, position count, and valuation date. `totalPortfolioValue` remains stock-value-only.
- `reward_corporate_action_outcome(reward_id, as_of)` generalizes the same outcome logic to an arbitrary date for historical snapshots. `historical_rewards_as_of(as_of)` uses it to value each reward at grant date (`as_of IS NULL`) or as of a requested date. Grant-date mode uses the original quantity and the adjusted reward-date price; as-of mode uses effective-dated quantity, terminal symbol, adjustments, delists, and price. A later delist never removes a historical row or changes its grant-date value. Zero-quantity rows use `price_status = 'not_required'` instead of being reported as valuation gaps. Missing prices remain visible with `price_status = 'missing'` and null `price`/`inr_value`.
- Adjustments take effect on `adjustments.effective_date` (defaults to `market_today()`). Quantity movements are normalized to reward-date units through the merger chain, so a post-split or post-merger correction is not scaled twice.
- `reward_ledger_reconciliation.stock_quantity_matches` compares movement-derived quantities with stock-unit postings after applying the same chain-aware event factors.

---

## Running the Project

### Prerequisites

- Go 1.25 or later
- PostgreSQL
- Docker and Docker Compose, if running with containers

### Running Locally

1. Set up PostgreSQL and create the configured database.
2. Configure environment variables in `.env.local` or your shell.
3. Run the API:

```bash
go run ./cmd/stocky-api/main.go
```

The API is available at:

```text
http://localhost:8080
```

### Using Docker

```bash
docker-compose up --build
```

For detached mode:

```bash
docker-compose up --build -d
```

---

## Environment Variables

The application loads `.env.local` by default. Set `ENV_FILE` to use another file, such as `.env.docker`.

Common local values:

```text
DB_HOST=localhost
DB_PORT=5432
POSTGRES_USER=postgres
POSTGRES_PASSWORD=9908
POSTGRES_DB=assignment
DB_SSLMODE=disable
HTTP_PORT=8080
MIGRATION_PATH=./internal/database/migrations
```

Common Docker values:

```text
DB_HOST=db
DB_PORT=5432
POSTGRES_USER=stocky_user
POSTGRES_PASSWORD=stocky_pass
POSTGRES_DB=stocky
DB_SSLMODE=disable
HTTP_PORT=8080
MIGRATION_PATH=/app/internal/database/migrations
```

Supported variables:

- `ENV_FILE` - Optional path to the env file to load.
- `ENV` - Application environment label.
- `DB_HOST` - Database host.
- `DB_PORT` - Database port.
- `POSTGRES_USER` - PostgreSQL user.
- `POSTGRES_PASSWORD` - PostgreSQL password.
- `POSTGRES_DB` - PostgreSQL database name.
- `DB_SSLMODE` - PostgreSQL SSL mode.
- `HTTP_PORT` - API port.
- `MIGRATION_PATH` - Path to database migrations.

### Market-data variables

`MARKET_DATA_PROVIDER=yahoo` is the default. The simulator remains available for offline development. Market-data durations use Go `time.ParseDuration` syntax (`60m`, `90m`, `1s`).

| Variable | Default | Purpose |
|---|---|---|
| `MARKET_DATA_ENABLED` | `true` | Master switch for quote and EOD background jobs. Stored prices are still served when false. |
| `MARKET_DATA_PROVIDER` | `yahoo` | `yahoo` or `simulator`. |
| `MARKET_DATA_EXCHANGE` | `NSE` | `NSE` or `BSE`. Bhavcopy requires `NSE`. |
| `MARKET_DATA_SYMBOL_SUFFIX` | `.NS` / `.BO` | Provider symbol suffix; derived from the exchange when empty. |
| `MARKET_DATA_TICK_INTERVAL` | `60m` | Polling interval for current quotes. |
| `MARKET_DATA_MARKET_HOURS_ONLY` | `true` | Restrict polling to the NSE session and one post-close quote. |
| `MARKET_DATA_FINAL_QUOTE_DELAY` | `5m` | Delay after 15:30 IST before the final quote. |
| `MARKET_DATA_MAX_STALE` | `90m` | Maximum quote age during a live session. |
| `MARKET_DATA_CLOSE_GRACE` | `20m` | Allowed last-trade age after the close. |
| `MARKET_DATA_HTTP_TIMEOUT` | `10s` | Provider HTTP timeout. |
| `MARKET_DATA_MAX_RETRIES` | `3` | Maximum total attempts per HTTP request (1–10). |
| `MARKET_DATA_RETRY_BASE` | `1s` | Base delay for exponential backoff. |
| `MARKET_DATA_RATE_LIMIT` | `1s` | Minimum spacing between provider requests; `0` disables the client-side limiter. |
| `MARKET_DATA_USER_AGENT` | Stocky user agent | `User-Agent` sent to providers. |
| `MARKET_DATA_YAHOO_BASE_URL` | `https://query1.finance.yahoo.com` | Yahoo API base URL. |
| `MARKET_DATA_BHAVCOPY_ENABLED` | `true` | Enable official NSE EOD ingestion. |
| `MARKET_DATA_BHAVCOPY_BASE_URL` | `https://nsearchives.nseindia.com` | NSE archive base URL. |
| `MARKET_DATA_BHAVCOPY_TIME` | `18:15` | First bhavcopy attempt (IST). |
| `MARKET_DATA_BHAVCOPY_RETRY_UNTIL` | `20:30` | Last bhavcopy retry (IST). |
| `MARKET_DATA_BHAVCOPY_RETRY_INTERVAL` | `30m` | Spacing between bhavcopy retries. |
| `MARKET_HOLIDAYS` | empty | Additional NSE closures as comma-separated `YYYY-MM-DD` dates. |

Configuration is validated at startup. Unsupported providers/exchanges, malformed URLs, invalid durations, invalid clock times, inverted bhavcopy retry windows, and bhavcopy enabled for a non-NSE exchange fail with an error naming the offending variable. The effective provider, schedule, exchange, and bhavcopy settings are logged at startup without secrets.

Example offline configuration:

```text
MARKET_DATA_PROVIDER=simulator
MARKET_DATA_BHAVCOPY_ENABLED=false
```

---

## Testing

Run all tests:

```bash
go test ./...
```

Run the PostgreSQL integration tests (they create and drop a disposable database per top-level test):

```bash
TEST_DATABASE_URL='postgres://postgres:password@localhost:5432/postgres?sslmode=disable' \
  go test ./internal/database -run Integration -v
```

The integration tests are skipped when `TEST_DATABASE_URL` is not set. They cover:

- clean and repeated schema migrations, required views/functions/indexes;
- unique and idempotency conflicts on the reward and adjustment write paths;
- concurrent keyed rewards, unkeyed same-day rewards, adjustment retries, and overdraft adjustments, asserting that each logical operation commits exactly one effect;
- reward replay after a later adjustment and ledger reconciliation;
- split, bonus, reverse-split, merger-chain, delist, and cash-in-lieu valuation semantics;
- explicit `missing` price status for current and historical valuations;
- restrictive deletion and user-deactivation retention behavior.

Run only the stocky handler tests:

```bash
go test -v github.com/LoganX64/stocky-api/internal/handlers/stocky
```

Test files in `internal/handlers/stocky/`:

- `adjustment_test.go` - Tests adjustment validation and reversal ledger behavior.
- `historical_test.go` - Tests historical endpoint validation, missing-history behavior, and provenance output.
- `portfolio_test.go` - Tests portfolio endpoint validation, cash outcomes, and missing-price holdings.
- `reward_handler_test.go` - Tests reward validation, route registration, idempotency, normalization, and fee ledger entries.
- `stats_test.go` - Tests stats endpoint validation and incomplete-valuation reporting.
- `todays_test.go` - Tests today's stock endpoint validation and missing-price rewards.
- `freshness_test.go` - Tests provenance/freshness fields, null freshness for missing prices, unknown legacy/seed provenance, and full-set stale counts under pagination.

Market-data and job tests cover:

- quote validation, including historical-close validation that bypasses only the live maximum age;
- Yahoo and bhavcopy parsing, HTTP retry/backoff, rate limiting, body limits, and 404 handling;
- NSE calendar, session, holiday, weekend, and freshness behavior;
- bhavcopy scheduling, retry windows, and precedence against Yahoo;
- provider failure preserving the last known price and cache;
- history backfill range walking, missing dates, per-date/per-symbol failures, and raw-close storage.

---

## Code Structure

- `/cmd/stocky-api/main.go` - API entry point.
- `/cmd/backfill-market-data/main.go` - Official NSE history backfill command.
- `/cmd/reset-migrations.go` - Migration reset utility.
- `/internal/config/` - Environment configuration.
- `/internal/database/migrations/` - SQL migrations and views.
- `/internal/handlers/stocky/` - HTTP handlers, route setup, and tests.
- `/internal/jobs/` - Quote scheduler, bhavcopy EOD ingestion, history backfill, and corporate-action jobs.
- `/internal/marketcal/` - NSE trading calendar and freshness policy.
- `/internal/marketdata/` - Provider contracts, Yahoo/bhavcopy/simulator providers, and HTTP client.
- `/internal/storage/models/` - Data models.
- `/internal/utils/` - Rounding and JSON helpers.
- `/internal/utils/response/` - JSON response helpers.
- `Dockerfile` - Docker image instructions.
- `docker-compose.yml` - Docker Compose setup.
- `Stocky-api.postman_collection.json` - Postman collection.

---

## Price Update System

Stocky uses two complementary market-data paths:

| Source | Used for | Timing | Nature |
|---|---|---|---|
| Yahoo Finance | current quotes | during the NSE session plus one final quote after the close | Unofficial and delayed, typically by about 15 minutes plus polling lag. It is **not** real-time tick data. |
| NSE bhavcopy CSV | official end-of-day close and daily history | 18:15 IST, retried every 30 minutes until 20:30 IST | Official NSE archive. Authoritative for its market date. |
| Simulator | local development and tests | each scheduler tick | Synthetic random walk. Never a real market price. |

`MARKET_DATA_PROVIDER=yahoo` is the default. `MARKET_DATA_PROVIDER=simulator` keeps the pipeline fully offline.

### Scheduling

- The quote scheduler polls every `MARKET_DATA_TICK_INTERVAL` (default `60m`) while `MARKET_DATA_MARKET_HOURS_ONLY=true`.
- It skips weekends and configured NSE holidays (`MARKET_HOLIDAYS`) and resumes at the next session open.
- It runs one final quote at 15:30 IST + `MARKET_DATA_FINAL_QUOTE_DELAY` (default 5 minutes).
- At startup it catches up when the latest stored quote is not classified as fresh. A failed cycle leaves the last known price untouched.
- With `MARKET_DATA_MARKET_HOURS_ONLY=false`, it polls continuously at the configured interval for manual testing.
- The bhavcopy job runs at `MARKET_DATA_BHAVCOPY_TIME` (default 18:15 IST) and retries every `MARKET_DATA_BHAVCOPY_RETRY_INTERVAL` until `MARKET_DATA_BHAVCOPY_RETRY_UNTIL` (default 20:30 IST). A startup after the first attempt performs one catch-up for the current market date; it does not recover earlier sessions (use the backfill command for that).
- Bhavcopy ingestion is skipped in simulator mode and requires `MARKET_DATA_EXCHANGE=NSE`.

### Source precedence

Current-price and history writes use the same deterministic rules:

- `nse_bhavcopy` is authoritative for its market date. A Yahoo or simulator quote for the same date cannot replace it.
- Bhavcopy may replace a Yahoo/simulator quote for the same date, even when Yahoo's timestamp is slightly later.
- A quote from a later market date can take over the current price.
- For equal timestamps, source rank is `nse_bhavcopy > yahoo > simulator > legacy/seed`.
- A rejected quote does not update the cache and is logged as `quote_ignored_older`.
- History rows are keyed by `(symbol, market_date(quote_time))`; a late fetch cannot move a quote into the wrong day.

### Provenance and freshness in responses

Current-price rows expose:

- `priceSource` - `yahoo`, `nse_bhavcopy`, `simulator`, `legacy`, or `seed`.
- `priceAsOf` - provider quote time, normalized to UTC.
- `priceFetchedAt` - when Stocky received the quote, normalized to UTC.
- `priceFreshness` - `fresh`, `stale`, or `unknown`; `null` when `priceStatus` is `missing` or `not_required`.

Historical rows expose `priceSource` and `priceFetchedAt` but no freshness flag, because a historical close is not expected to be current.

Example current row:

```json
{
  "stockSymbol": "TCS",
  "currentPrice": 2502.6,
  "priceStatus": "available",
  "priceSource": "yahoo",
  "priceAsOf": "2026-10-30T09:58:16Z",
  "priceFetchedAt": "2026-10-30T10:00:05Z",
  "priceFreshness": "fresh"
}
```

The freshness policy is calendar-aware and shared with the scheduler:

- During the session: fresh when the quote is at or after the open and no older than `MARKET_DATA_MAX_STALE`.
- After the close: fresh when the quote is at or after 15:30 IST minus `MARKET_DATA_CLOSE_GRACE`.
- Before the open, on weekends, and on holidays: the previous session's close is fresh.
- A quote more than five minutes in the future is stale.
- A row with no provenance or `legacy`/`seed` provenance is `unknown`, never `fresh`.

Summary fields:

- `/today-stocks` - `staleRewards`, `staleSymbols`, `pricesFresh`.
- `/portfolio` and `/stats` - `stalePositions`, `staleSymbols`, `pricesFresh`.
- Stale counts cover the full user set, not just the requested page.
- `pricesFresh` is `true` only when every available price is `fresh`; `unknown` makes it `false`, while `missing`/`not_required` rows are not counted as stale.
- `priceStatus` and `valuationComplete` keep their existing meaning: `valuationComplete` is about missing prices only, not stale ones.

### Failure behavior

- A provider error, timeout, non-2xx response, malformed payload, future timestamp, wrong currency/exchange, or symbol mismatch writes nothing.
- The last known price and history remain in place; the cache is updated only after a successful database save.
- The API keeps returning the stored price with its original provenance and freshness, so clients can see its age and whether it is fresh, stale, or unknown.
- HTTP requests retry only for retryable statuses (`408`, `429`, and `5xx`) with capped exponential backoff, jitter, and `Retry-After` support. Other `4xx` responses fail immediately.
- Logs are structured and include a `cycle_id` for scheduler cycles plus provider, symbol, attempt, HTTP status, latency, quote time, fetch time, and write outcome. Full response bodies and secrets are not logged.
- Bhavcopy does not delete prices for symbols that are absent from the file. A 404 means "not published yet" and is retried within the evening window.

### Simulator mode

For offline development:

```text
MARKET_DATA_PROVIDER=simulator
MARKET_DATA_BHAVCOPY_ENABLED=false
```

The simulator generates a random walk from the last stored price and writes through the same validation, precedence, history, and provenance path as a real provider. Its prices are synthetic and must not be treated as market data. Bhavcopy ingestion is skipped in simulator mode.

### Historical backfill

The `backfill-market-data` command imports **raw official bhavcopy closes** into `stock_price_history` for an inclusive date range:

```bash
go run ./cmd/backfill-market-data -from 2026-01-01 -to 2026-10-30
```

Options:

- `-from YYYY-MM-DD` - required first market date.
- `-to YYYY-MM-DD` - last market date; defaults to yesterday.
- `-symbols TCS,INFY` - optional comma-separated symbols; defaults to every symbol in `stock_prices`.

Behavior:

- Only `stock_price_history` is written; current prices are never changed.
- The command walks NSE trading days, skips weekends/holidays, and imports each date's raw close.
- Official `nse_bhavcopy` history is never overwritten by a different source. The shared precedence rules also let bhavcopy replace a non-official row for the same date.
- A missing bhavcopy file is reported in `missingDates` and left as an explicit gap; no flat or synthetic price is fabricated.
- Per-date and per-symbol failures are reported in `failed`; successful imports are kept, and the command exits non-zero when any failure occurred.
- The close is the raw bhavcopy close. Splits and bonuses are applied later by `stock_price_history_adjusted`; using an adjusted vendor close would apply corporate actions twice.

The older `backfill_price_history()` SQL helper still exists for demo data:

```sql
SELECT backfill_price_history('2026-10-27');
```

It copies the latest known current price into missing history rows. Do not use it when real historical closes are required. `/historical-inr` still marks any remaining gap as `priceStatus: "missing"` rather than hiding the reward.

### Rollback

- To stop all market-data writes while continuing to serve stored prices: set `MARKET_DATA_ENABLED=false` and restart. The API still reports provenance and freshness for existing rows.
- To return to a fully offline local pipeline: set `MARKET_DATA_PROVIDER=simulator` and `MARKET_DATA_BHAVCOPY_ENABLED=false`. This writes synthetic prices, so do not use it against production data.
- To stop only official EOD ingestion: set `MARKET_DATA_BHAVCOPY_ENABLED=false`. Yahoo current quotes continue.
- Migration `0025` is additive. Rolling it back drops the provenance columns and restores the pre-A3 price shape; export or record provenance first if it is needed for audit.

---

## Edge Cases Handled

- **Duplicate rewards** - Prevented with same-day uniqueness and idempotency handling.
- **Missing idempotency key** - Stored as SQL `NULL`; API scans it back as an empty string when returning inserted rows.
- **Stock symbol casing** - All instrument symbols (current prices, price history, rewards, ledger, and stock events) are trimmed and uppercased at write time. Normalized uniqueness and exact canonical joins prevent case variants from duplicating rows, multiplying valuations, or making price lookup nondeterministic.
- **Stock events** - Views account for splits, bonus issues, mergers, and delisting, including merger cash consideration, terminal cash-exit/write-off outcomes, and per-event fractional-share settlement.
- **Adjustments/refunds** - Stored in `adjustments` and reflected in ledger entries.
- **Negative quantities after adjustment** - Prevented by transactional validation.
- **Rounding** - Quantities use 6 decimal places and INR amounts use 4. Fee and monetary calculations use exact decimal arithmetic in the write path, with documented half-away-from-zero rounding points. See **Fee assumptions, precision, and rounding**.
- **Missing current prices** - `today_rewards` and `user_portfolio` use a left join to adjusted current prices. Unpriced rewards/holdings remain visible with `priceStatus: "missing"` and null price/value; `/portfolio` and `/stats` return `valuationComplete`, an unpriced count, and `unpricedSymbols`. `totalPortfolioValue` is priced-holdings-only.
- **Stale current prices** - Priced rows expose `priceSource`, `priceAsOf`, `priceFetchedAt`, and `priceFreshness`. `/today-stocks` returns `staleRewards`, `staleSymbols`, and `pricesFresh`; `/portfolio` and `/stats` return `stalePositions`, `staleSymbols`, and `pricesFresh` for the full user set. Staleness does not change `priceStatus` or `valuationComplete`.
- **Market-data provenance** - `stock_prices` and `stock_price_history` record the provider source, provider symbol, exchange, currency, quote time, and fetch time. `nse_bhavcopy` is authoritative for its market date; Yahoo/simulator quotes cannot replace it for that date.
- **Missing historical prices** - `historical_rewards` left-joins adjusted reward-date prices. Unpriced rewards remain visible with `priceStatus: "missing"` and null price/value; `/historical-inr` returns full-set `valuationComplete`, `unpricedRewards`, and `unpricedSymbols`.
- **Transaction safety** - Reward creation and adjustment workflows use transactions.

### Ledger reconciliation

After applying migrations, inspect all reward-level ledger discrepancies with:

```sql
SELECT *
FROM reward_ledger_reconciliation
WHERE NOT is_reconciled
ORDER BY reward_id;
```

The view compares movement-derived reward quantity to stock-unit postings under the original reward symbol after normalizing both for corporate actions, adjustment amounts to their INR ledger postings, and the original reward cash flow and fees to the stored creation response. It also compares derived corporate-action cash with tagged `inr_outflow` postings through `corporate_action_cash_matches`; corporate-action cash is excluded from the adjustment-cash comparison.

---

## Response Format

Successful responses return JSON with status code `200`.

Error responses use:

```json
{
  "error": "error message here"
}
```

---

## Author

Developed by Jitin K

---

## License

MIT License
