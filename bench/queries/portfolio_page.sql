\set ON_ERROR_STOP on
\set user_id 1
\set page_limit 100
\set page_offset 0

EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT
    p.stock_symbol,
    p.adjusted_quantity,
    p.current_price,
    p.inr_value,
    p.price_status,
    p.price_as_of,
    p.price_source,
    p.price_fetched_at
FROM user_portfolio AS p
WHERE p.user_id = :user_id
ORDER BY p.stock_symbol
LIMIT :page_limit
OFFSET :page_offset;
