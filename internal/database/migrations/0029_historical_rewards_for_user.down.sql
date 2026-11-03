-- Drop the page function first: it calls the summary function.
DROP FUNCTION IF EXISTS historical_rewards_for_user(INTEGER, DATE, INTEGER, INTEGER);
DROP FUNCTION IF EXISTS historical_unpriced_rewards_for_user(INTEGER, DATE);
