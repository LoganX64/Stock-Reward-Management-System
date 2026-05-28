DROP VIEW IF EXISTS today_rewards CASCADE;

CREATE OR REPLACE VIEW today_rewards AS
SELECT
    r.id AS reward_event_id,
    r.user_id,
    r.stock_symbol,
    r.quantity AS adjusted_quantity,                    
    sp.price AS current_price,
    COALESCE(SUM(a.delta_amount), 0) AS total_adjustment_amount,
    ROUND(r.quantity * sp.price, 4) AS inr_value
FROM rewards r
LEFT JOIN adjustments a ON r.id = a.reward_id
JOIN stock_prices sp ON UPPER(r.stock_symbol) = UPPER(sp.stock_symbol)
WHERE r.created_at::date = CURRENT_DATE
GROUP BY r.id, r.user_id, r.stock_symbol, r.quantity, sp.price;