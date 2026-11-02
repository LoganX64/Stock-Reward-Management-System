# Stocky Load Test — Phase 0 Baseline

This document records the Phase 0 load-test setup and the baseline findings for
the Stocky read path. It is separate from
[`read-path-projection-plan.md`](read-path-projection-plan.md): the plan
describes the projection architecture, while this file describes what was
measured before any projection exists.

**Scope:** Phase 0 only. No projection tables, no handler changes, no view
changes. The purpose is to record the current behaviour so later phases can be
compared against a fixed baseline.

---

## 1. What is being tested

The load test exercises the real Gin API against the real PostgreSQL schema.
There is no mocking.

| Item | Value |
|---|---|
| API | `cmd/stocky-api` on `http://127.0.0.1:18080` |
| Database | `stocky_bench` (dedicated benchmark database) |
| Schema | Same migrations as the application |
| Data | Synthetic, deterministic seed |
| Market-data jobs | Disabled |
| Load generator | `bench/loadgen` (Go) |
| Orchestrator | `bench/phase0` (Go) |

The benchmark database lives on the same PostgreSQL server as the development
database. It shares server-level settings and hardware, but it contains
synthetic data. It is **logically isolated, not physically isolated**: heavy
load can still consume CPU, memory, and I/O on the machine.

---

## 2. Environment

| Item | Value |
|---|---|
| OS | Windows |
| CPU | 12 vCPU |
| RAM | 8 GB |
| Go | 1.25.1 |
| PostgreSQL | 17.6 |
| `shared_buffers` | 128 MB |
| `max_connections` | 100 |
| `shared_preload_libraries` | empty (no `pg_stat_statements`) |
| API DB pool | `MaxOpenConns=25`, `MaxIdleConns=10` |
| API DB lifetime | 30 minutes max, 5 minutes max idle |

`pg_stat_statements` is optional. It requires a PostgreSQL restart, so the
current baseline does not include aggregated SQL statistics.

---

## 3. Dataset

Seeded into `stocky_bench` by `bench/seed/seed_bench.sql` through
`bench/dbtool seed`.

| Relation | Rows |
|---|---:|
| `users` | 20,000 |
| `rewards` | 500,000 |
| `adjustments` | 100,000 |
| `stock_prices` | 3,001 |
| `stock_price_history` | 180,000 |
| `stock_events` | 515 |
| `ledger` | 0 |

Seed completion time: **15.807 s**.

The seed includes splits, bonuses, mergers (including a successor symbol),
cash-exit delists, and write-off delists, so the corporate-action lifecycle is
exercised.

---

## 4. Load profile

### Endpoint mix

| Endpoint | Share |
|---|---:|
| `GET /api/v1/portfolio/{userID}` | 70% |
| `GET /api/v1/today-stocks/{userID}` | 15% |
| `GET /api/v1/stats/{userID}` | 10% |
| `GET /api/v1/historical-inr/{userID}` | 5% |

### Phases

Quick run (`-quick`):

| Phase | Rate | Duration |
|---|---:|---:|
| Warm-up | 50 req/s | 30 s |
| Steady | 278 req/s (1M/hour) | 2 min |
| Peak | 1,000 req/s | 1 min |

Full run:

| Phase | Rate | Duration |
|---|---:|---:|
| Warm-up | 50 req/s | 60 s |
| Steady | 278 req/s | 10 min |
| Peak | 1,000 req/s | 5 min |

Each virtual worker owns a ticker and a random user ID. With 512 workers and a
target rate `R`, each worker ticks every `512 / R` seconds, so the aggregate
rate is approximately `R` while allowing up to 512 concurrent requests.

### Target SLO

| Metric | Target |
|---|---|
| p95 latency | ≤ 200 ms |
| p99 latency | ≤ 500 ms |
| Error rate | < 0.1% |
| 5xx responses | 0 under sustained load |

---

## 5. How to run

Use the Go orchestrator from the repository root.

```powershell
# Quick baseline (plans + load test)
go run ./bench/phase0 -quick

# Full baseline
go run ./bench/phase0

# Load test only; skip plan capture
go run ./bench/phase0 -quick -skip-plans

# Only the 1,000 req/s peak phase, reusing existing data
go run ./bench/phase0 -only-peak -skip-plans -skip-seed

# Plans only; no load test
go run ./bench/phase0 -skip-load
```

Build once if preferred:

```powershell
go build -o .\bench\bin\phase0.exe .\bench\phase0
.\bench\bin\phase0.exe -quick
```

The orchestrator builds the API, `dbtool`, and `loadgen`; creates the
benchmark database; starts the API against it; seeds; captures plans; runs the
load phases; captures statistics; and stops the API.

Useful flags: `-users`, `-port`, `-database`, `-skip-plans`, `-skip-load`,
`-explain-timeout`, `-output-root`.

---

## 6. Baseline findings — query plans

Captured with `EXPLAIN (ANALYZE, BUFFERS)` against the seeded dataset for
`user_id = 1`. Artifact: `bench/results/manual-phase0/plans.txt`.

### Measured execution times

| Query file | Statement | Execution time |
|---|---|---:|
| `historical.sql` | grant-date mode | **387 ms** |
| `historical.sql` | as-of mode (30 days ago) | **693 ms** |
| `portfolio_cash.sql` | corporate-action cash | **87,975 ms** (1m 28s) |
| `portfolio_freshness.sql` | freshness fields | **60,059 ms** (1m 00s) |
| `portfolio_page.sql` | first page | **59,370 ms** (59.4s) |
| `portfolio_totals.sql` | totals | **59,536 ms** (59.5s) |
| `portfolio_unpriced.sql` | unpriced symbols | **58,475 ms** (58.5s) |
| `today.sql` | today's rewards | **46,433 ms** (46.4s) |

### Root cause

Every `user_portfolio` query plans as:

```text
GroupAggregate (25 rows)
  -> Nested Loop
       -> Seq Scan on rewards r          rows=500,000
       -> Function Scan on reward_action_settlements
                                        loops=500,000
```

The view chain is:

```text
user_portfolio
  -> reward_corporate_action_outcomes
       -> rewards r
       CROSS JOIN LATERAL reward_action_settlement(r.id, market_today())
```

`reward_action_settlement` is a PL/pgSQL function, so PostgreSQL cannot inline
it and cannot push the outer `user_id = $1` filter through it. A single user's
page therefore evaluates the corporate-action lifecycle for **all 500,000
rewards**, then discards 499,975 rows.

The same full scan is repeated for each of the five queries the portfolio
handler runs on its repeatable-read snapshot:

1. page
2. totals
3. corporate-action cash
4. unpriced symbols
5. freshness

That is roughly **5 × 60 s ≈ 5 minutes of database work per portfolio
request** if the request is not cancelled first.

### Why historical is fast

`historical_rewards_as_of` is a SQL function and its `user_id` predicate is
pushed into `reward_rows`. The plan still seq-scans `rewards`, but the
per-row lifecycle function runs only **25 times** (`loops=25`), not 500,000
times. Result: 387–693 ms instead of 60 seconds.

This is the key architectural clue for Phase 1: the problem is not PostgreSQL
itself, it is the uncorrelated per-reward function call in the current views.

### Capacity implication

With the 25-connection API pool:

- one portfolio query takes ~60 s, so 25 connections can complete roughly
  **0.42 queries/s**;
- five serial queries per portfolio request reduce that to roughly
  **0.08 portfolio requests/s**;
- the 278 req/s steady target would require thousands of concurrent query
  slots, not 25;
- the 10-second client timeout will cancel requests long before the database
  finishes them, so the load test is expected to record client timeouts and
  very low successful throughput.

These are predictions from the measured plans. The load phases record what the
end-to-end API actually does.

---

## 7. Load test results

### Run A — quick profile, stopped during peak

Run: `bench/results/phase0-20261102-233306` (plans skipped). The run was
stopped during the peak phase, so the load generator did not write
`summary.json` / `summary.txt`. The numbers below are from its streamed console
output. Warm-up and steady completed; peak has no data.

| Phase | Target req/s | Duration | Requests | Achieved req/s | Errors | Error rate | p50 | p95 | p99 | Max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| warm-up | 50 | 30 s | 983 | 32.8 | 983 | 100% | 10,054.7 ms | 13,902.4 ms | 16,119.7 ms | 17,933.4 ms |
| steady | 278 | 120 s | 6,489 | 54.1 | 6,489 | 100% | 10,005.1 ms | 10,585.9 ms | 11,674.1 ms | 12,112.8 ms |
| peak | 1,000 | 60 s | — | — | — | — | — | — | — | — |

### Run B — peak only

Run: `bench/results/phase0-20261102-234039` (1,000 req/s for 60 s, seeded data
reused, plans skipped, no warm-up or steady phase).

| Phase | Target req/s | Duration | Requests | Achieved req/s | Errors | Error rate | p50 | p95 | p99 | Max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| peak | 1,000 | 60 s | 3,079 | 51.3 | 3,076 | 99.90% | 10,025.7 ms | 13,348.5 ms | 15,751.5 ms | 15,784.6 ms |

Status codes: **3 × HTTP 200**, 3,076 transport/timeout errors.

Endpoint counts: portfolio 2,161 · today 441 · stats 322 · historical 155.

### Interpretation

- **Offered load barely changes completed throughput.** The steady phase
  achieved 54.1 req/s and the peak-only phase achieved 51.3 req/s. Raising the
  target from 278 to 1,000 req/s did not raise throughput; the system was
  already saturated.
- The generator itself caps out at roughly `512 workers / 10 s ≈ 51 req/s`
  when every request reaches the 10-second timeout. The measured rate matching
  that ceiling is another sign that essentially nothing completes promptly.
- The peak run had only **3 successful requests out of 3,079** — a 99.90% error
  rate, versus the 0.1% SLO.
- The 10-second HTTP client timeout dominates p50. Values above 10 s reflect
  client-side scheduling and connection overhead while the benchmark machine is
  saturated.
- **The failure is not limited to the portfolio endpoint.** Although 70% of
  requests are portfolio, the 25-connection pool is fully occupied by
  long-running portfolio queries. Requests for the normally fast historical
  endpoint queue behind them and also time out. This is head-of-line blocking
  at the connection pool, not just slow individual handlers.
- After the peak run, **25 orphaned `today_rewards` queries were still running**
  even though the API process had been killed. Closing the client socket does
  not immediately cancel a CPU-bound PostgreSQL query. They were terminated
  manually, and `bench/phase0` now calls `dbtool stop-sessions` during cleanup so
  a run cannot leave the machine saturated after it ends.

The measured plan times in section 6 explain the result: each portfolio request
issues five serial queries that each scan 500,000 rewards and call the
lifecycle function 500,000 times. The pool cannot absorb that work, and the
client timeout cancels requests long before the database finishes them.

---

## 8. Artifacts

| Artifact | Path |
|---|---|
| Query plans | `bench/results/manual-phase0/plans.txt` |
| Seed script | `bench/seed/seed_bench.sql` |
| Query files | `bench/queries/*.sql` |
| Orchestrator | `bench/phase0/main.go` |
| Load generator | `bench/loadgen/main.go` |
| Database helper | `bench/dbtool/main.go` |
| Run output (quick, stopped) | `bench/results/phase0-20261102-233306/` |
| Run output (peak only) | `bench/results/phase0-20261102-234039/` |
| Run output (new runs) | `bench/results/phase0-YYYYMMDD-HHMMSS/` |

Typical run output:

```text
bench/results/phase0-YYYYMMDD-HHMMSS/
  summary.json
  summary.txt
  plans.txt
  row-counts.txt
  database-settings.txt
  pg-stat-statements.txt
  db-activity.txt
  run-info.txt
  api.out.log
  api.err.log
```

---

## 9. Phase boundary

Phase 0 stops at measurement. The next phase must remove the per-reward
lifecycle work from the read path by maintaining projection tables, while
keeping the base tables authoritative and retaining the existing view names and
columns for compatibility. That work belongs to Phase 1 and is not part of this
baseline.
