CREATE TABLE stream_reservations(date TEXT NOT NULL,route_id TEXT NOT NULL,pending_bytes BIGINT NOT NULL DEFAULT 0,PRIMARY KEY(date,route_id),CHECK(pending_bytes>=0));
ALTER TABLE access_leases ADD COLUMN paced BIGINT NOT NULL DEFAULT 0;
DELETE FROM access_rate;
