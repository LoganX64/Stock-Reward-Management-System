\set ON_ERROR_STOP on
\set user_id 1

EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT
    COALESCE(SUM(inr_value), 0),
    COUNT(*),
    market_today()::TEXT
FROM user_portfolio
WHERE user_id = :user_id;
