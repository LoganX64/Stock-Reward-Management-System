\set ON_ERROR_STOP on
\set user_id 1

EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT rs.terminal_symbol, rs.adjusted_quantity::NUMERIC
FROM reward_state rs
WHERE rs.user_id = :user_id
  AND rs.reward_date = market_today()
  AND rs.is_active;
