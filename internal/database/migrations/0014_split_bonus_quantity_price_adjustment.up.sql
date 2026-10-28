-- Corporate-action ratios are cumulative multiplicative factors. Prices remain
-- stored raw; views apply the inverse factor exactly once at read time.
CREATE OR REPLACE FUNCTION stock_action_factor(p_symbol TEXT, p_as_of DATE)
RETURNS NUMERIC
LANGUAGE SQL
STABLE
AS $$
    SELECT COALESCE(EXP(SUM(LN(se.ratio_num / se.ratio_den))), 1::numeric)
    FROM stock_events se
    WHERE UPPER(se.stock_symbol) = UPPER(p_symbol)
      AND se.event_type IN ('split', 'bonus')
      AND se.effective_date::date <= p_as_of
      AND se.ratio_num > 0
      AND se.ratio_den > 0;
$$;

-- Quantity factors also include merger exchange ratios. Merger prices are in
-- successor-instrument units, so they are not inverse-adjusted like splits.
CREATE OR REPLACE FUNCTION stock_quantity_factor(p_symbol TEXT, p_as_of DATE)
RETURNS NUMERIC
LANGUAGE SQL
STABLE
AS $$
    SELECT COALESCE(EXP(SUM(LN(se.ratio_num / se.ratio_den))), 1::numeric)
    FROM stock_events se
    WHERE UPPER(se.stock_symbol) = UPPER(p_symbol)
      AND se.event_type IN ('split', 'bonus', 'merge')
      AND se.effective_date::date <= p_as_of
      AND se.ratio_num > 0
      AND se.ratio_den > 0;
$$;

DROP VIEW IF EXISTS reward_ledger_reconciliation CASCADE;
DROP VIEW IF EXISTS user_portfolio CASCADE;
DROP VIEW IF EXISTS today_rewards CASCADE;
DROP VIEW IF EXISTS historical_rewards CASCADE;
DROP VIEW IF EXISTS reward_current_balances CASCADE;
DROP VIEW IF EXISTS reward_quantity_movements CASCADE;
DROP VIEW IF EXISTS stock_price_history_adjusted CASCADE;
DROP VIEW IF EXISTS stock_prices_adjusted CASCADE;

-- Adjustments are denominated in units on their effective date (the creation
-- date for API writes). Existing rows take their original posting date.
ALTER TABLE adjustments
    ADD COLUMN effective_date DATE;

UPDATE adjustments
SET effective_date = COALESCE(created_at, NOW())::date
WHERE effective_date IS NULL;

ALTER TABLE adjustments
    ALTER COLUMN effective_date SET DEFAULT CURRENT_DATE;

ALTER TABLE adjustments
    ALTER COLUMN effective_date SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_adjustments_reward_effective_date
    ON adjustments (reward_id, effective_date);

-- Recover the true original request quantity for legacy rewards whose
-- original_quantity was captured after an adjustment had already mutated the
-- mutable balance.
UPDATE rewards r
SET original_quantity = GREATEST(r.quantity - totals.delta_quantity, 0)
FROM (
    SELECT reward_id, SUM(delta_quantity) AS delta_quantity
    FROM adjustments
    GROUP BY reward_id
) totals
WHERE totals.reward_id = r.id;

-- Keep rewards.quantity as the balance in reward-date units. Reporting reads
-- effective-dated movements and applies only later corporate actions.
WITH balances AS (
    SELECT
        r.id,
        r.original_quantity + COALESCE(SUM(
            a.delta_quantity
            * stock_quantity_factor(r.stock_symbol, r.created_at::date)
            / stock_quantity_factor(r.stock_symbol, a.effective_date)
        ), 0) AS normalized_quantity
    FROM rewards r
    LEFT JOIN adjustments a ON a.reward_id = r.id
    GROUP BY r.id, r.original_quantity, r.stock_symbol, r.created_at
)
UPDATE rewards r
SET quantity = GREATEST(balances.normalized_quantity, 0)
FROM balances
WHERE balances.id = r.id;

CREATE OR REPLACE VIEW stock_prices_adjusted AS
SELECT
    sp.id,
    sp.stock_symbol,
    sp.price AS raw_price,
    sp.price / stock_action_factor(sp.stock_symbol, CURRENT_DATE) AS adjusted_price,
    sp.updated_at
FROM stock_prices sp;

CREATE OR REPLACE VIEW stock_price_history_adjusted AS
SELECT
    sph.id,
    sph.stock_symbol,
    sph.price AS raw_price,
    sph.price / stock_action_factor(sph.stock_symbol, sph.date) AS adjusted_price,
    sph.date,
    sph.created_at
FROM stock_price_history sph;

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
        * stock_quantity_factor(r.stock_symbol, r.created_at::date)
        / stock_quantity_factor(r.stock_symbol, a.effective_date) AS normalized_quantity_delta
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
        * stock_quantity_factor(r.stock_symbol, CURRENT_DATE)
        / stock_quantity_factor(r.stock_symbol, r.created_at::date) AS adjusted_quantity
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
    COALESCE(lm.new_symbol, r.stock_symbol) AS stock_symbol,
    r.original_quantity AS adjusted_quantity,
    sph.adjusted_price AS price,
    COALESCE(ra.total_adjustment_amount, 0) AS total_adjustment_amount,
    ROUND(r.original_quantity * sph.adjusted_price, 4) AS inr_value
FROM rewards r
LEFT JOIN reward_adjustments ra ON r.id = ra.reward_id
LEFT JOIN LATERAL (
    SELECT se.new_symbol
    FROM stock_events se
    WHERE se.stock_symbol = r.stock_symbol
      AND se.event_type = 'merge'
      AND se.effective_date > r.created_at
      AND se.effective_date::date <= r.created_at::date
      AND se.new_symbol IS NOT NULL
    ORDER BY se.effective_date DESC
    LIMIT 1
) lm ON TRUE
JOIN stock_price_history_adjusted sph
    ON UPPER(COALESCE(lm.new_symbol, r.stock_symbol)) = UPPER(sph.stock_symbol)
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
    rcb.stock_symbol,
    rcb.adjusted_quantity,
    spa.adjusted_price AS current_price,
    COALESCE(ra.total_adjustment_amount, 0) AS total_adjustment_amount,
    ROUND(rcb.adjusted_quantity * spa.adjusted_price, 4) AS inr_value
FROM rewards r
JOIN reward_current_balances rcb ON rcb.reward_id = r.id
JOIN stock_prices_adjusted spa
    ON UPPER(rcb.stock_symbol) = UPPER(spa.stock_symbol)
LEFT JOIN reward_adjustments ra ON ra.reward_id = r.id
WHERE r.created_at::date = CURRENT_DATE
  AND rcb.adjusted_quantity > 0;

CREATE OR REPLACE VIEW user_portfolio AS
WITH reward_adjustments AS (
    SELECT reward_id, SUM(delta_amount) AS total_adjustment_amount
    FROM adjustments
    GROUP BY reward_id
)
SELECT
    rcb.user_id,
    COALESCE(lm.new_symbol, rcb.stock_symbol) AS stock_symbol,
    SUM(rcb.adjusted_quantity) AS adjusted_quantity,
    spa.adjusted_price AS current_price,
    ROUND(SUM(rcb.adjusted_quantity) * spa.adjusted_price, 4) AS inr_value,
    COALESCE(SUM(ra.total_adjustment_amount), 0) AS total_adjustment_amount
FROM rewards r
JOIN reward_current_balances rcb ON rcb.reward_id = r.id
LEFT JOIN reward_adjustments ra ON ra.reward_id = r.id
LEFT JOIN LATERAL (
    SELECT se.new_symbol
    FROM stock_events se
    WHERE se.stock_symbol = r.stock_symbol
      AND se.event_type = 'merge'
      AND se.effective_date > r.created_at
      AND se.effective_date::date <= CURRENT_DATE
      AND se.new_symbol IS NOT NULL
    ORDER BY se.effective_date DESC
    LIMIT 1
) lm ON TRUE
JOIN stock_prices_adjusted spa
    ON UPPER(COALESCE(lm.new_symbol, rcb.stock_symbol)) = UPPER(spa.stock_symbol)
WHERE rcb.adjusted_quantity > 0
  AND NOT EXISTS (
      SELECT 1
      FROM stock_events se_delist
      WHERE se_delist.event_type = 'delist'
        AND se_delist.effective_date <= CURRENT_DATE
        AND UPPER(se_delist.stock_symbol) = UPPER(r.stock_symbol)
  )
GROUP BY rcb.user_id, COALESCE(lm.new_symbol, rcb.stock_symbol), spa.adjusted_price;

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
            * stock_quantity_factor(r.stock_symbol, r.created_at::date)
            / stock_quantity_factor(r.stock_symbol, l.created_at::date)
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
            * stock_quantity_factor(r.stock_symbol, CURRENT_DATE)
            / stock_quantity_factor(r.stock_symbol, r.created_at::date) AS reward_quantity,
        COALESCE(mb.normalized_quantity, 0) AS reward_normalized_quantity,
        COALESCE(sp.normalized_quantity, 0)
            * stock_quantity_factor(r.stock_symbol, CURRENT_DATE)
            / stock_quantity_factor(r.stock_symbol, r.created_at::date) AS ledger_stock_quantity,
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
