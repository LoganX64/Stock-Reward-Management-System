-- Restore the pre-projection compatibility view definitions.
--
-- 0027 down must leave working computational views, not empty projection-backed
-- views. These are the exact definitions from 0025 (price/provenance views) and
-- 0022 (lifecycle and cash views).

CREATE OR REPLACE VIEW reward_corporate_action_outcomes AS
WITH reward_adjustments AS (
    SELECT reward_id, SUM(delta_amount) AS total_adjustment_amount
    FROM adjustments
    GROUP BY reward_id
),
balances AS (
    SELECT
        r.id AS reward_id,
        r.user_id,
        r.stock_symbol AS original_symbol,
        r.reward_date,
        s.terminal_symbol,
        s.settled_quantity AS current_adjusted_quantity,
        s.fraction_quantity,
        s.cash_in_lieu,
        s.merger_cash
    FROM rewards r
    CROSS JOIN LATERAL reward_action_settlement(r.id, market_today()) s
),
delist AS (
    SELECT
        b.reward_id,
        d.event_id AS delist_event_id,
        d.effective_date AS delist_effective_date,
        d.outcome AS delist_outcome,
        d.cash_per_unit AS delist_cash_per_unit,
        ROUND(reward_quantity_at(b.reward_id, d.effective_date), 6) AS quantity_at_delist
    FROM balances b
    LEFT JOIN LATERAL (
        SELECT *
        FROM instrument_delist_event(b.original_symbol, b.reward_date, market_today())
    ) d ON TRUE
)
SELECT
    b.reward_id,
    b.user_id,
    b.original_symbol,
    b.terminal_symbol,
    b.reward_date,
    CASE
        WHEN d.delist_event_id IS NULL THEN b.current_adjusted_quantity
        ELSE 0::NUMERIC
    END AS adjusted_quantity,
    d.delist_event_id,
    d.delist_effective_date,
    COALESCE(d.delist_outcome, 'active') AS outcome_type,
    ROUND(
        b.merger_cash
        + b.cash_in_lieu
        + CASE
            WHEN d.delist_outcome = 'cash_exit'
                THEN d.quantity_at_delist * d.delist_cash_per_unit
            ELSE 0::NUMERIC
          END,
        4
    ) AS cash_proceeds,
    CASE
        WHEN d.delist_outcome = 'write_off' THEN d.quantity_at_delist
        ELSE 0::NUMERIC
    END AS written_off_quantity,
    COALESCE(ra.total_adjustment_amount, 0) AS total_adjustment_amount,
    (d.delist_event_id IS NULL AND b.current_adjusted_quantity > 0) AS is_active,
    ROUND(b.cash_in_lieu, 4) AS cash_in_lieu_proceeds,
    ROUND(b.fraction_quantity, 6) AS fraction_quantity
FROM balances b
LEFT JOIN delist d ON d.reward_id = b.reward_id
LEFT JOIN reward_adjustments ra ON ra.reward_id = b.reward_id;

CREATE OR REPLACE VIEW user_corporate_action_cash AS
SELECT
    user_id,
    ROUND(COALESCE(SUM(cash_proceeds), 0), 4) AS cash_proceeds,
    ROUND(COALESCE(SUM(written_off_quantity), 0), 6) AS written_off_quantity,
    ROUND(COALESCE(SUM(cash_in_lieu_proceeds), 0), 4) AS cash_in_lieu_proceeds
FROM reward_corporate_action_outcomes
GROUP BY user_id;

CREATE OR REPLACE VIEW stock_prices_adjusted AS
SELECT
    sp.id,
    sp.stock_symbol,
    sp.price AS raw_price,
    sp.price / stock_action_factor(sp.stock_symbol, market_today()) AS adjusted_price,
    sp.updated_at,
    sp.source,
    sp.fetched_at,
    sp.provider_symbol,
    sp.exchange,
    sp.currency
FROM stock_prices sp;

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
    spa.updated_at AS price_as_of,
    spa.source AS price_source,
    spa.fetched_at AS price_fetched_at
FROM reward_corporate_action_outcomes o
LEFT JOIN stock_prices_adjusted spa
    ON spa.stock_symbol = o.terminal_symbol
WHERE o.reward_date = market_today()
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
    spa.updated_at AS price_as_of,
    spa.source AS price_source,
    spa.fetched_at AS price_fetched_at
FROM reward_corporate_action_outcomes o
LEFT JOIN stock_prices_adjusted spa
    ON spa.stock_symbol = o.terminal_symbol
WHERE o.is_active
GROUP BY
    o.user_id,
    o.terminal_symbol,
    spa.adjusted_price,
    spa.updated_at,
    spa.source,
    spa.fetched_at;
