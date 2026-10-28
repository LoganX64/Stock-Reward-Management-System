-- Revert case normalization across instruments to the Phase 3.16 state.
--
-- The data normalization is intentionally one-way: symbols already stored in
-- canonical uppercase/trimmed form stay that way. This down migration only
-- restores the previous schema behavior (case-insensitive joins, UPPER-based
-- reward uniqueness, no symbol triggers/checks). It cannot recover the original
-- casing or surrounding whitespace.

-- ---------------------------------------------------------------------------
-- Restore Phase 3.16 function and view definitions
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION stock_action_factor(p_symbol TEXT, p_as_of DATE)
RETURNS NUMERIC
LANGUAGE SQL
STABLE
AS $$
    SELECT COALESCE(EXP(SUM(LN(se.ratio_num / se.ratio_den))), 1::numeric)
    FROM stock_events se
    WHERE UPPER(se.stock_symbol) = UPPER(p_symbol)
      AND se.event_type IN ('split', 'bonus')
      AND se.effective_date <= p_as_of
      AND se.ratio_num > 0
      AND se.ratio_den > 0;
$$;

CREATE OR REPLACE FUNCTION stock_quantity_factor(p_symbol TEXT, p_as_of DATE)
RETURNS NUMERIC
LANGUAGE SQL
STABLE
AS $$
    SELECT COALESCE(EXP(SUM(LN(se.ratio_num / se.ratio_den))), 1::numeric)
    FROM stock_events se
    WHERE UPPER(se.stock_symbol) = UPPER(p_symbol)
      AND se.event_type IN ('split', 'bonus', 'merge')
      AND se.effective_date <= p_as_of
      AND se.ratio_num > 0
      AND se.ratio_den > 0;
$$;

CREATE OR REPLACE VIEW instrument_lineage AS
SELECT
    se.id AS source_event_id,
    UPPER(se.stock_symbol) AS predecessor_symbol,
    UPPER(se.new_symbol) AS successor_symbol,
    se.ratio_num,
    se.ratio_den,
    se.effective_date
FROM stock_events se
WHERE se.event_type = 'merge'
  AND se.new_symbol IS NOT NULL
  AND BTRIM(se.new_symbol) <> ''
  AND se.ratio_num > 0
  AND se.ratio_den > 0
  AND UPPER(se.new_symbol) <> UPPER(se.stock_symbol);

CREATE OR REPLACE FUNCTION instrument_chain(
    p_symbol TEXT,
    p_from DATE,
    p_as_of DATE
)
RETURNS TABLE (
    step INTEGER,
    symbol TEXT,
    entry_date DATE,
    exit_date DATE,
    merge_ratio NUMERIC
)
LANGUAGE SQL
STABLE
AS $$
    WITH RECURSIVE chain AS (
        SELECT
            0 AS step,
            UPPER(p_symbol)::TEXT AS symbol,
            p_from AS entry_date,
            1::NUMERIC AS merge_ratio,
            ARRAY[UPPER(p_symbol)::TEXT] AS visited
        WHERE p_from <= p_as_of
        UNION ALL
        SELECT
            c.step + 1,
            il.successor_symbol,
            il.effective_date,
            il.ratio_num / il.ratio_den,
            c.visited || il.successor_symbol
        FROM chain c
        JOIN instrument_lineage il
          ON il.predecessor_symbol = c.symbol
        WHERE il.effective_date > c.entry_date
          AND il.effective_date <= p_as_of
          AND NOT (il.successor_symbol = ANY(c.visited))
    )
    SELECT
        c.step,
        c.symbol,
        c.entry_date,
        LEAD(c.entry_date) OVER (ORDER BY c.step) AS exit_date,
        c.merge_ratio
    FROM chain c;
$$;

CREATE OR REPLACE FUNCTION instrument_terminal_symbol(
    p_symbol TEXT,
    p_from DATE,
    p_as_of DATE
)
RETURNS TEXT
LANGUAGE SQL
STABLE
AS $$
    SELECT COALESCE(
        (
            SELECT c.symbol
            FROM instrument_chain(p_symbol, p_from, p_as_of) c
            ORDER BY c.step DESC
            LIMIT 1
        ),
        UPPER(p_symbol)
    );
$$;

CREATE OR REPLACE FUNCTION instrument_delist_event(
    p_symbol TEXT,
    p_from DATE,
    p_as_of DATE
)
RETURNS TABLE (
    event_id INTEGER,
    symbol TEXT,
    effective_date DATE,
    outcome TEXT,
    cash_per_unit NUMERIC
)
LANGUAGE SQL
STABLE
AS $$
    SELECT
        se.id,
        UPPER(se.stock_symbol)::TEXT,
        se.effective_date,
        se.delist_outcome,
        se.cash_per_unit
    FROM stock_events se
    WHERE se.event_type = 'delist'
      AND UPPER(se.stock_symbol) = instrument_terminal_symbol(p_symbol, p_from, p_as_of)
      AND se.effective_date <= p_as_of
    ORDER BY se.effective_date, se.id
    LIMIT 1;
$$;

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
        rw.reward_date
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
                se.effective_date AS action_date,
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
             AND se.effective_date > c.entry_date
             AND se.effective_date <= COALESCE(c.exit_date, p_as_of)
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
             AND il.effective_date = c.exit_date
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

CREATE OR REPLACE FUNCTION reward_corporate_action_outcome(
    p_reward_id INTEGER,
    p_as_of DATE DEFAULT market_today()
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
            r.reward_date
        FROM rewards r
        WHERE r.id = p_reward_id
          AND r.reward_date <= p_as_of
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
    price_as_of TIMESTAMPTZ,
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
            COALESCE(p_as_of, market_today()) AS lifecycle_as_of,
            (p_as_of IS NULL) AS grant_mode
    ),
    reward_rows AS (
        SELECT
            r.id AS reward_event_id,
            r.user_id,
            r.stock_symbol AS original_symbol,
            r.original_quantity,
            r.reward_date,
            p.as_of,
            p.lifecycle_as_of,
            p.grant_mode
        FROM rewards r
        CROSS JOIN params p
        WHERE p.as_of IS NULL
           OR r.reward_date <= p.as_of
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
            WHEN p.valued_quantity = 0 THEN NULL::TIMESTAMPTZ
            ELSE p.matched_price_as_of
        END AS price_as_of,
        p.outcome_type,
        p.is_active,
        p.delist_effective_date,
        p.cash_proceeds,
        p.written_off_quantity
    FROM priced p;
$$;

CREATE OR REPLACE VIEW historical_rewards AS
SELECT * FROM historical_rewards_as_of(NULL::DATE);

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
    spa.updated_at AS price_as_of
FROM reward_corporate_action_outcomes o
LEFT JOIN stock_prices_adjusted spa
    ON UPPER(o.terminal_symbol) = UPPER(spa.stock_symbol)
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
    spa.updated_at AS price_as_of
FROM reward_corporate_action_outcomes o
LEFT JOIN stock_prices_adjusted spa
    ON UPPER(o.terminal_symbol) = UPPER(spa.stock_symbol)
WHERE o.is_active
GROUP BY
    o.user_id,
    o.terminal_symbol,
    spa.adjusted_price,
    spa.updated_at;

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
                r.reward_date,
                market_date(l.created_at)
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
                r.reward_date,
                market_today()
            ) AS reward_quantity,
        COALESCE(mb.normalized_quantity, 0) AS reward_normalized_quantity,
        COALESCE(sp.normalized_quantity, 0)
            * instrument_quantity_factor(
                r.stock_symbol,
                r.reward_date,
                market_today()
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

-- Restore the Phase 3.16 stock-event normalization trigger body.
CREATE OR REPLACE FUNCTION normalize_stock_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.stock_symbol := UPPER(BTRIM(NEW.stock_symbol, E' \t\n\r'));

    IF NEW.new_symbol IS NOT NULL THEN
        NEW.new_symbol := UPPER(BTRIM(NEW.new_symbol, E' \t\n\r'));
    END IF;

    IF NEW.stock_symbol = '' THEN
        RAISE EXCEPTION 'stock event requires a non-blank stock_symbol';
    END IF;

    IF NEW.ratio_num <= 0 OR NEW.ratio_den <= 0 THEN
        RAISE EXCEPTION
            '% event for % requires positive ratios',
            NEW.event_type, NEW.stock_symbol;
    END IF;

    IF NEW.event_type = 'split' THEN
        IF NEW.ratio_num = NEW.ratio_den THEN
            RAISE EXCEPTION
                'split event for % requires a ratio other than 1:1',
                NEW.stock_symbol;
        END IF;
    ELSIF NEW.event_type = 'bonus' THEN
        IF NEW.ratio_num <= NEW.ratio_den THEN
            RAISE EXCEPTION
                'bonus event for % requires a ratio greater than 1:1',
                NEW.stock_symbol;
        END IF;
    ELSIF NEW.event_type = 'merge' THEN
        IF NEW.new_symbol IS NULL OR NEW.new_symbol = '' THEN
            RAISE EXCEPTION
                'merge event for % requires new_symbol',
                NEW.stock_symbol;
        END IF;
    ELSIF NEW.event_type = 'delist' THEN
        IF NEW.ratio_num <> NEW.ratio_den THEN
            RAISE EXCEPTION
                'delist event for % must use a 1:1 ratio',
                NEW.stock_symbol;
        END IF;

        NEW.ratio_num := 1;
        NEW.ratio_den := 1;
    END IF;

    RETURN NEW;
END;
$$;

-- ---------------------------------------------------------------------------
-- Remove canonical triggers, checks, and indexes
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS trg_00_normalize_stock_price_symbol ON stock_prices;
DROP TRIGGER IF EXISTS trg_00_normalize_stock_price_history_symbol ON stock_price_history;
DROP TRIGGER IF EXISTS trg_00_normalize_reward_symbol ON rewards;
DROP TRIGGER IF EXISTS trg_00_normalize_ledger_symbol ON ledger;

ALTER TABLE stock_prices
    DROP CONSTRAINT IF EXISTS stock_prices_symbol_canonical,
    DROP CONSTRAINT IF EXISTS stock_prices_symbol_not_blank;

ALTER TABLE stock_price_history
    DROP CONSTRAINT IF EXISTS stock_price_history_symbol_canonical,
    DROP CONSTRAINT IF EXISTS stock_price_history_symbol_not_blank;

ALTER TABLE rewards
    DROP CONSTRAINT IF EXISTS rewards_symbol_canonical,
    DROP CONSTRAINT IF EXISTS rewards_symbol_not_blank;

ALTER TABLE stock_events
    DROP CONSTRAINT IF EXISTS stock_events_new_symbol_canonical,
    DROP CONSTRAINT IF EXISTS stock_events_symbol_canonical;

ALTER TABLE ledger
    DROP CONSTRAINT IF EXISTS ledger_symbol_canonical;

DROP INDEX IF EXISTS unique_stock_price_symbol_normalized;
DROP INDEX IF EXISTS unique_stock_price_history_symbol_date_normalized;
DROP INDEX IF EXISTS unique_user_stock_date_normalized;

CREATE UNIQUE INDEX unique_user_stock_date_upper
    ON rewards (user_id, UPPER(stock_symbol), reward_date);

COMMENT ON COLUMN stock_prices.stock_symbol IS NULL;
COMMENT ON COLUMN stock_price_history.stock_symbol IS NULL;
COMMENT ON COLUMN rewards.stock_symbol IS NULL;
COMMENT ON COLUMN ledger.stock_symbol IS NULL;
COMMENT ON COLUMN stock_events.stock_symbol IS
    'Normalized uppercase, trimmed instrument symbol';
COMMENT ON COLUMN stock_events.new_symbol IS
    'Normalized uppercase, trimmed merger successor symbol';

DROP FUNCTION IF EXISTS normalize_instrument_symbol_trigger();
DROP FUNCTION IF EXISTS normalize_instrument_symbol(TEXT);
