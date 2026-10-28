-- Missing historical prices and history gaps.
--
-- Historical valuations must not disappear when stock_price_history has no row
-- for a reward's date. historical_rewards now uses a LEFT JOIN and exposes an
-- explicit price_status:
--   * available: a reward-date price row exists;
--   * missing:   no reward-date price row exists.
--
-- price and inr_value are NULL for missing prices. price_as_of carries the
-- history row's creation timestamp for available prices.
--
-- Price history is seeded/backfilled from stock_prices through the supported
-- backfill_price_history(date) function. It only inserts missing rows and uses
-- the latest known price, which is suitable for local/demo data; real market
-- history should be imported from an authoritative source.

CREATE OR REPLACE FUNCTION backfill_price_history(p_date DATE DEFAULT CURRENT_DATE)
RETURNS INTEGER
LANGUAGE plpgsql
AS $$
DECLARE
    inserted_rows INTEGER;
BEGIN
    INSERT INTO stock_price_history (stock_symbol, price, date)
    SELECT sp.stock_symbol, sp.price, p_date
    FROM stock_prices sp
    ON CONFLICT (stock_symbol, date) DO NOTHING;

    GET DIAGNOSTICS inserted_rows = ROW_COUNT;
    RETURN inserted_rows;
END;
$$;

SELECT backfill_price_history(CURRENT_DATE);

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
