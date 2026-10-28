-- Merger lineage and chains.
--
-- stock_events remains the action source of truth. instrument_lineage is a
-- normalized, derived view of merger edges so a chain (A -> B -> C) can be
-- resolved with one recursive traversal instead of a one-hop symbol lookup.

DO $$
DECLARE
    duplicate_predecessors INTEGER;
    invalid_merges INTEGER;
BEGIN
    SELECT COUNT(*) INTO duplicate_predecessors
    FROM (
        SELECT UPPER(stock_symbol)
        FROM stock_events
        WHERE event_type = 'merge'
        GROUP BY UPPER(stock_symbol)
        HAVING COUNT(*) > 1
    ) duplicates;

    IF duplicate_predecessors > 0 THEN
        RAISE EXCEPTION
            'Cannot create merger lineage: % predecessor symbol(s) have multiple merge events; keep one merge per predecessor',
            duplicate_predecessors;
    END IF;

    SELECT COUNT(*) INTO invalid_merges
    FROM stock_events
    WHERE event_type = 'merge'
      AND (
          new_symbol IS NULL
          OR BTRIM(new_symbol) = ''
          OR ratio_num <= 0
          OR ratio_den <= 0
          OR UPPER(new_symbol) = UPPER(stock_symbol)
      );

    IF invalid_merges > 0 THEN
        RAISE EXCEPTION
            'Cannot create merger lineage: % merge event(s) have a missing successor, non-positive ratio, or self-reference',
            invalid_merges;
    END IF;
END;
$$;

-- One successor per predecessor avoids ambiguous chains. The normalized
-- expression also rejects case-variant duplicate merge events.
CREATE UNIQUE INDEX IF NOT EXISTS unique_merge_predecessor_upper
    ON stock_events (UPPER(stock_symbol))
    WHERE event_type = 'merge';

CREATE INDEX IF NOT EXISTS idx_stock_events_upper_symbol_effective
    ON stock_events (UPPER(stock_symbol), effective_date);

CREATE OR REPLACE FUNCTION validate_merge_lineage_cycle()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.event_type <> 'merge' THEN
        RETURN NEW;
    END IF;

    IF NEW.new_symbol IS NULL OR BTRIM(NEW.new_symbol) = '' THEN
        RAISE EXCEPTION 'merge event for % requires new_symbol', NEW.stock_symbol;
    END IF;

    IF UPPER(NEW.new_symbol) = UPPER(NEW.stock_symbol) THEN
        RAISE EXCEPTION 'merge event for % cannot target itself', NEW.stock_symbol;
    END IF;

    IF NEW.ratio_num <= 0 OR NEW.ratio_den <= 0 THEN
        RAISE EXCEPTION 'merge event for % requires positive ratios', NEW.stock_symbol;
    END IF;

    IF EXISTS (
        WITH RECURSIVE reach(symbol, visited) AS (
            SELECT
                UPPER(NEW.new_symbol),
                ARRAY[UPPER(NEW.new_symbol)]
            UNION ALL
            SELECT
                UPPER(se.new_symbol),
                reach.visited || UPPER(se.new_symbol)
            FROM reach
            JOIN stock_events se
              ON se.event_type = 'merge'
             AND se.new_symbol IS NOT NULL
             AND UPPER(se.stock_symbol) = reach.symbol
             AND se.id <> COALESCE(NEW.id, -1)
             AND NOT (UPPER(se.new_symbol) = ANY(reach.visited))
        )
        SELECT 1
        FROM reach
        WHERE symbol = UPPER(NEW.stock_symbol)
    ) THEN
        RAISE EXCEPTION 'merge event for % creates an instrument lineage cycle', NEW.stock_symbol;
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_validate_merge_lineage_cycle ON stock_events;

CREATE TRIGGER trg_validate_merge_lineage_cycle
BEFORE INSERT OR UPDATE ON stock_events
FOR EACH ROW
EXECUTE FUNCTION validate_merge_lineage_cycle();

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

-- Returns one row per instrument in the merger chain. exit_date is the date
-- the holding leaves that instrument for its successor (null for the terminal
-- instrument). A segment only receives split/bonus events that happen while
-- the holding is actually in that instrument.
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
            il.effective_date::DATE,
            il.ratio_num / il.ratio_den,
            c.visited || il.successor_symbol
        FROM chain c
        JOIN instrument_lineage il
          ON il.predecessor_symbol = c.symbol
        WHERE il.effective_date::DATE > c.entry_date
          AND il.effective_date::DATE <= p_as_of
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

-- Composes merger ratios and split/bonus factors across the chain. The result
-- is the number of terminal-instrument units represented by one p_symbol unit
-- held from p_from through p_as_of.
CREATE OR REPLACE FUNCTION instrument_quantity_factor(
    p_symbol TEXT,
    p_from DATE,
    p_as_of DATE
)
RETURNS NUMERIC
LANGUAGE SQL
STABLE
AS $$
    SELECT ROUND(
        COALESCE(
            EXP(SUM(LN(
                c.merge_ratio
                * stock_action_factor(c.symbol, COALESCE(c.exit_date, p_as_of))
                / stock_action_factor(c.symbol, c.entry_date)
            ))),
            1::NUMERIC
        ),
        12
    )
    FROM instrument_chain(p_symbol, p_from, p_as_of) c;
$$;

-- A predecessor delist is ignored once the holding has moved to a successor.
-- Only a delist on the terminal instrument removes the holding.
CREATE OR REPLACE FUNCTION instrument_is_delisted(
    p_symbol TEXT,
    p_from DATE,
    p_as_of DATE
)
RETURNS BOOLEAN
LANGUAGE SQL
STABLE
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM stock_events se
        WHERE se.event_type = 'delist'
          AND UPPER(se.stock_symbol) = instrument_terminal_symbol(p_symbol, p_from, p_as_of)
          AND se.effective_date::DATE <= p_as_of
    );
$$;

DROP VIEW IF EXISTS reward_ledger_reconciliation CASCADE;
DROP VIEW IF EXISTS user_portfolio CASCADE;
DROP VIEW IF EXISTS today_rewards CASCADE;
DROP VIEW IF EXISTS historical_rewards CASCADE;
DROP VIEW IF EXISTS reward_current_balances CASCADE;
DROP VIEW IF EXISTS reward_quantity_movements CASCADE;

CREATE OR REPLACE VIEW reward_quantity_movements AS
SELECT
    r.id AS reward_id,
    r.user_id,
    r.stock_symbol,
    r.created_at::date AS effective_date,
    'reward_initial'::text AS movement_type,
    r.id AS source_id,
    r.original_quantity AS quantity_delta,
    r.original_quantity AS normalized_quantity_delta
FROM rewards r
UNION ALL
SELECT
    a.reward_id,
    r.user_id,
    r.stock_symbol,
    a.effective_date,
    'adjustment'::text AS movement_type,
    a.id AS source_id,
    a.delta_quantity AS quantity_delta,
    a.delta_quantity
        / instrument_quantity_factor(
            r.stock_symbol,
            r.created_at::date,
            a.effective_date
        ) AS normalized_quantity_delta
FROM adjustments a
JOIN rewards r ON r.id = a.reward_id;

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

CREATE OR REPLACE VIEW today_rewards AS
WITH reward_adjustments AS (
    SELECT reward_id, SUM(delta_amount) AS total_adjustment_amount
    FROM adjustments
    GROUP BY reward_id
)
SELECT
    rcb.reward_id AS reward_event_id,
    rcb.user_id,
    rcb.terminal_symbol AS stock_symbol,
    rcb.adjusted_quantity,
    spa.adjusted_price AS current_price,
    COALESCE(ra.total_adjustment_amount, 0) AS total_adjustment_amount,
    ROUND(rcb.adjusted_quantity * spa.adjusted_price, 4) AS inr_value
FROM rewards r
JOIN reward_current_balances rcb ON rcb.reward_id = r.id
JOIN stock_prices_adjusted spa
    ON UPPER(rcb.terminal_symbol) = UPPER(spa.stock_symbol)
LEFT JOIN reward_adjustments ra ON ra.reward_id = r.id
WHERE r.created_at::date = CURRENT_DATE
  AND rcb.adjusted_quantity > 0
  AND NOT instrument_is_delisted(
      r.stock_symbol,
      r.created_at::date,
      CURRENT_DATE
  );

CREATE OR REPLACE VIEW user_portfolio AS
WITH reward_adjustments AS (
    SELECT reward_id, SUM(delta_amount) AS total_adjustment_amount
    FROM adjustments
    GROUP BY reward_id
)
SELECT
    rcb.user_id,
    rcb.terminal_symbol AS stock_symbol,
    SUM(rcb.adjusted_quantity) AS adjusted_quantity,
    spa.adjusted_price AS current_price,
    ROUND(SUM(rcb.adjusted_quantity) * spa.adjusted_price, 4) AS inr_value,
    COALESCE(SUM(ra.total_adjustment_amount), 0) AS total_adjustment_amount
FROM rewards r
JOIN reward_current_balances rcb ON rcb.reward_id = r.id
LEFT JOIN reward_adjustments ra ON ra.reward_id = r.id
JOIN stock_prices_adjusted spa
    ON UPPER(rcb.terminal_symbol) = UPPER(spa.stock_symbol)
WHERE rcb.adjusted_quantity > 0
  AND NOT instrument_is_delisted(
      r.stock_symbol,
      r.created_at::date,
      CURRENT_DATE
  )
GROUP BY rcb.user_id, rcb.terminal_symbol, spa.adjusted_price;

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
