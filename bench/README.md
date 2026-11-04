# Benchmark Harness

This directory contains the measurement harness for the read-path projection
plan. Phase 0 established the baseline against the original computational views;
Phase 2 added write-overhead and live-rebuild checks for the maintained
projections; Phase 3 rewrote the read path and replaced the portfolio query set
with the handler's two-query page-plus-summary shape.

See `../read-path-projection-plan.md` for the full plan and
`../load-test.md` for the Phase 0 load-test setup and baseline findings.

The test is run with Go. `bench/phase0` is the orchestrator.

## What it measures

- `EXPLAIN (ANALYZE, BUFFERS)` for the current hot queries:
  - portfolio page and the full-set portfolio summary
  - stats today rewards
  - today rewards
  - historical grant-date and as-of modes through
    `historical_rewards_for_user()`, including its full-set unpriced summary
- End-to-end latency at three load levels:
  - warm-up: 50 req/s
  - steady: 278 req/s (1M requests/hour)
  - peak: 1,000 req/s (default), or 2,000 req/s with `-profile peak-2x-hour`
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

1. Builds `stocky-api`, `dbtool`, `loadgen`, and `rebuild-projections` into
   `bench/bin/`.
2. Drops and recreates `stocky_bench` (unless `-skip-seed` is used), so the API
   applies the full migration set to an empty database.
3. Starts the API on `http://127.0.0.1:18080` against `stocky_bench`, with
   market-data jobs disabled.
4. Seeds synthetic data (20,000 users, 500,000 rewards by default) with
   projection triggers suppressed.
5. Rebuilds the read projections with `rebuild-projections --online`.
6. Captures `EXPLAIN (ANALYZE, BUFFERS)` plans.
7. Runs the warm-up, steady, and peak load phases.
8. Captures database settings, connection states, `pg_stat_statements`, and
   the API's Prometheus `/metrics` output.
9. Stops the API.

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

## Phase 6 one-hour profiles

The built-in profiles all start with a 60 s warm-up at 50 req/s:

| Profile | Steady | Peak | Purpose |
|---|---|---|---|
| `-quick` | 2m @ 278 req/s | 1m @ 1,000 req/s | smoke test |
| `steady-hour` | 1h @ 278 req/s | — | 1M/hour proof |
| `peak-2x-hour` | — | 1h @ 2,000 req/s | 2× design peak |

Steady proof on one API:

```powershell
go run ./bench/phase0 -profile steady-hour -app-role api `
  -skip-seed -skip-plans -skip-rebuild
```

Peak proof with three APIs behind `bench/lb`, then a 10% reconciliation capped
at 2,000 users:

```powershell
go run ./bench/phase0 -profile peak-2x-hour `
  -app-role api -api-instances 3 `
  -db-max-open-conns 6 -db-max-idle-conns 6 `
  -skip-seed -skip-plans -skip-rebuild `
  -reconcile -reconcile-sample-percent 10 -reconcile-max-users 2000
```

`-api-instances` starts that many `stocky-api` processes on consecutive ports
starting at `-port`, plus `bench/lb` on `-lb-port`; the load generator targets
the load balancer. `-db-max-open-conns` and `-db-max-idle-conns` override the API
pool for the run. `-api-gomaxprocs`, `-loadgen-gomaxprocs`, and `-lb-gomaxprocs`
limit the Go scheduler per process; change one variable at a time.

`-reconcile` runs `dbtool reconcile` after the load and writes
`reconciliation.txt` into the result directory. Bound the sample with
`-reconcile-sample-percent`, `-reconcile-max-users`, `-reconcile-max-details`,
and `-reconcile-timeout`.

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
# Database setup, seed, projections, plans, statistics
.\bench\bin\dbtool.exe -database stocky_bench -recreate setup
.\bench\bin\dbtool.exe -database stocky_bench -users 20000 seed
$env:POSTGRES_DB = 'stocky_bench'
.\bench\bin\rebuild-projections.exe --online
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

## Phase 5 multi-instance checks

Phase 5 adds `ETag`/`304 Not Modified`, a short-lived per-process response
cache, and per-user read rate limiting. The cache and limiter are deliberately
local to each API process; PostgreSQL is the only shared state.

Build the API and the local load balancer:

```powershell
go build -o .\bench\bin\stocky-api.exe .\cmd\stocky-api
go build -o .\bench\bin\lb.exe .\bench\lb
```

Apply migrations exactly once, using the migration-only role:

```powershell
$env:POSTGRES_DB = 'stocky_bench'
$env:APP_ROLE = 'migrate'
$env:RUN_MIGRATIONS_ON_START = 'true'
.\bench\bin\stocky-api.exe
```

Start three API instances in separate shells. Each shell needs its own
`HTTP_PORT`; `APP_ROLE=api` prevents background jobs and disables startup
migrations:

```powershell
$env:POSTGRES_DB = 'stocky_bench'
$env:APP_ROLE = 'api'
$env:RUN_MIGRATIONS_ON_START = 'false'
$env:MARKET_DATA_ENABLED = 'false'
$env:MARKET_DATA_BHAVCOPY_ENABLED = 'false'
$env:HTTP_PORT = '18080'   # use 18082 and 18083 in the other shells
.\bench\bin\stocky-api.exe
```

Start the load balancer:

```powershell
.\bench\bin\lb.exe `
  -listen 127.0.0.1:18081 `
  -backends http://127.0.0.1:18080,http://127.0.0.1:18082,http://127.0.0.1:18083 `
  -strategy round-robin
```

Then run the normal load generator against `http://127.0.0.1:18081`. Use round
robin to prove horizontal scaling. Use `-strategy user` to keep each `userId` on
one instance and exercise that instance's ETag cache:

```powershell
.\bench\bin\loadgen.exe `
  -base-url http://127.0.0.1:18081 `
  -users 20000 `
  -quick `
  -out bench\results\manual-phase5
```

The load generator does not send `If-None-Match`, so its latency numbers still
pay for the handler and database work (plus ETag computation and cache store).
Measure 304 savings separately:

```powershell
$r = Invoke-WebRequest http://127.0.0.1:18081/api/v1/portfolio/1
$etag = $r.Headers.ETag
curl.exe -s -o NUL -w "%{http_code}`n" -H "If-None-Match: $etag" `
  http://127.0.0.1:18081/api/v1/portfolio/1
```

The first response is `200`; the conditional response is `304` with an empty
body. Repeat the same user immediately to hit the local cache. A write on one
instance invalidates that user's cache on that instance only; the 5-second TTL
bounds staleness on the other instances.

To check the rate limiter, restart one instance with a small limit:

```powershell
$env:HTTP_RATE_LIMIT_RPS = '1'
$env:HTTP_RATE_LIMIT_BURST = '2'
```

Then request the same user three times in a row. The first two return `200`; the
third returns `429` with `Retry-After` and `X-RateLimit-*` headers. Requests for
another user still return `200`. Per-instance limits mean three replicas with
the default `20 rps` allow roughly 60 rps for one user in aggregate.

Per-instance cache and limiter counters are exposed on each API process:

```powershell
curl.exe -s http://127.0.0.1:18080/metrics | Select-String "stocky_http_cache|stocky_http_rate_limited"
```

`stocky_http_cache_events_total{event="hit"|"miss"|"revalidated"|"evicted"}`
shows how often a conditional request was answered without the handler. Raw
`200` load-generator traffic records misses and evictions, not hits.

## Phase 2 write-path checks

`dbtool` provides two checks for the maintained projections.

### Write overhead

`write-overhead` creates a disposable user and symbol, then times reward and
adjustment inserts with projection triggers enabled and suppressed
(`stocky.skip_projection_refresh=on`). Each measured operation is one transaction,
so commit cost is included. The fixture is removed afterwards.

```powershell
go build -o .\bench\bin\dbtool.exe .\bench\dbtool
.\bench\bin\dbtool.exe -database stocky_bench -iterations 50 -warmup 5 `
  -out bench\results\write-overhead.txt write-overhead
```

Output columns: `operation`, `mode`, `samples`, `mean_ms`, `p50_ms`, `p95_ms`,
`p99_ms`, `max_ms`.

Measured on 3 Nov 2026 against the default 20,000-user / 500,000-reward seed:

| Operation | Triggers | p50 | p95 |
|---|---|---:|---:|
| reward insert | on | 1.816 ms | 3.041 ms |
| reward insert | off | 0.591 ms | 1.708 ms |
| adjustment insert | on | 1.910 ms | 2.534 ms |
| adjustment insert | off | 0.532 ms | 0.669 ms |

The trigger adds roughly 1.2–1.4 ms per write on this dataset while keeping
writes in the low-single-digit millisecond range. The default synchronous
fan-out limit of 50 users is well above the one-user cost measured here; a
larger fan-out is queued rather than charged to the writer.

### Live rebuild check

`rebuild-check` runs `cmd/rebuild-projections --online` while concurrent writers
create rewards and adjustments for disposable users. After each run it verifies,
in one statement snapshot, that the fixtures have one `reward_state` row per
reward and zero reconciliation mismatches. It then stops the writers and
repeats the check on the quiesced fixture.

```powershell
go build -o .\bench\bin\rebuild-projections.exe .\cmd\rebuild-projections
.\bench\bin\dbtool.exe -database stocky_bench -rebuild-runs 2 -writers 4 `
  -write-interval 500ms -write-limit 250 `
  -out bench\results\rebuild-check.txt rebuild-check
```

Output columns: `run`, `seconds`, `concurrent_writes`, `fixture_rewards`,
`fixture_reward_state`, `mismatches`, followed by `concurrent_writes` and
`quiesced_mismatches` totals. A non-zero mismatch count fails the command.

Measured on 3 Nov 2026 against the default seed, with four writers capped at 250
rewards each:

| Run | Seconds | Cumulative concurrent writes | Fixture rewards | Fixture `reward_state` | Mismatches |
|---|---:|---:|---:|---:|---:|
| 1 | 146.939 | 1,000 | 1,000 | 1,000 | 0 |
| 2 | 119.389 | 1,000 | 1,000 | 1,000 | 0 |

`quiesced_mismatches` was `0`, and the disposable users and prices were removed
after the run. Run 1 overlapped the writers for most of its duration; run 2
measured the repeat online pass after the writers had reached their cap.
`--online` did not overwrite or remove any trigger-maintained rows.

The command assumes the standard seed is already loaded and the rebuild binary
exists. It does not reseed or migrate the database.

Raw output and test logs for these checks are recorded in
[`results/manual-phase2/README.md`](results/manual-phase2/README.md). That record
also lists the Phase 2 integration tests and the status of the HTTP load test.

## Phase 3 read-path queries

Phase 3 removed the repeated `user_portfolio` scans. The files in `queries/`
mirror the SQL the handlers now issue:

| Query file | Endpoint | Queries per request |
|---|---|---:|
| `portfolio_page.sql` | `/portfolio` page | 1 |
| `portfolio_summary.sql` | `/portfolio` totals, unpriced set, freshness, and cash | 1 |
| `stats_today.sql` | `/stats` today rewards | 1 |
| `portfolio_summary.sql` | `/stats` totals, unpriced set, freshness, and cash | 1 |
| `today.sql` | `/today-stocks` | 1 |
| `historical.sql` | `/historical-inr` grant-date and as-of modes | 1 |

Query counts per endpoint:

| Endpoint | Before Phase 3 | After Phase 3 |
|---|---:|---:|
| `GET /portfolio` | 5 | 2 |
| `GET /stats` | 5 | 2 |
| `GET /today-stocks` | 1 | 1 |
| `GET /historical-inr` | 2 | 1 |

`/portfolio` and `/stats` keep one read-only `REPEATABLE READ` transaction so
the page and the summary describe the same MVCC snapshot and market date.
`/today-stocks` and `/historical-inr` are single statements and do not open a
snapshot transaction.

The expected plan properties on the seeded dataset are:

- `portfolio_page.sql` uses the `portfolio_position` primary key
  `(user_id, terminal_symbol)` and joins `stock_prices_adjusted` by symbol.
- `portfolio_summary.sql` reads the user's `portfolio_position` rows once,
  joins `stock_prices_adjusted`, and reads the single `user_cash_position` row.
  The `(SELECT 1) AS anchor` keeps one result row for an empty portfolio.
- `stats_today.sql` filters `reward_state` by `user_id`,
  `reward_date = market_today()`, and `is_active` through
  `idx_reward_state_user_date`. `today.sql` applies the same filter before its
  price join.
- `historical.sql` filters `rewards` by `user_id` and date in the page CTE
  before calling `reward_corporate_action_outcome()` only for the page rows. The
  summary function performs its own user-scoped pass so an empty page still
  reports valuation completeness.

Capture the plans with the standard command:

```powershell
.\bench\bin\dbtool.exe -database stocky_bench -out bench\results\plans.txt explain
```

Measured on 3 Nov 2026 against the seeded `stocky_bench` database after
migration `0029`:

| Query file | Rows | Execution time |
|---|---:|---:|
| `portfolio_page.sql` | 25 | 8.021 ms |
| `portfolio_summary.sql` | 25 | 4.162 ms |
| `stats_today.sql` | 0 / 1 | 1.485 ms / 0.040 ms |
| `today.sql` | 0 / 1 | 0.151 ms / 0.311 ms |
| `historical.sql` grant-date | 25 | 41.570 ms |
| `historical.sql` as-of | 23 | 11.531 ms |

The `stats_today` and `today` rows show user 1 (no rewards today) and a
row-bearing user 40. Full plans and plan notes are in
[`results/manual-phase3/README.md`](results/manual-phase3/README.md) and
[`results/manual-phase3/plans.txt`](results/manual-phase3/plans.txt). The plans
use `portfolio_position_pkey`, `idx_reward_state_user_date`, and
`idx_rewards_user_date`; none sequentially scans `rewards`, `reward_state`,
`portfolio_position`, or `reward_corporate_action_outcomes`.

The removed `portfolio_totals.sql`, `portfolio_freshness.sql`,
`portfolio_unpriced.sql`, and `portfolio_cash.sql` files described the Phase 0
five-query implementation and no longer match the API.

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
| `reward_state` | 500,000 |
| `portfolio_position` | up to 500,000 |
| `user_cash_position` | 20,000 |
| `instrument_factor` | ~3,001 |
| `projection_refresh_queue` | 0 after a completed rebuild |
| `projection_reconciliation_focus` | 0 after seed |

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
  api-metrics.txt            # Prometheus /metrics output after the load
  run-info.txt               # run parameters
  api.out.log / api.err.log  # API logs
```

Multi-instance runs add one API log pair and metrics file per port
(`api-<port>.out.log`, `api-<port>.err.log`, `api-metrics-<port>.txt`), plus
`lb.out.log`, `lb.err.log`, and `lb-backends.json`. With `-reconcile`, the
directory also contains `reconciliation.txt`.

`api-metrics.txt` is captured after the load phases. It contains the HTTP
latency histograms, named-query timings, DB pool gauges/counters, and projection
health gauges. A missing file is a warning, not a failed run; set
`METRICS_ENABLED=true` (the default) to capture it.

To tune the pool, compare `stocky_db_pool_wait_count_total` and
`stocky_db_pool_wait_duration_seconds_total` with the p95/p99 latency in
`summary.txt`. A non-zero wait count with rising p99 means the pool is too small
or the database is saturated. Check PostgreSQL CPU and `max_connections` before
raising `DB_MAX_OPEN_CONNS`.

## Reading the results

The Phase 6 steady profile met the 1M/hour target on the benchmark host with
wide latency headroom: 1,000,284 requests in one hour, 0 errors, p95 11.2 ms.
The 2,000 req/s peak profile did not: three APIs behind `bench/lb` sustained
~1,529 req/s for an hour with p95 861 ms. Short peak smoke runs reached
~1,895 req/s. Treat the 2,000 req/s figure as a capacity target for larger or
additional hosts, not as a demonstrated result on one 6-core machine.

Suggested SLO for comparison:

| Metric | Target |
|---|---|
| p95 latency | ≤ 200 ms |
| p99 latency | ≤ 500 ms |
| error rate | < 0.1% |
| 5xx responses | 0 under sustained load |

If a plan exceeds `-explain-timeout`, `plans.txt` records the error and the
orchestrator continues to the load test. A timeout is itself a finding.

## Phase boundary

The harness only measures. Phase 1 added the projection schema and rebuild step
used above. Phase 2 added the queue, rollover due checks, reconciliation, and the
two write-path checks. Phase 3 replaced the read-path SQL and the query set used
for plan capture; the handler and integration tests live outside this directory.
Phase 4 added the API/jobs pool split, request deadlines, optional read-replica
routing, and the `api-metrics.txt` capture. Phase 5 adds ETag/304 handling, the
optional per-process LRU cache, per-user rate limiting, and the `lb` harness for
multi-instance runs. Phase 6 adds the one-hour steady and peak profiles,
multi-instance pool overrides, and the post-run reconciliation capture.
