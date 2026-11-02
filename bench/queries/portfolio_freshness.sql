\set ON_ERROR_STOP on
\set user_id 1

EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT
    stock_symbol,
    price_status,
    price_as_of,
    price_source
FROM user_portfolio
WHERE user_id = :user_id
ORDER BY stock_symbol;
