CREATE INDEX IF NOT EXISTS audit_logs_created ON audit_logs(created_at);
CREATE INDEX IF NOT EXISTS access_tokens_expiry ON access_tokens(expires_at);
CREATE INDEX IF NOT EXISTS access_leases_expiry ON access_leases(expires_at);
CREATE INDEX IF NOT EXISTS portal_sessions_expiry ON portal_sessions(expires_at);
CREATE INDEX IF NOT EXISTS access_devices_seen ON access_devices(last_seen);
CREATE INDEX IF NOT EXISTS enroll_tokens_expiry ON enroll_tokens(expires_at);
CREATE INDEX IF NOT EXISTS remote_sessions_updated ON remote_sessions(updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS access_account_username_unique ON objects(lower(json_extract(payload,'$.username'))) WHERE kind='access_account';
CREATE TABLE IF NOT EXISTS totp_replay(user_id TEXT PRIMARY KEY,step INTEGER NOT NULL);
