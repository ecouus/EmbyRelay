ALTER TABLE stream_sessions ADD COLUMN client_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE stream_sessions ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE stream_sessions ADD COLUMN client_name TEXT NOT NULL DEFAULT '';
ALTER TABLE stream_sessions ADD COLUMN client_version TEXT NOT NULL DEFAULT '';
