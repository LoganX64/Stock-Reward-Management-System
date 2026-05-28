CREATE UNIQUE INDEX IF NOT EXISTS unique_user_stock_date_upper
ON rewards (user_id, UPPER(stock_symbol), reward_date);
