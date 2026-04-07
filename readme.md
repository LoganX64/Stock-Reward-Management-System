# Stock Reward Management System (Golang)

## Project Overview

Stocky is a stock rewards management API built in **Golang** using the **Gin** framework and **PostgreSQL**.  
It allows tracking of rewards in Indian stocks, applying adjustments, and calculating the portfolio value in INR.  
The system maintains a **double-entry ledger** to track stock units, INR outflows, and company-incurred fees.

### Technologies Used:

- **Go** (Golang) - Backend programming language
- **Gin** - Web framework for HTTP routing and middleware
- **PostgreSQL** - Relational database
- **golang-migrate** - Database migration tool
- **logrus** - Structured logging
- **go-playground/validator** - Input validation
- **lib/pq** - PostgreSQL driver

### Key Features:

- Record stock rewards for users with idempotency support.
- Track adjustments and refunds for previous rewards (reversals, fee refunds, manual corrections).
- Maintain a double-entry ledger for stock units, cash flows, and fees.
- Automatic fee calculation (brokerage, STT, GST) for positive rewards.
- Fetch latest stock prices and calculate INR valuations.
- Support stock splits, mergers, bonus issues, and delisting events.
- Provide historical and portfolio statistics.
- Standardized response handling across all endpoints.
- Request ID tracking for better debugging and logging.
- Background Go worker for automatic price updates with caching and fallback mechanisms.
- Graceful handling of external API downtime.

---

## API Endpoints

| Method | Endpoint                         | Description                                  |
| ------ | -------------------------------- | -------------------------------------------- |
| GET    | `/health`                        | Health check endpoint.                       |
| POST   | `/api/v1/reward`                 | Create a reward entry.                       |
| GET    | `/api/v1/today-stocks/:userId`   | Fetch rewards for today with adjustments.    |
| GET    | `/api/v1/historical-inr/:userId` | Get historical INR valuation (before today). |
| GET    | `/api/v1/stats/:userId`          | Get total today rewards and portfolio value. |
| GET    | `/api/v1/portfolio/:userId`      | Get portfolio details per stock.             |
| POST   | `/api/v1/adjustments/:id`        | Apply adjustment to a reward.                |

---

## Database Schema

The project uses PostgreSQL with the following tables:

- `users`: User information.
- `rewards`: Records reward events.
- `ledger`: Double-entry ledger tracking stock units, INR outflow, and fees.
- `stock_prices`: Latest stock prices.
- `stock_events`: Tracks stock splits, mergers, bonus issues, delisting.
- `adjustments`: Tracks manual corrections, fee refunds, or reward reversals.
- `user_portfolio` (VIEW): Aggregates portfolio holdings with adjustments applied.

### Key Relationships:

- `users` → `rewards` (user_id)
- `rewards` → `ledger` (reward_id)
- `rewards` → `adjustments` (reward_id)
- `stock_events` → `stock_prices` (stock_symbol)
- `user_portfolio` aggregates all relevant data.

---

## Running the Project

### Prerequisites

- **For Local Development:**
  - Go (version 1.25 or later) installed
  - PostgreSQL installed and running locally
  - Create a database named `assignment` (or update in `.env.local`)

- **For Docker:**
  - Docker & Docker Compose installed

### Running Locally

1. **Set up PostgreSQL:**
   - Ensure PostgreSQL is running with user `postgres`, password `9908` (or update `.env.local`).
   - Create database: `assignment`.

2. **Clone and Run:**

   ```bash
   git clone <repository-url>
   cd stocky-api
   go run ./cmd/stocky-api/main.go
   ```

3. **Access the API:**
   - API available at: `http://localhost:8080`
   - Background price updater (Go worker) runs automatically to fetch and update stock prices.

### Using Docker

1. **Build and Start Services:**

   ```bash
   docker-compose up --build
   ```

   - For detached mode: `docker-compose up --build -d`

2. **Access the API:**
   - API available at: `http://localhost:8080`
   - Database: PostgreSQL on port 5432
   - Background price updater runs inside the container.

### Environment Variables

- **Local (`.env.local`):**
  - `DB_HOST=localhost`
  - `POSTGRES_USER=postgres`
  - `POSTGRES_PASSWORD=9908`
  - `POSTGRES_DB=assignment`
  - `DB_PORT=5432`
  - `HTTP_PORT=8080`
  - `MIGRATION_PATH=./internal/database/migrations`

- **Docker (`.env.docker`):**
  - `DB_HOST=db`
  - `POSTGRES_USER=stocky_user`
  - `POSTGRES_PASSWORD=stocky_pass`
  - `POSTGRES_DB=stocky`
  - `DB_PORT=5432`
  - `HTTP_PORT=8080`
  - `MIGRATION_PATH=/app/internal/database/migrations`

The application automatically loads the appropriate `.env` file based on the `ENV_FILE` environment variable.

- `DB_HOST` — Database host (PostgreSQL service)
- `DB_PORT` — Database port (default: 5432)
- `DB_USER` — PostgreSQL user
- `DB_PASSWORD` — PostgreSQL password
- `DB_NAME` — Database name -`PORT` — API port (default: 8080)

---

## Testing

The project includes unit tests for the API handlers to ensure correctness of reward processing, adjustments, and portfolio calculations.

### Test Files

The test files are located in the `internal/handlers/stocky/` directory:
- `adjustment_test.go` — Tests for manual corrections and reversals.
- `historical_test.go` — Tests for historical INR valuation.
- `portfolio_test.go` — Tests for portfolio aggregation.
- `reward_test.go` — Tests for reward creation and fee calculation.
- `stats_test.go` — Tests for overall statistics.
- `todays_test.go` — Tests for today's stock rewards.

### Running Tests

To run all tests in the stocky handler package, use the following command:

```bash
go test -v github.com/LoganX64/stocky-api/internal/handlers/stocky
```

---

## Code Structure

- `/cmd/stocky-api/main.go` — Entry point of the application.
- `/cmd/reset-migrations.go` — Utility to reset database migrations.
- `/cmd/seed/` — Database seeding utilities.
- `/internal/handlers/stocky/` — API route definitions and handlers.
  - `routes.go` — Route configuration and middleware.
  - `reward_handler.go` — Reward creation endpoints.
  - `adjustment_handler.go` — Adjustment/reversal endpoints.
  - `portfolio_handler.go` — Portfolio retrieval endpoints.
  - `today_handler.go` — Today's stocks endpoints.
  - `historical_handler.go` — Historical data endpoints.
  - `stats_handler.go` — Statistics endpoints.
- `/internal/storage/models/` — Database models and data structures.
- `/internal/config/` — Configuration management.
- `/internal/utils/response/` — Standardized HTTP response utilities.
  - `response.go` — Response formatting functions (WriteJson, ErrorResponse, etc.).
- `/internal/utils/` — Utility functions (rounding, JSON helpers).
- `/internal/jobs/` — Background jobs (price updater worker).
- `/internal/database/migrations/` — SQL migrations for tables and schema.
- `Dockerfile` — Docker image instructions
- `docker-compose.yml` — Docker Compose setup
- `Stocky-api.postman_collection.json` — Postman collection for API testing.

---

    depends_on:
      - db

volumes:
db-data:

```

# Start services:

```

docker-compose up -d

````

## Response Format

The API uses a standardized response package for consistent error and success responses across all endpoints.

- **Success responses**: Return JSON with status code 200 and relevant data.
- **Error responses**: Return JSON with appropriate HTTP status code and error message:
  ```json
  {
    "error": "error message here"
  }
````

### Response Package Functions:

- `WriteJson()` — Writes JSON responses with proper headers.
- `ErrorResponse()` — Creates standardized error responses.

## System Resilience & Edge Cases

### Price Update System

The system includes a robust price update mechanism with:

1. Hourly automatic updates
2. In-memory price caching
3. Multiple fallback layers:
   - External API attempt
   - Recent cached prices
   - Last known good price
4. Staleness tracking
5. Configurable retry mechanisms

### Resilience Features

- Automatic retries on failure
- Cache with configurable staleness threshold
- Graceful degradation of price accuracy
- Clear logging of fallback usage
- Transaction safety

### Edge Cases Handled

- **Duplicate rewards** — Prevented via date and user checks with idempotency keys.
- **Stock events** — Handles splits, mergers, bonus issues, and delisting.
- **Adjustments/refunds** — Tracked in `adjustments` table with validation.
- **Rounding errors** — Proper rounding using `RoundAmount()` and `RoundQuantity()` utilities.
- **Price API downtime** — Robust fallback system with caching and graceful degradation.
- **Negative quantities** — Prevented through validation before adjustments.
- **Transaction safety** — All operations use database transactions for data consistency.
- **Data staleness** — Tracking and handling of stale price data with clear indicators.

---

## Author

Developed by Jitin K

---

## License

MIT License
