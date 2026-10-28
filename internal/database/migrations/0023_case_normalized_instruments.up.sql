-- Case normalization across instruments.
--
-- Instrument symbols are fundamental join keys for prices, price history,
-- corporate events, rewards, and ledger postings. PostgreSQL string equality
-- and unique constraints are case-sensitive, so "TCS", "tcs", and " TCS " could
-- previously coexist. The valuation views joined with UPPER(...), which meant
-- case variants could multiply rows or make a price lookup nondeterministic.
--
-- This migration establishes one canonical representation:
--
--     normalize_instrument_symbol(symbol)
--         = UPPER(BTRIM(symbol, E' \t\n\r'))
--
-- Rules:
--   * every symbol-bearing table stores the canonical form;
--   * BEFORE INSERT OR UPDATE triggers normalize new writes;
--   * CHECK constraints reject non-canonical values if a trigger is bypassed;
--   * functional unique indexes enforce normalized uniqueness;
--   * joins, lookups, and settlement functions use exact canonical equality.
--
-- Existing duplicate policy:
--   * stock_prices and stock_price_history are reference data. The latest row
--     wins, deterministically, and older case/whitespace variants are removed.
--   * rewards and stock_events are financial/audit records. If normalized
--     duplicates exist, the migration aborts instead of merging or deleting
--     them. Deleting a stock_event would cascade to ledger postings.
--   * ledger symbols are normalized in place; ledger has no symbol uniqueness.
--
-- Data normalization is one-way. The down migration restores the previous
-- schema behavior but cannot restore the original casing or whitespace.

-- ---------------------------------------------------------------------------
-- Canonical normalization helper
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION normalize_instrument_symbol(p_symbol TEXT)
RETURNS TEXT
LANGUAGE SQL
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT UPPER(BTRIM(p_symbol, E' \t\n\r'));
$$;

COMMENT ON FUNCTION normalize_instrument_symbol(TEXT) IS
    'Canonical instrument symbol: trimmed ASCII whitespace and uppercased. Used for storage, uniqueness, and joins.';

-- Shared row trigger for symbols that don't have additional event fields.
CREATE OR REPLACE FUNCTION normalize_instrument_symbol_trigger()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.stock_symbol := normalize_instrument_symbol(NEW.stock_symbol);
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION normalize_instrument_symbol_trigger() IS
    'BEFORE trigger that canonicalizes NEW.stock_symbol using normalize_instrument_symbol(TEXT).';

-- ---------------------------------------------------------------------------
-- Preflight: reject ambiguous financial duplicates before changing data
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    v_blank_prices INTEGER;
    v_blank_history INTEGER;
    v_blank_rewards INTEGER;
    v_reward_duplicates INTEGER;
    v_event_duplicates INTEGER;
BEGIN
    SELECT COUNT(*) INTO v_blank_prices
    FROM stock_prices
    WHERE normalize_instrument_symbol(stock_symbol) = '';

    SELECT COUNT(*) INTO v_blank_history
    FROM stock_price_history
    WHERE normalize_instrument_symbol(stock_symbol) = '';

    SELECT COUNT(*) INTO v_blank_rewards
    FROM rewards
    WHERE normalize_instrument_symbol(stock_symbol) = '';

    IF v_blank_prices > 0 OR v_blank_history > 0 OR v_blank_rewards > 0 THEN
        RAISE EXCEPTION
            'Cannot normalize instrument symbols: blank symbols exist (stock_prices=%, stock_price_history=%, rewards=%)',
            v_blank_prices, v_blank_history, v_blank_rewards;
    END IF;

    SELECT COUNT(*) INTO v_reward_duplicates
    FROM (
        SELECT 1
        FROM rewards
        GROUP BY
            user_id,
            normalize_instrument_symbol(stock_symbol),
            reward_date
        HAVING COUNT(*) > 1
    ) duplicates;

    IF v_reward_duplicates > 0 THEN
        RAISE EXCEPTION
            'Cannot normalize rewards: % normalized symbol/user/date duplicate group(s) exist. Resolve them manually; rewards are financial records and are never deleted automatically.',
            v_reward_duplicates;
    END IF;

    SELECT COUNT(*) INTO v_event_duplicates
    FROM (
        SELECT 1
        FROM stock_events
        GROUP BY
            normalize_instrument_symbol(stock_symbol),
            event_type,
            effective_date
        HAVING COUNT(*) > 1
    ) duplicates;

    IF v_event_duplicates > 0 THEN
        RAISE EXCEPTION
            'Cannot normalize stock events: % normalized symbol/type/date duplicate group(s) exist. Resolve them manually; deleting an event would cascade to ledger postings.',
            v_event_duplicates;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Resolve reference-data duplicates and normalize every stored symbol
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    v_deleted_prices INTEGER;
    v_deleted_history INTEGER;
    v_normalized_prices INTEGER;
    v_normalized_history INTEGER;
    v_normalized_rewards INTEGER;
    v_normalized_ledger INTEGER;
    v_normalized_events INTEGER;
BEGIN
    -- Latest update wins. On a timestamp tie, prefer the row that is already
    -- canonical, then the newest row id. This makes the outcome deterministic.
    WITH ranked AS (
        SELECT
            id,
            ROW_NUMBER() OVER (
                PARTITION BY normalize_instrument_symbol(stock_symbol)
                ORDER BY
                    updated_at DESC NULLS LAST,
                    (stock_symbol = normalize_instrument_symbol(stock_symbol)) DESC,
                    id DESC
            ) AS rn
        FROM stock_prices
    )
    DELETE FROM stock_prices sp
    USING ranked r
    WHERE sp.id = r.id
      AND r.rn > 1;

    GET DIAGNOSTICS v_deleted_prices = ROW_COUNT;

    -- Latest history write wins per normalized symbol and market date.
    WITH ranked AS (
        SELECT
            id,
            ROW_NUMBER() OVER (
                PARTITION BY normalize_instrument_symbol(stock_symbol), date
                ORDER BY
                    created_at DESC NULLS LAST,
                    (stock_symbol = normalize_instrument_symbol(stock_symbol)) DESC,
                    id DESC
            ) AS rn
        FROM stock_price_history
    )
    DELETE FROM stock_price_history sph
    USING ranked r
    WHERE sph.id = r.id
      AND r.rn > 1;

    GET DIAGNOSTICS v_deleted_history = ROW_COUNT;

    UPDATE stock_prices
    SET stock_symbol = normalize_instrument_symbol(stock_symbol)
    WHERE stock_symbol <> normalize_instrument_symbol(stock_symbol);
    GET DIAGNOSTICS v_normalized_prices = ROW_COUNT;

    UPDATE stock_price_history
    SET stock_symbol = normalize_instrument_symbol(stock_symbol)
    WHERE stock_symbol <> normalize_instrument_symbol(stock_symbol);
    GET DIAGNOSTICS v_normalized_history = ROW_COUNT;

    UPDATE rewards
    SET stock_symbol = normalize_instrument_symbol(stock_symbol)
    WHERE stock_symbol <> normalize_instrument_symbol(stock_symbol);
    GET DIAGNOSTICS v_normalized_rewards = ROW_COUNT;

    UPDATE ledger
    SET stock_symbol = normalize_instrument_symbol(stock_symbol)
    WHERE stock_symbol IS NOT NULL
      AND stock_symbol <> normalize_instrument_symbol(stock_symbol);
    GET DIAGNOSTICS v_normalized_ledger = ROW_COUNT;

    -- Updating stock_events fires the corporate-action cash processor. Disable
    -- it while symbols are rewritten; the rewrite is semantically neutral.
    ALTER TABLE stock_events
        DISABLE TRIGGER trg_process_corporate_action_cash;

    UPDATE stock_events
    SET stock_symbol = normalize_instrument_symbol(stock_symbol),
        new_symbol = CASE
            WHEN new_symbol IS NULL THEN NULL
            ELSE normalize_instrument_symbol(new_symbol)
        END
    WHERE stock_symbol <> normalize_instrument_symbol(stock_symbol)
       OR (
           new_symbol IS NOT NULL
           AND new_symbol <> normalize_instrument_symbol(new_symbol)
       );
    GET DIAGNOSTICS v_normalized_events = ROW_COUNT;

    ALTER TABLE stock_events
        ENABLE TRIGGER trg_process_corporate_action_cash;

    RAISE NOTICE
        'Instrument normalization removed % stock_prices row(s) and % stock_price_history row(s); normalized % prices, % history rows, % rewards, % ledger rows, and % stock events',
        v_deleted_prices,
        v_deleted_history,
        v_normalized_prices,
        v_normalized_history,
        v_normalized_rewards,
        v_normalized_ledger,
        v_normalized_events;
END
$$;

-- ---------------------------------------------------------------------------
-- Database-enforced canonical storage
-- ---------------------------------------------------------------------------

ALTER TABLE stock_prices
    ADD CONSTRAINT stock_prices_symbol_not_blank
        CHECK (normalize_instrument_symbol(stock_symbol) <> ''),
    ADD CONSTRAINT stock_prices_symbol_canonical
        CHECK (stock_symbol = normalize_instrument_symbol(stock_symbol));

ALTER TABLE stock_price_history
    ADD CONSTRAINT stock_price_history_symbol_not_blank
        CHECK (normalize_instrument_symbol(stock_symbol) <> ''),
    ADD CONSTRAINT stock_price_history_symbol_canonical
        CHECK (stock_symbol = normalize_instrument_symbol(stock_symbol));

ALTER TABLE rewards
    ADD CONSTRAINT rewards_symbol_not_blank
        CHECK (normalize_instrument_symbol(stock_symbol) <> ''),
    ADD CONSTRAINT rewards_symbol_canonical
        CHECK (stock_symbol = normalize_instrument_symbol(stock_symbol));

-- stock_events already has non-blank checks from 0018; add canonical checks.
ALTER TABLE stock_events
    ADD CONSTRAINT stock_events_symbol_canonical
        CHECK (stock_symbol = normalize_instrument_symbol(stock_symbol)),
    ADD CONSTRAINT stock_events_new_symbol_canonical
        CHECK (
            new_symbol IS NULL
            OR new_symbol = normalize_instrument_symbol(new_symbol)
        );

-- Ledger keeps NULL and empty-string symbols for non-stock postings. Any
-- non-empty symbol must already be canonical.
ALTER TABLE ledger
    ADD CONSTRAINT ledger_symbol_canonical
        CHECK (
            stock_symbol IS NULL
            OR stock_symbol = normalize_instrument_symbol(stock_symbol)
        );

DROP TRIGGER IF EXISTS trg_00_normalize_stock_price_symbol ON stock_prices;
CREATE TRIGGER trg_00_normalize_stock_price_symbol
BEFORE INSERT OR UPDATE OF stock_symbol ON stock_prices
FOR EACH ROW
EXECUTE FUNCTION normalize_instrument_symbol_trigger();

DROP TRIGGER IF EXISTS trg_00_normalize_stock_price_history_symbol ON stock_price_history;
CREATE TRIGGER trg_00_normalize_stock_price_history_symbol
BEFORE INSERT OR UPDATE OF stock_symbol ON stock_price_history
FOR EACH ROW
EXECUTE FUNCTION normalize_instrument_symbol_trigger();

DROP TRIGGER IF EXISTS trg_00_normalize_reward_symbol ON rewards;
CREATE TRIGGER trg_00_normalize_reward_symbol
BEFORE INSERT OR UPDATE OF stock_symbol ON rewards
FOR EACH ROW
EXECUTE FUNCTION normalize_instrument_symbol_trigger();

DROP TRIGGER IF EXISTS trg_00_normalize_ledger_symbol ON ledger;
CREATE TRIGGER trg_00_normalize_ledger_symbol
BEFORE INSERT OR UPDATE OF stock_symbol ON ledger
FOR EACH ROW
EXECUTE FUNCTION normalize_instrument_symbol_trigger();

-- stock_events already has trg_00_normalize_stock_event. Recreate its function
-- so event normalization shares the canonical helper, including new_symbol.
CREATE OR REPLACE FUNCTION normalize_stock_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.stock_symbol := normalize_instrument_symbol(NEW.stock_symbol);

    IF NEW.new_symbol IS NOT NULL THEN
        NEW.new_symbol := normalize_instrument_symbol(NEW.new_symbol);
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
-- Normalized uniqueness
-- ---------------------------------------------------------------------------

-- Replaces the older UPPER-only reward index. The exact unique constraint is
-- kept because existing insertion paths and external clients may rely on it.
DROP INDEX IF EXISTS unique_user_stock_date_upper;

CREATE UNIQUE INDEX unique_user_stock_date_normalized
    ON rewards (
        user_id,
        normalize_instrument_symbol(stock_symbol),
        reward_date
    );

-- Exact unique constraints remain in place for ON CONFLICT inference. These
-- functional indexes are the cross-case invariant.
CREATE UNIQUE INDEX unique_stock_price_symbol_normalized
    ON stock_prices (normalize_instrument_symbol(stock_symbol));

CREATE UNIQUE INDEX unique_stock_price_history_symbol_date_normalized
    ON stock_price_history (
        normalize_instrument_symbol(stock_symbol),
        date
    );

COMMENT ON INDEX unique_user_stock_date_normalized IS
    'Case/whitespace-insensitive reward uniqueness: one reward per user, normalized symbol, and market date.';
COMMENT ON INDEX unique_stock_price_symbol_normalized IS
    'One current price row per normalized instrument symbol.';
COMMENT ON INDEX unique_stock_price_history_symbol_date_normalized IS
    'One daily history row per normalized instrument symbol and market date.';

COMMENT ON COLUMN stock_prices.stock_symbol IS
    'Canonical uppercase, trimmed instrument symbol; kept canonical by trg_00_normalize_stock_price_symbol.';
COMMENT ON COLUMN stock_price_history.stock_symbol IS
    'Canonical uppercase, trimmed instrument symbol; kept canonical by trg_00_normalize_stock_price_history_symbol.';
COMMENT ON COLUMN rewards.stock_symbol IS
    'Canonical uppercase, trimmed instrument symbol; kept canonical by trg_00_normalize_reward_symbol.';
COMMENT ON COLUMN ledger.stock_symbol IS
    'Canonical uppercase, trimmed instrument symbol for stock postings; kept canonical by trg_00_normalize_ledger_symbol.';
COMMENT ON COLUMN stock_events.stock_symbol IS
    'Canonical uppercase, trimmed instrument symbol; kept canonical by trg_00_normalize_stock_event.';
COMMENT ON COLUMN stock_events.new_symbol IS
    'Canonical uppercase, trimmed merger successor symbol; kept canonical by trg_00_normalize_stock_event.';

-- ---------------------------------------------------------------------------
-- Exact canonical joins and lookups
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION stock_action_factor(p_symbol TEXT, p_as_of DATE)
RETURNS NUMERIC
LANGUAGE SQL
STABLE
AS $$
    SELECT COALESCE(EXP(SUM(LN(se.ratio_num / se.ratio_den))), 1::numeric)
    FROM stock_events se
    WHERE se.stock_symbol = normalize_instrument_symbol(p_symbol)
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
    WHERE se.stock_symbol = normalize_instrument_symbol(p_symbol)
      AND se.event_type IN ('split', 'bonus', 'merge')
      AND se.effective_date <= p_as_of
      AND se.ratio_num > 0
      AND se.ratio_den > 0;
$$;

CREATE OR REPLACE VIEW instrument_lineage AS
SELECT
    se.id AS source_event_id,
    se.stock_symbol::TEXT AS predecessor_symbol,
    se.new_symbol::TEXT AS successor_symbol,
    se.ratio_num,
    se.ratio_den,
    se.effective_date
FROM stock_events se
WHERE se.event_type = 'merge'
  AND se.new_symbol IS NOT NULL
  AND se.new_symbol <> ''
  AND se.ratio_num > 0
  AND se.ratio_den > 0
  AND se.new_symbol <> se.stock_symbol;

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
            normalize_instrument_symbol(p_symbol)::TEXT AS symbol,
            p_from AS entry_date,
            1::NUMERIC AS merge_ratio,
            ARRAY[normalize_instrument_symbol(p_symbol)::TEXT] AS visited
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
        normalize_instrument_symbol(p_symbol)
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
        se.stock_symbol::TEXT,
        se.effective_date,
        se.delist_outcome,
        se.cash_per_unit
    FROM stock_events se
    WHERE se.event_type = 'delist'
      AND se.stock_symbol = instrument_terminal_symbol(p_symbol, p_from, p_as_of)
      AND se.effective_date <= p_as_of
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
    v_symbol := r.stock_symbol::TEXT;

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
                se.stock_symbol::text AS symbol,
                (se.ratio_num / se.ratio_den)::NUMERIC AS ratio,
                se.fraction_policy::text AS fraction_policy,
                se.fraction_cash_per_unit,
                NULL::NUMERIC AS cash_per_unit,
                NULL::text AS successor_symbol,
                NULL::NUMERIC AS delta_quantity
            FROM chain c
            JOIN stock_events se
              ON se.stock_symbol = c.symbol
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
            r.reward_date,
            p_as_of
        ) d
        WHERE d.outcome = 'cash_exit'
          AND d.cash_per_unit > 0
    )
    SELECT * FROM event_cash WHERE cash_amount > 0
    UNION ALL
    SELECT * FROM delist_cash WHERE cash_amount > 0;
$$;

CREATE OR REPLACE FUNCTION process_corporate_action_cash(
    p_as_of DATE DEFAULT market_today()
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
    PERFORM process_corporate_action_cash(market_today());
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION backfill_price_history(p_date DATE DEFAULT market_today())
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
            r.stock_symbol AS original_symbol,
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

COMMENT ON FUNCTION reward_corporate_action_outcome(INTEGER, DATE) IS
    'Successor-aware lifecycle and corporate-action outcome for one reward as of a market date. The market_today wrapper is reward_corporate_action_outcomes.';

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
            WHERE h.stock_symbol = v.valued_symbol
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

COMMENT ON FUNCTION historical_rewards_as_of(DATE) IS
    'Historical reward valuation in market dates. NULL as_of = grant-date stock value with current lifecycle annotations; non-NULL = point-in-time quantity, price, delists, and adjustments as of that market date.';

CREATE OR REPLACE VIEW historical_rewards AS
SELECT * FROM historical_rewards_as_of(NULL::DATE);

COMMENT ON VIEW historical_rewards IS
    'Grant-date historical valuation with current lifecycle annotations. A later delist never removes a row or changes its grant-date value.';

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
    spa.updated_at AS price_as_of
FROM reward_corporate_action_outcomes o
LEFT JOIN stock_prices_adjusted spa
    ON spa.stock_symbol = o.terminal_symbol
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
            WHERE l.stock_symbol IS DISTINCT FROM r.stock_symbol
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

-- Keep the current market day populated after the price/history rewrite.
SELECT backfill_price_history(market_today());
