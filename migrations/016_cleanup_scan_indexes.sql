CREATE INDEX IF NOT EXISTS stream_sessions_ended ON stream_sessions(ended_at);
CREATE INDEX IF NOT EXISTS access_targets_token ON access_targets(token_hash);
CREATE INDEX IF NOT EXISTS remote_sessions_started ON remote_sessions((CAST(json_extract(payload,'$.started_at') AS BIGINT)));
