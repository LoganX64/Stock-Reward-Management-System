-- Phase 0 baseline seed data.
--
-- Run only against a dedicated benchmark database (default: stocky_bench).
-- This script truncates the financial tables, so never point it at development
-- or production data.
--
-- Defaults are sized for a 12 vCPU / 8 GB local machine:
--   20,000 users x 25 rewards = 500,000 rewards
--    3,000 instruments x 60 days = 180,000 historical prices
--   20% of rewards receive an adjustment
--
-- Override the psql variables on the command line if needed, for example:
--   psql -v users=5000 -v rewards_per_user=20 -f bench/seed/seed_bench.sql

\set ON_ERROR_STOP on
\set users 20000
\set rewards_per_user 25
\set symbols 3000
\set history_days 60

BEGIN;

SET LOCAL synchronous_commit = off;

TRUNCATE TABLE
    ledger,
    adjustments,
    rewards,
    stock_events,
    stock_price_history,
    stock_prices,
    users
RESTART IDENTITY CASCADE;

-- ---------------------------------------------------------------------------
-- Users
-- ---------------------------------------------------------------------------

INSERT INTO users (username, email)
SELECT
    'bench_user_' || lpad(g::text, 6, '0'),
    'bench_user_' || lpad(g::text, 6, '0') || '@bench.local'
FROM generate_series(1, :users) AS g;

-- ---------------------------------------------------------------------------
-- Current prices
-- ---------------------------------------------------------------------------

INSERT INTO stock_prices (
    stock_symbol, price, updated_at, source, fetched_at,
    provider_symbol, exchange, currency
)
SELECT
    'SYM' || lpad(g::text, 4, '0'),
    (100 + (g % 400))::numeric(18, 4),
    NOW() - make_interval(mins => (g % 120)),
    'simulator',
    NOW(),
    'SYM' || lpad(g::text, 4, '0') || '.NS',
    'NSE',
    'INR'
FROM generate_series(1, :symbols) AS g;

-- Successor used by the merger fixtures.
INSERT INTO stock_prices (
    stock_symbol, price, updated_at, source, fetched_at,
    provider_symbol, exchange, currency
)
VALUES (
    'SYM9999', 250.0000, NOW(), 'simulator', NOW(), 'SYM9999.NS', 'NSE', 'INR'
);

-- ---------------------------------------------------------------------------
-- Historical prices
-- ---------------------------------------------------------------------------

INSERT INTO stock_price_history (
    stock_symbol, price, date, created_at, source, fetched_at,
    provider_symbol, exchange, currency
)
SELECT
    s.symbol,
    (100 + ((s.n * 7 + d * 3) % 400))::numeric(18, 4),
    CURRENT_DATE - d,
    (CURRENT_DATE - d) + time '15:30',
    'simulator',
    NOW(),
    s.symbol || '.NS',
    'NSE',
    'INR'
FROM (
    SELECT g AS n, 'SYM' || lpad(g::text, 4, '0') AS symbol
    FROM generate_series(1, :symbols) AS g
) AS s
CROSS JOIN generate_series(0, :history_days - 1) AS d;

-- ---------------------------------------------------------------------------
-- Corporate actions
-- ---------------------------------------------------------------------------

-- Splits on every 10th instrument, 180 days ago.
INSERT INTO stock_events (
    stock_symbol, event_type, ratio_num, ratio_den, effective_date
)
SELECT
    'SYM' || lpad(g::text, 4, '0'),
    'split',
    2,
    1,
    (CURRENT_DATE - 180)::timestamp
FROM generate_series(10, :symbols, 10) AS g;

-- Bonuses on every 15th instrument, 90 days ago. The validation trigger
-- requires a bonus ratio greater than 1:1, so use 2:1.
INSERT INTO stock_events (
    stock_symbol, event_type, ratio_num, ratio_den, effective_date
)
SELECT
    'SYM' || lpad(g::text, 4, '0'),
    'bonus',
    2,
    1,
    (CURRENT_DATE - 90)::timestamp
FROM generate_series(15, :symbols, 15) AS g;

-- Mergers: ten predecessors into SYM9999, 60 days ago.
INSERT INTO stock_events (
    stock_symbol, event_type, ratio_num, ratio_den, new_symbol,
    effective_date, cash_per_unit
)
SELECT
    'SYM' || lpad(g::text, 4, '0'),
    'merge',
    1,
    1,
    'SYM9999',
    (CURRENT_DATE - 60)::timestamp,
    50
FROM generate_series(1, 10) AS g;

-- Delists: three cash exits and two write-offs, 30 days ago.
INSERT INTO stock_events (
    stock_symbol, event_type, ratio_num, ratio_den, effective_date,
    delist_outcome, cash_per_unit
)
SELECT
    'SYM' || lpad(g::text, 4, '0'),
    'delist',
    1,
    1,
    (CURRENT_DATE - 30)::timestamp,
    CASE WHEN g <= 3 THEN 'cash_exit' ELSE 'write_off' END,
    CASE WHEN g <= 3 THEN 100 ELSE NULL END
FROM generate_series(11, 15) AS g;

-- ---------------------------------------------------------------------------
-- Rewards
-- ---------------------------------------------------------------------------
--
-- Each user receives 25 rewards across different symbols and reward dates.
-- The symbol offset (d * 101) is unique for d in 1..25, so the
-- (user_id, stock_symbol, reward_date) constraint cannot collide.

INSERT INTO rewards (
    user_id, stock_symbol, original_quantity, quantity, created_at
)
SELECT
    u.id,
    'SYM' || lpad((((u.id * 37 + d * 101) % :symbols) + 1)::text, 4, '0'),
    (5 + ((u.id + d) % 20))::numeric(18, 6),
    (5 + ((u.id + d) % 20))::numeric(18, 6),
    ((CURRENT_DATE - ((u.id + d * 13) % 365)) + time '09:15')::timestamp
FROM users AS u
CROSS JOIN generate_series(1, :rewards_per_user) AS d
WHERE u.username LIKE 'bench_user_%';

-- ---------------------------------------------------------------------------
-- Adjustments
-- ---------------------------------------------------------------------------

INSERT INTO adjustments (
    reward_id, adjustment_type, delta_quantity, delta_amount, reason, effective_date
)
SELECT
    r.id,
    'manual_correction',
    0,
    5.0000,
    'bench adjustment',
    r.reward_date
FROM rewards AS r
WHERE r.id % 5 = 0;

COMMIT;

ANALYZE users;
ANALYZE rewards;
ANALYZE adjustments;
ANALYZE stock_events;
ANALYZE stock_prices;
ANALYZE stock_price_history;
ANALYZE ledger;

\echo 'Benchmark seed complete.'
