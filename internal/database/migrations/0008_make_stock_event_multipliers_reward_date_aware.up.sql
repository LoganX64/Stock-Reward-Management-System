DROP VIEW IF EXISTS historical_rewards CASCADE;

CREATE OR REPLACE VIEW historical_rewards AS
WITH
reward_adjustments AS (
    SELECT
        reward_id,
        SUM(delta_amount) AS total_adjustment_amount
    FROM adjustments
    GROUP BY reward_id
)

SELECT
    r.user_id,
    r.created_at::date                  AS reward_date,
    r.id                                AS reward_event_id,
    COALESCE(lm.new_symbol, r.stock_symbol) AS stock_symbol,

    (r.quantity * COALESCE(em.total_multiplier, 1.0)) AS adjusted_quantity,

    sph.price                           AS price,

    -- Keep adjustment amount for audit
    COALESCE(ra.total_adjustment_amount, 0) AS total_adjustment_amount,

    -- Reward-date valuation only applies events after the reward and up to that date.
    ROUND(
        (r.quantity * COALESCE(em.total_multiplier, 1.0) * sph.price),
        4
    ) AS inr_value

FROM rewards r
LEFT JOIN reward_adjustments ra ON r.id = ra.reward_id
LEFT JOIN LATERAL (
    SELECT
        EXP(SUM(LN(se.ratio_num::numeric / se.ratio_den))) AS total_multiplier
    FROM stock_events se
    WHERE se.stock_symbol = r.stock_symbol
      AND se.effective_date > r.created_at
      AND se.effective_date::date <= r.created_at::date
      AND se.event_type != 'delist'
      AND se.ratio_num > 0
      AND se.ratio_den > 0
) em ON TRUE
LEFT JOIN LATERAL (
    SELECT se.new_symbol
    FROM stock_events se
    WHERE se.stock_symbol = r.stock_symbol
      AND se.event_type = 'merge'
      AND se.effective_date > r.created_at
      AND se.effective_date::date <= r.created_at::date
      AND se.new_symbol IS NOT NULL
    ORDER BY se.effective_date DESC
    LIMIT 1
) lm ON TRUE
JOIN stock_price_history sph
    ON UPPER(COALESCE(lm.new_symbol, r.stock_symbol)) = UPPER(sph.stock_symbol)
    AND sph.date = r.created_at::date
;

DROP VIEW IF EXISTS user_portfolio CASCADE;

CREATE OR REPLACE VIEW user_portfolio AS
WITH
reward_adjustments AS (
    SELECT reward_id,
           SUM(delta_amount) AS total_adjustment_amount
    FROM adjustments
    GROUP BY reward_id
)

SELECT
    r.user_id,
    COALESCE(lm.new_symbol, r.stock_symbol) AS stock_symbol,
    SUM(r.quantity * COALESCE(em.total_multiplier, 1)) AS adjusted_quantity,
    sp.price AS current_price,
    ROUND(SUM(r.quantity * COALESCE(em.total_multiplier, 1)) * sp.price, 4) AS inr_value,
    COALESCE(SUM(ra.total_adjustment_amount), 0) AS total_adjustment_amount
FROM rewards r
LEFT JOIN reward_adjustments ra ON r.id = ra.reward_id
LEFT JOIN LATERAL (
    SELECT
        EXP(SUM(LN(se.ratio_num::numeric / se.ratio_den))) AS total_multiplier
    FROM stock_events se
    WHERE se.stock_symbol = r.stock_symbol
      AND se.effective_date > r.created_at
      AND se.effective_date::date <= CURRENT_DATE
      AND se.event_type != 'delist'
      AND se.ratio_num > 0
      AND se.ratio_den > 0
) em ON TRUE
LEFT JOIN LATERAL (
    SELECT se.new_symbol
    FROM stock_events se
    WHERE se.stock_symbol = r.stock_symbol
      AND se.event_type = 'merge'
      AND se.effective_date > r.created_at
      AND se.effective_date::date <= CURRENT_DATE
      AND se.new_symbol IS NOT NULL
    ORDER BY se.effective_date DESC
    LIMIT 1
) lm ON TRUE
JOIN stock_prices sp ON UPPER(COALESCE(lm.new_symbol, r.stock_symbol)) = UPPER(sp.stock_symbol)
WHERE NOT EXISTS (
    SELECT 1
    FROM stock_events se_delist
    WHERE se_delist.event_type = 'delist'
      AND se_delist.effective_date <= CURRENT_DATE
      AND UPPER(se_delist.stock_symbol) = UPPER(r.stock_symbol)
)
GROUP BY r.user_id, COALESCE(lm.new_symbol, r.stock_symbol), sp.price;
