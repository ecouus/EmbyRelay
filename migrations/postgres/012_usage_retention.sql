-- Keep the same conservative retention and settlement metadata as SQLite.
ALTER TABLE access_usage ADD COLUMN retired_at BIGINT NOT NULL DEFAULT 0;
CREATE INDEX access_usage_retired ON access_usage(retired_at,account_id) WHERE retired_at>0;
ALTER TABLE access_leases ADD COLUMN charged_cycle TEXT NOT NULL DEFAULT '';
ALTER TABLE access_leases ADD COLUMN charged_bytes BIGINT NOT NULL DEFAULT 0;
DROP TABLE IF EXISTS relay_reports;
