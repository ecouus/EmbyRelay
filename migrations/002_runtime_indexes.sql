-- Also upgrades databases created by the early V1 integration preview.
CREATE TABLE IF NOT EXISTS quota_leases(date TEXT NOT NULL,route_id TEXT NOT NULL,node_id TEXT NOT NULL,granted INTEGER NOT NULL,reported INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(date,route_id,node_id));
CREATE TABLE IF NOT EXISTS remote_sessions(node_id TEXT NOT NULL,id TEXT NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(node_id,id));
CREATE INDEX IF NOT EXISTS sessions_started ON stream_sessions(started_at DESC);
CREATE INDEX IF NOT EXISTS sessions_status ON stream_sessions(status);
CREATE INDEX IF NOT EXISTS login_expiry ON login_sessions(expires_at);
CREATE INDEX IF NOT EXISTS nonces_created ON nonces(created_at);
CREATE UNIQUE INDEX IF NOT EXISTS route_alias_unique ON objects(json_extract(payload,'$.alias')) WHERE kind='route';
