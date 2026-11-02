# Phase 0 — Baseline Benchmark

This directory contains the Phase 0 measurement harness for the read-path
projection plan. **Phase 0 changes no application code and adds no schema
migrations.** It measures the current implementation so later phases can be
compared against a fixed baseline.

See `../read-path-projection-plan.md` for the full plan and
`../load-test.md` for the Phase 0 load-test setup and baseline findings.

The test is run with Go. `bench/phase0` is the orchestrator.

## What it measures

- `EXPLAIN (ANALYZE, BUFFERS)` for the current hot queries:
  - portfolio page, totals, freshness, unpriced symbols, corporate-action cash
  - today
  - historical grant-date and as-of modes
- End-to-end latency at three load levels:
  - warm-up: 50 req/s
  - steady: 278 req/s (1M requests/hour)
  - peak: 1,000 req/s
- Endpoint mix: 70% portfolio, 15% today, 10% stats, 5% historical.
- `pg_stat_statements` totals and PostgreSQL settings.

## Prerequisites

- Go 1.25+
- PostgreSQL 17 running locally
- The same `.env` / `.env.local` configuration the API uses, so the Go tools
  can connect without copying a password into the shell

Do **not** point this at development or production data. The seed script
truncates the financial tables in the target database. Phase 0 creates and uses
a dedicated database, `stocky_bench`, by default.

## Run the benchmark

Run from the repository root:

```powershell
cd "D:\Assignment\backup\Stock Reward Management System"

# Fast smoke run: 30s warm-up, 2m steady, 1m peak
go run ./bench/phase0 -quick
```

The smoke run takes roughly 5–10 minutes (plus seed and plan capture).

Full baseline:

```powershell
go run ./bench/phase0
```

Full durations: 60s warm-up at 50 req/s, 10m steady at 278 req/s, 5m peak at
1,000 req/s.

Build once if you do not want `go run` to recompile each time:

```powershell
go build -o .\bench\bin\phase0.exe .\bench\phase0
.\bench\bin\phase0.exe -quick
```

### What the orchestrator does

1. Builds `stocky-api`, `dbtool`, and `loadgen` into `bench/bin/`.
2. Creates `stocky_bench` if it does not exist.
3. Starts the API on `http://127.0.0.1:18080` against `stocky_bench`, with
   market-data jobs disabled.
4. Seeds synthetic data (20,000 users, 500,000 rewards by default).
5. Captures `EXPLAIN (ANALYZE, BUFFERS)` plans.
6. Runs the warm-up, steady, and peak load phases.
7. Captures database settings, connection states, and `pg_stat_statements`.
8. Stops the API.

### Useful options

All options go before the subcommand-style flags shown here; they are normal Go
flags:

```powershell
# Seed and capture plans only, no load test
.\bench\bin\phase0.exe -skip-load

# Skip EXPLAIN capture and go straight to the load test
.\bench\bin\phase0.exe -quick -skip-plans

# Only the 1,000 req/s peak phase, reusing the existing seed
.\bench\bin\phase0.exe -only-peak -skip-plans -skip-seed

# Bound each EXPLAIN statement (default 10m)
.\bench\bin\phase0.exe -quick -explain-timeout 2m

# Different dataset size
.\bench\bin\phase0.exe -quick -users 10000

# Different API port
.\bench\bin\phase0.exe -quick -port 18081

# Different database
.\bench\bin\phase0.exe -quick -database stocky_bench_2
```

## Optional: enable pg_stat_statements

The benchmark runs without it, but `pg-stat-statements.txt` will be missing or
empty. Enabling it needs `shared_preload_libraries` plus a PostgreSQL restart,
which requires an elevated shell. This is a **one-time database setup** step,
not the benchmark runner, and it restarts the whole PostgreSQL service:

```powershell
.\bench\bin\dbtool.exe -database stocky_bench -service postgresql-x64-17 enable-stats
```

The command adds `pg_stat_statements` to `shared_preload_libraries`, restarts
`postgresql-x64-17`, waits for PostgreSQL, and creates the extension. Use
`-service` if your service has a different name.

If you cannot restart PostgreSQL, skip this step. Plans and end-to-end latency
are still captured.

## Run the pieces separately

The orchestrator is convenient, but each step is also a standalone Go command.
This is useful when the API is already running or when you want to rerun only
the load test.

```powershell
# Database setup, seed, plans, statistics
.\bench\bin\dbtool.exe -database stocky_bench setup
.\bench\bin\dbtool.exe -database stocky_bench -users 20000 seed
.\bench\bin\dbtool.exe -database stocky_bench -out bench\results\plans.txt explain
.\bench\bin\dbtool.exe -database stocky_bench -out bench\results\settings.txt settings
.\bench\bin\dbtool.exe -database stocky_bench -out bench\results\activity.txt activity
.\bench\bin\dbtool.exe -database stocky_bench -out bench\results\pg-stat-statements.txt stats
.\bench\bin\dbtool.exe -database stocky_bench sessions
.\bench\bin\dbtool.exe -database stocky_bench stop-sessions
```

Start the API against the benchmark database:

```powershell
$env:POSTGRES_DB = 'stocky_bench'
$env:HTTP_PORT = '18080'
$env:MARKET_DATA_ENABLED = 'false'
$env:MARKET_DATA_BHAVCOPY_ENABLED = 'false'
.\bench\bin\stocky-api.exe
```

Then, in another shell, run only the load generator:

```powershell
.\bench\bin\loadgen.exe `
  -base-url http://127.0.0.1:18080 `
  -users 20000 `
  -quick `
  -out bench\results\manual-load
```

## Dataset

Defaults are sized for a 12 vCPU / 8 GB local machine:

| Relation | Rows |
|---|---:|
| `users` | 20,000 |
| `rewards` | 500,000 |
| `adjustments` | 100,000 |
| `stock_prices` | 3,001 |
| `stock_price_history` | 180,000 |
| `stock_events` | ~315 |

The seed includes splits, bonuses, mergers (including a successor symbol),
cash-exit delists, and write-off delists so the corporate-action lifecycle is
exercised.

The benchmark database shares the same PostgreSQL server, schema migrations,
and server-level settings as the development database, but it contains
synthetic data. It is logically isolated, not physically isolated: heavy load
can still consume CPU, memory, and I/O on the same machine.

## Output

Each run writes a timestamped directory:

```text
bench/results/phase0-YYYYMMDD-HHMMSS/
  summary.json               # machine-readable load results
  summary.txt                # human-readable percentiles
  plans.txt                  # EXPLAIN (ANALYZE, BUFFERS) output
  row-counts.txt             # seeded row counts
  database-settings.txt      # server version, shared_buffers, etc.
  pg-stat-statements.txt     # top queries by total execution time
  db-activity.txt            # connection states after the run
  run-info.txt               # run parameters
  api.out.log / api.err.log  # API logs
```

## Reading the baseline

The current design is expected to fail the 1M/hour target. That is the point:
record the failure, the query plans, and the top SQL statements before changing
anything.

Suggested SLO for later comparison:

| Metric | Target |
|---|---|
| p95 latency | ≤ 200 ms |
| p99 latency | ≤ 500 ms |
| error rate | < 0.1% |
| 5xx responses | 0 under sustained load |

If a plan exceeds `-explain-timeout`, `plans.txt` records the error and the
orchestrator continues to the load test. A timeout is itself a baseline
finding.

## Phase boundary

This harness stops at measurement. Do not add projection tables, rewrite
handlers, or change view definitions here. Those belong to Phase 1 and later.
