\set ON_ERROR_STOP on
\set user_id 1

EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT stock_symbol
FROM user_portfolio
WHERE user_id = :user_id
  AND current_price IS NULL
ORDER BY stock_symbol;
