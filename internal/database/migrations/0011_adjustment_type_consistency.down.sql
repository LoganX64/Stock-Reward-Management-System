ALTER TABLE adjustments
    DROP CONSTRAINT IF EXISTS adjustments_type_and_delta_consistency;

ALTER TABLE adjustments
    ALTER COLUMN adjustment_type SET DEFAULT 'manual';
