-- Revert corporate-action input validation.
--
-- Symbol normalization and delist-ratio normalization are semantically
-- neutral data backfills and are intentionally left in place.

DROP TRIGGER IF EXISTS trg_00_normalize_stock_event ON stock_events;
DROP FUNCTION IF EXISTS normalize_stock_event();

DROP INDEX IF EXISTS unique_stock_event_symbol_type_date_upper;
DROP INDEX IF EXISTS idx_stock_events_upper_new_symbol;

ALTER TABLE stock_events
    DROP CONSTRAINT IF EXISTS stock_events_delist_fields,
    DROP CONSTRAINT IF EXISTS stock_events_merge_fields,
    DROP CONSTRAINT IF EXISTS stock_events_split_bonus_fields,
    DROP CONSTRAINT IF EXISTS stock_events_delist_ratio_unit,
    DROP CONSTRAINT IF EXISTS stock_events_bonus_ratio_forward,
    DROP CONSTRAINT IF EXISTS stock_events_split_ratio_not_unit,
    DROP CONSTRAINT IF EXISTS stock_events_ratio_positive,
    DROP CONSTRAINT IF EXISTS stock_events_new_symbol_not_blank,
    DROP CONSTRAINT IF EXISTS stock_events_symbol_not_blank;

COMMENT ON COLUMN stock_events.stock_symbol IS NULL;
COMMENT ON COLUMN stock_events.new_symbol IS NULL;
