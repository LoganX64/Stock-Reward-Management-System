-- Restore the pre-0024 cascade behavior. This is intentionally destructive:
-- the down migration is only for development rollback, not for production
-- retention operations.

ALTER TABLE ledger
    DROP CONSTRAINT IF EXISTS ledger_stock_event_id_fkey;

ALTER TABLE ledger
    ADD CONSTRAINT ledger_stock_event_id_fkey
    FOREIGN KEY (stock_event_id) REFERENCES stock_events(id) ON DELETE CASCADE;

ALTER TABLE adjustments
    DROP CONSTRAINT IF EXISTS adjustments_reward_id_fkey;

ALTER TABLE adjustments
    ADD CONSTRAINT adjustments_reward_id_fkey
    FOREIGN KEY (reward_id) REFERENCES rewards(id) ON DELETE CASCADE;

ALTER TABLE ledger
    DROP CONSTRAINT IF EXISTS ledger_reward_id_fkey;

ALTER TABLE ledger
    ADD CONSTRAINT ledger_reward_id_fkey
    FOREIGN KEY (reward_id) REFERENCES rewards(id) ON DELETE CASCADE;

ALTER TABLE rewards
    DROP CONSTRAINT IF EXISTS rewards_user_id_fkey;

ALTER TABLE rewards
    ADD CONSTRAINT rewards_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;

ALTER TABLE users
    DROP COLUMN IF EXISTS deleted_at;
