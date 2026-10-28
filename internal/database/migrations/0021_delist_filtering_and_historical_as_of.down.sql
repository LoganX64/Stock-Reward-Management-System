-- Revert point-in-time historical valuation and lifecycle annotations to the
-- Phase 3.13 historical view.
--
-- The active-reporting lifecycle source (reward_corporate_action_outcomes) and
-- the delist behavior of today_rewards / user_portfolio are unchanged.

DROP VIEW IF EXISTS historical_rewards CASCADE;

DROP FUNCTION IF EXISTS historical_rewards_as_of(DATE);
DROP FUNCTION IF EXISTS reward_corporate_action_outcome(INTEGER, DATE);

CREATE OR REPLACE VIEW historical_rewards AS
WITH reward_adjustments AS (
    SELECT
        reward_id,
        SUM(delta_amount) AS total_adjustment_amount
    FROM adjustments
    GROUP BY reward_id
)
SELECT
    r.user_id,
    r.created_at::date AS reward_date,
    r.id AS reward_event_id,
    ts.stock_symbol,
    r.original_quantity AS adjusted_quantity,
    sph.adjusted_price AS price,
    COALESCE(ra.total_adjustment_amount, 0) AS total_adjustment_amount,
    CASE
        WHEN sph.adjusted_price IS NULL THEN NULL::NUMERIC
        ELSE ROUND(r.original_quantity * sph.adjusted_price, 4)
    END AS inr_value,
    CASE
        WHEN sph.adjusted_price IS NULL THEN 'missing'
        ELSE 'available'
    END AS price_status,
    sph.created_at AS price_as_of
FROM rewards r
LEFT JOIN reward_adjustments ra ON r.id = ra.reward_id
LEFT JOIN LATERAL (
    SELECT instrument_terminal_symbol(
        r.stock_symbol,
        r.created_at::date,
        r.created_at::date
    ) AS stock_symbol
) ts ON TRUE
LEFT JOIN stock_price_history_adjusted sph
    ON UPPER(ts.stock_symbol) = UPPER(sph.stock_symbol)
    AND sph.date = r.created_at::date;

COMMENT ON VIEW historical_rewards IS NULL;
