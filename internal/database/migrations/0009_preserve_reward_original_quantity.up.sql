ALTER TABLE rewards
    ADD COLUMN original_quantity NUMERIC(18,6);

-- Existing rewards have no retained request payload, so their current quantity
-- is the best available value to preserve during the migration.
UPDATE rewards
SET original_quantity = quantity
WHERE original_quantity IS NULL;

ALTER TABLE rewards
    ALTER COLUMN original_quantity SET NOT NULL;

ALTER TABLE rewards
    ADD COLUMN original_response JSONB;

-- Reconstruct the original response for existing rewards from their initial
-- ledger postings. The first INR posting is the grant cash flow; fee postings
-- are immutable initial charges.
UPDATE rewards r
SET original_response = jsonb_build_object(
    'message', 'Reward created successfully',
    'rewardId', r.id,
    'amount_inr', COALESCE((
        SELECT ABS(l.amount)
        FROM ledger l
        WHERE l.reward_id = r.id AND l.entry_type = 'inr_outflow'
        ORDER BY l.id
        LIMIT 1
    ), 0),
    'fees', jsonb_build_object(
        'brokerage', COALESCE((SELECT ABS(l.amount) FROM ledger l WHERE l.reward_id = r.id AND l.entry_type = 'brokerage_fee' ORDER BY l.id LIMIT 1), 0),
        'stt', COALESCE((SELECT ABS(l.amount) FROM ledger l WHERE l.reward_id = r.id AND l.entry_type = 'stt_fee' ORDER BY l.id LIMIT 1), 0),
        'gst', COALESCE((SELECT ABS(l.amount) FROM ledger l WHERE l.reward_id = r.id AND l.entry_type = 'gst_fee' ORDER BY l.id LIMIT 1), 0),
        'total', COALESCE((SELECT SUM(ABS(l.amount)) FROM ledger l WHERE l.reward_id = r.id AND l.entry_type IN ('brokerage_fee', 'stt_fee', 'gst_fee')), 0)
    ),
    'is_reversal', false
);
