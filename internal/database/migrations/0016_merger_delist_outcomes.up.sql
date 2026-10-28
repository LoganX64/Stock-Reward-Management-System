-- Merger and delisting outcomes.
--
-- A stock event now carries explicit settlement terms:
--   * cash_per_unit: cash consideration per predecessor share for a merger,
--     or the cash exit price per share for a cash_exit delist.
--   * delist_outcome: cash_exit or write_off. Legacy delists are backfilled to
--     write_off, which matches the previous "remove from portfolio" behavior.
--   * fraction_policy: only 'retain' is implemented here; cash-in-lieu is a
--     later settlement phase.
--
-- A cash-only acquisition is represented as a delist with cash_exit because the
-- holder receives cash and no successor security.

DO $$
DECLARE
    duplicate_delists INTEGER;
    invalid_delist_order INTEGER;
BEGIN
    SELECT COUNT(*) INTO duplicate_delists
    FROM (
        SELECT UPPER(stock_symbol)
        FROM stock_events
        WHERE event_type = 'delist'
        GROUP BY UPPER(stock_symbol)
        HAVING COUNT(*) > 1
    ) duplicates;

    IF duplicate_delists > 0 THEN
        RAISE EXCEPTION
            'Cannot create delist outcomes: % symbol(s) have multiple delist events; keep one delist per symbol',
            duplicate_delists;
    END IF;

    SELECT COUNT(*) INTO invalid_delist_order
    FROM stock_events m
    JOIN stock_events d
      ON d.event_type = 'delist'
     AND (
          UPPER(d.stock_symbol) = UPPER(m.stock_symbol)
          OR UPPER(d.stock_symbol) = UPPER(m.new_symbol)
     )
    WHERE m.event_type = 'merge'
      AND d.effective_date::date <= m.effective_date::date;

    IF invalid_delist_order > 0 THEN
        RAISE EXCEPTION
            'Cannot create delist outcomes: % merge/delist pair(s) have a delist on or before the merge date',
            invalid_delist_order;
    END IF;
END;
$$;

ALTER TABLE stock_events
    ADD COLUMN cash_per_unit NUMERIC(18,4),
    ADD COLUMN delist_outcome VARCHAR(16),
    ADD COLUMN fraction_policy VARCHAR(16) NOT NULL DEFAULT 'retain';

UPDATE stock_events
SET delist_outcome = 'write_off'
WHERE event_type = 'delist'
  AND delist_outcome IS NULL;

ALTER TABLE stock_events
    ADD CONSTRAINT stock_events_cash_per_unit_nonnegative
        CHECK (cash_per_unit IS NULL OR cash_per_unit >= 0),
    ADD CONSTRAINT stock_events_cash_per_unit_event_type
        CHECK (cash_per_unit IS NULL OR event_type IN ('merge', 'delist')),
    ADD CONSTRAINT stock_events_delist_outcome_values
        CHECK (delist_outcome IS NULL OR delist_outcome IN ('cash_exit', 'write_off')),
    ADD CONSTRAINT stock_events_delist_outcome_event_type
        CHECK (delist_outcome IS NULL OR event_type = 'delist'),
    ADD CONSTRAINT stock_events_cash_exit_requires_cash
        CHECK (delist_outcome IS DISTINCT FROM 'cash_exit' OR cash_per_unit > 0),
    ADD CONSTRAINT stock_events_write_off_requires_no_cash
        CHECK (delist_outcome IS DISTINCT FROM 'write_off' OR cash_per_unit IS NULL OR cash_per_unit = 0),
    ADD CONSTRAINT stock_events_fraction_policy_values
        CHECK (fraction_policy = 'retain');

CREATE UNIQUE INDEX IF NOT EXISTS unique_delist_symbol_upper
    ON stock_events (UPPER(stock_symbol))
    WHERE event_type = 'delist';

DROP TRIGGER IF EXISTS trg_validate_merge_lineage_cycle ON stock_events;
DROP FUNCTION IF EXISTS validate_merge_lineage_cycle();

CREATE OR REPLACE FUNCTION validate_stock_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.event_type IN ('split', 'bonus') THEN
        IF NEW.ratio_num <= 0 OR NEW.ratio_den <= 0 THEN
            RAISE EXCEPTION '% event for % requires positive ratios', NEW.event_type, NEW.stock_symbol;
        END IF;
        IF NEW.new_symbol IS NOT NULL
           OR NEW.cash_per_unit IS NOT NULL
           OR NEW.delist_outcome IS NOT NULL THEN
            RAISE EXCEPTION '% event for % cannot carry merger or delist fields', NEW.event_type, NEW.stock_symbol;
        END IF;

    ELSIF NEW.event_type = 'merge' THEN
        IF NEW.delist_outcome IS NOT NULL THEN
            RAISE EXCEPTION 'merge event for % cannot carry a delist outcome', NEW.stock_symbol;
        END IF;
        IF NEW.new_symbol IS NULL OR BTRIM(NEW.new_symbol) = '' THEN
            RAISE EXCEPTION 'merge event for % requires new_symbol; use a cash_exit delist for an all-cash acquisition', NEW.stock_symbol;
        END IF;
        IF UPPER(NEW.new_symbol) = UPPER(NEW.stock_symbol) THEN
            RAISE EXCEPTION 'merge event for % cannot target itself', NEW.stock_symbol;
        END IF;
        IF NEW.ratio_num <= 0 OR NEW.ratio_den <= 0 THEN
            RAISE EXCEPTION 'merge event for % requires positive ratios', NEW.stock_symbol;
        END IF;
        IF EXISTS (
            SELECT 1
            FROM stock_events se
            WHERE se.event_type = 'delist'
              AND UPPER(se.stock_symbol) = UPPER(NEW.stock_symbol)
              AND se.effective_date::date <= NEW.effective_date::date
              AND se.id <> COALESCE(NEW.id, -1)
        ) THEN
            RAISE EXCEPTION 'merge event for % cannot occur on or after a delist of the predecessor', NEW.stock_symbol;
        END IF;
        IF EXISTS (
            SELECT 1
            FROM stock_events se
            WHERE se.event_type = 'delist'
              AND UPPER(se.stock_symbol) = UPPER(NEW.new_symbol)
              AND se.effective_date::date <= NEW.effective_date::date
              AND se.id <> COALESCE(NEW.id, -1)
        ) THEN
            RAISE EXCEPTION 'merge event for % cannot target delisted successor %', NEW.stock_symbol, NEW.new_symbol;
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

    ELSIF NEW.event_type = 'delist' THEN
        IF NEW.delist_outcome IS NULL THEN
            RAISE EXCEPTION 'delist event for % requires delist_outcome cash_exit or write_off', NEW.stock_symbol;
        END IF;
        IF NEW.delist_outcome NOT IN ('cash_exit', 'write_off') THEN
            RAISE EXCEPTION 'delist event for % has invalid delist_outcome %', NEW.stock_symbol, NEW.delist_outcome;
        END IF;
        IF NEW.delist_outcome = 'cash_exit'
           AND (NEW.cash_per_unit IS NULL OR NEW.cash_per_unit <= 0) THEN
            RAISE EXCEPTION 'cash_exit delist for % requires positive cash_per_unit', NEW.stock_symbol;
        END IF;
        IF NEW.delist_outcome = 'write_off'
           AND COALESCE(NEW.cash_per_unit, 0) <> 0 THEN
            RAISE EXCEPTION 'write_off delist for % cannot have cash_per_unit', NEW.stock_symbol;
        END IF;
        IF NEW.new_symbol IS NOT NULL THEN
            RAISE EXCEPTION 'delist event for % cannot have new_symbol', NEW.stock_symbol;
        END IF;
        IF EXISTS (
            SELECT 1
            FROM stock_events se
            WHERE se.event_type = 'merge'
              AND (
                  UPPER(se.stock_symbol) = UPPER(NEW.stock_symbol)
                  OR UPPER(se.new_symbol) = UPPER(NEW.stock_symbol)
              )
              AND se.effective_date::date >= NEW.effective_date::date
              AND se.id <> COALESCE(NEW.id, -1)
        ) THEN
            RAISE EXCEPTION 'delist event for % cannot precede an existing merge', NEW.stock_symbol;
        END IF;

    ELSE
        RAISE EXCEPTION 'unsupported stock event type %', NEW.event_type;
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_validate_stock_event
BEFORE INSERT OR UPDATE ON stock_events
FOR EACH ROW
EXECUTE FUNCTION validate_stock_event();

-- Returns the terminal instrument's effective delist event, if any. A delist
-- on a predecessor is ignored because instrument_terminal_symbol resolves the
-- holding to its successor first.
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
        se.effective_date::DATE,
        se.delist_outcome,
        se.cash_per_unit
    FROM stock_events se
    WHERE se.event_type = 'delist'
      AND UPPER(se.stock_symbol) = instrument_terminal_symbol(p_symbol, p_from, p_as_of)
      AND se.effective_date::DATE <= p_as_of
    ORDER BY se.effective_date, se.id
    LIMIT 1;
$$;

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
        FROM instrument_delist_event(p_symbol, p_from, p_as_of)
    );
$$;

-- Quantity on a given date. Adjustments are included only through p_as_of and
-- are already normalized to reward-date units by reward_quantity_movements.
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

-- Cash consideration paid along the merger chain. cash_per_unit is denominated
-- in predecessor shares on the merger effective date, so the successor quantity
-- at that date is divided back by the merger ratio before multiplying.
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

-- One row per reward. Current reporting paths use is_active; a terminal delist
-- always removes the stock holding and records either cash proceeds or a
-- written-off quantity. Historical valuation remains point-in-time.
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

DROP VIEW IF EXISTS today_rewards CASCADE;
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

DROP VIEW IF EXISTS user_portfolio CASCADE;
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
