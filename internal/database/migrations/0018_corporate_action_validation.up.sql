-- Corporate-action input validation.
--
-- Enforces at the database layer:
--   * symbols are trimmed and uppercased;
--   * one event per normalized symbol, event type, and effective date;
--   * positive ratios;
--   * split ratios are not no-op 1:1;
--   * bonus ratios increase the share count;
--   * delist ratios are 1:1 because a delist does not transform quantity;
--   * event-type-specific fields are coherent.
--
-- Existing rows are validated before constraints are added. The only silent
-- data normalizations are trimming/uppercasing symbols and rewriting equal
-- delist ratios to 1/1; both are semantically neutral.

DO $$
DECLARE
    duplicate_events INTEGER;
    blank_symbols INTEGER;
    invalid_ratios INTEGER;
    noop_splits INTEGER;
    reverse_bonuses INTEGER;
    invalid_delists INTEGER;
    self_merges INTEGER;
    incoherent_fields INTEGER;
    merge_cycles INTEGER;
BEGIN
    SELECT COUNT(*) INTO duplicate_events
    FROM (
        SELECT 1
        FROM stock_events
        GROUP BY
            UPPER(BTRIM(stock_symbol, E' \t\n\r')),
            event_type,
            effective_date::date
        HAVING COUNT(*) > 1
    ) duplicates;

    IF duplicate_events > 0 THEN
        RAISE EXCEPTION
            'Cannot add corporate-action validation: % duplicate event group(s) share a normalized symbol, event type, and effective date',
            duplicate_events;
    END IF;

    SELECT COUNT(*) INTO blank_symbols
    FROM stock_events
    WHERE BTRIM(stock_symbol, E' \t\n\r') = ''
       OR (
           new_symbol IS NOT NULL
           AND BTRIM(new_symbol, E' \t\n\r') = ''
       );

    IF blank_symbols > 0 THEN
        RAISE EXCEPTION
            'Cannot add corporate-action validation: % event(s) have a blank stock_symbol or new_symbol',
            blank_symbols;
    END IF;

    SELECT COUNT(*) INTO invalid_ratios
    FROM stock_events
    WHERE ratio_num <= 0 OR ratio_den <= 0;

    IF invalid_ratios > 0 THEN
        RAISE EXCEPTION
            'Cannot add corporate-action validation: % event(s) have non-positive ratios',
            invalid_ratios;
    END IF;

    SELECT COUNT(*) INTO noop_splits
    FROM stock_events
    WHERE event_type = 'split'
      AND ratio_num = ratio_den;

    IF noop_splits > 0 THEN
        RAISE EXCEPTION
            'Cannot add corporate-action validation: % split event(s) use a no-op 1:1 ratio',
            noop_splits;
    END IF;

    SELECT COUNT(*) INTO reverse_bonuses
    FROM stock_events
    WHERE event_type = 'bonus'
      AND ratio_num <= ratio_den;

    IF reverse_bonuses > 0 THEN
        RAISE EXCEPTION
            'Cannot add corporate-action validation: % bonus event(s) do not increase the share count',
            reverse_bonuses;
    END IF;

    SELECT COUNT(*) INTO invalid_delists
    FROM stock_events
    WHERE event_type = 'delist'
      AND ratio_num <> ratio_den;

    IF invalid_delists > 0 THEN
        RAISE EXCEPTION
            'Cannot add corporate-action validation: % delist event(s) do not use a 1:1 ratio',
            invalid_delists;
    END IF;

    SELECT COUNT(*) INTO self_merges
    FROM stock_events
    WHERE event_type = 'merge'
      AND new_symbol IS NOT NULL
      AND UPPER(BTRIM(new_symbol, E' \t\n\r'))
          = UPPER(BTRIM(stock_symbol, E' \t\n\r'));

    IF self_merges > 0 THEN
        RAISE EXCEPTION
            'Cannot add corporate-action validation: % merge event(s) target themselves',
            self_merges;
    END IF;

    SELECT COUNT(*) INTO incoherent_fields
    FROM stock_events
    WHERE
        (
            event_type IN ('split', 'bonus')
            AND (
                new_symbol IS NOT NULL
                OR cash_per_unit IS NOT NULL
                OR delist_outcome IS NOT NULL
            )
        )
        OR (
            event_type = 'merge'
            AND (
                new_symbol IS NULL
                OR BTRIM(new_symbol, E' \t\n\r') = ''
                OR delist_outcome IS NOT NULL
            )
        )
        OR (
            event_type = 'delist'
            AND (
                new_symbol IS NOT NULL
                OR delist_outcome IS NULL
            )
        );

    IF incoherent_fields > 0 THEN
        RAISE EXCEPTION
            'Cannot add corporate-action validation: % event(s) have fields inconsistent with their event type',
            incoherent_fields;
    END IF;

    WITH RECURSIVE edges AS (
        SELECT
            UPPER(BTRIM(stock_symbol, E' \t\n\r')) AS predecessor,
            UPPER(BTRIM(new_symbol, E' \t\n\r')) AS successor
        FROM stock_events
        WHERE event_type = 'merge'
          AND new_symbol IS NOT NULL
          AND BTRIM(new_symbol, E' \t\n\r') <> ''
    ),
    walk AS (
        SELECT
            e.predecessor AS start_symbol,
            e.successor AS symbol,
            ARRAY[e.predecessor, e.successor] AS path,
            1 AS depth
        FROM edges e
        UNION ALL
        SELECT
            w.start_symbol,
            e.successor,
            w.path || e.successor,
            w.depth + 1
        FROM walk w
        JOIN edges e ON e.predecessor = w.symbol
        WHERE NOT (e.successor = ANY(w.path))
          AND w.depth < 100
    )
    SELECT COUNT(DISTINCT start_symbol)
    INTO merge_cycles
    FROM walk
    WHERE symbol = start_symbol;

    IF merge_cycles > 0 THEN
        RAISE EXCEPTION
            'Cannot add corporate-action validation: % merger cycle(s) exist',
            merge_cycles;
    END IF;
END;
$$;

-- Semantically neutral backfill: delists do not transform quantity, so an
-- equal ratio is rewritten to the canonical 1/1 representation.
UPDATE stock_events
SET ratio_num = 1,
    ratio_den = 1
WHERE event_type = 'delist'
  AND ratio_num = ratio_den
  AND (ratio_num <> 1 OR ratio_den <> 1);

UPDATE stock_events
SET stock_symbol = UPPER(BTRIM(stock_symbol, E' \t\n\r')),
    new_symbol = CASE
        WHEN new_symbol IS NULL THEN NULL
        ELSE UPPER(BTRIM(new_symbol, E' \t\n\r'))
    END
WHERE stock_symbol <> UPPER(BTRIM(stock_symbol, E' \t\n\r'))
   OR (
       new_symbol IS NOT NULL
       AND new_symbol <> UPPER(BTRIM(new_symbol, E' \t\n\r'))
   );

ALTER TABLE stock_events
    ADD CONSTRAINT stock_events_symbol_not_blank
        CHECK (BTRIM(stock_symbol, E' \t\n\r') <> ''),
    ADD CONSTRAINT stock_events_new_symbol_not_blank
        CHECK (
            new_symbol IS NULL
            OR BTRIM(new_symbol, E' \t\n\r') <> ''
        ),
    ADD CONSTRAINT stock_events_ratio_positive
        CHECK (ratio_num > 0 AND ratio_den > 0),
    ADD CONSTRAINT stock_events_split_ratio_not_unit
        CHECK (event_type <> 'split' OR ratio_num <> ratio_den),
    ADD CONSTRAINT stock_events_bonus_ratio_forward
        CHECK (event_type <> 'bonus' OR ratio_num > ratio_den),
    ADD CONSTRAINT stock_events_delist_ratio_unit
        CHECK (
            event_type <> 'delist'
            OR (ratio_num = 1 AND ratio_den = 1)
        ),
    ADD CONSTRAINT stock_events_split_bonus_fields
        CHECK (
            event_type NOT IN ('split', 'bonus')
            OR (
                new_symbol IS NULL
                AND cash_per_unit IS NULL
                AND delist_outcome IS NULL
            )
        ),
    ADD CONSTRAINT stock_events_merge_fields
        CHECK (
            event_type <> 'merge'
            OR (
                new_symbol IS NOT NULL
                AND BTRIM(new_symbol, E' \t\n\r') <> ''
                AND delist_outcome IS NULL
            )
        ),
    ADD CONSTRAINT stock_events_delist_fields
        CHECK (
            event_type <> 'delist'
            OR (
                new_symbol IS NULL
                AND delist_outcome IS NOT NULL
            )
        );

-- Case, whitespace, and same-day duplicate prevention. The existing exact
-- uniqueness constraint remains in place.
CREATE UNIQUE INDEX IF NOT EXISTS unique_stock_event_symbol_type_date_upper
    ON stock_events (
        UPPER(BTRIM(stock_symbol, E' \t\n\r')),
        event_type,
        ((effective_date)::date)
    );

-- Supports successor-symbol lookups in lineage validation.
CREATE INDEX IF NOT EXISTS idx_stock_events_upper_new_symbol
    ON stock_events (UPPER(new_symbol))
    WHERE new_symbol IS NOT NULL;

-- Runs before the existing validation triggers (PostgreSQL fires same-timing
-- triggers in name order), so later checks see normalized symbols.
CREATE OR REPLACE FUNCTION normalize_stock_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.stock_symbol := UPPER(BTRIM(NEW.stock_symbol, E' \t\n\r'));

    IF NEW.new_symbol IS NOT NULL THEN
        NEW.new_symbol := UPPER(BTRIM(NEW.new_symbol, E' \t\n\r'));
    END IF;

    IF NEW.stock_symbol = '' THEN
        RAISE EXCEPTION 'stock event requires a non-blank stock_symbol';
    END IF;

    IF NEW.ratio_num <= 0 OR NEW.ratio_den <= 0 THEN
        RAISE EXCEPTION
            '% event for % requires positive ratios',
            NEW.event_type, NEW.stock_symbol;
    END IF;

    IF NEW.event_type = 'split' THEN
        IF NEW.ratio_num = NEW.ratio_den THEN
            RAISE EXCEPTION
                'split event for % requires a ratio other than 1:1',
                NEW.stock_symbol;
        END IF;
    ELSIF NEW.event_type = 'bonus' THEN
        IF NEW.ratio_num <= NEW.ratio_den THEN
            RAISE EXCEPTION
                'bonus event for % requires a ratio greater than 1:1',
                NEW.stock_symbol;
        END IF;
    ELSIF NEW.event_type = 'merge' THEN
        IF NEW.new_symbol IS NULL OR NEW.new_symbol = '' THEN
            RAISE EXCEPTION
                'merge event for % requires new_symbol',
                NEW.stock_symbol;
        END IF;
    ELSIF NEW.event_type = 'delist' THEN
        IF NEW.ratio_num <> NEW.ratio_den THEN
            RAISE EXCEPTION
                'delist event for % must use a 1:1 ratio',
                NEW.stock_symbol;
        END IF;

        NEW.ratio_num := 1;
        NEW.ratio_den := 1;
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_00_normalize_stock_event ON stock_events;

CREATE TRIGGER trg_00_normalize_stock_event
BEFORE INSERT OR UPDATE ON stock_events
FOR EACH ROW
EXECUTE FUNCTION normalize_stock_event();

COMMENT ON COLUMN stock_events.stock_symbol IS
    'Normalized uppercase, trimmed instrument symbol';
COMMENT ON COLUMN stock_events.new_symbol IS
    'Normalized uppercase, trimmed merger successor symbol';
COMMENT ON FUNCTION normalize_stock_event() IS
    'Normalizes stock-event symbols and enforces ratio and event-field rules before row validation';
