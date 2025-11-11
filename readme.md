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
- Robust price update system with caching and fallback mechanisms.
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

### Prerequisites:

- Go >= 1.20
- PostgreSQL
- Git
- golang-migrate v4

### Setup Steps:

1. Clone the repository:

   ```bash
   git clone <repository-url>
   cd stocky-api
   ```

2. Create `config/local.yaml` file:

   ```yaml
   env: 'dev'

   db:
     host: 'localhost'
     port: '5432'
     user: 'your_db_user'
     password: 'your_db_password'
     dbname: 'assignment'
     sslmode: 'disable'

   http_server:
     port: ':8080'
   ```

   Note: You can also use environment variables instead of a config file. The config loader checks for:

   - `CONFIG_PATH` environment variable for the config file path
   - Individual environment variables: `HOST`, `DB_PORT`, `USER`, `PASSWORD`, `DBNAME`, `SSLMODE`, `PORT`

3. Create PostgreSQL database:

   ```sql
   CREATE DATABASE assignment;
   ```

4. Run the API:

   You need to provide the config file path either via command-line flag or environment variable:

   **Option 1: Using command-line flag:**

   ```bash
   go run .\cmd\stocky-api\main.go -config config/local.yaml
   ```

   **Option 2: Using environment variable:**

   ```bash
   set CONFIG_PATH=config/local.yaml
   go run .\cmd\stocky-api\main.go
   ```

   **On Linux/Mac:**

   ```bash
   export CONFIG_PATH=config/local.yaml
   go run ./cmd/stocky-api/main.go
   ```

5. API will run on:
   ```
   http://localhost:8080
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
- `/internal/jobs/` — Background jobs (price updater).
- `/internal/database/migrations/` — SQL migrations for tables and schema.
- `/config/` — Configuration files (local.yaml).
- `Stocky-api.postman_collection.json` — Postman collection for API testing.

---

## Response Format

The API uses a standardized response package for consistent error and success responses across all endpoints.

- **Success responses**: Return JSON with status code 200 and relevant data.
- **Error responses**: Return JSON with appropriate HTTP status code and error message:
  ```json
  {
    "error": "error message here"
  }
  ```

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
