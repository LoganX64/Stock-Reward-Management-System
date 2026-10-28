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
- Run a simulated background price updater with worker concurrency and database retry logic.

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
  "cashProceeds": 0,
  "cashInLieuProceeds": 0,
  "writtenOffQuantity": 0
}
```

`totalPortfolioValue` is the full-set stock value and sums only holdings with an available current price. `totalPositions` counts every active holding, including unpriced ones. `valuationDate` is the market (IST) date used by the shared reporting views. `/portfolio` and `/stats` read the same `user_portfolio` view inside one read-only `REPEATABLE READ` snapshot, so their totals and position counts describe the same valuation date and MVCC state.

`cashProceeds` is the total cash received from merger consideration, cash-in-lieu fraction settlement, and cash-exit delistings. `cashInLieuProceeds` is the subset produced by fractional-share settlement. `writtenOffQuantity` is the quantity removed by write-off delistings. All three are zero when the user has no terminal corporate-action outcomes.

A holding is never dropped because its current price is missing. When no `stock_prices` row exists for a holding's terminal symbol, the row is still returned with `priceStatus: "missing"`, `currentPrice: null`, `inrValue: null`, and `priceAsOf: null`. The response then reports `valuationComplete: false`, `unpricedPositions`, and the affected `unpricedSymbols`. For priced holdings, `priceStatus` is `"available"`, and `priceAsOf` contains the price row's last update timestamp normalized to UTC.

`/today-stocks` applies the same rule per reward: an unpriced reward remains in `stocks` with a null price/value and is counted in `unpricedRewards` and `unpricedSymbols`.

Historical rewards use the same explicit-gap rule. In grant-date mode, when `stock_price_history` has no row for a reward's date and terminal symbol, the reward remains in `history` with `priceStatus: "missing"`, `price: null`, `inrValue: null`, and `priceAsOf: null`. With `asOf`, the price is looked up on that date instead. `priceStatus` is `"not_required"` when the effective quantity is zero (for example after a terminal delist or a full reversal); that is a closed position, not a valuation gap. The `/historical-inr` response returns `valuationComplete`, `unpricedRewards`, and `unpricedSymbols` for the user's complete set at the same grant-date or `asOf` reference, not just the requested page. For available prices, `priceStatus` is `"available"` and `priceAsOf` is the history row's last write timestamp normalized to UTC. An `asOf` date is validated against `market_today()` inside the same read snapshot, so it cannot be in the future in any server timezone.

`/stats` returns `todayRewards`, the stock-only `totalPortfolioValue`, the full-set `totalPositions`, `valuationDate`, and the same `cashProceeds`, `cashInLieuProceeds`, and `writtenOffQuantity` totals as `/portfolio`. Its reads run in the same read-only snapshot, so `/stats` and `/portfolio` agree on the active symbol set, total value, position count, and valuation date. `totalPortfolioValue` sums only holdings with an available current price; check `valuationComplete`, `unpricedPositions`, and `unpricedSymbols` before treating it as a complete portfolio value.

---

## Database Schema

The project uses PostgreSQL with the following main tables and views:

- `users`: User information.
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

- `users` -> `rewards` (`user_id`)
- `rewards` -> `ledger` (`reward_id`)
- `rewards` -> `adjustments` (`reward_id`)
- `stock_events` -> `stock_prices` (`stock_symbol`)

### Business dates and timezones

Stocky is an Indian-equity system, so every business date is a market date in **Asia/Kolkata (IST, UTC+05:30, no daylight saving)**. The timezone is fixed in the database and is not read from the server or connection session:

- `market_timezone()` returns the fixed zone name.
- `market_date(timestamptz)` returns the IST calendar date of an absolute instant.
- `market_today()` returns the current IST date and replaces `CURRENT_DATE` in all business logic.

Timestamp and date columns are deliberately typed:

- `rewards.reward_date` is an explicit `DATE` derived from `rewards.created_at` by the `trg_set_reward_date` trigger. Existing reward dates were preserved when the former generated column was converted. Same-day uniqueness and indexes continue to use `reward_date`.
- `rewards.created_at`, `ledger.created_at`, `adjustments.created_at`, `users.created_at`, `stock_prices.updated_at`, and `stock_price_history.created_at` are `TIMESTAMPTZ`. API output converts them to UTC.
- `stock_events.effective_date`, `adjustments.effective_date`, and `stock_price_history.date` are `DATE`. Corporate actions become effective at 00:00 IST on their date, and every event type — including delists — uses the same inclusive boundary.

Migration `0022_business_date_timezone_semantics` interprets legacy `TIMESTAMP` values as IST wall-clock time (`AT TIME ZONE 'Asia/Kolkata'`), so existing business dates do not move. The boundary behavior is covered by `internal/database/timezone_integration_test.go`, which runs the migrations against a disposable database under `UTC`, `Asia/Kolkata`, `America/Los_Angeles`, and `Pacific/Kiritimati` when `TEST_DATABASE_URL` is set.

### Instrument symbol normalization

Migration `0023_case_normalized_instruments` makes instrument symbols canonical across the whole valuation path. `normalize_instrument_symbol(text)` returns `UPPER(BTRIM(text, E' \t\n\r'))`, and the same rule is implemented by `utils.NormalizeStockSymbol` in Go.

- `stock_prices`, `stock_price_history`, `rewards`, `ledger`, and `stock_events` store only canonical symbols. `BEFORE INSERT OR UPDATE` triggers normalize new writes, and `CHECK` constraints reject non-canonical values if a trigger is bypassed.
- Functional unique indexes enforce one current price, one daily history row per date, and one reward per user/symbol/market date after normalization. `stock_events` keeps its existing normalized unique index.
- Valuation, lineage, settlement, and reconciliation joins use exact canonical equality, so `AAPL` and `aapl` cannot multiply rows. Reward creation also looks up the price with an exact symbol match and a deterministic `updated_at DESC, id DESC` tie-break.
- Migration `0023` resolves existing `stock_prices` / `stock_price_history` case variants by keeping the latest row (`updated_at` / `created_at`). It aborts instead of deleting if `rewards` or `stock_events` contain normalized duplicates, because those are financial/audit records (and event deletion cascades to ledger postings).
- The data normalization is one-way; the down migration restores the previous schema behavior but cannot restore original casing.

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

---

## Testing

Run all tests:

```bash
go test ./...
```

Run the PostgreSQL timezone integration tests (they create and drop a disposable database):

```bash
TEST_DATABASE_URL='postgres://postgres:password@localhost:5432/postgres?sslmode=disable' \
  go test ./internal/database -run Integration -v
```

The integration tests are skipped when `TEST_DATABASE_URL` is not set.

Run only the stocky handler tests:

```bash
go test -v github.com/LoganX64/stocky-api/internal/handlers/stocky
```

Test files in `internal/handlers/stocky/`:

- `adjustment_test.go` - Tests adjustment validation and reversal ledger behavior.
- `historical_test.go` - Tests historical endpoint validation and missing-history behavior.
- `portfolio_test.go` - Tests portfolio endpoint validation, cash outcomes, and missing-price holdings.
- `reward_handler_test.go` - Tests reward validation, route registration, idempotency, normalization, and fee ledger entries.
- `stats_test.go` - Tests stats endpoint validation and incomplete-valuation reporting.
- `todays_test.go` - Tests today's stock endpoint validation and missing-price rewards.

---

## Code Structure

- `/cmd/stocky-api/main.go` - API entry point.
- `/cmd/reset-migrations.go` - Migration reset utility.
- `/internal/config/` - Environment configuration.
- `/internal/database/migrations/` - SQL migrations and views.
- `/internal/handlers/stocky/` - HTTP handlers, route setup, and tests.
- `/internal/jobs/` - Background price updater.
- `/internal/storage/models/` - Data models.
- `/internal/utils/` - Rounding and JSON helpers.
- `/internal/utils/response/` - JSON response helpers.
- `Dockerfile` - Docker image instructions.
- `docker-compose.yml` - Docker Compose setup.
- `Stocky-api.postman_collection.json` - Postman collection.

---

## Price Update System

The current price updater is a simulated updater for local development and testing. It does not call a real external market-data API.

Current behavior:

1. Reads symbols and prices from `stock_prices`.
2. Uses `RandomPriceFetcher` to generate a new price from the last stored price.
3. Sometimes simulates a fetch failure and returns a dampened fallback price.
4. Updates `stock_prices` when the generated price differs from the stored price.
5. Inserts or updates the current day's row in `stock_price_history` on every cycle, including when the price is unchanged.
6. Stores recently updated prices in an in-memory `PriceCache`.

Reliability features:

- Worker pool for concurrent symbol processing.
- Buffered-channel semaphore to limit concurrent database writes.
- Retry logic for database updates and history inserts.
- Panic recovery in update cycles and workers.
- Context-aware shutdown.

### Seeding and backfilling price history

Migration `0020_missing_historical_prices` adds a supported database function that fills missing history rows from the latest known `stock_prices` values:

```sql
-- Seed today's missing history rows.
SELECT backfill_price_history();

-- Seed a specific date.
SELECT backfill_price_history('2026-10-27');
```

The function only inserts symbol/date pairs that have no history row and returns the number of rows inserted. It uses the latest stored price, which is intended for local/demo data; real historical prices should be imported from an authoritative market-data source. `/historical-inr` still marks any remaining gap as `priceStatus: "missing"` rather than hiding the reward.

---

## Edge Cases Handled

- **Duplicate rewards** - Prevented with same-day uniqueness and idempotency handling.
- **Missing idempotency key** - Stored as SQL `NULL`; API scans it back as an empty string when returning inserted rows.
- **Stock symbol casing** - All instrument symbols (current prices, price history, rewards, ledger, and stock events) are trimmed and uppercased at write time. Normalized uniqueness and exact canonical joins prevent case variants from duplicating rows, multiplying valuations, or making price lookup nondeterministic.
- **Stock events** - Views account for splits, bonus issues, mergers, and delisting, including merger cash consideration, terminal cash-exit/write-off outcomes, and per-event fractional-share settlement.
- **Adjustments/refunds** - Stored in `adjustments` and reflected in ledger entries.
- **Negative quantities after adjustment** - Prevented by transactional validation.
- **Rounding** - Amounts and quantities are rounded consistently.
- **Missing current prices** - `today_rewards` and `user_portfolio` use a left join to adjusted current prices. Unpriced rewards/holdings remain visible with `priceStatus: "missing"` and null price/value; `/portfolio` and `/stats` return `valuationComplete`, an unpriced count, and `unpricedSymbols`. `totalPortfolioValue` is priced-holdings-only.
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
