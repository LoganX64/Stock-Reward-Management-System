-- Revert the Phase 1 projection foundation.
--
-- 0026 replaced the stock-event cash trigger, so dropping only the new
-- projection objects would leave stock_events without cash processing. Restore
-- the exact 0022 trigger function and statement trigger first, then remove the
-- projection machinery.

DROP TRIGGER IF EXISTS trg_process_corporate_action_cash_insert ON stock_events;
DROP TRIGGER IF EXISTS trg_process_corporate_action_cash_update ON stock_events;
DROP TRIGGER IF EXISTS trg_process_corporate_action_cash_delete ON stock_events;
DROP TRIGGER IF EXISTS trg_process_corporate_action_cash ON stock_events;

CREATE OR REPLACE FUNCTION sync_corporate_action_cash_trigger()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM process_corporate_action_cash(market_today());
    RETURN NULL;
END;
$$;

CREATE TRIGGER trg_process_corporate_action_cash
AFTER INSERT OR UPDATE OR DELETE ON stock_events
FOR EACH STATEMENT
EXECUTE FUNCTION sync_corporate_action_cash_trigger();

DROP TRIGGER IF EXISTS trg_reward_projection ON rewards;
DROP TRIGGER IF EXISTS trg_adjustment_projection ON adjustments;

DROP FUNCTION IF EXISTS refresh_reward_projection();
DROP FUNCTION IF EXISTS refresh_adjustment_projection();
DROP FUNCTION IF EXISTS refresh_effective_corporate_actions(DATE);
DROP FUNCTION IF EXISTS refresh_reward_state(INTEGER);
DROP FUNCTION IF EXISTS refresh_user_projections(INTEGER);
DROP FUNCTION IF EXISTS refresh_instrument_factors(DATE);
DROP FUNCTION IF EXISTS refresh_instrument_factors_for_symbols(TEXT[], DATE);
DROP FUNCTION IF EXISTS backfill_instrument_factors(DATE);
DROP FUNCTION IF EXISTS projection_refresh_disabled();
DROP FUNCTION IF EXISTS affected_reward_ids_for_symbols(TEXT[]);
DROP FUNCTION IF EXISTS reward_corporate_action_outcome_base(INTEGER);

DROP VIEW IF EXISTS reward_corporate_action_outcomes_base;

DROP TABLE IF EXISTS projection_refresh_queue;
DROP TABLE IF EXISTS projection_meta;
DROP TABLE IF EXISTS instrument_factor;
DROP TABLE IF EXISTS user_cash_position;
DROP TABLE IF EXISTS portfolio_position;
DROP TABLE IF EXISTS reward_state;
