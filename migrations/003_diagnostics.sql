CREATE TABLE request_diagnostics(id TEXT PRIMARY KEY,status INTEGER NOT NULL,reason TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE INDEX diagnostics_created ON request_diagnostics(created_at);
