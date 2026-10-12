-- Historical usage starts its retention period only after maintenance confirms
-- that the cycle is obsolete and the account has no unsettled playback lease.
ALTER TABLE access_usage ADD COLUMN retired_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX access_usage_retired ON access_usage(retired_at,account_id) WHERE retired_at>0;
ALTER TABLE access_leases ADD COLUMN charged_cycle TEXT NOT NULL DEFAULT '';
ALTER TABLE access_leases ADD COLUMN charged_bytes INTEGER NOT NULL DEFAULT 0;
DROP TABLE IF EXISTS relay_reports;
