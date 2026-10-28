-- Revert explicit missing-historical-price reporting to the prior inner-join
-- behavior. Missing reward-date prices will hide historical rewards again.
--
-- Seeded/backfilled stock_price_history rows are intentionally left in place:
-- the down migration reverts schema behavior, not data repair.

DROP FUNCTION IF EXISTS backfill_price_history(DATE);

DROP VIEW IF EXISTS historical_rewards CASCADE;

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
    ROUND(r.original_quantity * sph.adjusted_price, 4) AS inr_value
FROM rewards r
LEFT JOIN reward_adjustments ra ON r.id = ra.reward_id
LEFT JOIN LATERAL (
    SELECT instrument_terminal_symbol(
        r.stock_symbol,
        r.created_at::date,
        r.created_at::date
    ) AS stock_symbol
) ts ON TRUE
JOIN stock_price_history_adjusted sph
    ON UPPER(ts.stock_symbol) = UPPER(sph.stock_symbol)
    AND sph.date = r.created_at::date;
