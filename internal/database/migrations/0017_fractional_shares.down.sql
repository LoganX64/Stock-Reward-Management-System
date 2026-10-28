-- Revert fractional-share settlement and restore the phase 2.9 definitions.

DROP VIEW IF EXISTS reward_ledger_reconciliation CASCADE;
DROP VIEW IF EXISTS user_corporate_action_cash CASCADE;
DROP VIEW IF EXISTS today_rewards CASCADE;
DROP VIEW IF EXISTS user_portfolio CASCADE;
DROP VIEW IF EXISTS reward_corporate_action_outcomes CASCADE;
DROP VIEW IF EXISTS reward_current_balances CASCADE;

DROP TRIGGER IF EXISTS trg_process_corporate_action_cash ON stock_events;
DROP FUNCTION IF EXISTS sync_corporate_action_cash_trigger();
DROP FUNCTION IF EXISTS process_corporate_action_cash(DATE);
DROP FUNCTION IF EXISTS reward_corporate_action_cash_details(DATE);
DROP FUNCTION IF EXISTS reward_action_settlement(INTEGER, DATE);
DROP FUNCTION IF EXISTS reward_action_settlements(INTEGER, DATE);
DROP FUNCTION IF EXISTS reward_quantity_at(INTEGER, DATE);
DROP FUNCTION IF EXISTS reward_merger_cash_proceeds(INTEGER, DATE);

DROP TRIGGER IF EXISTS trg_validate_stock_event_fraction ON stock_events;
DROP FUNCTION IF EXISTS validate_stock_event_fraction();

DELETE FROM ledger WHERE stock_event_id IS NOT NULL;

DROP INDEX IF EXISTS unique_ledger_reward_stock_event;
DROP INDEX IF EXISTS idx_ledger_stock_event_id;

ALTER TABLE ledger
    DROP COLUMN IF EXISTS stock_event_id;

UPDATE stock_events
SET fraction_policy = 'retain',
    fraction_cash_per_unit = NULL
WHERE fraction_policy <> 'retain'
   OR fraction_cash_per_unit IS NOT NULL;

ALTER TABLE stock_events
    DROP CONSTRAINT IF EXISTS stock_events_retain_has_no_fraction_price,
    DROP CONSTRAINT IF EXISTS stock_events_cash_in_lieu_requires_price,
    DROP CONSTRAINT IF EXISTS stock_events_fraction_cash_per_unit_event_type,
    DROP CONSTRAINT IF EXISTS stock_events_fraction_cash_per_unit_positive,
    DROP CONSTRAINT IF EXISTS stock_events_fraction_policy_values,
    DROP COLUMN IF EXISTS fraction_cash_per_unit;

ALTER TABLE stock_events
    ADD CONSTRAINT stock_events_fraction_policy_values
        CHECK (fraction_policy = 'retain');

COMMENT ON COLUMN stock_events.fraction_policy IS NULL;

-- ---------------------------------------------------------------------------
-- Restore phase 2.9 functions
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION reward_quantity_at(
    p_reward_id INTEGER,
    p_as_of DATE
)
RETURNS NUMERIC
LANGUAGE SQL
STABLE
AS $$
    SELECT ROUND(
        COALESCE(SUM(m.normalized_quantity_delta), 0)
        * instrument_quantity_factor(r.stock_symbol, r.created_at::date, p_as_of),
        6
    )
    FROM rewards r
    LEFT JOIN reward_quantity_movements m
      ON m.reward_id = r.id
     AND m.effective_date <= p_as_of
    WHERE r.id = p_reward_id
    GROUP BY r.id, r.stock_symbol, r.created_at;
$$;

CREATE OR REPLACE FUNCTION reward_merger_cash_proceeds(
    p_reward_id INTEGER,
    p_as_of DATE
)
RETURNS NUMERIC
LANGUAGE SQL
STABLE
AS $$
    WITH RECURSIVE walk AS (
        SELECT
            r.id AS reward_id,
            0 AS step,
            UPPER(r.stock_symbol)::TEXT AS symbol,
            r.created_at::date AS entry_date,
            ARRAY[UPPER(r.stock_symbol)::TEXT] AS visited
        FROM rewards r
        WHERE r.id = p_reward_id
        UNION ALL
        SELECT
            w.reward_id,
            w.step + 1,
            il.successor_symbol,
            il.effective_date::DATE,
            w.visited || il.successor_symbol
        FROM walk w
        JOIN instrument_lineage il
          ON il.predecessor_symbol = w.symbol
        WHERE il.effective_date::DATE > w.entry_date
          AND il.effective_date::DATE <= p_as_of
          AND NOT (il.successor_symbol = ANY(w.visited))
    )
    SELECT ROUND(COALESCE(SUM(
        (
            reward_quantity_at(w.reward_id, il.effective_date::DATE)
            / (il.ratio_num / il.ratio_den)
        ) * COALESCE(se.cash_per_unit, 0)
    ), 0), 4)
    FROM walk w
    JOIN instrument_lineage il
      ON il.predecessor_symbol = w.symbol
    JOIN stock_events se
      ON se.id = il.source_event_id
    WHERE il.effective_date::DATE > w.entry_date
      AND il.effective_date::DATE <= p_as_of
      AND COALESCE(se.cash_per_unit, 0) > 0;
$$;

-- ---------------------------------------------------------------------------
-- Restore phase 2.8 balance view (needed by the phase 2.9 outcome view)
-- ---------------------------------------------------------------------------

CREATE OR REPLACE VIEW reward_current_balances AS
WITH movement_totals AS (
    SELECT
        reward_id,
        SUM(normalized_quantity_delta) AS normalized_quantity
    FROM reward_quantity_movements
    GROUP BY reward_id
)
SELECT
    r.id AS reward_id,
    r.user_id,
    r.stock_symbol,
    mt.normalized_quantity AS normalized_quantity,
    mt.normalized_quantity
        * instrument_quantity_factor(
            r.stock_symbol,
            r.created_at::date,
            CURRENT_DATE
        ) AS adjusted_quantity,
    instrument_terminal_symbol(
        r.stock_symbol,
        r.created_at::date,
        CURRENT_DATE
    ) AS terminal_symbol
FROM rewards r
JOIN movement_totals mt ON mt.reward_id = r.id;

-- ---------------------------------------------------------------------------
-- Restore phase 2.9 outcome views
-- ---------------------------------------------------------------------------

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
        r.created_at::date AS reward_date,
        rcb.terminal_symbol,
        rcb.adjusted_quantity AS current_adjusted_quantity
    FROM rewards r
    JOIN reward_current_balances rcb ON rcb.reward_id = r.id
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
        FROM instrument_delist_event(b.original_symbol, b.reward_date, CURRENT_DATE)
    ) d ON TRUE
),
cash AS (
    SELECT
        b.reward_id,
        ROUND(reward_merger_cash_proceeds(b.reward_id, CURRENT_DATE), 4) AS merger_cash_proceeds
    FROM balances b
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
        COALESCE(c.merger_cash_proceeds, 0)
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
    (d.delist_event_id IS NULL AND b.current_adjusted_quantity > 0) AS is_active
FROM balances b
LEFT JOIN delist d ON d.reward_id = b.reward_id
LEFT JOIN cash c ON c.reward_id = b.reward_id
LEFT JOIN reward_adjustments ra ON ra.reward_id = b.reward_id;

CREATE OR REPLACE VIEW user_corporate_action_cash AS
SELECT
    user_id,
    ROUND(COALESCE(SUM(cash_proceeds), 0), 4) AS cash_proceeds,
    ROUND(COALESCE(SUM(written_off_quantity), 0), 6) AS written_off_quantity
FROM reward_corporate_action_outcomes
GROUP BY user_id;

CREATE OR REPLACE VIEW today_rewards AS
SELECT
    o.reward_id AS reward_event_id,
    o.user_id,
    o.terminal_symbol AS stock_symbol,
    o.adjusted_quantity,
    spa.adjusted_price AS current_price,
    o.total_adjustment_amount,
    ROUND(o.adjusted_quantity * spa.adjusted_price, 4) AS inr_value
FROM reward_corporate_action_outcomes o
JOIN stock_prices_adjusted spa
    ON UPPER(o.terminal_symbol) = UPPER(spa.stock_symbol)
WHERE o.reward_date = CURRENT_DATE
  AND o.is_active;

CREATE OR REPLACE VIEW user_portfolio AS
SELECT
    o.user_id,
    o.terminal_symbol AS stock_symbol,
    SUM(o.adjusted_quantity) AS adjusted_quantity,
    spa.adjusted_price AS current_price,
    ROUND(SUM(o.adjusted_quantity) * spa.adjusted_price, 4) AS inr_value,
    COALESCE(SUM(o.total_adjustment_amount), 0) AS total_adjustment_amount
FROM reward_corporate_action_outcomes o
JOIN stock_prices_adjusted spa
    ON UPPER(o.terminal_symbol) = UPPER(spa.stock_symbol)
WHERE o.is_active
GROUP BY o.user_id, o.terminal_symbol, spa.adjusted_price;

-- ---------------------------------------------------------------------------
-- Restore phase 2.8 reconciliation view
-- ---------------------------------------------------------------------------

CREATE OR REPLACE VIEW reward_ledger_reconciliation AS
WITH movement_balances AS (
    SELECT
        reward_id,
        COALESCE(SUM(normalized_quantity_delta), 0) AS normalized_quantity
    FROM reward_quantity_movements
    GROUP BY reward_id
),
stock_postings AS (
    SELECT
        l.reward_id,
        COALESCE(SUM(l.quantity), 0) AS recorded_quantity,
        COALESCE(SUM(
            l.quantity
            / instrument_quantity_factor(
                r.stock_symbol,
                r.created_at::date,
                l.created_at::date
            )
        ), 0) AS normalized_quantity,
        COUNT(*) FILTER (
            WHERE l.stock_symbol IS NULL
               OR UPPER(l.stock_symbol) <> UPPER(r.stock_symbol)
        ) AS wrong_symbol_count
    FROM ledger l
    JOIN rewards r ON r.id = l.reward_id
    WHERE l.entry_type = 'stock_units'
    GROUP BY l.reward_id
),
adjustment_totals AS (
    SELECT reward_id, COALESCE(SUM(delta_amount), 0) AS amount
    FROM adjustments
    GROUP BY reward_id
),
ledger_adjustment_cash AS (
    SELECT
        l.reward_id,
        COALESCE(SUM(l.amount) FILTER (WHERE l.id <> initial_cash.id), 0) AS amount
    FROM ledger l
    JOIN LATERAL (
        SELECT id
        FROM ledger initial
        WHERE initial.reward_id = l.reward_id
          AND initial.entry_type = 'inr_outflow'
        ORDER BY initial.id
        LIMIT 1
    ) initial_cash ON TRUE
    WHERE l.entry_type = 'inr_outflow'
    GROUP BY l.reward_id
),
fee_postings AS (
    SELECT
        reward_id,
        COALESCE(SUM(amount) FILTER (WHERE entry_type = 'brokerage_fee'), 0) AS brokerage,
        COALESCE(SUM(amount) FILTER (WHERE entry_type = 'stt_fee'), 0) AS stt,
        COALESCE(SUM(amount) FILTER (WHERE entry_type = 'gst_fee'), 0) AS gst
    FROM ledger
    GROUP BY reward_id
),
checks AS (
    SELECT
        r.id AS reward_id,
        r.user_id,
        r.stock_symbol,
        COALESCE(mb.normalized_quantity, 0)
            * instrument_quantity_factor(
                r.stock_symbol,
                r.created_at::date,
                CURRENT_DATE
            ) AS reward_quantity,
        COALESCE(mb.normalized_quantity, 0) AS reward_normalized_quantity,
        COALESCE(sp.normalized_quantity, 0)
            * instrument_quantity_factor(
                r.stock_symbol,
                r.created_at::date,
                CURRENT_DATE
            ) AS ledger_stock_quantity,
        COALESCE(sp.normalized_quantity, 0) AS ledger_normalized_quantity,
        COALESCE(sp.recorded_quantity, 0) AS ledger_recorded_quantity,
        COALESCE(sp.wrong_symbol_count, 0) AS wrong_symbol_stock_postings,
        COALESCE(mb.normalized_quantity, 0) = COALESCE(sp.normalized_quantity, 0)
            AND COALESCE(sp.wrong_symbol_count, 0) = 0 AS stock_quantity_matches,
        COALESCE(at.amount, 0) AS adjustments_amount,
        COALESCE(lac.amount, 0) AS ledger_adjustment_amount,
        COALESCE(at.amount, 0) = COALESCE(lac.amount, 0) AS adjustment_amount_matches,
        COALESCE((r.original_response #>> '{fees,brokerage}')::numeric, 0) AS expected_brokerage_fee,
        -COALESCE(fp.brokerage, 0) AS ledger_brokerage_fee,
        COALESCE((r.original_response #>> '{fees,brokerage}')::numeric, 0) = -COALESCE(fp.brokerage, 0)
            AS brokerage_fee_matches,
        COALESCE((r.original_response #>> '{fees,stt}')::numeric, 0) AS expected_stt_fee,
        -COALESCE(fp.stt, 0) AS ledger_stt_fee,
        COALESCE((r.original_response #>> '{fees,stt}')::numeric, 0) = -COALESCE(fp.stt, 0)
            AS stt_fee_matches,
        COALESCE((r.original_response #>> '{fees,gst}')::numeric, 0) AS expected_gst_fee,
        -COALESCE(fp.gst, 0) AS ledger_gst_fee,
        COALESCE((r.original_response #>> '{fees,gst}')::numeric, 0) = -COALESCE(fp.gst, 0)
            AS gst_fee_matches,
        COALESCE(initial_cash.amount, 0) AS initial_ledger_cash_flow,
        -COALESCE((r.original_response ->> 'amount_inr')::numeric, 0) AS expected_initial_cash_flow,
        COALESCE(initial_cash.amount, 0) = -COALESCE((r.original_response ->> 'amount_inr')::numeric, 0)
            AS initial_cash_flow_matches
    FROM rewards r
    LEFT JOIN movement_balances mb ON mb.reward_id = r.id
    LEFT JOIN stock_postings sp ON sp.reward_id = r.id
    LEFT JOIN adjustment_totals at ON at.reward_id = r.id
    LEFT JOIN ledger_adjustment_cash lac ON lac.reward_id = r.id
    LEFT JOIN fee_postings fp ON fp.reward_id = r.id
    LEFT JOIN LATERAL (
        SELECT amount
        FROM ledger l
        WHERE l.reward_id = r.id AND l.entry_type = 'inr_outflow'
        ORDER BY l.id
        LIMIT 1
    ) initial_cash ON TRUE
)
SELECT
    checks.*,
    (
        stock_quantity_matches
        AND adjustment_amount_matches
        AND brokerage_fee_matches
        AND stt_fee_matches
        AND gst_fee_matches
        AND initial_cash_flow_matches
    ) AS is_reconciled
FROM checks;
