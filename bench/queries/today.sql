\set ON_ERROR_STOP on
\set user_id 1

EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT
    rs.reward_id AS reward_event_id,
    rs.terminal_symbol AS stock_symbol,
    rs.adjusted_quantity::NUMERIC AS adjusted_quantity,
    spa.adjusted_price AS current_price,
    rs.total_adjustment_amount::NUMERIC AS total_adjustment_amount,
    CASE
        WHEN spa.adjusted_price IS NULL THEN NULL::NUMERIC
        ELSE ROUND(rs.adjusted_quantity * spa.adjusted_price, 4)
    END AS inr_value,
    CASE
        WHEN spa.adjusted_price IS NULL THEN 'missing'
        ELSE 'available'
    END AS price_status,
    spa.updated_at AS price_as_of,
    spa.source AS price_source,
    spa.fetched_at AS price_fetched_at
FROM reward_state rs
LEFT JOIN stock_prices_adjusted spa
    ON spa.stock_symbol = rs.terminal_symbol
WHERE rs.user_id = :user_id
  AND rs.reward_date = market_today()
  AND rs.is_active
ORDER BY rs.terminal_symbol, rs.reward_id;
