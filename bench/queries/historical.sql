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
    outcome_type,
    is_active,
    delist_effective_date,
    cash_proceeds,
    written_off_quantity,
    price_source,
    price_fetched_at,
    valuation_date,
    as_of_allowed,
    unpriced_rewards,
    unpriced_symbols
FROM historical_rewards_for_user(
    :user_id,
    NULL::date,
    :page_limit,
    :page_offset
);

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
    outcome_type,
    is_active,
    delist_effective_date,
    cash_proceeds,
    written_off_quantity,
    price_source,
    price_fetched_at,
    valuation_date,
    as_of_allowed,
    unpriced_rewards,
    unpriced_symbols
FROM historical_rewards_for_user(
    :user_id,
    (market_today() - 30)::date,
    :page_limit,
    :page_offset
);
