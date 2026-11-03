-- Read-path projections, phase 2 (write-path hardening).
--
-- This migration is additive. It hardens the Phase 1 write path:
--   * the stock-event trigger refreshes small fan-outs synchronously and
--     enqueues large fan-outs instead of doing an unbounded synchronous loop;
--   * the queue has retry/backoff/dead-letter state and is drained one user per
--     transaction by the API worker;
--   * users touched by corporate actions are tracked for reconciliation;
--   * rollover work is guarded by per-market-date due checks and advisory locks;
--   * reconciliation compares a sampled/focus user set with the authoritative
--     lifecycle view and reports drift.
--
-- Queueing is opt-in after a worker is running. `queue_sync_user_limit` is
-- seeded high so applying this migration alone leaves Phase 1 behavior intact;
-- the worker lowers it to the configured value on its first successful tick.

-- ---------------------------------------------------------------------------
-- Queue hardening
-- ---------------------------------------------------------------------------

ALTER TABLE projection_refresh_queue
    ADD COLUMN IF NOT EXISTS attempts    INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_error  TEXT,
    ADD COLUMN IF NOT EXISTS available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS failed_at   TIMESTAMPTZ;

-- The queue was unused in Phase 1, but clean up defensively before adding the
-- foreign key and the partial unique index.
DELETE FROM projection_refresh_queue q
WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = q.user_id);

DELETE FROM projection_refresh_queue q
USING projection_refresh_queue older
WHERE q.processed_at IS NULL
  AND q.failed_at IS NULL
  AND older.processed_at IS NULL
  AND older.failed_at IS NULL
  AND q.user_id = older.user_id
  AND q.id > older.id;

ALTER TABLE projection_refresh_queue
    ADD CONSTRAINT projection_refresh_queue_user_fk
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;

-- One pending refresh per user. A dead-lettered row (failed_at set) no longer
-- blocks a fresh enqueue.
CREATE UNIQUE INDEX uq_projection_refresh_queue_pending_user
    ON projection_refresh_queue (user_id)
    WHERE processed_at IS NULL AND failed_at IS NULL;

CREATE INDEX idx_projection_refresh_queue_ready
    ON projection_refresh_queue (available_at, id)
    WHERE processed_at IS NULL AND failed_at IS NULL;

CREATE INDEX idx_projection_refresh_queue_processed
    ON projection_refresh_queue (processed_at)
    WHERE processed_at IS NOT NULL;

CREATE INDEX idx_projection_refresh_queue_failed
    ON projection_refresh_queue (failed_at)
    WHERE failed_at IS NOT NULL;

COMMENT ON COLUMN projection_refresh_queue.attempts IS
    'Number of failed refresh attempts; reset only by inserting a new queue row.';
COMMENT ON COLUMN projection_refresh_queue.available_at IS
    'Earliest time the worker may retry this row.';
COMMENT ON COLUMN projection_refresh_queue.failed_at IS
    'Set when attempts reach the worker limit; the row is a dead letter until retention removes it.';
COMMENT ON INDEX uq_projection_refresh_queue_pending_user IS
    'Coalesces multiple corporate actions for the same user into one pending refresh.';

-- ---------------------------------------------------------------------------
-- Corporate-action reconciliation focus
-- ---------------------------------------------------------------------------

CREATE TABLE projection_reconciliation_focus (
    user_id       INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    reason        TEXT NOT NULL,
    touched_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reconciled_at TIMESTAMPTZ
);

CREATE INDEX idx_projection_reconciliation_focus_touched
    ON projection_reconciliation_focus (touched_at, user_id);

COMMENT ON TABLE projection_reconciliation_focus IS
    'Users touched by a corporate action; reconciliation checks them even when they fall outside the random sample.';

-- ---------------------------------------------------------------------------
-- Queue and focus helpers
-- ---------------------------------------------------------------------------

-- Reads the configured synchronous fan-out limit. The fallback is deliberately
-- high: a database with 0028 applied but no worker must never enqueue work.
CREATE OR REPLACE FUNCTION projection_sync_user_limit()
RETURNS INTEGER
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    v_raw TEXT;
BEGIN
    SELECT value INTO v_raw
    FROM projection_meta
    WHERE key = 'queue_sync_user_limit';

    IF v_raw IS NULL OR v_raw !~ '^[0-9]+$' THEN
        RETURN 1000000;
    END IF;

    RETURN v_raw::INTEGER;
END;
$$;

COMMENT ON FUNCTION projection_sync_user_limit() IS
    'Affected-user count at or below which the stock-event trigger refreshes synchronously.';

CREATE OR REPLACE FUNCTION projection_touch_reconciliation_users(
    p_user_ids INTEGER[],
    p_reason TEXT
)
RETURNS INTEGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_count INTEGER := 0;
BEGIN
    IF p_user_ids IS NULL OR cardinality(p_user_ids) = 0 THEN
        RETURN 0;
    END IF;

    INSERT INTO projection_reconciliation_focus (
        user_id, reason, touched_at, reconciled_at
    )
    SELECT DISTINCT u.id, p_reason, NOW(), NULL::TIMESTAMPTZ
    FROM users u
    WHERE u.id = ANY(p_user_ids)
    ON CONFLICT (user_id) DO UPDATE
    SET reason = EXCLUDED.reason,
        touched_at = NOW(),
        reconciled_at = NULL::TIMESTAMPTZ;

    GET DIAGNOSTICS v_count = ROW_COUNT;
    RETURN v_count;
END;
$$;

COMMENT ON FUNCTION projection_touch_reconciliation_users(INTEGER[], TEXT) IS
    'Marks users for reconciliation after a corporate action.';

CREATE OR REPLACE FUNCTION projection_enqueue_users(
    p_user_ids INTEGER[],
    p_reason TEXT
)
RETURNS INTEGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_inserted INTEGER := 0;
BEGIN
    IF p_user_ids IS NULL OR cardinality(p_user_ids) = 0 THEN
        RETURN 0;
    END IF;

    INSERT INTO projection_refresh_queue (user_id, reason)
    SELECT DISTINCT u.id, p_reason
    FROM users u
    WHERE u.id = ANY(p_user_ids)
    ON CONFLICT (user_id) WHERE processed_at IS NULL AND failed_at IS NULL
    DO NOTHING;

    GET DIAGNOSTICS v_inserted = ROW_COUNT;

    PERFORM projection_touch_reconciliation_users(p_user_ids, p_reason);

    RETURN v_inserted;
END;
$$;

COMMENT ON FUNCTION projection_enqueue_users(INTEGER[], TEXT) IS
    'Enqueues one pending projection refresh per affected user and records reconciliation focus.';

-- Process exactly one ready queue row. The worker calls this repeatedly, so one
-- user refresh is one transaction and no per-user advisory locks are held
-- across a large batch. The inner exception block keeps the failure accounting
-- when refresh_user_projections raises.
CREATE OR REPLACE FUNCTION process_projection_refresh_queue(
    p_max_attempts INTEGER DEFAULT 5
)
RETURNS TABLE (
    outcome TEXT,
    user_id INTEGER,
    attempts INTEGER,
    available_at TIMESTAMPTZ,
    last_error TEXT
)
LANGUAGE plpgsql
AS $$
DECLARE
    v_row projection_refresh_queue%ROWTYPE;
    v_attempts INTEGER;
    v_backoff INTERVAL;
BEGIN
    IF p_max_attempts < 1 THEN
        RAISE EXCEPTION 'p_max_attempts must be at least 1';
    END IF;

    SELECT q.* INTO v_row
    FROM projection_refresh_queue q
    WHERE q.processed_at IS NULL
      AND q.failed_at IS NULL
      AND q.available_at <= NOW()
    ORDER BY q.available_at, q.id
    FOR UPDATE SKIP LOCKED
    LIMIT 1;

    IF NOT FOUND THEN
        RETURN QUERY
        SELECT 'idle'::TEXT, NULL::INTEGER, NULL::INTEGER,
               NULL::TIMESTAMPTZ, NULL::TEXT;
        RETURN;
    END IF;

    BEGIN
        PERFORM refresh_user_projections(v_row.user_id);

        UPDATE projection_refresh_queue
        SET processed_at = NOW(),
            last_error = NULL
        WHERE id = v_row.id;

        RETURN QUERY
        SELECT 'processed'::TEXT, v_row.user_id, v_row.attempts,
               NULL::TIMESTAMPTZ, NULL::TEXT;
    EXCEPTION WHEN OTHERS THEN
        v_attempts := v_row.attempts + 1;
        v_backoff := make_interval(
            secs => LEAST(300, POWER(2, LEAST(v_attempts, 8)))::DOUBLE PRECISION
        );

        UPDATE projection_refresh_queue
        SET attempts = v_attempts,
            last_error = SQLERRM,
            available_at = CASE
                WHEN v_attempts >= p_max_attempts THEN NOW()
                ELSE NOW() + v_backoff
            END,
            failed_at = CASE
                WHEN v_attempts >= p_max_attempts THEN NOW()
                ELSE NULL
            END
        WHERE id = v_row.id;

        RETURN QUERY
        SELECT CASE
                   WHEN v_attempts >= p_max_attempts THEN 'failed'::TEXT
                   ELSE 'retry'::TEXT
               END,
               v_row.user_id,
               v_attempts,
               NOW() + v_backoff,
               SQLERRM;
    END;
END;
$$;

COMMENT ON FUNCTION process_projection_refresh_queue(INTEGER) IS
    'Processes one ready queue row per transaction with exponential retry and dead-letter support.';

CREATE OR REPLACE FUNCTION projection_queue_stats()
RETURNS TABLE (
    pending BIGINT,
    ready BIGINT,
    failed BIGINT,
    processed BIGINT,
    oldest_pending_seconds BIGINT,
    max_attempts INTEGER
)
LANGUAGE SQL
STABLE
AS $$
    SELECT
        COUNT(*) FILTER (
            WHERE processed_at IS NULL AND failed_at IS NULL
        ) AS pending,
        COUNT(*) FILTER (
            WHERE processed_at IS NULL
              AND failed_at IS NULL
              AND available_at <= NOW()
        ) AS ready,
        COUNT(*) FILTER (
            WHERE failed_at IS NOT NULL
        ) AS failed,
        COUNT(*) FILTER (
            WHERE processed_at IS NOT NULL
        ) AS processed,
        COALESCE(
            EXTRACT(EPOCH FROM (
                NOW() - MIN(created_at) FILTER (
                    WHERE processed_at IS NULL AND failed_at IS NULL
                )
            ))::BIGINT,
            0
        ) AS oldest_pending_seconds,
        COALESCE(MAX(attempts), 0)::INTEGER AS max_attempts
    FROM projection_refresh_queue;
$$;

COMMENT ON FUNCTION projection_queue_stats() IS
    'Operational queue depth, failure, and age summary for the worker and alerting.';

CREATE OR REPLACE FUNCTION prune_projection_refresh_queue(
    p_processed_retention INTERVAL DEFAULT INTERVAL '7 days',
    p_failed_retention INTERVAL DEFAULT INTERVAL '30 days'
)
RETURNS INTEGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_count INTEGER := 0;
BEGIN
    DELETE FROM projection_refresh_queue
    WHERE (processed_at IS NOT NULL
           AND processed_at < NOW() - p_processed_retention)
       OR (failed_at IS NOT NULL
           AND failed_at < NOW() - p_failed_retention);

    GET DIAGNOSTICS v_count = ROW_COUNT;
    RETURN v_count;
END;
$$;

COMMENT ON FUNCTION prune_projection_refresh_queue(INTERVAL, INTERVAL) IS
    'Removes old processed rows and old dead letters.';

-- ---------------------------------------------------------------------------
-- Rollover due checks
-- ---------------------------------------------------------------------------

-- Full factor refresh is O(instruments) and must run once per market date.
-- The advisory lock serialises multiple API instances.
CREATE OR REPLACE FUNCTION refresh_instrument_factors_if_due(
    p_as_of DATE DEFAULT market_today()
)
RETURNS INTEGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_last  DATE;
    v_count INTEGER;
BEGIN
    PERFORM pg_advisory_xact_lock(810201712);

    SELECT value::DATE INTO v_last
    FROM projection_meta
    WHERE key = 'last_instrument_factor_date';

    IF v_last IS NOT NULL AND p_as_of <= v_last THEN
        RETURN -1;
    END IF;

    v_count := refresh_instrument_factors(p_as_of);

    INSERT INTO projection_meta (key, value, updated_at)
    VALUES ('last_instrument_factor_date', p_as_of::TEXT, NOW())
    ON CONFLICT (key) DO UPDATE
    SET value = EXCLUDED.value,
        updated_at = NOW();

    RETURN v_count;
END;
$$;

COMMENT ON FUNCTION refresh_instrument_factors_if_due(DATE) IS
    'Runs the full instrument-factor refresh at most once per market date. Returns -1 when skipped.';

CREATE OR REPLACE FUNCTION process_corporate_action_cash_if_due(
    p_as_of DATE DEFAULT market_today()
)
RETURNS INTEGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_last  DATE;
    v_count INTEGER;
BEGIN
    PERFORM pg_advisory_xact_lock(810201713);

    SELECT value::DATE INTO v_last
    FROM projection_meta
    WHERE key = 'last_corporate_action_cash_date';

    IF v_last IS NOT NULL AND p_as_of <= v_last THEN
        RETURN -1;
    END IF;

    v_count := process_corporate_action_cash(p_as_of);

    INSERT INTO projection_meta (key, value, updated_at)
    VALUES ('last_corporate_action_cash_date', p_as_of::TEXT, NOW())
    ON CONFLICT (key) DO UPDATE
    SET value = EXCLUDED.value,
        updated_at = NOW();

    RETURN v_count;
END;
$$;

COMMENT ON FUNCTION process_corporate_action_cash_if_due(DATE) IS
    'Runs the idempotent corporate-action cash posting at most once per market date. Returns -1 when skipped.';

-- The existing function remains the source of truth for date-sensitive state.
-- This wrapper adds an advisory lock and records the users it refreshed so
-- reconciliation checks them on the next run.
CREATE OR REPLACE FUNCTION refresh_effective_corporate_actions_if_due(
    p_as_of DATE DEFAULT market_today()
)
RETURNS INTEGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_last    DATE;
    v_started TIMESTAMPTZ;
    v_count   INTEGER;
BEGIN
    PERFORM pg_advisory_xact_lock(810201714);

    SELECT value::DATE INTO v_last
    FROM projection_meta
    WHERE key = 'last_effective_corporate_actions_date';

    IF v_last IS NOT NULL AND p_as_of <= v_last THEN
        RETURN -1;
    END IF;

    -- NOW() is the transaction timestamp, which is also what
    -- refresh_reward_state() writes to reward_state.updated_at.
    v_started := NOW();
    v_count := refresh_effective_corporate_actions(p_as_of);

    INSERT INTO projection_reconciliation_focus (
        user_id, reason, touched_at, reconciled_at
    )
    SELECT DISTINCT rs.user_id,
           'effective_corporate_action',
           NOW(),
           NULL::TIMESTAMPTZ
    FROM reward_state rs
    WHERE rs.updated_at >= v_started
    ON CONFLICT (user_id) DO UPDATE
    SET reason = EXCLUDED.reason,
        touched_at = NOW(),
        reconciled_at = NULL::TIMESTAMPTZ;

    RETURN v_count;
END;
$$;

COMMENT ON FUNCTION refresh_effective_corporate_actions_if_due(DATE) IS
    'Serialises the daily effective-event refresh and tracks refreshed users for reconciliation.';

-- ---------------------------------------------------------------------------
-- Reconciliation
-- ---------------------------------------------------------------------------

-- User-scoped copy of reward_corporate_action_outcomes_base. Only reconciliation
-- and tests call this; the online refresh path still uses the per-reward point
-- function.
CREATE OR REPLACE FUNCTION reward_corporate_action_outcomes_base_for_users(
    p_user_ids INTEGER[]
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
    WITH reward_adjustments AS (
        SELECT
            a.reward_id,
            SUM(a.delta_amount) AS total_adjustment_amount
        FROM adjustments a
        JOIN rewards r ON r.id = a.reward_id
        WHERE r.user_id = ANY(COALESCE(p_user_ids, ARRAY[]::INTEGER[]))
        GROUP BY a.reward_id
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
        WHERE r.user_id = ANY(COALESCE(p_user_ids, ARRAY[]::INTEGER[]))
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
            FROM instrument_delist_event(
                b.original_symbol::TEXT,
                b.reward_date,
                market_today()
            )
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
$$;

COMMENT ON FUNCTION reward_corporate_action_outcomes_base_for_users(INTEGER[]) IS
    'User-scoped lifecycle baseline for reconciliation; never used by the online refresh path.';

CREATE OR REPLACE FUNCTION projection_mismatches_for_users(
    p_user_ids INTEGER[]
)
RETURNS TABLE (
    user_id INTEGER,
    mismatch_type TEXT,
    entity_key TEXT,
    expected JSONB,
    actual JSONB
)
LANGUAGE SQL
STABLE
AS $$
    WITH expected_rewards AS (
        SELECT *
        FROM reward_corporate_action_outcomes_base_for_users(
            COALESCE(p_user_ids, ARRAY[]::INTEGER[])
        )
    ),
    projected_rewards AS (
        SELECT *
        FROM reward_state
        WHERE user_id = ANY(COALESCE(p_user_ids, ARRAY[]::INTEGER[]))
    ),
    reward_diffs AS (
        SELECT
            COALESCE(e.user_id, rs.user_id) AS user_id,
            CASE
                WHEN rs.reward_id IS NULL THEN 'reward_state_missing'
                WHEN e.reward_id IS NULL THEN 'reward_state_extra'
                ELSE 'reward_state_diff'
            END AS mismatch_type,
            COALESCE(e.reward_id, rs.reward_id)::TEXT AS entity_key,
            to_jsonb(e) AS expected,
            to_jsonb(rs) AS actual
        FROM expected_rewards e
        FULL OUTER JOIN projected_rewards rs ON rs.reward_id = e.reward_id
        WHERE rs.reward_id IS NULL
           OR e.reward_id IS NULL
           OR ROW(
                rs.user_id,
                rs.original_symbol,
                rs.terminal_symbol,
                rs.reward_date,
                rs.adjusted_quantity,
                rs.delist_event_id,
                rs.delist_effective_date,
                rs.outcome_type,
                rs.cash_proceeds,
                rs.written_off_quantity,
                rs.total_adjustment_amount,
                rs.is_active,
                rs.cash_in_lieu_proceeds,
                rs.fraction_quantity
              ) IS DISTINCT FROM ROW(
                e.user_id,
                e.original_symbol::TEXT,
                e.terminal_symbol,
                e.reward_date,
                e.adjusted_quantity::NUMERIC(18,6),
                e.delist_event_id,
                e.delist_effective_date,
                e.outcome_type,
                e.cash_proceeds::NUMERIC(18,4),
                e.written_off_quantity::NUMERIC(18,6),
                e.total_adjustment_amount::NUMERIC(18,4),
                e.is_active,
                e.cash_in_lieu_proceeds::NUMERIC(18,4),
                e.fraction_quantity::NUMERIC(18,6)
              )
    ),
    expected_portfolio AS (
        SELECT
            e.user_id,
            e.terminal_symbol,
            SUM(e.adjusted_quantity)::NUMERIC(18,6) AS quantity,
            SUM(e.total_adjustment_amount)::NUMERIC(18,4) AS total_adjustment_amount
        FROM expected_rewards e
        WHERE e.is_active
        GROUP BY e.user_id, e.terminal_symbol
    ),
    projected_portfolio AS (
        SELECT *
        FROM portfolio_position
        WHERE user_id = ANY(COALESCE(p_user_ids, ARRAY[]::INTEGER[]))
    ),
    portfolio_diffs AS (
        SELECT
            COALESCE(e.user_id, p.user_id) AS user_id,
            CASE
                WHEN p.user_id IS NULL THEN 'portfolio_missing'
                WHEN e.user_id IS NULL THEN 'portfolio_extra'
                ELSE 'portfolio_diff'
            END AS mismatch_type,
            COALESCE(e.user_id, p.user_id)::TEXT
                || ':' || COALESCE(e.terminal_symbol, p.terminal_symbol) AS entity_key,
            to_jsonb(e) AS expected,
            to_jsonb(p) AS actual
        FROM expected_portfolio e
        FULL OUTER JOIN projected_portfolio p
          ON p.user_id = e.user_id
         AND p.terminal_symbol = e.terminal_symbol
        WHERE p.user_id IS NULL
           OR e.user_id IS NULL
           OR ROW(p.quantity, p.total_adjustment_amount)
              IS DISTINCT FROM ROW(e.quantity, e.total_adjustment_amount)
    ),
    expected_cash AS (
        SELECT
            e.user_id,
            SUM(e.cash_proceeds)::NUMERIC(18,4) AS cash_proceeds,
            SUM(e.written_off_quantity)::NUMERIC(18,6) AS written_off_quantity,
            SUM(e.cash_in_lieu_proceeds)::NUMERIC(18,4) AS cash_in_lieu_proceeds
        FROM expected_rewards e
        GROUP BY e.user_id
    ),
    projected_cash AS (
        SELECT *
        FROM user_cash_position
        WHERE user_id = ANY(COALESCE(p_user_ids, ARRAY[]::INTEGER[]))
    ),
    cash_diffs AS (
        SELECT
            COALESCE(e.user_id, c.user_id) AS user_id,
            CASE
                WHEN c.user_id IS NULL THEN 'cash_missing'
                WHEN e.user_id IS NULL THEN 'cash_extra'
                ELSE 'cash_diff'
            END AS mismatch_type,
            COALESCE(e.user_id, c.user_id)::TEXT AS entity_key,
            to_jsonb(e) AS expected,
            to_jsonb(c) AS actual
        FROM expected_cash e
        FULL OUTER JOIN projected_cash c ON c.user_id = e.user_id
        WHERE c.user_id IS NULL
           OR e.user_id IS NULL
           OR ROW(c.cash_proceeds, c.written_off_quantity, c.cash_in_lieu_proceeds)
              IS DISTINCT FROM ROW(
                  e.cash_proceeds,
                  e.written_off_quantity,
                  e.cash_in_lieu_proceeds
              )
    )
    SELECT * FROM (
        SELECT * FROM reward_diffs
        UNION ALL
        SELECT * FROM portfolio_diffs
        UNION ALL
        SELECT * FROM cash_diffs
    ) mismatches
    ORDER BY mismatches.user_id, mismatches.mismatch_type, mismatches.entity_key;
$$;

COMMENT ON FUNCTION projection_mismatches_for_users(INTEGER[]) IS
    'Compares reward_state, portfolio_position, and user_cash_position with the user-scoped lifecycle baseline.';

-- Focus users first (oldest touches), then a bounded random sample of active
-- users. Reconciliation is allowed to be slow; correctness matters more than a
-- perfectly uniform sample.
CREATE OR REPLACE FUNCTION projection_reconciliation_users(
    p_sample_percent NUMERIC DEFAULT 0.1,
    p_max_users INTEGER DEFAULT 500
)
RETURNS TABLE (
    user_id INTEGER,
    source TEXT
)
LANGUAGE plpgsql
AS $$
DECLARE
    v_focus_count INTEGER;
    v_sample_limit INTEGER;
BEGIN
    IF p_max_users < 1 THEN
        RAISE EXCEPTION 'p_max_users must be at least 1';
    END IF;
    IF p_sample_percent < 0 OR p_sample_percent > 100 THEN
        RAISE EXCEPTION 'p_sample_percent must be between 0 and 100';
    END IF;

    SELECT COUNT(*) INTO v_focus_count
    FROM projection_reconciliation_focus f
    WHERE f.reconciled_at IS NULL OR f.reconciled_at < f.touched_at;

    v_sample_limit := GREATEST(
        0,
        LEAST(
            p_max_users - LEAST(v_focus_count, p_max_users),
            CEIL(
                (SELECT COUNT(*) FROM users WHERE deleted_at IS NULL)
                * p_sample_percent / 100.0
            )::INTEGER
        )
    );

    RETURN QUERY
    WITH focus AS (
        SELECT f.user_id, 'focus'::TEXT AS source
        FROM projection_reconciliation_focus f
        WHERE f.reconciled_at IS NULL OR f.reconciled_at < f.touched_at
        ORDER BY f.touched_at, f.user_id
        LIMIT p_max_users
    ),
    sample AS (
        SELECT u.id AS user_id, 'sample'::TEXT AS source
        FROM users u
        WHERE u.deleted_at IS NULL
          AND NOT EXISTS (SELECT 1 FROM focus f WHERE f.user_id = u.id)
        ORDER BY random()
        LIMIT v_sample_limit
    )
    SELECT selected.user_id, selected.source
    FROM (
        SELECT * FROM focus
        UNION ALL
        SELECT * FROM sample
    ) selected
    ORDER BY selected.user_id;
END;
$$;

COMMENT ON FUNCTION projection_reconciliation_users(NUMERIC, INTEGER) IS
    'Returns focus users plus a bounded random sample for reconciliation.';

-- One-call reconciliation for the worker and manual checks. The advisory lock
-- prevents duplicate work when multiple API instances run the job. Mismatch
-- details are capped, but mismatch_count always reflects every mismatch.
CREATE OR REPLACE FUNCTION run_projection_reconciliation(
    p_sample_percent NUMERIC DEFAULT 0.1,
    p_max_users INTEGER DEFAULT 500,
    p_max_details INTEGER DEFAULT 1000
)
RETURNS TABLE (
    users_checked INTEGER,
    mismatch_count INTEGER,
    mismatches JSONB
)
LANGUAGE plpgsql
AS $$
DECLARE
    v_started          TIMESTAMPTZ := clock_timestamp();
    v_users            INTEGER[];
    v_users_checked    INTEGER := 0;
    v_all              JSONB := '[]'::JSONB;
    v_mismatches       JSONB := '[]'::JSONB;
    v_mismatched_users INTEGER[];
    v_count            INTEGER := 0;
BEGIN
    IF p_max_details < 0 THEN
        RAISE EXCEPTION 'p_max_details must not be negative';
    END IF;

    PERFORM pg_advisory_xact_lock(810201715);

    SELECT ARRAY(
        SELECT selected.user_id
        FROM projection_reconciliation_users(p_sample_percent, p_max_users) selected
    ) INTO v_users;

    v_users_checked := COALESCE(cardinality(v_users), 0);

    IF v_users_checked > 0 THEN
        SELECT COALESCE(
            jsonb_agg(to_jsonb(m) ORDER BY m.user_id, m.mismatch_type, m.entity_key),
            '[]'::JSONB
        )
        INTO v_all
        FROM projection_mismatches_for_users(v_users) m;

        v_count := jsonb_array_length(v_all);

        SELECT ARRAY(
            SELECT DISTINCT (detail->>'user_id')::INTEGER
            FROM jsonb_array_elements(v_all) detail
        ) INTO v_mismatched_users;

        SELECT COALESCE(
            jsonb_agg(detail ORDER BY ordinality),
            '[]'::JSONB
        )
        INTO v_mismatches
        FROM jsonb_array_elements(v_all) WITH ORDINALITY AS x(detail, ordinality)
        WHERE ordinality <= p_max_details;

        UPDATE projection_reconciliation_focus
        SET reconciled_at = NOW()
        WHERE user_id = ANY(v_users)
          AND NOT (user_id = ANY(COALESCE(v_mismatched_users, ARRAY[]::INTEGER[])));
    END IF;

    INSERT INTO projection_meta (key, value, updated_at)
    VALUES
        ('last_reconciled_at', NOW()::TEXT, NOW()),
        ('last_reconciliation_users', v_users_checked::TEXT, NOW()),
        ('last_reconciliation_mismatches', v_count::TEXT, NOW()),
        ('last_reconciliation_duration_ms',
         (EXTRACT(EPOCH FROM (clock_timestamp() - v_started)) * 1000)::BIGINT::TEXT,
         NOW())
    ON CONFLICT (key) DO UPDATE
    SET value = EXCLUDED.value,
        updated_at = NOW();

    RETURN QUERY SELECT v_users_checked, v_count, v_mismatches;
END;
$$;

COMMENT ON FUNCTION run_projection_reconciliation(NUMERIC, INTEGER, INTEGER) IS
    'Samples focus users, compares projections with the lifecycle baseline, and records the result in projection_meta.';

CREATE OR REPLACE FUNCTION projection_factor_status()
RETURNS TABLE (
    total BIGINT,
    stale BIGINT,
    invalid BIGINT
)
LANGUAGE SQL
STABLE
AS $$
    SELECT
        COUNT(*) AS total,
        COUNT(*) FILTER (WHERE as_of IS DISTINCT FROM market_today()) AS stale,
        COUNT(*) FILTER (WHERE factor <= 0) AS invalid
    FROM instrument_factor;
$$;

COMMENT ON FUNCTION projection_factor_status() IS
    'Factor-cache health for alerting; stale rows force the safe fallback.';

-- ---------------------------------------------------------------------------
-- Corporate-action trigger: bounded synchronous fan-out or queue
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION sync_corporate_action_cash_trigger()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_symbols        TEXT[];
    v_factor_symbols TEXT[];
    v_rewards        INTEGER[];
    v_users          INTEGER[];
    v_user           INTEGER;
    v_reward         INTEGER;
    v_reason         TEXT;
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

    -- Resolve the affected rewards once. The helper walks instrument_lineage
    -- backwards, so an event on an intermediate merger symbol (A -> B -> C,
    -- event on B) also matches rewards that started at A.
    v_rewards := ARRAY(
        SELECT DISTINCT reward_id
        FROM affected_reward_ids_for_symbols(v_symbols)
        ORDER BY reward_id
    );

    IF v_rewards IS NOT NULL AND cardinality(v_rewards) > 0 THEN
        FOREACH v_reward IN ARRAY v_rewards LOOP
            PERFORM refresh_reward_state(v_reward);
        END LOOP;

        SELECT ARRAY(
            SELECT DISTINCT rs.user_id
            FROM reward_state rs
            WHERE rs.reward_id = ANY(v_rewards)
            ORDER BY rs.user_id
        ) INTO v_users;
    END IF;

    IF v_users IS NULL OR cardinality(v_users) = 0 THEN
        RETURN NULL;
    END IF;

    v_reason := 'corporate_action_' || lower(TG_OP);

    IF cardinality(v_users) <= projection_sync_user_limit() THEN
        FOREACH v_user IN ARRAY v_users LOOP
            PERFORM refresh_user_projections(v_user);
        END LOOP;

        PERFORM projection_touch_reconciliation_users(v_users, v_reason);
    ELSE
        PERFORM projection_enqueue_users(v_users, v_reason);
    END IF;

    RETURN NULL;
END;
$$;

COMMENT ON FUNCTION sync_corporate_action_cash_trigger() IS
    'Statement trigger for stock_events: posts cash, refreshes factors/reward_state, then refreshes small user fan-outs synchronously or enqueues large ones.';

-- ---------------------------------------------------------------------------
-- Metadata
-- ---------------------------------------------------------------------------

-- Safe default for a rolling deploy: no user is enqueued until a worker lowers
-- this value on its first successful tick.
INSERT INTO projection_meta (key, value)
VALUES ('queue_sync_user_limit', '1000000')
ON CONFLICT (key) DO NOTHING;

INSERT INTO projection_meta (key, value)
VALUES ('schema_version', '2')
ON CONFLICT (key) DO UPDATE
SET value = EXCLUDED.value,
    updated_at = NOW();
