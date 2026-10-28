-- Missing current prices.
--
-- A reward or holding must not disappear because stock_prices has no row for
-- its terminal instrument. The current reporting views now use a LEFT JOIN and
-- expose an explicit price_status:
--   * available: a current price row exists;
--   * missing:   no current price row exists.
--
-- current_price and inr_value are NULL for missing prices. price_as_of carries
-- the last update timestamp of the matched price row so callers can judge
-- freshness. Historical-price gaps remain a separate reporting concern.

CREATE OR REPLACE VIEW today_rewards AS
SELECT
    o.reward_id AS reward_event_id,
    o.user_id,
    o.terminal_symbol AS stock_symbol,
    o.adjusted_quantity,
    spa.adjusted_price AS current_price,
    o.total_adjustment_amount,
    CASE
        WHEN spa.adjusted_price IS NULL THEN NULL::NUMERIC
        ELSE ROUND(o.adjusted_quantity * spa.adjusted_price, 4)
    END AS inr_value,
    CASE
        WHEN spa.adjusted_price IS NULL THEN 'missing'
        ELSE 'available'
    END AS price_status,
    spa.updated_at AS price_as_of
FROM reward_corporate_action_outcomes o
LEFT JOIN stock_prices_adjusted spa
    ON UPPER(o.terminal_symbol) = UPPER(spa.stock_symbol)
WHERE o.reward_date = CURRENT_DATE
  AND o.is_active;

CREATE OR REPLACE VIEW user_portfolio AS
SELECT
    o.user_id,
    o.terminal_symbol AS stock_symbol,
    SUM(o.adjusted_quantity) AS adjusted_quantity,
    spa.adjusted_price AS current_price,
    CASE
        WHEN spa.adjusted_price IS NULL THEN NULL::NUMERIC
        ELSE ROUND(SUM(o.adjusted_quantity) * spa.adjusted_price, 4)
    END AS inr_value,
    COALESCE(SUM(o.total_adjustment_amount), 0) AS total_adjustment_amount,
    CASE
        WHEN spa.adjusted_price IS NULL THEN 'missing'
        ELSE 'available'
    END AS price_status,
    spa.updated_at AS price_as_of
FROM reward_corporate_action_outcomes o
LEFT JOIN stock_prices_adjusted spa
    ON UPPER(o.terminal_symbol) = UPPER(spa.stock_symbol)
WHERE o.is_active
GROUP BY
    o.user_id,
    o.terminal_symbol,
    spa.adjusted_price,
    spa.updated_at;
