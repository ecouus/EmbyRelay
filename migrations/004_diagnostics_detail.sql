ALTER TABLE request_diagnostics ADD COLUMN method TEXT NOT NULL DEFAULT '';
ALTER TABLE request_diagnostics ADD COLUMN path TEXT NOT NULL DEFAULT '';
ALTER TABLE request_diagnostics ADD COLUMN source TEXT NOT NULL DEFAULT 'local';
