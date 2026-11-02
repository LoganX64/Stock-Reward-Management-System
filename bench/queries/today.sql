\set ON_ERROR_STOP on
\set user_id 1

EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT
    reward_event_id,
    stock_symbol,
    adjusted_quantity,
    current_price,
    total_adjustment_amount,
    inr_value,
    price_status,
    price_as_of,
    price_source,
    price_fetched_at
FROM today_rewards
WHERE user_id = :user_id
ORDER BY reward_event_id DESC
LIMIT 100;
