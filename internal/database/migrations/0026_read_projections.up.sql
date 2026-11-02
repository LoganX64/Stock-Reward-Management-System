-- Read-path projections, phase 1 (additive).
--
-- This migration adds the maintained projection tables and the write-path
-- refresh machinery. It deliberately does not replace any compatibility view:
-- the API keeps reading the current computational views until the operator has
-- populated the projections and passed the parity check in 0027.
--
-- Rules:
--   * base tables remain authoritative; every projection row can be rebuilt;
--   * reward and adjustment writes refresh projections in the same transaction
--     through AFTER row triggers, so direct SQL and handlers behave alike;
--   * stock-event writes refresh factors and affected users synchronously;
--   * `stocky.skip_projection_refresh = on` suppresses projection work during
--     bulk loads; an explicit rebuild is required afterwards.

-- ---------------------------------------------------------------------------
-- Projection tables
-- ---------------------------------------------------------------------------

CREATE TABLE reward_state (
    reward_id               INTEGER PRIMARY KEY REFERENCES rewards(id) ON DELETE CASCADE,
    user_id                 INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    original_symbol         TEXT NOT NULL,
    terminal_symbol         TEXT NOT NULL,
    reward_date             DATE NOT NULL,
    adjusted_quantity       NUMERIC(18,6) NOT NULL,
    total_adjustment_amount NUMERIC(18,4) NOT NULL DEFAULT 0,
    outcome_type            TEXT NOT NULL DEFAULT 'active',
    is_active               BOOLEAN NOT NULL,
    delist_event_id         INTEGER,
    delist_effective_date   DATE,
    cash_proceeds           NUMERIC(18,4) NOT NULL DEFAULT 0,
    written_off_quantity    NUMERIC(18,6) NOT NULL DEFAULT 0,
    cash_in_lieu_proceeds   NUMERIC(18,4) NOT NULL DEFAULT 0,
    fraction_quantity       NUMERIC(18,6) NOT NULL DEFAULT 0,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_reward_state_user_date
    ON reward_state (user_id, reward_date DESC, reward_id DESC);

CREATE INDEX idx_reward_state_user_active_symbol
    ON reward_state (user_id, terminal_symbol)
    WHERE is_active;

CREATE INDEX idx_reward_state_terminal_symbol
    ON reward_state (terminal_symbol);

CREATE INDEX idx_reward_state_original_symbol
    ON reward_state (original_symbol);

CREATE TABLE portfolio_position (
    user_id                 INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    terminal_symbol         TEXT NOT NULL,
    quantity                NUMERIC(18,6) NOT NULL,
    total_adjustment_amount NUMERIC(18,4) NOT NULL DEFAULT 0,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, terminal_symbol)
);

CREATE TABLE user_cash_position (
    user_id                INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    cash_proceeds          NUMERIC(18,4) NOT NULL DEFAULT 0,
    written_off_quantity   NUMERIC(18,6) NOT NULL DEFAULT 0,
    cash_in_lieu_proceeds  NUMERIC(18,4) NOT NULL DEFAULT 0,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The factor is deliberately unconstrained NUMERIC. Rounding this cache would
-- change adjusted prices and break exact parity with stock_action_factor().
CREATE TABLE instrument_factor (
    stock_symbol  TEXT PRIMARY KEY,
    factor        NUMERIC NOT NULL DEFAULT 1,
    as_of         DATE NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE projection_meta (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE projection_refresh_queue (
    id           BIGSERIAL PRIMARY KEY,
    user_id      INTEGER NOT NULL,
    reason       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ
);

COMMENT ON TABLE reward_state IS
    'Current corporate-action lifecycle result per reward; disposable and rebuildable from base tables.';
COMMENT ON TABLE portfolio_position IS
    'Per-user, per-terminal-symbol aggregate of active reward_state rows.';
COMMENT ON TABLE user_cash_position IS
    'Per-user aggregate of corporate-action cash outcomes; one row per user with rewards.';
COMMENT ON TABLE instrument_factor IS
    'Cached current split/bonus factor per canonical instrument symbol.';
COMMENT ON TABLE projection_meta IS
    'Operational metadata for projection rebuilds and reconciliation.';
COMMENT ON TABLE projection_refresh_queue IS
    'Reserved for Phase 2 asynchronous corporate-action fan-out.';

COMMENT ON COLUMN instrument_factor.factor IS
    'Divisor used by stock_prices_adjusted: adjusted_price = raw_price / factor.';
COMMENT ON COLUMN instrument_factor.as_of IS
    'Market date the cached factor was computed for; stock_prices_adjusted falls back when this is not market_today().';

-- ---------------------------------------------------------------------------
-- Private per-reward base view and point function
-- ---------------------------------------------------------------------------

-- Exact pre-projection definition. Batch backfill, cmd/rebuild-projections,
-- and reconciliation may read this view. The per-reward refresh path must use
-- reward_corporate_action_outcome_base() instead: the CTEs below are
-- materialized and would scan every reward if filtered after the fact.
CREATE VIEW reward_corporate_action_outcomes_base AS
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

COMMENT ON VIEW reward_corporate_action_outcomes_base IS
    'Private, exact pre-projection lifecycle view for batch backfill and parity. Never read by the online refresh path.';

-- Point function with the same column set as the base view. It evaluates the
-- lifecycle for one reward only. The current public view contract sums all
-- adjustments; this function follows that all-adjustments rule.
CREATE OR REPLACE FUNCTION reward_corporate_action_outcome_base(p_reward_id INTEGER)
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
            r.id AS reward_id,
            r.user_id,
            r.stock_symbol AS original_symbol,
            r.reward_date
        FROM rewards r
        WHERE r.id = p_reward_id
    ),
    balances AS (
        SELECT
            rr.reward_id,
            rr.user_id,
            rr.original_symbol,
            rr.reward_date,
            s.terminal_symbol,
            s.settled_quantity AS current_adjusted_quantity,
            s.fraction_quantity,
            s.cash_in_lieu,
            s.merger_cash
        FROM reward_row rr
        CROSS JOIN LATERAL reward_action_settlement(rr.reward_id, market_today()) s
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
            FROM instrument_delist_event(b.original_symbol::TEXT, b.reward_date, market_today())
        ) d ON TRUE
    ),
    adjustment_totals AS (
        SELECT COALESCE(SUM(a.delta_amount), 0) AS total_adjustment_amount
        FROM adjustments a
        JOIN reward_row rr ON rr.reward_id = a.reward_id
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
        adj.total_adjustment_amount,
        (d.delist_event_id IS NULL AND b.current_adjusted_quantity > 0) AS is_active,
        ROUND(b.cash_in_lieu, 4) AS cash_in_lieu_proceeds,
        ROUND(b.fraction_quantity, 6) AS fraction_quantity
    FROM balances b
    LEFT JOIN delist d ON d.reward_id = b.reward_id
    CROSS JOIN adjustment_totals adj;
$$;

COMMENT ON FUNCTION reward_corporate_action_outcome_base(INTEGER) IS
    'Point lifecycle outcome for one reward using the current-view adjustment rule. Used by refresh_reward_state.';

-- ---------------------------------------------------------------------------
-- Refresh helpers
-- ---------------------------------------------------------------------------

-- The benchmark seed and bulk rebuilds can suppress projection work with:
--   SET LOCAL stocky.skip_projection_refresh = 'on';
CREATE OR REPLACE FUNCTION projection_refresh_disabled()
RETURNS BOOLEAN
LANGUAGE SQL
STABLE
AS $$
    SELECT COALESCE(
        current_setting('stocky.skip_projection_refresh', true),
        ''
    ) IN ('on', 'true', '1');
$$;

-- Symbols whose rewards are affected by a stock event. The recursive term
-- walks instrument_lineage backwards, so an event on an intermediate merger
-- symbol (A -> B -> C, event on B) also matches rewards that started at A.
CREATE OR REPLACE FUNCTION affected_reward_ids_for_symbols(p_symbols TEXT[])
RETURNS TABLE (reward_id INTEGER)
LANGUAGE SQL
STABLE
AS $$
    WITH RECURSIVE affected_symbols(symbol) AS (
        SELECT DISTINCT normalize_instrument_symbol(x)
        FROM unnest(p_symbols) AS x
        WHERE normalize_instrument_symbol(x) <> ''
        UNION
        SELECT il.predecessor_symbol
        FROM instrument_lineage il
        JOIN affected_symbols a ON il.successor_symbol = a.symbol
    )
    SELECT DISTINCT rs.reward_id
    FROM reward_state rs
    WHERE rs.original_symbol IN (SELECT symbol FROM affected_symbols)
       OR rs.terminal_symbol IN (SELECT symbol FROM affected_symbols);
$$;

COMMENT ON FUNCTION affected_reward_ids_for_symbols(TEXT[]) IS
    'Resolves event symbols (including merger-chain predecessors) to reward_state reward ids.';

-- Refresh one row from the point function. No row means the reward is not yet
-- effective (future reward_date) or no longer exists, so the projection row is
-- removed. This function never reads the all-rewards base view.
CREATE OR REPLACE FUNCTION refresh_reward_state(p_reward_id INTEGER)
RETURNS VOID
LANGUAGE plpgsql
AS $$
BEGIN
    INSERT INTO reward_state (
        reward_id,
        user_id,
        original_symbol,
        terminal_symbol,
        reward_date,
        adjusted_quantity,
        total_adjustment_amount,
        outcome_type,
        is_active,
        delist_event_id,
        delist_effective_date,
        cash_proceeds,
        written_off_quantity,
        cash_in_lieu_proceeds,
        fraction_quantity
    )
    SELECT
        o.reward_id,
        o.user_id,
        o.original_symbol::TEXT,
        o.terminal_symbol::TEXT,
        o.reward_date,
        o.adjusted_quantity::NUMERIC(18,6),
        o.total_adjustment_amount::NUMERIC(18,4),
        o.outcome_type::TEXT,
        o.is_active,
        o.delist_event_id,
        o.delist_effective_date,
        o.cash_proceeds::NUMERIC(18,4),
        o.written_off_quantity::NUMERIC(18,6),
        o.cash_in_lieu_proceeds::NUMERIC(18,4),
        o.fraction_quantity::NUMERIC(18,6)
    FROM reward_corporate_action_outcome_base(p_reward_id) o
    ON CONFLICT (reward_id) DO UPDATE
    SET user_id = EXCLUDED.user_id,
        original_symbol = EXCLUDED.original_symbol,
        terminal_symbol = EXCLUDED.terminal_symbol,
        reward_date = EXCLUDED.reward_date,
        adjusted_quantity = EXCLUDED.adjusted_quantity,
        total_adjustment_amount = EXCLUDED.total_adjustment_amount,
        outcome_type = EXCLUDED.outcome_type,
        is_active = EXCLUDED.is_active,
        delist_event_id = EXCLUDED.delist_event_id,
        delist_effective_date = EXCLUDED.delist_effective_date,
        cash_proceeds = EXCLUDED.cash_proceeds,
        written_off_quantity = EXCLUDED.written_off_quantity,
        cash_in_lieu_proceeds = EXCLUDED.cash_in_lieu_proceeds,
        fraction_quantity = EXCLUDED.fraction_quantity,
        updated_at = NOW();

    IF NOT FOUND THEN
        DELETE FROM reward_state WHERE reward_id = p_reward_id;
    END IF;
END;
$$;

-- Rebuild one user's aggregates. Cash is recomputed as well as the portfolio;
-- a user with no reward_state rows gets no cash row, while a user with rewards
-- but no cash-producing outcomes gets the zero row the old cash view exposed.
CREATE OR REPLACE FUNCTION refresh_user_projections(p_user_id INTEGER)
RETURNS VOID
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(p_user_id::BIGINT);

    DELETE FROM portfolio_position WHERE user_id = p_user_id;

    INSERT INTO portfolio_position (
        user_id,
        terminal_symbol,
        quantity,
        total_adjustment_amount,
        updated_at
    )
    SELECT
        user_id,
        terminal_symbol,
        SUM(adjusted_quantity),
        SUM(total_adjustment_amount),
        NOW()
    FROM reward_state
    WHERE user_id = p_user_id
      AND is_active
    GROUP BY user_id, terminal_symbol;

    DELETE FROM user_cash_position WHERE user_id = p_user_id;

    INSERT INTO user_cash_position (
        user_id,
        cash_proceeds,
        written_off_quantity,
        cash_in_lieu_proceeds,
        updated_at
    )
    SELECT
        user_id,
        SUM(cash_proceeds),
        SUM(written_off_quantity),
        SUM(cash_in_lieu_proceeds),
        NOW()
    FROM reward_state
    WHERE user_id = p_user_id
    GROUP BY user_id;
END;
$$;

-- ---------------------------------------------------------------------------
-- Instrument factor cache
-- ---------------------------------------------------------------------------

-- Fill cache rows that do not exist yet. Used by online rebuilds and by the
-- full refresh below.
CREATE OR REPLACE FUNCTION backfill_instrument_factors(p_as_of DATE DEFAULT market_today())
RETURNS INTEGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_count INTEGER := 0;
BEGIN
    INSERT INTO instrument_factor (stock_symbol, factor, as_of, updated_at)
    SELECT
        s.stock_symbol,
        stock_action_factor(s.stock_symbol, p_as_of),
        p_as_of,
        NOW()
    FROM (
        SELECT DISTINCT normalize_instrument_symbol(x.symbol) AS stock_symbol
        FROM (
            SELECT stock_symbol::TEXT AS symbol FROM stock_events
            UNION
            SELECT new_symbol::TEXT FROM stock_events WHERE new_symbol IS NOT NULL
            UNION
            SELECT stock_symbol::TEXT FROM stock_prices
            UNION
            SELECT stock_symbol::TEXT FROM rewards
        ) x
        WHERE normalize_instrument_symbol(x.symbol) <> ''
    ) s
    ON CONFLICT (stock_symbol) DO NOTHING;

    GET DIAGNOSTICS v_count = ROW_COUNT;
    RETURN v_count;
END;
$$;

-- Recompute the affected symbols after a stock-event change. Symbols are
-- already canonical, but normalizing again keeps direct SQL callers safe.
CREATE OR REPLACE FUNCTION refresh_instrument_factors_for_symbols(
    p_symbols TEXT[],
    p_as_of DATE DEFAULT market_today()
)
RETURNS INTEGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_count INTEGER := 0;
BEGIN
    INSERT INTO instrument_factor (stock_symbol, factor, as_of, updated_at)
    SELECT
        s.stock_symbol,
        stock_action_factor(s.stock_symbol, p_as_of),
        p_as_of,
        NOW()
    FROM (
        SELECT DISTINCT normalize_instrument_symbol(x) AS stock_symbol
        FROM unnest(p_symbols) AS x
        WHERE normalize_instrument_symbol(x) <> ''
    ) s
    ON CONFLICT (stock_symbol) DO UPDATE
    SET factor = EXCLUDED.factor,
        as_of = EXCLUDED.as_of,
        updated_at = NOW();

    GET DIAGNOSTICS v_count = ROW_COUNT;
    RETURN v_count;
END;
$$;

-- Daily rollover refresh: insert missing symbols, then recompute every row and
-- re-stamp as_of so stock_prices_adjusted does not fall back after midnight.
CREATE OR REPLACE FUNCTION refresh_instrument_factors(p_as_of DATE DEFAULT market_today())
RETURNS INTEGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_count INTEGER := 0;
BEGIN
    PERFORM backfill_instrument_factors(p_as_of);

    UPDATE instrument_factor
    SET factor = stock_action_factor(stock_symbol, p_as_of),
        as_of = p_as_of,
        updated_at = NOW();

    GET DIAGNOSTICS v_count = ROW_COUNT;
    RETURN v_count;
END;
$$;

-- ---------------------------------------------------------------------------
-- Date-sensitive corporate-action refresh
-- ---------------------------------------------------------------------------

-- Future-dated events can change is_active, terminal symbol, delist outcome,
-- and cash proceeds without a row write. The last processed market date is
-- tracked in projection_meta so a missed run is caught up instead of skipped.
-- On the first run, events effective yesterday and today are processed; the
-- backfill is responsible for the older historical state.
--
-- A reward whose reward_date becomes effective, or whose future-dated
-- adjustment becomes effective, also changes without a row write:
-- reward_action_settlement() filters adjustments by effective_date, while the
-- compatibility contract sums all adjustments into total_adjustment_amount.
CREATE OR REPLACE FUNCTION refresh_effective_corporate_actions(p_as_of DATE DEFAULT market_today())
RETURNS INTEGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_from    DATE;
    v_symbols TEXT[];
    v_rewards INTEGER[];
    v_users   INTEGER[];
    v_user    INTEGER;
    v_reward  INTEGER;
    v_count   INTEGER := 0;
BEGIN
    SELECT value::DATE
    INTO v_from
    FROM projection_meta
    WHERE key = 'last_effective_corporate_actions_date';

    IF v_from IS NULL THEN
        v_from := p_as_of - 1;
    END IF;

    IF p_as_of <= v_from THEN
        RETURN 0;
    END IF;

    SELECT ARRAY(
        SELECT DISTINCT s
        FROM (
            SELECT se.stock_symbol::TEXT AS s
            FROM stock_events se
            WHERE se.effective_date > v_from
              AND se.effective_date <= p_as_of
            UNION
            SELECT se.new_symbol::TEXT
            FROM stock_events se
            WHERE se.effective_date > v_from
              AND se.effective_date <= p_as_of
              AND se.new_symbol IS NOT NULL
        ) symbols
        WHERE s IS NOT NULL
          AND s <> ''
        ORDER BY s
    ) INTO v_symbols;

    SELECT ARRAY(
        SELECT DISTINCT reward_id
        FROM (
            SELECT reward_id
            FROM affected_reward_ids_for_symbols(
                COALESCE(v_symbols, ARRAY[]::TEXT[])
            )
            UNION
            SELECT r.id
            FROM rewards r
            WHERE r.reward_date > v_from
              AND r.reward_date <= p_as_of
            UNION
            SELECT a.reward_id
            FROM adjustments a
            WHERE a.effective_date > v_from
              AND a.effective_date <= p_as_of
        ) affected
        ORDER BY reward_id
    ) INTO v_rewards;

    IF v_rewards IS NOT NULL AND cardinality(v_rewards) > 0 THEN
        SELECT ARRAY(
            SELECT DISTINCT r.user_id
            FROM rewards r
            WHERE r.id = ANY(v_rewards)
            ORDER BY r.user_id
        ) INTO v_users;

        FOREACH v_reward IN ARRAY v_rewards LOOP
            PERFORM refresh_reward_state(v_reward);
            v_count := v_count + 1;
        END LOOP;

        IF v_users IS NOT NULL THEN
            FOREACH v_user IN ARRAY v_users LOOP
                PERFORM refresh_user_projections(v_user);
            END LOOP;
        END IF;
    END IF;

    INSERT INTO projection_meta (key, value, updated_at)
    VALUES ('last_effective_corporate_actions_date', p_as_of::TEXT, NOW())
    ON CONFLICT (key) DO UPDATE
    SET value = EXCLUDED.value,
        updated_at = NOW();

    RETURN v_count;
END;
$$;

-- ---------------------------------------------------------------------------
-- Write-path triggers
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION refresh_reward_projection()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_old_user_id INTEGER;
    v_new_user_id INTEGER;
BEGIN
    IF projection_refresh_disabled() THEN
        RETURN NULL;
    END IF;

    IF TG_OP = 'DELETE' THEN
        v_old_user_id := OLD.user_id;
        -- Remove the reward before recomputing the user. Do not rely on FK
        -- cascade ordering relative to this trigger.
        DELETE FROM reward_state WHERE reward_id = OLD.id;
        PERFORM refresh_user_projections(v_old_user_id);
        RETURN NULL;
    END IF;

    -- The projection must move to the new owner before either user's aggregate
    -- is recomputed; otherwise the old user keeps a stale position.
    PERFORM refresh_reward_state(NEW.id);

    IF TG_OP = 'UPDATE' AND OLD.user_id IS DISTINCT FROM NEW.user_id THEN
        v_old_user_id := OLD.user_id;
        v_new_user_id := NEW.user_id;

        IF v_old_user_id < v_new_user_id THEN
            PERFORM refresh_user_projections(v_old_user_id);
            PERFORM refresh_user_projections(v_new_user_id);
        ELSE
            PERFORM refresh_user_projections(v_new_user_id);
            PERFORM refresh_user_projections(v_old_user_id);
        END IF;
    ELSE
        PERFORM refresh_user_projections(NEW.user_id);
    END IF;

    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS trg_reward_projection ON rewards;

-- created_at is included because trg_set_reward_date derives reward_date from
-- it; changing created_at changes reward_date even when reward_date is not in
-- the UPDATE SET list.
CREATE TRIGGER trg_reward_projection
AFTER INSERT OR DELETE
   OR UPDATE OF user_id, stock_symbol, original_quantity, reward_date, created_at
ON rewards
FOR EACH ROW
EXECUTE FUNCTION refresh_reward_projection();

COMMENT ON TRIGGER trg_reward_projection ON rewards IS
    'Refreshes reward_state and user projections in the same transaction as the reward write.';

CREATE OR REPLACE FUNCTION refresh_adjustment_projection()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_old_reward_id INTEGER;
    v_new_reward_id INTEGER;
    v_old_user_id   INTEGER;
    v_new_user_id   INTEGER;
BEGIN
    IF projection_refresh_disabled() THEN
        RETURN NULL;
    END IF;

    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        v_old_reward_id := OLD.reward_id;
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        v_new_reward_id := NEW.reward_id;
    END IF;

    IF v_old_reward_id IS NOT DISTINCT FROM v_new_reward_id THEN
        SELECT user_id INTO v_new_user_id
        FROM rewards
        WHERE id = v_new_reward_id;

        IF v_new_user_id IS NOT NULL THEN
            PERFORM refresh_reward_state(v_new_reward_id);
            PERFORM refresh_user_projections(v_new_user_id);
        END IF;
        RETURN NULL;
    END IF;

    -- The adjustment moved between rewards. Refresh both rewards and both
    -- users in ascending user order so concurrent triggers cannot deadlock.
    SELECT user_id INTO v_old_user_id FROM rewards WHERE id = v_old_reward_id;
    SELECT user_id INTO v_new_user_id FROM rewards WHERE id = v_new_reward_id;

    IF v_old_user_id IS NOT NULL THEN
        PERFORM refresh_reward_state(v_old_reward_id);
    END IF;
    IF v_new_user_id IS NOT NULL THEN
        PERFORM refresh_reward_state(v_new_reward_id);
    END IF;

    IF v_old_user_id IS NOT NULL
       AND v_new_user_id IS NOT NULL
       AND v_old_user_id <> v_new_user_id THEN
        IF v_old_user_id < v_new_user_id THEN
            PERFORM refresh_user_projections(v_old_user_id);
            PERFORM refresh_user_projections(v_new_user_id);
        ELSE
            PERFORM refresh_user_projections(v_new_user_id);
            PERFORM refresh_user_projections(v_old_user_id);
        END IF;
    ELSIF v_old_user_id IS NOT NULL THEN
        PERFORM refresh_user_projections(v_old_user_id);
    ELSIF v_new_user_id IS NOT NULL THEN
        PERFORM refresh_user_projections(v_new_user_id);
    END IF;

    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS trg_adjustment_projection ON adjustments;

CREATE TRIGGER trg_adjustment_projection
AFTER INSERT OR UPDATE OR DELETE ON adjustments
FOR EACH ROW
EXECUTE FUNCTION refresh_adjustment_projection();

COMMENT ON TRIGGER trg_adjustment_projection ON adjustments IS
    'Refreshes the affected reward and user projections in the same transaction as the adjustment write.';

-- Replace the existing cash trigger with one that also refreshes projections.
-- PostgreSQL does not allow REFERENCING on a multi-event trigger, so the same
-- function is attached to three event-specific statement triggers.
CREATE OR REPLACE FUNCTION sync_corporate_action_cash_trigger()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_symbols        TEXT[];
    v_factor_symbols TEXT[];
    v_users          INTEGER[];
    v_user           INTEGER;
    v_reward         INTEGER;
BEGIN
    PERFORM process_corporate_action_cash(market_today());

    IF projection_refresh_disabled() THEN
        RETURN NULL;
    END IF;

    -- All symbols touched by the event set, for reward_state refresh. A symbol
    -- change must refresh both the old and the new side.
    IF TG_OP = 'INSERT' THEN
        SELECT ARRAY(
            SELECT DISTINCT s
            FROM (
                SELECT stock_symbol::TEXT AS s FROM new_events
                UNION
                SELECT new_symbol::TEXT FROM new_events WHERE new_symbol IS NOT NULL
            ) symbols
            WHERE s IS NOT NULL AND s <> ''
            ORDER BY s
        ) INTO v_symbols;
    ELSIF TG_OP = 'UPDATE' THEN
        SELECT ARRAY(
            SELECT DISTINCT s
            FROM (
                SELECT stock_symbol::TEXT AS s FROM old_events
                UNION
                SELECT new_symbol::TEXT FROM old_events WHERE new_symbol IS NOT NULL
                UNION
                SELECT stock_symbol::TEXT AS s FROM new_events
                UNION
                SELECT new_symbol::TEXT FROM new_events WHERE new_symbol IS NOT NULL
            ) symbols
            WHERE s IS NOT NULL AND s <> ''
            ORDER BY s
        ) INTO v_symbols;
    ELSE
        SELECT ARRAY(
            SELECT DISTINCT s
            FROM (
                SELECT stock_symbol::TEXT AS s FROM old_events
                UNION
                SELECT new_symbol::TEXT FROM old_events WHERE new_symbol IS NOT NULL
            ) symbols
            WHERE s IS NOT NULL AND s <> ''
            ORDER BY s
        ) INTO v_symbols;
    END IF;

    -- Only split/bonus events change stock_action_factor(). Merges, delists,
    -- and cash events are handled by reward_state refresh below.
    IF TG_OP = 'INSERT' THEN
        IF EXISTS (
            SELECT 1 FROM new_events WHERE event_type IN ('split', 'bonus')
        ) THEN
            v_factor_symbols := v_symbols;
        END IF;
    ELSIF TG_OP = 'UPDATE' THEN
        IF EXISTS (
            SELECT 1 FROM old_events WHERE event_type IN ('split', 'bonus')
        ) OR EXISTS (
            SELECT 1 FROM new_events WHERE event_type IN ('split', 'bonus')
        ) THEN
            v_factor_symbols := v_symbols;
        END IF;
    ELSE
        IF EXISTS (
            SELECT 1 FROM old_events WHERE event_type IN ('split', 'bonus')
        ) THEN
            v_factor_symbols := v_symbols;
        END IF;
    END IF;

    IF v_factor_symbols IS NOT NULL AND cardinality(v_factor_symbols) > 0 THEN
        PERFORM refresh_instrument_factors_for_symbols(
            v_factor_symbols,
            market_today()
        );
    END IF;

    IF v_symbols IS NULL OR cardinality(v_symbols) = 0 THEN
        RETURN NULL;
    END IF;

    SELECT ARRAY(
        SELECT DISTINCT rs.user_id
        FROM reward_state rs
        WHERE rs.reward_id IN (
            SELECT reward_id FROM affected_reward_ids_for_symbols(v_symbols)
        )
        ORDER BY rs.user_id
    ) INTO v_users;

    FOR v_reward IN
        SELECT reward_id FROM affected_reward_ids_for_symbols(v_symbols)
    LOOP
        PERFORM refresh_reward_state(v_reward);
    END LOOP;

    IF v_users IS NOT NULL THEN
        FOREACH v_user IN ARRAY v_users LOOP
            PERFORM refresh_user_projections(v_user);
        END LOOP;
    END IF;

    RETURN NULL;
END;
$$;

COMMENT ON FUNCTION sync_corporate_action_cash_trigger() IS
    'Statement trigger for stock_events: posts corporate-action cash, refreshes instrument factors, reward_state, and affected user projections.';

DROP TRIGGER IF EXISTS trg_process_corporate_action_cash ON stock_events;
DROP TRIGGER IF EXISTS trg_process_corporate_action_cash_insert ON stock_events;
DROP TRIGGER IF EXISTS trg_process_corporate_action_cash_update ON stock_events;
DROP TRIGGER IF EXISTS trg_process_corporate_action_cash_delete ON stock_events;

CREATE TRIGGER trg_process_corporate_action_cash_insert
AFTER INSERT ON stock_events
REFERENCING NEW TABLE AS new_events
FOR EACH STATEMENT
EXECUTE FUNCTION sync_corporate_action_cash_trigger();

CREATE TRIGGER trg_process_corporate_action_cash_update
AFTER UPDATE ON stock_events
REFERENCING OLD TABLE AS old_events NEW TABLE AS new_events
FOR EACH STATEMENT
EXECUTE FUNCTION sync_corporate_action_cash_trigger();

CREATE TRIGGER trg_process_corporate_action_cash_delete
AFTER DELETE ON stock_events
REFERENCING OLD TABLE AS old_events
FOR EACH STATEMENT
EXECUTE FUNCTION sync_corporate_action_cash_trigger();

-- ---------------------------------------------------------------------------
-- Initial factor backfill and metadata
-- ---------------------------------------------------------------------------

-- Only the factor cache is backfilled here; it is bounded by the number of
-- instruments, not rewards. reward_state and the aggregates are populated by
-- cmd/rebuild-projections so the startup migration never holds a 500k-row lock.
SELECT backfill_instrument_factors(market_today());

INSERT INTO projection_meta (key, value)
VALUES ('schema_version', '1')
ON CONFLICT (key) DO NOTHING;
