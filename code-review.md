# Code Review

## Findings

### P0 - Resolved: `go.mod` is now parseable

**Location:** `go.mod`

The duplicate `module` and `go` directives were removed. The module now has one canonical path:

- `module github.com/LoganX64/stocky-api`

It also has one Go directive:

- `go 1.25.1`

The direct dependencies were consolidated into one block, and `go-sqlmock` was corrected to the published `v1.5.2` version with matching `go.sum` entries.

**Verification:** `go list ./...` now loads and lists all project packages, confirming the module parse blocker is fixed.

### P1 - `InsertReward` does not match the actual `rewards` schema

**Location:** `internal/handlers/stocky/reward_handler.go:38`

`InsertReward` inserts into `rewards (user_id, stock_id, reward_date, idempotency_key, quantity, created_at)`, but the migration defines `rewards` with `stock_symbol` instead of `stock_id`, and `reward_date` is a generated column. If this helper is used against PostgreSQL, the insert will fail.

**Recommendation:** Either remove this test-only helper or update it to use the production schema: `user_id`, `stock_symbol`, `quantity`, `idempotency_key`, and `created_at`.

### P1 - Portfolio quantities are over-counted when a reward has multiple adjustments

**Location:** `internal/database/migrations/0006_create_user_portfolio_view.up.sql:31`, `internal/database/migrations/0006_create_user_portfolio_view.up.sql:36`

The `user_portfolio` view joins `rewards` to `adjustments` and then sums `r.quantity`. A reward with two adjustment rows appears twice in the join, so its quantity and INR value are counted twice.

**Recommendation:** Aggregate adjustments separately by `reward_id` before joining them to rewards, or compute portfolio quantity from rewards in a CTE that is not multiplied by the adjustment join.

### P1 - Reward creation accepts negative quantities without holding checks

**Location:** `internal/handlers/stocky/reward_handler.go:55`, `internal/handlers/stocky/reward_handler.go:188`

`CreateReward` only rejects zero quantity. A client can post a negative reward, which is treated as a reversal and creates a cash inflow, but it does not verify that the user has enough existing quantity to reverse. The adjustment endpoint has a negative-balance check, but this path bypasses it.

**Recommendation:** Reject negative quantities in `CreateReward`, or move reversal behavior to a dedicated endpoint that locks and verifies current holdings before writing ledger entries.

### P2 - Concurrent adjustments can bypass the negative quantity check

**Location:** `internal/handlers/stocky/adjustment_handler.go:69`, `internal/handlers/stocky/adjustment_handler.go:84`, `internal/handlers/stocky/adjustment_handler.go:116`

The adjustment handler reads the current reward quantity, checks whether the delta would make it negative, and later updates the row. Without row locking, two concurrent requests can both pass the check using the same starting quantity and then both update the row.

**Recommendation:** Lock the reward row with `SELECT ... FOR UPDATE` inside the transaction, or make the update conditional with `WHERE quantity + $1 >= 0` and check the affected row count.

### P2 - Historical invalid-user test calls the wrong route

**Location:** `internal/handlers/stocky/historical_test.go:17`, `internal/handlers/stocky/historical_test.go:25`

The test registers `/api/v1/historical-inr/:userId` but sends the request to `/api/v1/historical/abc`. After `go.mod` is fixed, this test will receive a 404 instead of the expected 400.

**Recommendation:** Change the request path to `/api/v1/historical-inr/abc`.

## Verification

Command run for the P0 fix:

```text
go list ./...
```

Result: the module loaded and all packages were listed. Full test execution is intentionally left for the next finding because the next known failure is the historical route test called out above.
