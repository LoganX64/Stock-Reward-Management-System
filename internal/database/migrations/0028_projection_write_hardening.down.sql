-- Revert Phase 2 write-path hardening.
--
-- Restore the Phase 1 synchronous stock-event trigger first, then remove the
-- queue/focus/wrapper objects. Queue rows are disposable by design, but drain
-- them before rolling back if projections must remain current without a rebuild.

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

DROP FUNCTION IF EXISTS run_projection_reconciliation(NUMERIC, INTEGER, INTEGER);
DROP FUNCTION IF EXISTS projection_mismatches_for_users(INTEGER[]);
DROP FUNCTION IF EXISTS projection_reconciliation_users(NUMERIC, INTEGER);
DROP FUNCTION IF EXISTS reward_corporate_action_outcomes_base_for_users(INTEGER[]);
DROP FUNCTION IF EXISTS projection_factor_status();
DROP FUNCTION IF EXISTS refresh_effective_corporate_actions_if_due(DATE);
DROP FUNCTION IF EXISTS process_corporate_action_cash_if_due(DATE);
DROP FUNCTION IF EXISTS refresh_instrument_factors_if_due(DATE);
DROP FUNCTION IF EXISTS prune_projection_refresh_queue(INTERVAL, INTERVAL);
DROP FUNCTION IF EXISTS projection_queue_stats();
DROP FUNCTION IF EXISTS process_projection_refresh_queue(INTEGER);
DROP FUNCTION IF EXISTS projection_enqueue_users(INTEGER[], TEXT);
DROP FUNCTION IF EXISTS projection_touch_reconciliation_users(INTEGER[], TEXT);
DROP FUNCTION IF EXISTS projection_sync_user_limit();

DROP TABLE IF EXISTS projection_reconciliation_focus;

DROP INDEX IF EXISTS uq_projection_refresh_queue_pending_user;
DROP INDEX IF EXISTS idx_projection_refresh_queue_ready;
DROP INDEX IF EXISTS idx_projection_refresh_queue_processed;
DROP INDEX IF EXISTS idx_projection_refresh_queue_failed;

ALTER TABLE projection_refresh_queue
    DROP CONSTRAINT IF EXISTS projection_refresh_queue_user_fk;

ALTER TABLE projection_refresh_queue
    DROP COLUMN IF EXISTS failed_at,
    DROP COLUMN IF EXISTS available_at,
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS attempts;

DELETE FROM projection_meta
WHERE key IN (
    'queue_sync_user_limit',
    'last_instrument_factor_date',
    'last_corporate_action_cash_date',
    'last_reconciled_at',
    'last_reconciliation_users',
    'last_reconciliation_mismatches',
    'last_reconciliation_duration_ms'
);

UPDATE projection_meta
SET value = '1',
    updated_at = NOW()
WHERE key = 'schema_version';
