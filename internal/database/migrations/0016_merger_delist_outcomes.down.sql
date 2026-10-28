DROP VIEW IF EXISTS user_corporate_action_cash CASCADE;
DROP VIEW IF EXISTS reward_corporate_action_outcomes CASCADE;
DROP VIEW IF EXISTS today_rewards CASCADE;
DROP VIEW IF EXISTS user_portfolio CASCADE;

DROP FUNCTION IF EXISTS reward_merger_cash_proceeds(INTEGER, DATE);
DROP FUNCTION IF EXISTS reward_quantity_at(INTEGER, DATE);
DROP FUNCTION IF EXISTS instrument_delist_event(TEXT, DATE, DATE);

DROP INDEX IF EXISTS unique_delist_symbol_upper;

DROP TRIGGER IF EXISTS trg_validate_stock_event ON stock_events;
DROP FUNCTION IF EXISTS validate_stock_event();

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

CREATE TRIGGER trg_validate_merge_lineage_cycle
BEFORE INSERT OR UPDATE ON stock_events
FOR EACH ROW
EXECUTE FUNCTION validate_merge_lineage_cycle();

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

ALTER TABLE stock_events
    DROP CONSTRAINT IF EXISTS stock_events_fraction_policy_values,
    DROP CONSTRAINT IF EXISTS stock_events_write_off_requires_no_cash,
    DROP CONSTRAINT IF EXISTS stock_events_cash_exit_requires_cash,
    DROP CONSTRAINT IF EXISTS stock_events_delist_outcome_event_type,
    DROP CONSTRAINT IF EXISTS stock_events_delist_outcome_values,
    DROP CONSTRAINT IF EXISTS stock_events_cash_per_unit_event_type,
    DROP CONSTRAINT IF EXISTS stock_events_cash_per_unit_nonnegative,
    DROP COLUMN IF EXISTS fraction_policy,
    DROP COLUMN IF EXISTS delist_outcome,
    DROP COLUMN IF EXISTS cash_per_unit;

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
