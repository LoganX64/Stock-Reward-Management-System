-- Revert explicit missing-current-price reporting to the prior inner-join
-- behavior. Missing prices will hide rows again after this migration.

DROP VIEW IF EXISTS user_portfolio CASCADE;
DROP VIEW IF EXISTS today_rewards CASCADE;

CREATE OR REPLACE VIEW today_rewards AS
SELECT
    o.reward_id AS reward_event_id,
    o.user_id,
    o.terminal_symbol AS stock_symbol,
    o.adjusted_quantity,
    spa.adjusted_price AS current_price,
    o.total_adjustment_amount,
    ROUND(o.adjusted_quantity * spa.adjusted_price, 4) AS inr_value
FROM reward_corporate_action_outcomes o
JOIN stock_prices_adjusted spa
    ON UPPER(o.terminal_symbol) = UPPER(spa.stock_symbol)
WHERE o.reward_date = CURRENT_DATE
  AND o.is_active;

CREATE OR REPLACE VIEW user_portfolio AS
SELECT
    o.user_id,
    o.terminal_symbol AS stock_symbol,
    SUM(o.adjusted_quantity) AS adjusted_quantity,
    spa.adjusted_price AS current_price,
    ROUND(SUM(o.adjusted_quantity) * spa.adjusted_price, 4) AS inr_value,
    COALESCE(SUM(o.total_adjustment_amount), 0) AS total_adjustment_amount
FROM reward_corporate_action_outcomes o
JOIN stock_prices_adjusted spa
    ON UPPER(o.terminal_symbol) = UPPER(spa.stock_symbol)
WHERE o.is_active
GROUP BY o.user_id, o.terminal_symbol, spa.adjusted_price;
