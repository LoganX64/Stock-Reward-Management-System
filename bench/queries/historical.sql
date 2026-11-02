\set ON_ERROR_STOP on
\set user_id 1
\set page_limit 100
\set page_offset 0

\echo '--- historical grant-date mode ---'
EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT
    reward_date,
    reward_event_id,
    stock_symbol,
    adjusted_quantity,
    price,
    total_adjustment_amount,
    inr_value,
    price_status,
    price_as_of,
    price_source,
    price_fetched_at,
    outcome_type,
    is_active,
    delist_effective_date,
    cash_proceeds,
    written_off_quantity
FROM historical_rewards_as_of(NULL::date)
WHERE user_id = :user_id
  AND reward_date <= market_today() - 1
ORDER BY reward_date DESC, reward_event_id DESC
LIMIT :page_limit
OFFSET :page_offset;

\echo '--- historical as-of mode (30 days ago) ---'
EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT
    reward_date,
    reward_event_id,
    stock_symbol,
    adjusted_quantity,
    price,
    total_adjustment_amount,
    inr_value,
    price_status,
    price_as_of,
    price_source,
    price_fetched_at,
    outcome_type,
    is_active,
    delist_effective_date,
    cash_proceeds,
    written_off_quantity
FROM historical_rewards_as_of((market_today() - 30)::date)
WHERE user_id = :user_id
  AND reward_date <= market_today() - 30
ORDER BY reward_date DESC, reward_event_id DESC
LIMIT :page_limit
OFFSET :page_offset;
