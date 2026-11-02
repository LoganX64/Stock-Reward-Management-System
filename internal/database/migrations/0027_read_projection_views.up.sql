-- Switch the compatibility views to the Phase 1 projections.
--
-- This migration must run only after cmd/rebuild-projections has populated the
-- projection tables and the parity check has passed. Every projection value is
-- cast explicitly so the public view shape (names, order, and types) matches
-- the pre-projection definitions exactly.

-- Rollout gate. A single `m.Up()` from 0025 would otherwise switch the API to
-- empty projections before an operator can run the backfill. Refuse to switch
-- when the projection has fewer rows than current rewards.
DO $$
DECLARE
    v_rewards   BIGINT;
    v_projected BIGINT;
BEGIN
    SELECT COUNT(*) INTO v_rewards
    FROM rewards
    WHERE reward_date <= market_today();

    SELECT COUNT(*) INTO v_projected
    FROM reward_state;

    IF v_projected < v_rewards THEN
        RAISE EXCEPTION
            '0027 requires populated projections: reward_state has % rows but % current rewards exist',
            v_projected, v_rewards
            USING HINT = 'Apply 0026, run cmd/rebuild-projections --online, verify parity, then apply 0027.';
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- Projection-backed lifecycle view
-- ---------------------------------------------------------------------------

CREATE OR REPLACE VIEW reward_corporate_action_outcomes AS
SELECT
    rs.reward_id,
    rs.user_id,
    rs.original_symbol::VARCHAR(20) AS original_symbol,
    rs.terminal_symbol,
    rs.reward_date,
    rs.adjusted_quantity::NUMERIC AS adjusted_quantity,
    rs.delist_event_id,
    rs.delist_effective_date,
    rs.outcome_type,
    rs.cash_proceeds::NUMERIC AS cash_proceeds,
    rs.written_off_quantity::NUMERIC AS written_off_quantity,
    rs.total_adjustment_amount::NUMERIC AS total_adjustment_amount,
    rs.is_active,
    rs.cash_in_lieu_proceeds::NUMERIC AS cash_in_lieu_proceeds,
    rs.fraction_quantity::NUMERIC AS fraction_quantity
FROM reward_state rs;

COMMENT ON VIEW reward_corporate_action_outcomes IS
    'Current corporate-action lifecycle per reward, served from reward_state. Rebuilt from reward_corporate_action_outcomes_base.';

-- ---------------------------------------------------------------------------
-- Cash and price views
-- ---------------------------------------------------------------------------

CREATE OR REPLACE VIEW user_corporate_action_cash AS
SELECT
    user_id,
    ROUND(cash_proceeds::NUMERIC, 4) AS cash_proceeds,
    ROUND(written_off_quantity::NUMERIC, 6) AS written_off_quantity,
    ROUND(cash_in_lieu_proceeds::NUMERIC, 4) AS cash_in_lieu_proceeds
FROM user_cash_position;

CREATE OR REPLACE VIEW stock_prices_adjusted AS
SELECT
    sp.id,
    sp.stock_symbol,
    sp.price AS raw_price,
    sp.price / COALESCE(
        CASE
            WHEN f.as_of = market_today() THEN f.factor
        END,
        stock_action_factor(sp.stock_symbol, market_today())
    ) AS adjusted_price,
    sp.updated_at,
    sp.source,
    sp.fetched_at,
    sp.provider_symbol,
    sp.exchange,
    sp.currency
FROM stock_prices sp
LEFT JOIN instrument_factor f
    ON f.stock_symbol = sp.stock_symbol;

-- ---------------------------------------------------------------------------
-- User-facing views
-- ---------------------------------------------------------------------------

CREATE OR REPLACE VIEW today_rewards AS
SELECT
    rs.reward_id AS reward_event_id,
    rs.user_id,
    rs.terminal_symbol AS stock_symbol,
    rs.adjusted_quantity::NUMERIC AS adjusted_quantity,
    spa.adjusted_price AS current_price,
    rs.total_adjustment_amount::NUMERIC AS total_adjustment_amount,
    CASE
        WHEN spa.adjusted_price IS NULL THEN NULL::NUMERIC
        ELSE ROUND(rs.adjusted_quantity * spa.adjusted_price, 4)
    END AS inr_value,
    CASE
        WHEN spa.adjusted_price IS NULL THEN 'missing'
        ELSE 'available'
    END AS price_status,
    spa.updated_at AS price_as_of,
    spa.source AS price_source,
    spa.fetched_at AS price_fetched_at
FROM reward_state rs
LEFT JOIN stock_prices_adjusted spa
    ON spa.stock_symbol = rs.terminal_symbol
WHERE rs.reward_date = market_today()
  AND rs.is_active;

CREATE OR REPLACE VIEW user_portfolio AS
SELECT
    p.user_id,
    p.terminal_symbol AS stock_symbol,
    p.quantity::NUMERIC AS adjusted_quantity,
    spa.adjusted_price AS current_price,
    CASE
        WHEN spa.adjusted_price IS NULL THEN NULL::NUMERIC
        ELSE ROUND(p.quantity * spa.adjusted_price, 4)
    END AS inr_value,
    p.total_adjustment_amount::NUMERIC AS total_adjustment_amount,
    CASE
        WHEN spa.adjusted_price IS NULL THEN 'missing'
        ELSE 'available'
    END AS price_status,
    spa.updated_at AS price_as_of,
    spa.source AS price_source,
    spa.fetched_at AS price_fetched_at
FROM portfolio_position p
LEFT JOIN stock_prices_adjusted spa
    ON spa.stock_symbol = p.terminal_symbol;
