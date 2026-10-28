-- Per-reward audit report. Query rows where is_reconciled is false to find
-- quantity, symbol, grant cash-flow, adjustment cash-flow, or fee mismatches.
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
