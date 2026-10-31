-- Market-data provenance.
--
-- Before this migration every price row was effectively anonymous: the API
-- could say when a price was stored but not where it came from, which vendor
-- symbol produced it, or when Stocky actually fetched it. A3 needs that
-- provenance before a real provider is enabled so precedence between sources
-- is explicit and deterministic.
--
-- Rules established here:
--   * source is one of legacy, simulator, yahoo, or nse_bhavcopy;
--   * updated_at (stock_prices) and created_at (stock_price_history) keep their
--     existing meaning as the provider quote time;
--   * fetched_at records when Stocky received and stored the quote;
--   * provider_symbol retains the exact vendor symbol used for the request;
--   * currency is INR and exchange is NSE or BSE for real market data;
--   * legacy/seed rows rank below simulator, which ranks below yahoo, which
--     ranks below nse_bhavcopy when two quotes share the same timestamp.
--
-- Existing price values and timestamps are not rewritten. Existing rows are
-- labelled legacy and their fetched_at is backfilled from the timestamp they
-- already carry so the migration does not invent a new fetch time.

-- ---------------------------------------------------------------------------
-- Provenance columns
-- ---------------------------------------------------------------------------

ALTER TABLE stock_prices
    ADD COLUMN source TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN fetched_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    ADD COLUMN provider_symbol TEXT,
    ADD COLUMN exchange TEXT,
    ADD COLUMN currency TEXT NOT NULL DEFAULT 'INR';

ALTER TABLE stock_price_history
    ADD COLUMN source TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN fetched_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    ADD COLUMN provider_symbol TEXT,
    ADD COLUMN exchange TEXT,
    ADD COLUMN currency TEXT NOT NULL DEFAULT 'INR';

-- Keep legacy fetch times honest. fetched_at is NOT NULL, so fall back to the
-- migration instant only if a legacy row has no timestamp at all.
UPDATE stock_prices
SET fetched_at = COALESCE(updated_at, CURRENT_TIMESTAMP);

UPDATE stock_price_history
SET fetched_at = COALESCE(created_at, CURRENT_TIMESTAMP);

-- ---------------------------------------------------------------------------
-- Provenance invariants
-- ---------------------------------------------------------------------------

ALTER TABLE stock_prices
    ADD CONSTRAINT stock_prices_price_positive
        CHECK (price > 0),
    ADD CONSTRAINT stock_prices_source_not_blank
        CHECK (btrim(source) <> ''),
    ADD CONSTRAINT stock_prices_currency_inr
        CHECK (currency = 'INR'),
    ADD CONSTRAINT stock_prices_exchange_valid
        CHECK (exchange IS NULL OR exchange IN ('NSE', 'BSE'));

ALTER TABLE stock_price_history
    ADD CONSTRAINT stock_price_history_price_positive
        CHECK (price > 0),
    ADD CONSTRAINT stock_price_history_source_not_blank
        CHECK (btrim(source) <> ''),
    ADD CONSTRAINT stock_price_history_currency_inr
        CHECK (currency = 'INR'),
    ADD CONSTRAINT stock_price_history_exchange_valid
        CHECK (exchange IS NULL OR exchange IN ('NSE', 'BSE'));

-- ---------------------------------------------------------------------------
-- Column comments
-- ---------------------------------------------------------------------------

COMMENT ON COLUMN stock_prices.updated_at IS
    'Provider quote time for the current price, not the database write time. API output is normalized to UTC.';
COMMENT ON COLUMN stock_prices.fetched_at IS
    'Instant Stocky received and stored the current quote.';
COMMENT ON COLUMN stock_prices.source IS
    'Quote provenance: legacy, simulator, yahoo, or nse_bhavcopy.';
COMMENT ON COLUMN stock_prices.provider_symbol IS
    'Exact vendor symbol used for the current quote request.';
COMMENT ON COLUMN stock_prices.exchange IS
    'Canonical exchange for the current quote: NSE or BSE.';
COMMENT ON COLUMN stock_prices.currency IS
    'ISO currency code for the current quote; always INR.';

COMMENT ON COLUMN stock_price_history.created_at IS
    'Provider quote time for the historical price, not the database write time. API output is normalized to UTC.';
COMMENT ON COLUMN stock_price_history.fetched_at IS
    'Instant Stocky received and stored the historical quote.';
COMMENT ON COLUMN stock_price_history.source IS
    'Quote provenance: legacy, simulator, yahoo, or nse_bhavcopy.';
COMMENT ON COLUMN stock_price_history.provider_symbol IS
    'Exact vendor symbol used for the historical quote request.';
COMMENT ON COLUMN stock_price_history.exchange IS
    'Canonical exchange for the historical quote: NSE or BSE.';
COMMENT ON COLUMN stock_price_history.currency IS
    'ISO currency code for the historical quote; always INR.';

-- ---------------------------------------------------------------------------
-- Source ranking
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION market_data_source_rank(p_source TEXT)
RETURNS INTEGER
LANGUAGE SQL
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT CASE LOWER(BTRIM(p_source))
        WHEN 'nse_bhavcopy' THEN 3
        WHEN 'yahoo' THEN 2
        WHEN 'simulator' THEN 1
        WHEN 'legacy' THEN 0
        WHEN 'seed' THEN 0
        ELSE 0
    END;
$$;

COMMENT ON FUNCTION market_data_source_rank(TEXT) IS
    'Deterministic precedence for equal-timestamp quotes: nse_bhavcopy(3) > yahoo(2) > simulator(1) > legacy/seed(0).';

-- ---------------------------------------------------------------------------
-- Adjusted price views expose provenance
-- ---------------------------------------------------------------------------

CREATE OR REPLACE VIEW stock_prices_adjusted AS
SELECT
    sp.id,
    sp.stock_symbol,
    sp.price AS raw_price,
    sp.price / stock_action_factor(sp.stock_symbol, market_today()) AS adjusted_price,
    sp.updated_at,
    sp.source,
    sp.fetched_at,
    sp.provider_symbol,
    sp.exchange,
    sp.currency
FROM stock_prices sp;

CREATE OR REPLACE VIEW stock_price_history_adjusted AS
SELECT
    sph.id,
    sph.stock_symbol,
    sph.price AS raw_price,
    sph.price / stock_action_factor(sph.stock_symbol, sph.date) AS adjusted_price,
    sph.date,
    sph.created_at,
    sph.source,
    sph.fetched_at,
    sph.provider_symbol,
    sph.exchange,
    sph.currency
FROM stock_price_history sph;

-- ---------------------------------------------------------------------------
-- Reporting views expose source and fetch time
-- ---------------------------------------------------------------------------

-- historical_rewards_as_of gains two return columns, so the dependent view and
-- the function itself must be dropped before they can be recreated.
DROP VIEW IF EXISTS historical_rewards;
DROP FUNCTION IF EXISTS historical_rewards_as_of(DATE);

CREATE OR REPLACE FUNCTION historical_rewards_as_of(
    p_as_of DATE DEFAULT NULL
)
RETURNS TABLE (
    user_id INTEGER,
    reward_date DATE,
    reward_event_id INTEGER,
    stock_symbol TEXT,
    adjusted_quantity NUMERIC,
    price NUMERIC,
    total_adjustment_amount NUMERIC,
    inr_value NUMERIC,
    price_status TEXT,
    price_as_of TIMESTAMPTZ,
    outcome_type TEXT,
    is_active BOOLEAN,
    delist_effective_date DATE,
    cash_proceeds NUMERIC,
    written_off_quantity NUMERIC,
    price_source TEXT,
    price_fetched_at TIMESTAMPTZ
)
LANGUAGE SQL
STABLE
AS $$
    WITH params AS (
        SELECT
            p_as_of AS as_of,
            COALESCE(p_as_of, market_today()) AS lifecycle_as_of,
            (p_as_of IS NULL) AS grant_mode
    ),
    reward_rows AS (
        SELECT
            r.id AS reward_event_id,
            r.user_id,
            r.stock_symbol AS original_symbol,
            r.original_quantity,
            r.reward_date,
            p.as_of,
            p.lifecycle_as_of,
            p.grant_mode
        FROM rewards r
        CROSS JOIN params p
        WHERE p.as_of IS NULL
           OR r.reward_date <= p.as_of
    ),
    lifecycle AS (
        SELECT
            rr.reward_event_id,
            o.terminal_symbol,
            o.adjusted_quantity AS effective_quantity,
            o.total_adjustment_amount,
            o.outcome_type,
            o.is_active,
            o.delist_effective_date,
            o.cash_proceeds,
            o.written_off_quantity
        FROM reward_rows rr
        CROSS JOIN LATERAL reward_corporate_action_outcome(
            rr.reward_event_id,
            rr.lifecycle_as_of
        ) o
    ),
    valued AS (
        SELECT
            rr.reward_event_id,
            rr.user_id,
            rr.reward_date,
            rr.grant_mode,
            rr.as_of,
            CASE
                WHEN rr.grant_mode THEN rr.original_quantity
                ELSE l.effective_quantity
            END AS valued_quantity,
            CASE
                WHEN rr.grant_mode THEN
                    instrument_terminal_symbol(
                        rr.original_symbol,
                        rr.reward_date,
                        rr.reward_date
                    )
                ELSE l.terminal_symbol
            END AS valued_symbol,
            l.total_adjustment_amount,
            l.outcome_type,
            l.is_active,
            l.delist_effective_date,
            l.cash_proceeds,
            l.written_off_quantity
        FROM reward_rows rr
        JOIN lifecycle l ON l.reward_event_id = rr.reward_event_id
    ),
    priced AS (
        SELECT
            v.*,
            sph.adjusted_price AS matched_price,
            sph.created_at AS matched_price_as_of,
            sph.source AS matched_price_source,
            sph.fetched_at AS matched_price_fetched_at
        FROM valued v
        LEFT JOIN LATERAL (
            SELECT
                h.adjusted_price,
                h.created_at,
                h.source,
                h.fetched_at
            FROM stock_price_history_adjusted h
            WHERE h.stock_symbol = v.valued_symbol
              AND h.date = CASE
                  WHEN v.grant_mode THEN v.reward_date
                  ELSE v.as_of
              END
            ORDER BY h.created_at DESC
            LIMIT 1
        ) sph ON TRUE
    )
    SELECT
        p.user_id,
        p.reward_date,
        p.reward_event_id,
        p.valued_symbol AS stock_symbol,
        ROUND(p.valued_quantity, 6) AS adjusted_quantity,
        CASE
            WHEN p.valued_quantity = 0 THEN NULL::NUMERIC
            ELSE p.matched_price
        END AS price,
        COALESCE(p.total_adjustment_amount, 0) AS total_adjustment_amount,
        CASE
            WHEN p.valued_quantity = 0 THEN 0::NUMERIC
            WHEN p.matched_price IS NULL THEN NULL::NUMERIC
            ELSE ROUND(p.valued_quantity * p.matched_price, 4)
        END AS inr_value,
        CASE
            WHEN p.valued_quantity = 0 THEN 'not_required'
            WHEN p.matched_price IS NULL THEN 'missing'
            ELSE 'available'
        END AS price_status,
        CASE
            WHEN p.valued_quantity = 0 THEN NULL::TIMESTAMPTZ
            ELSE p.matched_price_as_of
        END AS price_as_of,
        p.outcome_type,
        p.is_active,
        p.delist_effective_date,
        p.cash_proceeds,
        p.written_off_quantity,
        CASE
            WHEN p.valued_quantity = 0 THEN NULL::TEXT
            ELSE p.matched_price_source
        END AS price_source,
        CASE
            WHEN p.valued_quantity = 0 THEN NULL::TIMESTAMPTZ
            ELSE p.matched_price_fetched_at
        END AS price_fetched_at
    FROM priced p;
$$;

COMMENT ON FUNCTION historical_rewards_as_of(DATE) IS
    'Historical reward valuation in market dates. NULL as_of = grant-date stock value with current lifecycle annotations; non-NULL = point-in-time quantity, price, delists, and adjustments as of that market date. Includes price provenance.';

CREATE OR REPLACE VIEW historical_rewards AS
SELECT * FROM historical_rewards_as_of(NULL::DATE);

COMMENT ON VIEW historical_rewards IS
    'Grant-date historical valuation with current lifecycle annotations. A later delist never removes a row or changes its grant-date value.';

CREATE OR REPLACE VIEW today_rewards AS
SELECT
    o.reward_id AS reward_event_id,
    o.user_id,
    o.terminal_symbol AS stock_symbol,
    o.adjusted_quantity,
    spa.adjusted_price AS current_price,
    o.total_adjustment_amount,
    CASE
        WHEN spa.adjusted_price IS NULL THEN NULL::NUMERIC
        ELSE ROUND(o.adjusted_quantity * spa.adjusted_price, 4)
    END AS inr_value,
    CASE
        WHEN spa.adjusted_price IS NULL THEN 'missing'
        ELSE 'available'
    END AS price_status,
    spa.updated_at AS price_as_of,
    spa.source AS price_source,
    spa.fetched_at AS price_fetched_at
FROM reward_corporate_action_outcomes o
LEFT JOIN stock_prices_adjusted spa
    ON spa.stock_symbol = o.terminal_symbol
WHERE o.reward_date = market_today()
  AND o.is_active;

CREATE OR REPLACE VIEW user_portfolio AS
SELECT
    o.user_id,
    o.terminal_symbol AS stock_symbol,
    SUM(o.adjusted_quantity) AS adjusted_quantity,
    spa.adjusted_price AS current_price,
    CASE
        WHEN spa.adjusted_price IS NULL THEN NULL::NUMERIC
        ELSE ROUND(SUM(o.adjusted_quantity) * spa.adjusted_price, 4)
    END AS inr_value,
    COALESCE(SUM(o.total_adjustment_amount), 0) AS total_adjustment_amount,
    CASE
        WHEN spa.adjusted_price IS NULL THEN 'missing'
        ELSE 'available'
    END AS price_status,
    spa.updated_at AS price_as_of,
    spa.source AS price_source,
    spa.fetched_at AS price_fetched_at
FROM reward_corporate_action_outcomes o
LEFT JOIN stock_prices_adjusted spa
    ON spa.stock_symbol = o.terminal_symbol
WHERE o.is_active
GROUP BY
    o.user_id,
    o.terminal_symbol,
    spa.adjusted_price,
    spa.updated_at,
    spa.source,
    spa.fetched_at;
