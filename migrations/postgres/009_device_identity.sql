ALTER TABLE access_tokens ADD COLUMN device_identity TEXT NOT NULL DEFAULT '';
ALTER TABLE access_tokens ADD COLUMN upstream_id TEXT NOT NULL DEFAULT '';
CREATE TABLE access_devices(
 id TEXT PRIMARY KEY,
 account_id TEXT NOT NULL,
 upstream_id TEXT NOT NULL,
 identity TEXT NOT NULL,
 last_seen BIGINT NOT NULL,
 last_login BIGINT NOT NULL DEFAULT 0,
 result TEXT NOT NULL DEFAULT '',
 source_status BIGINT NOT NULL DEFAULT 0,
 last_request_status BIGINT NOT NULL DEFAULT 0,
 last_request_at BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX access_devices_scope ON access_devices(account_id,upstream_id,last_seen);
