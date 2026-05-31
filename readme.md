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
- Support stock splits, bonus issues, mergers, and delisting.
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
  "portfolio": []
}
```

---

## Database Schema

The project uses PostgreSQL with the following main tables and views:

- `users`: User information.
- `rewards`: Reward events with optional idempotency keys.
- `ledger`: Stock units, INR outflows, and fee entries.
- `stock_prices`: Latest stock prices.
- `stock_price_history`: Daily stock price history.
- `stock_events`: Splits, bonus issues, mergers, and delisting events.
- `adjustments`: Manual corrections, fee refunds, and reward reversals.
- `today_rewards` (VIEW): Today's reward values.
- `historical_rewards` (VIEW): Historical reward values.
- `user_portfolio` (VIEW): Aggregated current portfolio holdings.

### Key Relationships

- `users` -> `rewards` (`user_id`)
- `rewards` -> `ledger` (`reward_id`)
- `rewards` -> `adjustments` (`reward_id`)
- `stock_events` -> `stock_prices` (`stock_symbol`)

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
- `portfolio_test.go` - Tests portfolio endpoint validation.
- `reward_handler_test.go` - Tests reward validation, route registration, idempotency, normalization, and fee ledger entries.
- `stats_test.go` - Tests stats endpoint validation.
- `todays_test.go` - Tests today's stock endpoint validation.

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
- **Stock events** - Views account for splits, bonus issues, mergers, and delisting.
- **Adjustments/refunds** - Stored in `adjustments` and reflected in ledger entries.
- **Negative quantities after adjustment** - Prevented by transactional validation.
- **Rounding** - Amounts and quantities are rounded consistently.
- **Transaction safety** - Reward creation and adjustment workflows use transactions.

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
