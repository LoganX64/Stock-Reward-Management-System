-- Consistent delist filtering and point-in-time historical valuation.
--
-- Phase 3.14 keeps reward_corporate_action_outcomes as the single
-- successor-aware lifecycle source for the active reporting views
-- (today_rewards, user_portfolio, user_corporate_action_cash). That view is
-- intentionally left unchanged so live reporting behavior does not change in
-- this migration.
--
-- Phase 3.15 adds a parameterized outcome function and an as-of historical
-- function:
--
--   * reward_corporate_action_outcome(reward_id, as_of) answers "what was the
--     lifecycle and corporate-action outcome of this reward on this date?"
--     It mirrors the active outcome view but accepts an arbitrary date.
--
--   * historical_rewards_as_of(as_of) returns historical reward rows.
--       - as_of IS NULL (default): grant-date stock value. Each reward is
--         valued with its original quantity and the adjusted price on its
--         reward date. Lifecycle annotations are current so a later delist is
--         visible rather than silently omitted.
--       - as_of IS NOT NULL: point-in-time valuation. Quantity, terminal
--         symbol, adjustments, delists, and price are all evaluated on as_of.
--         A lot that was delisted on or before as_of has quantity 0,
--         price_status='not_required', and reports its cash/write-off outcome.
--
--   * historical_rewards remains a view over the grant-date mode for backward
--     compatibility and gains the lifecycle columns.
--
-- A later delist never removes a historical row and never changes a grant-date
-- value. Only active reporting filters on is_active.

CREATE OR REPLACE FUNCTION reward_corporate_action_outcome(
    p_reward_id INTEGER,
    p_as_of DATE DEFAULT CURRENT_DATE
)
RETURNS TABLE (
    reward_id INTEGER,
    user_id INTEGER,
    original_symbol VARCHAR(20),
    terminal_symbol TEXT,
    reward_date DATE,
    adjusted_quantity NUMERIC,
    delist_event_id INTEGER,
    delist_effective_date DATE,
    outcome_type TEXT,
    cash_proceeds NUMERIC,
    written_off_quantity NUMERIC,
    total_adjustment_amount NUMERIC,
    is_active BOOLEAN,
    cash_in_lieu_proceeds NUMERIC,
    fraction_quantity NUMERIC
)
LANGUAGE SQL
STABLE
AS $$
    WITH reward_row AS (
        SELECT
            r.id,
            r.user_id,
            UPPER(r.stock_symbol)::VARCHAR(20) AS original_symbol,
            r.created_at::DATE AS reward_date
        FROM rewards r
        WHERE r.id = p_reward_id
          AND r.created_at::DATE <= p_as_of
    ),
    settlement AS (
        SELECT s.*
        FROM reward_row rr
        CROSS JOIN LATERAL reward_action_settlement(rr.id, p_as_of) s
    ),
    delist AS (
        SELECT d.*
        FROM reward_row rr
        CROSS JOIN LATERAL instrument_delist_event(
            rr.original_symbol,
            rr.reward_date,
            p_as_of
        ) d
    ),
    adjustment_totals AS (
        SELECT
            COALESCE(SUM(a.delta_amount), 0) AS total_adjustment_amount
        FROM adjustments a
        JOIN reward_row rr ON rr.id = a.reward_id
        WHERE a.effective_date <= p_as_of
    )
    SELECT
        rr.id AS reward_id,
        rr.user_id,
        rr.original_symbol,
        s.terminal_symbol,
        rr.reward_date,
        CASE
            WHEN d.event_id IS NULL THEN s.settled_quantity
            ELSE 0::NUMERIC
        END AS adjusted_quantity,
        d.event_id AS delist_event_id,
        d.effective_date AS delist_effective_date,
        COALESCE(d.outcome, 'active') AS outcome_type,
        ROUND(
            s.merger_cash
            + s.cash_in_lieu
            + CASE
                WHEN d.outcome = 'cash_exit'
                    THEN reward_quantity_at(rr.id, d.effective_date) * d.cash_per_unit
                ELSE 0::NUMERIC
              END,
            4
        ) AS cash_proceeds,
        CASE
            WHEN d.outcome = 'write_off'
                THEN ROUND(reward_quantity_at(rr.id, d.effective_date), 6)
            ELSE 0::NUMERIC
        END AS written_off_quantity,
        adj.total_adjustment_amount,
        (d.event_id IS NULL AND s.settled_quantity > 0) AS is_active,
        ROUND(s.cash_in_lieu, 4) AS cash_in_lieu_proceeds,
        ROUND(s.fraction_quantity, 6) AS fraction_quantity
    FROM reward_row rr
    CROSS JOIN settlement s
    LEFT JOIN delist d ON TRUE
    CROSS JOIN adjustment_totals adj;
$$;

COMMENT ON FUNCTION reward_corporate_action_outcome(INTEGER, DATE) IS
    'Successor-aware lifecycle and corporate-action outcome for one reward as of a date. The CURRENT_DATE wrapper is reward_corporate_action_outcomes.';

CREATE OR REPLACE FUNCTION historical_rewards_as_of(
    p_as_of DATE DEFAULT NULL
)
RETURNS TABLE (
    user_id INTEGER,
    reward_date DATE,
    reward_event_id INTEGER,
    stock_symbol TEXT,
    adjusted_quantity NUMERIC,
    price NUMERIC,
    total_adjustment_amount NUMERIC,
    inr_value NUMERIC,
    price_status TEXT,
    price_as_of TIMESTAMP,
    outcome_type TEXT,
    is_active BOOLEAN,
    delist_effective_date DATE,
    cash_proceeds NUMERIC,
    written_off_quantity NUMERIC
)
LANGUAGE SQL
STABLE
AS $$
    WITH params AS (
        SELECT
            p_as_of AS as_of,
            COALESCE(p_as_of, CURRENT_DATE) AS lifecycle_as_of,
            (p_as_of IS NULL) AS grant_mode
    ),
    reward_rows AS (
        SELECT
            r.id AS reward_event_id,
            r.user_id,
            r.stock_symbol AS original_symbol,
            r.original_quantity,
            r.created_at::DATE AS reward_date,
            p.as_of,
            p.lifecycle_as_of,
            p.grant_mode
        FROM rewards r
        CROSS JOIN params p
        WHERE p.as_of IS NULL
           OR r.created_at::DATE <= p.as_of
    ),
    lifecycle AS (
        SELECT
            rr.reward_event_id,
            o.terminal_symbol,
            o.adjusted_quantity AS effective_quantity,
            o.total_adjustment_amount,
            o.outcome_type,
            o.is_active,
            o.delist_effective_date,
            o.cash_proceeds,
            o.written_off_quantity
        FROM reward_rows rr
        CROSS JOIN LATERAL reward_corporate_action_outcome(
            rr.reward_event_id,
            rr.lifecycle_as_of
        ) o
    ),
    valued AS (
        SELECT
            rr.reward_event_id,
            rr.user_id,
            rr.reward_date,
            rr.grant_mode,
            rr.as_of,
            CASE
                WHEN rr.grant_mode THEN rr.original_quantity
                ELSE l.effective_quantity
            END AS valued_quantity,
            CASE
                WHEN rr.grant_mode THEN
                    instrument_terminal_symbol(
                        rr.original_symbol,
                        rr.reward_date,
                        rr.reward_date
                    )
                ELSE l.terminal_symbol
            END AS valued_symbol,
            l.total_adjustment_amount,
            l.outcome_type,
            l.is_active,
            l.delist_effective_date,
            l.cash_proceeds,
            l.written_off_quantity
        FROM reward_rows rr
        JOIN lifecycle l ON l.reward_event_id = rr.reward_event_id
    ),
    priced AS (
        SELECT
            v.*,
            sph.adjusted_price AS matched_price,
            sph.created_at AS matched_price_as_of
        FROM valued v
        LEFT JOIN LATERAL (
            SELECT h.adjusted_price, h.created_at
            FROM stock_price_history_adjusted h
            WHERE UPPER(h.stock_symbol) = UPPER(v.valued_symbol)
              AND h.date = CASE
                  WHEN v.grant_mode THEN v.reward_date
                  ELSE v.as_of
              END
            ORDER BY h.created_at DESC
            LIMIT 1
        ) sph ON TRUE
    )
    SELECT
        p.user_id,
        p.reward_date,
        p.reward_event_id,
        p.valued_symbol AS stock_symbol,
        ROUND(p.valued_quantity, 6) AS adjusted_quantity,
        CASE
            WHEN p.valued_quantity = 0 THEN NULL::NUMERIC
            ELSE p.matched_price
        END AS price,
        COALESCE(p.total_adjustment_amount, 0) AS total_adjustment_amount,
        CASE
            WHEN p.valued_quantity = 0 THEN 0::NUMERIC
            WHEN p.matched_price IS NULL THEN NULL::NUMERIC
            ELSE ROUND(p.valued_quantity * p.matched_price, 4)
        END AS inr_value,
        CASE
            WHEN p.valued_quantity = 0 THEN 'not_required'
            WHEN p.matched_price IS NULL THEN 'missing'
            ELSE 'available'
        END AS price_status,
        CASE
            WHEN p.valued_quantity = 0 THEN NULL::TIMESTAMP
            ELSE p.matched_price_as_of
        END AS price_as_of,
        p.outcome_type,
        p.is_active,
        p.delist_effective_date,
        p.cash_proceeds,
        p.written_off_quantity
    FROM priced p;
$$;

COMMENT ON FUNCTION historical_rewards_as_of(DATE) IS
    'Historical reward valuation. NULL as_of = grant-date stock value with current lifecycle annotations; non-NULL = point-in-time quantity, price, delists, and adjustments as of that date.';

DROP VIEW IF EXISTS historical_rewards CASCADE;

CREATE OR REPLACE VIEW historical_rewards AS
SELECT * FROM historical_rewards_as_of(NULL::DATE);

COMMENT ON VIEW historical_rewards IS
    'Grant-date historical valuation with current lifecycle annotations. A later delist never removes a row or changes its grant-date value.';
