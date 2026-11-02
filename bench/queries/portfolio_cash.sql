\set ON_ERROR_STOP on
\set user_id 1

EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT
    cash_proceeds,
    written_off_quantity,
    cash_in_lieu_proceeds
FROM user_corporate_action_cash
WHERE user_id = :user_id;
