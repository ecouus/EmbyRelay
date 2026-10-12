ALTER TABLE access_tokens ADD COLUMN key_id TEXT NOT NULL DEFAULT '';
UPDATE access_tokens SET key_id=COALESCE((SELECT json_extract(payload,'$.key_id') FROM objects WHERE kind='access_account' AND id=access_tokens.account_id),'');
CREATE INDEX access_tokens_key ON access_tokens(key_id);
CREATE TABLE portal_sessions(hash TEXT PRIMARY KEY,account_id TEXT NOT NULL,expires_at BIGINT NOT NULL);
CREATE INDEX portal_sessions_account ON portal_sessions(account_id);
