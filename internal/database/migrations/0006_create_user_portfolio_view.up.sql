DROP VIEW IF EXISTS user_portfolio CASCADE;

CREATE OR REPLACE VIEW user_portfolio AS
WITH
cumulative_multiplier AS (
    SELECT
        stock_symbol,
        EXP(SUM(LN(ratio_num::numeric / ratio_den))) AS total_multiplier
    FROM stock_events
    WHERE effective_date <= CURRENT_DATE
      AND event_type != 'delist'
      AND ratio_num > 0 AND ratio_den > 0
    GROUP BY stock_symbol
),

latest_merge AS (
    SELECT DISTINCT ON (r.stock_symbol)
        r.stock_symbol,
        se.new_symbol
    FROM rewards r
    JOIN stock_events se 
        ON r.stock_symbol = se.stock_symbol
       AND se.event_type = 'merge'
       AND se.effective_date > r.created_at::date
    ORDER BY r.stock_symbol, se.effective_date DESC
),

reward_adjustments AS (
    SELECT reward_id,
           SUM(delta_amount) AS total_adjustment_amount
    FROM adjustments
    GROUP BY reward_id
)

SELECT
    r.user_id,
    COALESCE(lm.new_symbol, r.stock_symbol) AS stock_symbol,
    SUM(r.quantity * COALESCE(cm.total_multiplier, 1)) AS adjusted_quantity,
    sp.price AS current_price,
    ROUND(SUM(r.quantity * COALESCE(cm.total_multiplier, 1)) * sp.price, 4) AS inr_value,
    COALESCE(SUM(ra.total_adjustment_amount), 0) AS total_adjustment_amount
FROM rewards r
LEFT JOIN reward_adjustments ra ON r.id = ra.reward_id
LEFT JOIN latest_merge lm ON r.stock_symbol = lm.stock_symbol
LEFT JOIN cumulative_multiplier cm ON r.stock_symbol = cm.stock_symbol
JOIN stock_prices sp ON UPPER(COALESCE(lm.new_symbol, r.stock_symbol)) = UPPER(sp.stock_symbol)
WHERE r.stock_symbol NOT IN (
    SELECT stock_symbol
    FROM stock_events
    WHERE event_type = 'delist' AND effective_date <= CURRENT_DATE
)
GROUP BY r.user_id, COALESCE(lm.new_symbol, r.stock_symbol), cm.total_multiplier, sp.price;