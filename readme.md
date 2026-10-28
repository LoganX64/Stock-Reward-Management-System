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
| GET    | `/api/v1/historical-inr/:userId` | Get historical INR valuation before today. |
| GET    | `/api/v1/stats/:userId`          | Get today's rewards and portfolio value.   |
| GET    | `/api/v1/portfolio/:userId`      | Get portfolio details per stock.           |

### Pagination

The historical INR and portfolio endpoints support `limit` and `offset` query parameters:

```text
GET /api/v1/historical-inr/:userId?limit=100&offset=0
GET /api/v1/portfolio/:userId?limit=100&offset=0
```

- `limit` defaults to `100`.
- `limit` is capped at `500`.
- `offset` defaults to `0`.
- `limit` must be a positive integer.
- `offset` must be zero or a positive integer.

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
  "history": []
}
```

Portfolio responses use the same pagination metadata:

```json
{
  "userId": 1,
  "limit": 100,
  "offset": 0,
  "portfolio": [],
  "valuationComplete": true,
  "unpricedPositions": 0,
  "unpricedSymbols": [],
  "cashProceeds": 0,
  "cashInLieuProceeds": 0,
  "writtenOffQuantity": 0
}
```

`cashProceeds` is the total cash received from merger consideration, cash-in-lieu fraction settlement, and cash-exit delistings. `cashInLieuProceeds` is the subset produced by fractional-share settlement. `writtenOffQuantity` is the quantity removed by write-off delistings. All three are zero when the user has no terminal corporate-action outcomes.

A holding is never dropped because its current price is missing. When no `stock_prices` row exists for a holding's terminal symbol, the row is still returned with `priceStatus: "missing"`, `currentPrice: null`, `inrValue: null`, and `priceAsOf: null`. The response then reports `valuationComplete: false`, `unpricedPositions`, and the affected `unpricedSymbols`. For priced holdings, `priceStatus` is `"available"`, and `priceAsOf` contains the price row's last update timestamp.

`/today-stocks` applies the same rule per reward: an unpriced reward remains in `stocks` with a null price/value and is counted in `unpricedRewards` and `unpricedSymbols`.

`/stats` returns `todayRewards`, the stock-only `totalPortfolioValue`, and the same `cashProceeds`, `cashInLieuProceeds`, and `writtenOffQuantity` totals. `totalPortfolioValue` sums only holdings with an available current price; check `valuationComplete`, `unpricedPositions`, and `unpricedSymbols` before treating it as a complete portfolio value.

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
- `reward_corporate_action_outcomes` (VIEW): Per-reward terminal outcome, total cash proceeds, cash-in-lieu proceeds, fraction quantity, and written-off quantity.
- `user_corporate_action_cash` (VIEW): Per-user totals of corporate-action cash, cash-in-lieu proceeds, and written-off quantity.
- `today_rewards` (VIEW): Today's reward values with explicit current-price status.
- `historical_rewards` (VIEW): Historical reward values.
- `user_portfolio` (VIEW): Aggregated current portfolio holdings, including holdings whose current price is missing.
- `reward_ledger_reconciliation` (VIEW): Per-reward stock, cash, adjustment, and fee reconciliation.

### Key Relationships

- `users` -> `rewards` (`user_id`)
- `rewards` -> `ledger` (`reward_id`)
- `rewards` -> `adjustments` (`reward_id`)
- `stock_events` -> `stock_prices` (`stock_symbol`)

### Corporate actions and adjustment timing

- Raw prices remain in `stock_prices` and `stock_price_history`. Split/bonus factors are applied by the adjusted views, never written back.
- A 2:1 forward split or 1:1 bonus uses ratio `2/1`; a 1:5 reverse split uses `1/5`. Only `split` and `bonus` events affect prices.
- Merger lineage is resolved recursively. A reward in A follows A→B→C, composing every merger ratio and applying split/bonus events only while the holding is in each successor instrument.
- Merger events must have a distinct successor and positive ratios. A database trigger rejects lineage cycles, and each predecessor may have only one merge event.
- Corporate-action symbols are trimmed and uppercased before storage. A unique index on normalized symbol, event type, and effective date rejects case, whitespace, and same-day duplicate events.
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
- `reward_corporate_action_outcomes` is the shared per-reward outcome source for `today_rewards` and `user_portfolio`; `/portfolio` and `/stats` therefore return the same cash and write-off totals. `totalPortfolioValue` remains stock-value-only.
- `historical_rewards` is a reward-date point-in-time valuation using the original quantity and the adjusted reward-date price.
- Adjustments take effect on `adjustments.effective_date` (currently the creation date). Quantity movements are normalized to reward-date units through the merger chain, so a post-split or post-merger correction is not scaled twice.
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

Run only the stocky handler tests:

```bash
go test -v github.com/LoganX64/stocky-api/internal/handlers/stocky
```

Test files in `internal/handlers/stocky/`:

- `adjustment_test.go` - Tests adjustment validation and reversal ledger behavior.
- `historical_test.go` - Tests historical endpoint validation.
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
4. Updates `stock_prices`.
5. Inserts or updates the current day's row in `stock_price_history`.
6. Stores recently updated prices in an in-memory `PriceCache`.

Reliability features:

- Worker pool for concurrent symbol processing.
- Buffered-channel semaphore to limit concurrent database writes.
- Retry logic for database updates and history inserts.
- Panic recovery in update cycles and workers.
- Context-aware shutdown.

---

## Edge Cases Handled

- **Duplicate rewards** - Prevented with same-day uniqueness and idempotency handling.
- **Missing idempotency key** - Stored as SQL `NULL`; API scans it back as an empty string when returning inserted rows.
- **Stock symbol casing** - Reward creation normalizes symbols with trimming and uppercase conversion.
- **Stock events** - Views account for splits, bonus issues, mergers, and delisting, including merger cash consideration, terminal cash-exit/write-off outcomes, and per-event fractional-share settlement.
- **Adjustments/refunds** - Stored in `adjustments` and reflected in ledger entries.
- **Negative quantities after adjustment** - Prevented by transactional validation.
- **Rounding** - Amounts and quantities are rounded consistently.
- **Missing current prices** - `today_rewards` and `user_portfolio` use a left join to adjusted current prices. Unpriced rewards/holdings remain visible with `priceStatus: "missing"` and null price/value; `/portfolio` and `/stats` return `valuationComplete`, an unpriced count, and `unpricedSymbols`. `totalPortfolioValue` is priced-holdings-only.
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
