CREATE TABLE request_diagnostics(id TEXT PRIMARY KEY,status BIGINT NOT NULL,reason TEXT NOT NULL,created_at BIGINT NOT NULL);
CREATE INDEX diagnostics_created ON request_diagnostics(created_at);
