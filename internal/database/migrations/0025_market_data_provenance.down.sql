-- Revert market-data provenance to the Phase 3.16 (migration 0024) schema.
--
-- Price values and quote timestamps are preserved. Only provenance columns,
-- their constraints/comments, the source-rank function, and the extended
-- reporting views are removed. The adjusted and reporting views are recreated
-- with their exact pre-0025 definitions because CREATE OR REPLACE VIEW cannot
-- remove columns from the end of an existing view.

-- ---------------------------------------------------------------------------
-- Drop provenance-aware objects
-- ---------------------------------------------------------------------------

DROP VIEW IF EXISTS historical_rewards;
DROP FUNCTION IF EXISTS historical_rewards_as_of(DATE);
DROP VIEW IF EXISTS today_rewards;
DROP VIEW IF EXISTS user_portfolio;
DROP VIEW IF EXISTS stock_prices_adjusted;
DROP VIEW IF EXISTS stock_price_history_adjusted;

-- ---------------------------------------------------------------------------
-- Restore pre-0025 adjusted price views (migration 0022)
-- ---------------------------------------------------------------------------

CREATE OR REPLACE VIEW stock_prices_adjusted AS
SELECT
    sp.id,
    sp.stock_symbol,
    sp.price AS raw_price,
    sp.price / stock_action_factor(sp.stock_symbol, market_today()) AS adjusted_price,
    sp.updated_at
FROM stock_prices sp;

CREATE OR REPLACE VIEW stock_price_history_adjusted AS
SELECT
    sph.id,
    sph.stock_symbol,
    sph.price AS raw_price,
    sph.price / stock_action_factor(sph.stock_symbol, sph.date) AS adjusted_price,
    sph.date,
    sph.created_at
FROM stock_price_history sph;

-- ---------------------------------------------------------------------------
-- Restore pre-0025 reporting views and function (migration 0023)
-- ---------------------------------------------------------------------------

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
    written_off_quantity NUMERIC
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
            sph.created_at AS matched_price_as_of
        FROM valued v
        LEFT JOIN LATERAL (
            SELECT h.adjusted_price, h.created_at
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
        p.written_off_quantity
    FROM priced p;
$$;

COMMENT ON FUNCTION historical_rewards_as_of(DATE) IS
    'Historical reward valuation in market dates. NULL as_of = grant-date stock value with current lifecycle annotations; non-NULL = point-in-time quantity, price, delists, and adjustments as of that market date.';

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
    spa.updated_at AS price_as_of
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
    spa.updated_at AS price_as_of
FROM reward_corporate_action_outcomes o
LEFT JOIN stock_prices_adjusted spa
    ON spa.stock_symbol = o.terminal_symbol
WHERE o.is_active
GROUP BY
    o.user_id,
    o.terminal_symbol,
    spa.adjusted_price,
    spa.updated_at;

-- ---------------------------------------------------------------------------
-- Drop provenance schema
-- ---------------------------------------------------------------------------

ALTER TABLE stock_prices
    DROP CONSTRAINT IF EXISTS stock_prices_price_positive,
    DROP CONSTRAINT IF EXISTS stock_prices_source_not_blank,
    DROP CONSTRAINT IF EXISTS stock_prices_currency_inr,
    DROP CONSTRAINT IF EXISTS stock_prices_exchange_valid,
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS fetched_at,
    DROP COLUMN IF EXISTS provider_symbol,
    DROP COLUMN IF EXISTS exchange,
    DROP COLUMN IF EXISTS currency;

ALTER TABLE stock_price_history
    DROP CONSTRAINT IF EXISTS stock_price_history_price_positive,
    DROP CONSTRAINT IF EXISTS stock_price_history_source_not_blank,
    DROP CONSTRAINT IF EXISTS stock_price_history_currency_inr,
    DROP CONSTRAINT IF EXISTS stock_price_history_exchange_valid,
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS fetched_at,
    DROP COLUMN IF EXISTS provider_symbol,
    DROP COLUMN IF EXISTS exchange,
    DROP COLUMN IF EXISTS currency;

DROP FUNCTION IF EXISTS market_data_source_rank(TEXT);

COMMENT ON COLUMN stock_prices.updated_at IS
    'Absolute last-price update instant; API output is normalized to UTC.';
COMMENT ON COLUMN stock_price_history.created_at IS NULL;
