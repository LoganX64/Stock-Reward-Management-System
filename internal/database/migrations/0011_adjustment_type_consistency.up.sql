-- Older versions could persist fee-refund quantity changes without a stock
-- ledger posting. Backfill those postings before classifying the legacy rows
-- as manual corrections.
INSERT INTO ledger (reward_id, entry_type, stock_symbol, quantity, amount, created_at)
SELECT a.reward_id, 'stock_units', r.stock_symbol, a.delta_quantity, 0, a.created_at
FROM adjustments a
JOIN rewards r ON r.id = a.reward_id
WHERE a.adjustment_type = 'fee_refund'
  AND a.delta_quantity <> 0;

-- Older versions used "manual" as the database default and did not enforce
-- sign/type semantics. Preserve those records as manual corrections.
UPDATE adjustments
SET adjustment_type = 'manual_correction'
WHERE adjustment_type IS NULL
   OR adjustment_type NOT IN ('reward_reversal', 'fee_refund', 'manual_correction')
   OR (adjustment_type = 'fee_refund' AND (delta_quantity <> 0 OR delta_amount < 0))
   OR (adjustment_type = 'reward_reversal' AND (delta_quantity > 0 OR delta_amount < 0));

ALTER TABLE adjustments
    ALTER COLUMN adjustment_type SET DEFAULT 'manual_correction';

ALTER TABLE adjustments
    ADD CONSTRAINT adjustments_type_and_delta_consistency CHECK (
        adjustment_type IN ('reward_reversal', 'fee_refund', 'manual_correction')
        AND (
            (adjustment_type = 'reward_reversal' AND delta_quantity <= 0 AND delta_amount >= 0)
            OR (adjustment_type = 'fee_refund' AND delta_quantity = 0 AND delta_amount >= 0)
            OR adjustment_type = 'manual_correction'
        )
    );
