CREATE TABLE access_usage(account_id TEXT NOT NULL,cycle TEXT NOT NULL,bytes INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(account_id,cycle));
CREATE TABLE access_tokens(hash TEXT PRIMARY KEY,account_id TEXT NOT NULL,source_token TEXT NOT NULL,user_id TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,device TEXT NOT NULL,client_ip TEXT NOT NULL,user_agent TEXT NOT NULL);
CREATE INDEX access_tokens_account ON access_tokens(account_id);
CREATE TABLE access_leases(id TEXT PRIMARY KEY,account_id TEXT NOT NULL,token_hash TEXT NOT NULL,node_id TEXT NOT NULL,expires_at INTEGER NOT NULL,is_stream INTEGER NOT NULL);
CREATE INDEX access_leases_account ON access_leases(account_id,expires_at);
CREATE TABLE access_rate(account_id TEXT PRIMARY KEY,next_ns INTEGER NOT NULL);
CREATE TABLE access_targets(id TEXT PRIMARY KEY,account_id TEXT NOT NULL,token_hash TEXT NOT NULL,target TEXT NOT NULL,expires_at INTEGER NOT NULL);
CREATE INDEX access_targets_expiry ON access_targets(expires_at);
