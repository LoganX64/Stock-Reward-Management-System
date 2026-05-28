
INSERT INTO users (username, email) 
VALUES ('testuser', 'test@example.com')
ON CONFLICT (username) DO NOTHING;

INSERT INTO stock_prices (stock_symbol, price) 
VALUES 
    ('TCS', 3500.00),
    ('INFY', 1450.00),
    ('RELIANCE', 2500.00)
ON CONFLICT (stock_symbol) 
DO UPDATE SET 
    price = EXCLUDED.price,
    updated_at = NOW();