-- Retention and deletion policy for financial records.
--
-- Before this migration, deleting a user, reward, or stock event cascaded
-- through the financial tables:
--
--     users  -> rewards -> ledger
--                       -> adjustments
--     stock_events -> ledger (corporate-action cash)
--
-- PostgreSQL's ON DELETE CASCADE removes the referencing rows automatically,
-- so a single DELETE could silently erase the only record of a reward, its
-- fees, its adjustments, and its corporate-action cash. A financial system
-- must not lose posted history because a parent row is removed.
--
-- Policy:
--   * Financial records are retained indefinitely. There is no automatic
--     purge job and no cascade from users, rewards, or stock events.
--   * Users are deactivated by setting users.deleted_at instead of being
--     hard-deleted while rewards exist.
--   * A reward with ledger postings or adjustments cannot be hard-deleted.
--   * A stock event with tagged ledger cash postings cannot be hard-deleted;
--     correct the event or archive it instead of deleting it.
--
-- ON DELETE RESTRICT is deliberate rather than NO ACTION: RESTRICT checks
-- immediately and cannot be deferred to the end of the transaction, so a
-- mistaken DELETE fails at the statement that causes it.

-- ---------------------------------------------------------------------------
-- User deactivation
-- ---------------------------------------------------------------------------

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

COMMENT ON COLUMN users.deleted_at IS
    'Set when a user is deactivated. Deactivated users retain all financial history and cannot receive new rewards.';

-- ---------------------------------------------------------------------------
-- Replace destructive cascades with restrictive deletion
-- ---------------------------------------------------------------------------

ALTER TABLE rewards
    DROP CONSTRAINT IF EXISTS rewards_user_id_fkey;

ALTER TABLE rewards
    ADD CONSTRAINT rewards_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT;

ALTER TABLE ledger
    DROP CONSTRAINT IF EXISTS ledger_reward_id_fkey;

ALTER TABLE ledger
    ADD CONSTRAINT ledger_reward_id_fkey
    FOREIGN KEY (reward_id) REFERENCES rewards(id) ON DELETE RESTRICT;

ALTER TABLE adjustments
    DROP CONSTRAINT IF EXISTS adjustments_reward_id_fkey;

ALTER TABLE adjustments
    ADD CONSTRAINT adjustments_reward_id_fkey
    FOREIGN KEY (reward_id) REFERENCES rewards(id) ON DELETE RESTRICT;

ALTER TABLE ledger
    DROP CONSTRAINT IF EXISTS ledger_stock_event_id_fkey;

ALTER TABLE ledger
    ADD CONSTRAINT ledger_stock_event_id_fkey
    FOREIGN KEY (stock_event_id) REFERENCES stock_events(id) ON DELETE RESTRICT;
