
DO $$ 
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'ledger_entry_type') THEN
        CREATE TYPE ledger_entry_type AS ENUM (
            'stock_units',    
            'inr_outflow',   
            'fees',          
            'brokerage_fee', 
            'stt_fee',       
            'gst_fee'        
        );
    END IF;
END$$;


DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'stock_event_type') THEN
        CREATE TYPE stock_event_type AS ENUM (
            'split',  
            'bonus',  
            'merge',  
            'delist'  
        );
    END IF;
END$$;


CREATE TABLE IF NOT EXISTS users (
    id SERIAL PRIMARY KEY,
    username VARCHAR(50) NOT NULL UNIQUE,
    email VARCHAR(100) NOT NULL UNIQUE,
    created_at TIMESTAMP DEFAULT NOW()
);


CREATE TABLE IF NOT EXISTS rewards (
    id SERIAL PRIMARY KEY,
    user_id INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    stock_symbol VARCHAR(20) NOT NULL,
    quantity NUMERIC(18,6) NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    idempotency_key VARCHAR(64) UNIQUE,
    reward_date DATE GENERATED ALWAYS AS (created_at::DATE) STORED,
    CONSTRAINT unique_user_stock_date UNIQUE (user_id, stock_symbol, reward_date)
);


CREATE TABLE IF NOT EXISTS ledger (
    id SERIAL PRIMARY KEY,
    reward_id INT NOT NULL REFERENCES rewards(id) ON DELETE CASCADE,
    entry_type ledger_entry_type NOT NULL,
    stock_symbol VARCHAR(20),
    quantity NUMERIC(18,6),
    amount NUMERIC(18,4),
    created_at TIMESTAMP DEFAULT NOW()
);


CREATE TABLE IF NOT EXISTS stock_prices (
    id SERIAL PRIMARY KEY,
    stock_symbol VARCHAR(20) NOT NULL UNIQUE,
    price NUMERIC(18,4) NOT NULL,
    updated_at TIMESTAMP DEFAULT NOW()
);


CREATE TABLE IF NOT EXISTS stock_price_history (
    id SERIAL PRIMARY KEY,
    stock_symbol VARCHAR(20) NOT NULL,
    price NUMERIC(18,4) NOT NULL,
    date DATE NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    CONSTRAINT unique_stock_price_date UNIQUE (stock_symbol, date)
);


CREATE TABLE IF NOT EXISTS stock_events (
    id SERIAL PRIMARY KEY,
    stock_symbol VARCHAR(20) NOT NULL,
    event_type stock_event_type NOT NULL,
    ratio_num NUMERIC(18,6) NOT NULL,      
    ratio_den NUMERIC(18,6) NOT NULL,      
    new_symbol VARCHAR(20),                
    effective_date TIMESTAMP NOT NULL,
    CONSTRAINT unique_stock_event UNIQUE (stock_symbol, effective_date, event_type)
);


CREATE TABLE IF NOT EXISTS adjustments (
    id SERIAL PRIMARY KEY,
    reward_id INT NOT NULL REFERENCES rewards(id) ON DELETE CASCADE,
    adjustment_type VARCHAR(64) DEFAULT 'manual', 
    delta_quantity NUMERIC(18,6) NOT NULL,  
    delta_amount NUMERIC(18,4) NOT NULL,    
    reason VARCHAR(255),                    
    created_at TIMESTAMP DEFAULT NOW()
);

-- indexing
CREATE INDEX IF NOT EXISTS idx_rewards_user_date
ON rewards(user_id, reward_date);

CREATE INDEX IF NOT EXISTS idx_adjustments_reward_id
ON adjustments(reward_id);

CREATE INDEX IF NOT EXISTS idx_ledger_reward_id
ON ledger(reward_id);

CREATE INDEX IF NOT EXISTS idx_stock_prices_symbol
ON stock_prices(stock_symbol);



