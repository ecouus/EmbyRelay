ALTER TABLE request_diagnostics ADD COLUMN node_id TEXT NOT NULL DEFAULT '';
ALTER TABLE request_diagnostics ADD COLUMN node_name TEXT NOT NULL DEFAULT '';
ALTER TABLE request_diagnostics ADD COLUMN uploaded INTEGER NOT NULL DEFAULT 0;
CREATE INDEX diagnostics_node_created ON request_diagnostics(node_id,created_at,id);
CREATE INDEX diagnostics_upload ON request_diagnostics(node_id,uploaded,created_at,id);
