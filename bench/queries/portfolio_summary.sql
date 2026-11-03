\set ON_ERROR_STOP on
\set user_id 1

EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT
    p.stock_symbol,
    p.price_status,
    p.price_as_of,
    p.price_source,
    p.current_price,
    p.inr_value,
    c.cash_proceeds,
    c.written_off_quantity,
    c.cash_in_lieu_proceeds,
    market_today()::TEXT AS valuation_date
FROM (SELECT 1) AS anchor
LEFT JOIN user_portfolio p
    ON p.user_id = :user_id
LEFT JOIN user_corporate_action_cash c
    ON c.user_id = :user_id
ORDER BY p.stock_symbol;
