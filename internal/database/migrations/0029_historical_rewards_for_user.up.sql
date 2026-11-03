-- User-scoped historical read path.
--
-- Phase 3 of the read-path projection plan replaces the API's all-rewards
-- historical_rewards_as_of() calls with a user-scoped, page-first function.
-- historical_rewards_as_of() remains in place for compatibility and for the
-- historical_rewards view.

-- Full-set unpriced summary for one user. This is intentionally separate from
-- the page function so an empty page can still report valuation completeness.
-- Grant-date mode only needs the original reward quantity and the terminal
-- symbol at the grant date; as-of mode evaluates the point-in-time lifecycle.
CREATE OR REPLACE FUNCTION historical_unpriced_rewards_for_user(
    p_user_id INTEGER,
    p_as_of   DATE DEFAULT NULL
)
RETURNS TABLE (
    valuation_date DATE,
    as_of_allowed BOOLEAN,
    unpriced_rewards BIGINT,
    unpriced_symbols TEXT[]
)
LANGUAGE SQL
STABLE
AS $$
    WITH params AS (
        SELECT
            p_as_of AS as_of,
            (p_as_of IS NULL) AS grant_mode,
            (p_as_of IS NULL OR p_as_of <= market_today()) AS allowed,
            COALESCE(p_as_of, market_today()) AS valuation_date
    ),
    full_valued AS (
        SELECT
            r.original_quantity AS valued_quantity,
            instrument_terminal_symbol(
                r.stock_symbol,
                r.reward_date,
                r.reward_date
            ) AS valued_symbol,
            r.reward_date AS price_date
        FROM rewards r
        CROSS JOIN params p
        WHERE p.grant_mode
          AND p.allowed
          AND r.user_id = p_user_id
          AND r.reward_date <= market_today() - 1

        UNION ALL

        SELECT
            o.adjusted_quantity AS valued_quantity,
            o.terminal_symbol AS valued_symbol,
            p.as_of AS price_date
        FROM rewards r
        CROSS JOIN params p
        CROSS JOIN LATERAL reward_corporate_action_outcome(
            r.id,
            p.as_of
        ) o
        WHERE NOT p.grant_mode
          AND p.allowed
          AND r.user_id = p_user_id
          AND r.reward_date <= p.as_of
    ),
    full_priced AS (
        SELECT
            fv.valued_quantity,
            fv.valued_symbol,
            EXISTS (
                SELECT 1
                FROM stock_price_history_adjusted h
                WHERE h.stock_symbol = fv.valued_symbol
                  AND h.date = fv.price_date
            ) AS has_price
        FROM full_valued fv
    ),
    summary AS (
        SELECT
            COUNT(*) FILTER (
                WHERE valued_quantity IS DISTINCT FROM 0
                  AND NOT has_price
            ) AS unpriced_rewards,
            COALESCE(
                array_agg(DISTINCT valued_symbol ORDER BY valued_symbol)
                    FILTER (
                        WHERE valued_quantity IS DISTINCT FROM 0
                          AND NOT has_price
                    ),
                '{}'::TEXT[]
            ) AS unpriced_symbols
        FROM full_priced
    )
    SELECT
        p.valuation_date,
        p.allowed AS as_of_allowed,
        s.unpriced_rewards,
        s.unpriced_symbols
    FROM params p
    CROSS JOIN summary s;
$$;

COMMENT ON FUNCTION historical_unpriced_rewards_for_user(INTEGER, DATE) IS
    'Full-set unpriced historical reward summary for one user at grant date (NULL) or as of a market date.';

-- Page-first historical valuation for one user. The page CTE applies the user
-- and date filters before any lifecycle work, so the expensive per-reward
-- outcome function runs only for the requested page. The summary columns are
-- repeated on every row; when the page is empty the LEFT JOIN yields one row
-- with NULL reward columns and the full-set summary.
CREATE OR REPLACE FUNCTION historical_rewards_for_user(
    p_user_id INTEGER,
    p_as_of   DATE DEFAULT NULL,
    p_limit   INTEGER DEFAULT NULL,
    p_offset  INTEGER DEFAULT 0
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
    price_fetched_at TIMESTAMPTZ,
    valuation_date DATE,
    as_of_allowed BOOLEAN,
    unpriced_rewards BIGINT,
    unpriced_symbols TEXT[]
)
LANGUAGE SQL
STABLE
AS $$
    WITH params AS (
        SELECT
            p_as_of AS as_of,
            COALESCE(p_as_of, market_today()) AS lifecycle_as_of,
            (p_as_of IS NULL) AS grant_mode,
            (p_as_of IS NULL OR p_as_of <= market_today()) AS allowed
    ),
    summary AS (
        SELECT *
        FROM historical_unpriced_rewards_for_user(p_user_id, p_as_of)
    ),
    page AS (
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
        WHERE r.user_id = p_user_id
          AND p.allowed
          AND r.reward_date <= COALESCE(p.as_of, market_today() - 1)
        ORDER BY r.reward_date DESC, r.id DESC
        LIMIT p_limit OFFSET p_offset
    ),
    lifecycle AS (
        SELECT
            pg.reward_event_id,
            o.terminal_symbol,
            o.adjusted_quantity AS effective_quantity,
            o.total_adjustment_amount,
            o.outcome_type,
            o.is_active,
            o.delist_effective_date,
            o.cash_proceeds,
            o.written_off_quantity
        FROM page pg
        CROSS JOIN LATERAL reward_corporate_action_outcome(
            pg.reward_event_id,
            pg.lifecycle_as_of
        ) o
    ),
    valued AS (
        SELECT
            pg.reward_event_id,
            pg.user_id,
            pg.reward_date,
            pg.grant_mode,
            pg.as_of,
            CASE
                WHEN pg.grant_mode THEN pg.original_quantity
                ELSE l.effective_quantity
            END AS valued_quantity,
            CASE
                WHEN pg.grant_mode THEN
                    instrument_terminal_symbol(
                        pg.original_symbol,
                        pg.reward_date,
                        pg.reward_date
                    )
                ELSE l.terminal_symbol
            END AS valued_symbol,
            l.total_adjustment_amount,
            l.outcome_type,
            l.is_active,
            l.delist_effective_date,
            l.cash_proceeds,
            l.written_off_quantity
        FROM page pg
        JOIN lifecycle l ON l.reward_event_id = pg.reward_event_id
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
        pv.user_id,
        pv.reward_date,
        pv.reward_event_id,
        pv.valued_symbol AS stock_symbol,
        ROUND(pv.valued_quantity, 6) AS adjusted_quantity,
        CASE
            WHEN pv.valued_quantity = 0 THEN NULL::NUMERIC
            ELSE pv.matched_price
        END AS price,
        COALESCE(pv.total_adjustment_amount, 0) AS total_adjustment_amount,
        CASE
            WHEN pv.valued_quantity = 0 THEN 0::NUMERIC
            WHEN pv.matched_price IS NULL THEN NULL::NUMERIC
            ELSE ROUND(pv.valued_quantity * pv.matched_price, 4)
        END AS inr_value,
        CASE
            WHEN pv.valued_quantity = 0 THEN 'not_required'
            WHEN pv.matched_price IS NULL THEN 'missing'
            ELSE 'available'
        END AS price_status,
        CASE
            WHEN pv.valued_quantity = 0 THEN NULL::TIMESTAMPTZ
            ELSE pv.matched_price_as_of
        END AS price_as_of,
        pv.outcome_type,
        pv.is_active,
        pv.delist_effective_date,
        pv.cash_proceeds,
        pv.written_off_quantity,
        CASE
            WHEN pv.valued_quantity = 0 THEN NULL::TEXT
            ELSE pv.matched_price_source
        END AS price_source,
        CASE
            WHEN pv.valued_quantity = 0 THEN NULL::TIMESTAMPTZ
            ELSE pv.matched_price_fetched_at
        END AS price_fetched_at,
        s.valuation_date,
        s.as_of_allowed,
        s.unpriced_rewards,
        s.unpriced_symbols
    FROM summary s
    LEFT JOIN priced pv ON TRUE
    ORDER BY pv.reward_date DESC NULLS LAST,
             pv.reward_event_id DESC NULLS LAST;
$$;

COMMENT ON FUNCTION historical_rewards_for_user(INTEGER, DATE, INTEGER, INTEGER) IS
    'Page-first historical reward valuation for one user. NULL as_of = grant-date mode; non-NULL = point-in-time mode. Includes the full-set unpriced summary so an empty page still reports valuation completeness.';
