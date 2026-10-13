CREATE TABLE stream_reservations(date TEXT NOT NULL,route_id TEXT NOT NULL,pending_bytes INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(date,route_id),CHECK(pending_bytes>=0));
ALTER TABLE access_leases ADD COLUMN paced INTEGER NOT NULL DEFAULT 0;
DELETE FROM access_rate;
