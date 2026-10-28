DROP INDEX IF EXISTS idx_adjustments_idempotency_key;

ALTER TABLE adjustments
    DROP COLUMN original_response,
    DROP COLUMN idempotency_key;
