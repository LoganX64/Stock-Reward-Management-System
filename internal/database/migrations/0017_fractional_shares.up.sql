-- Fractional-share settlement.
--
-- Supported precision:
--   * quantities: 6 decimal places (NUMERIC(18,6))
--   * cash:       4 decimal places (NUMERIC(18,4))
--
-- Per-event policy:
--   * retain (default): keep fractional units at 6 decimal places.
--   * cash_in_lieu: floor the post-event quantity to whole units and pay the
--     fractional remainder at fraction_cash_per_unit. The price is expressed
--     per post-event (successor/current instrument) share.
--
-- Settlement is per reward lot. Fraction cash is posted to the ledger as a
-- positive inr_outflow row tagged with stock_event_id, so it is excluded from
-- adjustment-cash reconciliation and can be reconciled against the outcome
-- view.

-- ---------------------------------------------------------------------------
-- Event terms and validation
-- ---------------------------------------------------------------------------

ALTER TABLE stock_events
    ADD COLUMN IF NOT EXISTS fraction_cash_per_unit NUMERIC(18,4);

ALTER TABLE stock_events
    DROP CONSTRAINT IF EXISTS stock_events_fraction_policy_values;

ALTER TABLE stock_events
    ADD CONSTRAINT stock_events_fraction_policy_values
        CHECK (fraction_policy IN ('retain', 'cash_in_lieu')),
    ADD CONSTRAINT stock_events_fraction_cash_per_unit_positive
        CHECK (fraction_cash_per_unit IS NULL OR fraction_cash_per_unit > 0),
    ADD CONSTRAINT stock_events_fraction_cash_per_unit_event_type
        CHECK (fraction_cash_per_unit IS NULL OR event_type IN ('split', 'bonus', 'merge')),
    ADD CONSTRAINT stock_events_cash_in_lieu_requires_price
        CHECK (fraction_policy <> 'cash_in_lieu' OR fraction_cash_per_unit > 0),
    ADD CONSTRAINT stock_events_retain_has_no_fraction_price
        CHECK (fraction_policy <> 'retain' OR fraction_cash_per_unit IS NULL);

COMMENT ON COLUMN stock_events.fraction_policy IS
    'retain = keep fractional units at 6 dp; cash_in_lieu = floor to whole units and pay the fractional remainder';
COMMENT ON COLUMN stock_events.fraction_cash_per_unit IS
    'Cash paid per post-event fractional unit; required for cash_in_lieu and null for retain';

CREATE OR REPLACE FUNCTION validate_stock_event_fraction()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.fraction_policy NOT IN ('retain', 'cash_in_lieu') THEN
        RAISE EXCEPTION
            'stock event for % has invalid fraction_policy %',
            NEW.stock_symbol, NEW.fraction_policy;
    END IF;

    IF NEW.event_type = 'delist' THEN
        IF NEW.fraction_policy <> 'retain' THEN
            RAISE EXCEPTION
                'delist event for % cannot use cash_in_lieu; use cash_exit',
                NEW.stock_symbol;
        END IF;
        IF NEW.fraction_cash_per_unit IS NOT NULL THEN
            RAISE EXCEPTION
                'delist event for % cannot have fraction_cash_per_unit',
                NEW.stock_symbol;
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.fraction_policy = 'cash_in_lieu' THEN
        IF NEW.fraction_cash_per_unit IS NULL OR NEW.fraction_cash_per_unit <= 0 THEN
            RAISE EXCEPTION
                '% event for % requires positive fraction_cash_per_unit for cash_in_lieu',
                NEW.event_type, NEW.stock_symbol;
        END IF;
    ELSIF NEW.fraction_cash_per_unit IS NOT NULL THEN
        RAISE EXCEPTION
            '% event for % has fraction_cash_per_unit but fraction_policy is retain',
            NEW.event_type, NEW.stock_symbol;
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_validate_stock_event_fraction ON stock_events;

CREATE TRIGGER trg_validate_stock_event_fraction
BEFORE INSERT OR UPDATE ON stock_events
FOR EACH ROW
EXECUTE FUNCTION validate_stock_event_fraction();

-- ---------------------------------------------------------------------------
-- Ledger linkage for corporate-action cash
-- ---------------------------------------------------------------------------

ALTER TABLE ledger
    ADD COLUMN IF NOT EXISTS stock_event_id INTEGER
        REFERENCES stock_events(id) ON DELETE CASCADE;

COMMENT ON COLUMN ledger.stock_event_id IS
    'Corporate-action event that produced this cash posting; null for reward/adjustment postings';

CREATE INDEX IF NOT EXISTS idx_ledger_stock_event_id
    ON ledger (stock_event_id)
    WHERE stock_event_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS unique_ledger_reward_stock_event
    ON ledger (reward_id, stock_event_id)
    WHERE stock_event_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Sequential action settlement
-- ---------------------------------------------------------------------------

-- Walks a reward through its initial quantity, adjustments, and corporate
-- actions in effective-date order and returns one row per action. Same-date
-- order is split -> bonus -> merge -> adjustment. A cash-in-lieu event floors
-- the post-event quantity and records the fractional remainder.
CREATE OR REPLACE FUNCTION reward_action_settlements(
    p_reward_id INTEGER,
    p_as_of DATE
)
RETURNS TABLE (
    action_date DATE,
    action_order SMALLINT,
    action_id INTEGER,
    action_type TEXT,
    symbol TEXT,
    quantity_before NUMERIC,
    ratio NUMERIC,
    quantity_after NUMERIC,
    gross_quantity_after NUMERIC,
    fraction_quantity NUMERIC,
    cash_in_lieu NUMERIC,
    merger_cash NUMERIC
)
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    r RECORD;
    a RECORD;
    v_qty NUMERIC;
    v_gross NUMERIC;
    v_fraction NUMERIC;
    v_symbol TEXT;
    v_after NUMERIC;
    v_whole NUMERIC;
    v_removed NUMERIC;
    v_cash NUMERIC;
    v_merger NUMERIC;
BEGIN
    SELECT
        rw.id,
        rw.stock_symbol,
        rw.original_quantity,
        rw.created_at::date AS reward_date
    INTO r
    FROM rewards rw
    WHERE rw.id = p_reward_id;

    IF NOT FOUND OR p_as_of < r.reward_date THEN
        RETURN;
    END IF;

    v_qty := ROUND(r.original_quantity, 6);
    v_gross := v_qty;
    v_fraction := 0;
    v_symbol := UPPER(r.stock_symbol);

    action_date := r.reward_date;
    action_order := 0;
    action_id := r.id;
    action_type := 'reward_initial';
    symbol := v_symbol;
    quantity_before := 0;
    ratio := 1;
    quantity_after := v_qty;
    gross_quantity_after := v_gross;
    fraction_quantity := v_fraction;
    cash_in_lieu := 0;
    merger_cash := 0;
    RETURN NEXT;

    FOR a IN
        WITH chain AS (
            SELECT *
            FROM instrument_chain(r.stock_symbol, r.reward_date, p_as_of)
        ),
        actions AS (
            SELECT
                se.effective_date::date AS action_date,
                CASE se.event_type
                    WHEN 'split' THEN 1::SMALLINT
                    ELSE 2::SMALLINT
                END AS action_order,
                se.id AS action_id,
                se.event_type::text AS action_type,
                UPPER(se.stock_symbol)::text AS symbol,
                (se.ratio_num / se.ratio_den)::NUMERIC AS ratio,
                se.fraction_policy::text AS fraction_policy,
                se.fraction_cash_per_unit,
                NULL::NUMERIC AS cash_per_unit,
                NULL::text AS successor_symbol,
                NULL::NUMERIC AS delta_quantity
            FROM chain c
            JOIN stock_events se
              ON UPPER(se.stock_symbol) = c.symbol
             AND se.event_type IN ('split', 'bonus')
             AND se.effective_date::date > c.entry_date
             AND se.effective_date::date <= COALESCE(c.exit_date, p_as_of)
            UNION ALL
            SELECT
                c.exit_date,
                3::SMALLINT,
                se.id,
                'merge'::text,
                c.symbol,
                (il.ratio_num / il.ratio_den)::NUMERIC,
                se.fraction_policy::text,
                se.fraction_cash_per_unit,
                se.cash_per_unit,
                il.successor_symbol,
                NULL::NUMERIC
            FROM chain c
            JOIN instrument_lineage il
              ON il.predecessor_symbol = c.symbol
             AND il.effective_date::date = c.exit_date
            JOIN stock_events se
              ON se.id = il.source_event_id
            WHERE c.exit_date IS NOT NULL
            UNION ALL
            SELECT
                adj.effective_date,
                4::SMALLINT,
                adj.id,
                'adjustment'::text,
                NULL::text,
                NULL::NUMERIC,
                NULL::text,
                NULL::NUMERIC,
                NULL::NUMERIC,
                NULL::text,
                adj.delta_quantity
            FROM adjustments adj
            WHERE adj.reward_id = r.id
              AND adj.effective_date >= r.reward_date
              AND adj.effective_date <= p_as_of
        )
        SELECT *
        FROM actions
        ORDER BY action_date, action_order, action_id
    LOOP
        IF a.action_type = 'adjustment' THEN
            quantity_before := v_qty;
            v_qty := ROUND(v_qty + a.delta_quantity, 6);
            IF v_qty < 0 THEN
                v_qty := 0;
            END IF;
            v_gross := ROUND(v_qty + v_fraction, 6);

            action_date := a.action_date;
            action_order := a.action_order;
            action_id := a.action_id;
            action_type := a.action_type;
            symbol := v_symbol;
            ratio := NULL;
            quantity_after := v_qty;
            gross_quantity_after := v_gross;
            fraction_quantity := v_fraction;
            cash_in_lieu := 0;
            merger_cash := 0;
            RETURN NEXT;
        ELSE
            quantity_before := v_qty;
            v_after := ROUND(v_qty * a.ratio, 6);
            v_cash := 0;
            v_merger := 0;

            IF a.action_type = 'merge'
               AND a.cash_per_unit IS NOT NULL THEN
                v_merger := ROUND(quantity_before * a.cash_per_unit, 4);
            END IF;

            IF a.fraction_policy = 'cash_in_lieu' THEN
                v_whole := FLOOR(v_after);
                v_removed := ROUND(v_after - v_whole, 6);
                v_cash := ROUND(
                    v_removed * COALESCE(a.fraction_cash_per_unit, 0),
                    4
                );
                v_qty := v_whole;
                v_fraction := ROUND(v_fraction * a.ratio + v_removed, 6);
            ELSE
                v_qty := v_after;
                v_fraction := ROUND(v_fraction * a.ratio, 6);
            END IF;

            IF a.action_type = 'merge' THEN
                v_symbol := a.successor_symbol;
            END IF;

            v_gross := ROUND(v_qty + v_fraction, 6);

            action_date := a.action_date;
            action_order := a.action_order;
            action_id := a.action_id;
            action_type := a.action_type;
            symbol := v_symbol;
            ratio := a.ratio;
            quantity_after := v_qty;
            gross_quantity_after := v_gross;
            fraction_quantity := v_fraction;
            cash_in_lieu := v_cash;
            merger_cash := v_merger;
            RETURN NEXT;
        END IF;
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION reward_action_settlement(
    p_reward_id INTEGER,
    p_as_of DATE
)
RETURNS TABLE (
    settled_quantity NUMERIC,
    gross_quantity NUMERIC,
    fraction_quantity NUMERIC,
    cash_in_lieu NUMERIC,
    merger_cash NUMERIC,
    terminal_symbol TEXT
)
LANGUAGE SQL
STABLE
AS $$
    WITH actions AS (
        SELECT *
        FROM reward_action_settlements(p_reward_id, p_as_of)
    ),
    last_action AS (
        SELECT *
        FROM actions
        ORDER BY action_date DESC, action_order DESC, action_id DESC
        LIMIT 1
    ),
    totals AS (
        SELECT
            COALESCE(SUM(cash_in_lieu), 0) AS cash_in_lieu,
            COALESCE(SUM(merger_cash), 0) AS merger_cash
        FROM actions
    )
    SELECT
        COALESCE(last_action.quantity_after, 0),
        COALESCE(last_action.gross_quantity_after, 0),
        COALESCE(last_action.fraction_quantity, 0),
        ROUND(COALESCE(totals.cash_in_lieu, 0), 4),
        ROUND(COALESCE(totals.merger_cash, 0), 4),
        last_action.symbol
    FROM last_action
    CROSS JOIN totals;
$$;

-- Quantity on a given date after fraction settlement. Used by delist
-- outcomes and portfolio derivations.
CREATE OR REPLACE FUNCTION reward_quantity_at(
    p_reward_id INTEGER,
    p_as_of DATE
)
RETURNS NUMERIC
LANGUAGE SQL
STABLE
AS $$
    SELECT COALESCE(
        (
            SELECT s.settled_quantity
            FROM reward_action_settlement(p_reward_id, p_as_of) s
        ),
        0
    );
$$;

-- Cash consideration paid along the merger chain. Merger cash is based on the
-- pre-merger quantity, so it is not reduced by cash-in-lieu flooring.
CREATE OR REPLACE FUNCTION reward_merger_cash_proceeds(
    p_reward_id INTEGER,
    p_as_of DATE
)
RETURNS NUMERIC
LANGUAGE SQL
STABLE
AS $$
    SELECT COALESCE(
        (
            SELECT s.merger_cash
            FROM reward_action_settlement(p_reward_id, p_as_of) s
        ),
        0
    );
$$;

-- Per-reward, per-event cash derived from fraction settlement, merger
-- consideration, and terminal cash-exit delistings.
CREATE OR REPLACE FUNCTION reward_corporate_action_cash_details(p_as_of DATE)
RETURNS TABLE (
    reward_id INTEGER,
    stock_event_id INTEGER,
    stock_symbol TEXT,
    cash_amount NUMERIC
)
LANGUAGE SQL
STABLE
AS $$
    WITH event_cash AS (
        SELECT
            r.id AS reward_id,
            s.action_id AS stock_event_id,
            s.symbol AS stock_symbol,
            ROUND(s.cash_in_lieu + s.merger_cash, 4) AS cash_amount
        FROM rewards r
        CROSS JOIN LATERAL reward_action_settlements(r.id, p_as_of) s
        WHERE s.action_id IS NOT NULL
          AND s.action_type IN ('split', 'bonus', 'merge')
    ),
    delist_cash AS (
        SELECT
            r.id AS reward_id,
            d.event_id AS stock_event_id,
            d.symbol AS stock_symbol,
            ROUND(
                reward_quantity_at(r.id, d.effective_date) * d.cash_per_unit,
                4
            ) AS cash_amount
        FROM rewards r
        CROSS JOIN LATERAL instrument_delist_event(
            r.stock_symbol,
            r.created_at::date,
            p_as_of
        ) d
        WHERE d.outcome = 'cash_exit'
          AND d.cash_per_unit > 0
    )
    SELECT * FROM event_cash WHERE cash_amount > 0
    UNION ALL
    SELECT * FROM delist_cash WHERE cash_amount > 0;
$$;

-- Idempotently posts corporate-action cash to the ledger. Re-running after an
-- event change updates or removes the derived postings.
CREATE OR REPLACE FUNCTION process_corporate_action_cash(
    p_as_of DATE DEFAULT CURRENT_DATE
)
RETURNS INTEGER
LANGUAGE plpgsql
VOLATILE
AS $$
DECLARE
    v_count INTEGER := 0;
BEGIN
    PERFORM pg_advisory_xact_lock(810201710);

    DELETE FROM ledger l
    WHERE l.stock_event_id IS NOT NULL
      AND NOT EXISTS (
          SELECT 1
          FROM reward_corporate_action_cash_details(p_as_of) d
          WHERE d.reward_id = l.reward_id
            AND d.stock_event_id = l.stock_event_id
            AND d.cash_amount > 0
      );

    INSERT INTO ledger (
        reward_id,
        stock_event_id,
        entry_type,
        stock_symbol,
        quantity,
        amount,
        created_at
    )
    SELECT
        d.reward_id,
        d.stock_event_id,
        'inr_outflow'::ledger_entry_type,
        d.stock_symbol,
        0,
        d.cash_amount,
        NOW()
    FROM reward_corporate_action_cash_details(p_as_of) d
    WHERE d.cash_amount > 0
    ON CONFLICT (reward_id, stock_event_id) WHERE stock_event_id IS NOT NULL
    DO UPDATE SET
        amount = EXCLUDED.amount,
        stock_symbol = EXCLUDED.stock_symbol;

    GET DIAGNOSTICS v_count = ROW_COUNT;
    RETURN v_count;
END;
$$;

CREATE OR REPLACE FUNCTION sync_corporate_action_cash_trigger()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM process_corporate_action_cash(CURRENT_DATE);
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS trg_process_corporate_action_cash ON stock_events;

CREATE TRIGGER trg_process_corporate_action_cash
AFTER INSERT OR UPDATE OR DELETE ON stock_events
FOR EACH STATEMENT
EXECUTE FUNCTION sync_corporate_action_cash_trigger();

-- ---------------------------------------------------------------------------
-- Reporting views
-- ---------------------------------------------------------------------------

DROP VIEW IF EXISTS reward_ledger_reconciliation CASCADE;
DROP VIEW IF EXISTS user_corporate_action_cash CASCADE;
DROP VIEW IF EXISTS today_rewards CASCADE;
DROP VIEW IF EXISTS user_portfolio CASCADE;
DROP VIEW IF EXISTS reward_corporate_action_outcomes CASCADE;
DROP VIEW IF EXISTS reward_current_balances CASCADE;

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
    s.settled_quantity AS adjusted_quantity,
    s.terminal_symbol,
    s.fraction_quantity
FROM rewards r
JOIN movement_totals mt ON mt.reward_id = r.id
CROSS JOIN LATERAL reward_action_settlement(r.id, CURRENT_DATE) s;

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
        s.terminal_symbol,
        s.settled_quantity AS current_adjusted_quantity,
        s.fraction_quantity,
        s.cash_in_lieu,
        s.merger_cash
    FROM rewards r
    CROSS JOIN LATERAL reward_action_settlement(r.id, CURRENT_DATE) s
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
          AND initial.stock_event_id IS NULL
        ORDER BY initial.id
        LIMIT 1
    ) initial_cash ON TRUE
    WHERE l.entry_type = 'inr_outflow'
      AND l.stock_event_id IS NULL
    GROUP BY l.reward_id
),
corporate_action_cash_postings AS (
    SELECT
        reward_id,
        COALESCE(SUM(amount), 0) AS amount
    FROM ledger
    WHERE stock_event_id IS NOT NULL
    GROUP BY reward_id
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
            AS initial_cash_flow_matches,
        COALESCE(rcb.adjusted_quantity, 0) AS settled_quantity,
        COALESCE(rcb.fraction_quantity, 0) AS fraction_quantity,
        COALESCE(oc.cash_proceeds, 0) AS derived_corporate_action_cash,
        COALESCE(cacp.amount, 0) AS ledger_corporate_action_cash,
        COALESCE(oc.cash_proceeds, 0) = COALESCE(cacp.amount, 0)
            AS corporate_action_cash_matches
    FROM rewards r
    LEFT JOIN movement_balances mb ON mb.reward_id = r.id
    LEFT JOIN stock_postings sp ON sp.reward_id = r.id
    LEFT JOIN adjustment_totals at ON at.reward_id = r.id
    LEFT JOIN ledger_adjustment_cash lac ON lac.reward_id = r.id
    LEFT JOIN corporate_action_cash_postings cacp ON cacp.reward_id = r.id
    LEFT JOIN fee_postings fp ON fp.reward_id = r.id
    LEFT JOIN reward_current_balances rcb ON rcb.reward_id = r.id
    LEFT JOIN reward_corporate_action_outcomes oc ON oc.reward_id = r.id
    LEFT JOIN LATERAL (
        SELECT amount
        FROM ledger l
        WHERE l.reward_id = r.id
          AND l.entry_type = 'inr_outflow'
          AND l.stock_event_id IS NULL
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
        AND corporate_action_cash_matches
    ) AS is_reconciled
FROM checks;

-- Backfill existing corporate-action cash under the new policy.
DO $$
BEGIN
    PERFORM process_corporate_action_cash(CURRENT_DATE);
END;
$$;
