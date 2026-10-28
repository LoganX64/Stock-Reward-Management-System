DROP VIEW IF EXISTS reward_ledger_reconciliation CASCADE;
DROP VIEW IF EXISTS user_portfolio CASCADE;
DROP VIEW IF EXISTS today_rewards CASCADE;
DROP VIEW IF EXISTS historical_rewards CASCADE;
DROP VIEW IF EXISTS reward_current_balances CASCADE;
DROP VIEW IF EXISTS reward_quantity_movements CASCADE;
DROP VIEW IF EXISTS stock_price_history_adjusted CASCADE;
DROP VIEW IF EXISTS stock_prices_adjusted CASCADE;

-- Restore the mutable reward balance used by the pre-0014 views.
UPDATE rewards r
SET quantity = GREATEST(
    r.original_quantity + COALESCE(totals.delta_quantity, 0),
    0
)
FROM (
    SELECT reward_id, SUM(delta_quantity) AS delta_quantity
    FROM adjustments
    GROUP BY reward_id
) totals
WHERE totals.reward_id = r.id;

DROP INDEX IF EXISTS idx_adjustments_reward_effective_date;

ALTER TABLE adjustments
    DROP COLUMN effective_date;

DROP FUNCTION IF EXISTS stock_quantity_factor(TEXT, DATE);
DROP FUNCTION IF EXISTS stock_action_factor(TEXT, DATE);

CREATE OR REPLACE VIEW historical_rewards AS
WITH
reward_adjustments AS (
    SELECT
        reward_id,
        SUM(delta_amount) AS total_adjustment_amount
    FROM adjustments
    GROUP BY reward_id
)

SELECT
    r.user_id,
    r.created_at::date                  AS reward_date,
    r.id                                AS reward_event_id,
    COALESCE(lm.new_symbol, r.stock_symbol) AS stock_symbol,

    (r.quantity * COALESCE(em.total_multiplier, 1.0)) AS adjusted_quantity,

    sph.price                           AS price,

    COALESCE(ra.total_adjustment_amount, 0) AS total_adjustment_amount,

    ROUND(
        (r.quantity * COALESCE(em.total_multiplier, 1.0) * sph.price),
        4
    ) AS inr_value

FROM rewards r
LEFT JOIN reward_adjustments ra ON r.id = ra.reward_id
LEFT JOIN LATERAL (
    SELECT
        EXP(SUM(LN(se.ratio_num::numeric / se.ratio_den))) AS total_multiplier
    FROM stock_events se
    WHERE se.stock_symbol = r.stock_symbol
      AND se.effective_date > r.created_at
      AND se.effective_date::date <= r.created_at::date
      AND se.event_type != 'delist'
      AND se.ratio_num > 0
      AND se.ratio_den > 0
) em ON TRUE
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
JOIN stock_price_history sph
    ON UPPER(COALESCE(lm.new_symbol, r.stock_symbol)) = UPPER(sph.stock_symbol)
    AND sph.date = r.created_at::date;

CREATE OR REPLACE VIEW today_rewards AS
SELECT
    r.id AS reward_event_id,
    r.user_id,
    r.stock_symbol,
    r.quantity AS adjusted_quantity,
    sp.price AS current_price,
    COALESCE(SUM(a.delta_amount), 0) AS total_adjustment_amount,
    ROUND(r.quantity * sp.price, 4) AS inr_value
FROM rewards r
LEFT JOIN adjustments a ON r.id = a.reward_id
JOIN stock_prices sp ON UPPER(r.stock_symbol) = UPPER(sp.stock_symbol)
WHERE r.created_at::date = CURRENT_DATE
  AND r.quantity > 0
GROUP BY r.id, r.user_id, r.stock_symbol, r.quantity, sp.price;

CREATE OR REPLACE VIEW user_portfolio AS
WITH reward_adjustments AS (
    SELECT reward_id,
           SUM(delta_amount) AS total_adjustment_amount
    FROM adjustments
    GROUP BY reward_id
)
SELECT
    r.user_id,
    COALESCE(lm.new_symbol, r.stock_symbol) AS stock_symbol,
    SUM(r.quantity * COALESCE(em.total_multiplier, 1)) AS adjusted_quantity,
    sp.price AS current_price,
    ROUND(SUM(r.quantity * COALESCE(em.total_multiplier, 1)) * sp.price, 4) AS inr_value,
    COALESCE(SUM(ra.total_adjustment_amount), 0) AS total_adjustment_amount
FROM rewards r
LEFT JOIN reward_adjustments ra ON r.id = ra.reward_id
LEFT JOIN LATERAL (
    SELECT EXP(SUM(LN(se.ratio_num::numeric / se.ratio_den))) AS total_multiplier
    FROM stock_events se
    WHERE se.stock_symbol = r.stock_symbol
      AND se.effective_date > r.created_at
      AND se.effective_date::date <= CURRENT_DATE
      AND se.event_type != 'delist'
      AND se.ratio_num > 0
      AND se.ratio_den > 0
) em ON TRUE
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
JOIN stock_prices sp ON UPPER(COALESCE(lm.new_symbol, r.stock_symbol)) = UPPER(sp.stock_symbol)
WHERE r.quantity > 0
  AND NOT EXISTS (
      SELECT 1
      FROM stock_events se_delist
      WHERE se_delist.event_type = 'delist'
        AND se_delist.effective_date <= CURRENT_DATE
        AND UPPER(se_delist.stock_symbol) = UPPER(r.stock_symbol)
  )
GROUP BY r.user_id, COALESCE(lm.new_symbol, r.stock_symbol), sp.price;

CREATE OR REPLACE VIEW reward_ledger_reconciliation AS
WITH stock_postings AS (
    SELECT
        l.reward_id,
        COALESCE(SUM(l.quantity) FILTER (WHERE UPPER(l.stock_symbol) = UPPER(r.stock_symbol)), 0) AS quantity,
        COUNT(*) FILTER (WHERE l.stock_symbol IS NULL OR UPPER(l.stock_symbol) <> UPPER(r.stock_symbol)) AS wrong_symbol_count
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
)
SELECT
    r.id AS reward_id,
    r.user_id,
    r.stock_symbol,
    r.quantity AS reward_quantity,
    COALESCE(sp.quantity, 0) AS ledger_stock_quantity,
    COALESCE(sp.wrong_symbol_count, 0) AS wrong_symbol_stock_postings,
    r.quantity = COALESCE(sp.quantity, 0)
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
    (
        r.quantity = COALESCE(sp.quantity, 0)
        AND COALESCE(sp.wrong_symbol_count, 0) = 0
        AND COALESCE(at.amount, 0) = COALESCE(lac.amount, 0)
        AND COALESCE((r.original_response #>> '{fees,brokerage}')::numeric, 0) = -COALESCE(fp.brokerage, 0)
        AND COALESCE((r.original_response #>> '{fees,stt}')::numeric, 0) = -COALESCE(fp.stt, 0)
        AND COALESCE((r.original_response #>> '{fees,gst}')::numeric, 0) = -COALESCE(fp.gst, 0)
        AND COALESCE(initial_cash.amount, 0) = -COALESCE((r.original_response ->> 'amount_inr')::numeric, 0)
    ) AS is_reconciled
FROM rewards r
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
) initial_cash ON TRUE;
