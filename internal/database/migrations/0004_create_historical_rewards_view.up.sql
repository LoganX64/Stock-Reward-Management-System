DROP VIEW IF EXISTS historical_rewards CASCADE;

CREATE OR REPLACE VIEW historical_rewards AS
WITH
cumulative_multiplier AS (
    SELECT
        stock_symbol,
        EXP(SUM(LN(ratio_num::numeric / ratio_den))) AS total_multiplier
    FROM stock_events
    WHERE effective_date <= CURRENT_DATE
      AND event_type != 'delist'
      AND ratio_num > 0 
      AND ratio_den > 0
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
)

SELECT
    r.user_id,
    r.created_at::date                  AS reward_date,
    r.id                                AS reward_event_id,
    COALESCE(lm.new_symbol, r.stock_symbol) AS stock_symbol,
    
    (r.quantity * COALESCE(cm.total_multiplier, 1.0)) AS adjusted_quantity,
    
    sph.price                           AS price,
    
    -- Keep adjustment amount for audit
    COALESCE(SUM(a.delta_amount), 0)    AS total_adjustment_amount,
    
    -- CORRECT INR Value: Only stock value 
    ROUND(
        (r.quantity * COALESCE(cm.total_multiplier, 1.0) * sph.price), 
        4
    ) AS inr_value

FROM rewards r
LEFT JOIN adjustments a ON r.id = a.reward_id
LEFT JOIN latest_merge lm 
    ON r.stock_symbol = lm.stock_symbol
LEFT JOIN cumulative_multiplier cm 
    ON r.stock_symbol = cm.stock_symbol
JOIN stock_price_history sph 
    ON UPPER(COALESCE(lm.new_symbol, r.stock_symbol)) = UPPER(sph.stock_symbol)
    AND sph.date = r.created_at::date
GROUP BY 
    r.user_id, 
    r.created_at::date, 
    r.id, 
    r.stock_symbol, 
    r.quantity, 
    lm.new_symbol, 
    cm.total_multiplier, 
    sph.price;