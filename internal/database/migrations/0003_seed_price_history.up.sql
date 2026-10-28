-- Seed the current day's price history from the latest known prices so fresh
-- databases start with a usable history row. Later days are recorded by the
-- price service, and missing dates can be filled with
-- SELECT backfill_price_history('YYYY-MM-DD');
INSERT INTO stock_price_history (stock_symbol, price, date)
SELECT stock_symbol, price, CURRENT_DATE
FROM stock_prices
ON CONFLICT (stock_symbol, date) DO NOTHING;
