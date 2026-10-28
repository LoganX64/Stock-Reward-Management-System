ALTER TABLE adjustments
    ADD COLUMN idempotency_key VARCHAR(64),
    ADD COLUMN original_response JSONB;

CREATE UNIQUE INDEX idx_adjustments_idempotency_key
    ON adjustments (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
