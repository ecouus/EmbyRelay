package migrations

import (
	"database/sql"
	"embed"
	"fmt"
)

//go:embed *.sql postgres/*.sql
var files embed.FS

const CurrentVersion = 12

// ApplyPostgres has independent SQL files but the same schema version as SQLite.
// A transaction-scoped advisory lock prevents simultaneous startup migrations.
func ApplyPostgres(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT pg_advisory_xact_lock(1735289201)"); err != nil {
		return err
	}
	if _, err = tx.Exec("CREATE TABLE IF NOT EXISTS schema_migrations(version BIGINT PRIMARY KEY)"); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRow("SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&version); err != nil {
		return err
	}
	if version > CurrentVersion {
		return fmt.Errorf("database schema newer than this binary")
	}
	if version == 0 {
		// All application JSON paths are simple $.field paths. Return text;
		// numeric projections use explicit BIGINT casts in shared queries.
		if _, err = tx.Exec(`CREATE FUNCTION json_extract(document TEXT, path TEXT) RETURNS TEXT
   LANGUAGE SQL IMMUTABLE STRICT PARALLEL SAFE
   AS $$ SELECT document::jsonb #>> string_to_array(substring(path FROM 3), '.') $$`); err != nil {
			return err
		}
	}
	for _, m := range []struct {
		version int
		name    string
	}{
		{1, "001_initial.sql"}, {2, "002_runtime_indexes.sql"}, {3, "003_diagnostics.sql"},
		{4, "004_diagnostics_detail.sql"}, {5, "005_diagnostic_context.sql"}, {6, "006_session_clients.sql"},
		{7, "007_access_subscriptions.sql"}, {8, "008_self_service.sql"}, {9, "009_device_identity.sql"}, {10, "010_agent_diagnostics.sql"}, {11, "011_cleanup_indexes.sql"},
		{12, "012_usage_retention.sql"},
	} {
		if version >= m.version {
			continue
		}
		if m.version == 11 {
			var duplicate int
			if err = tx.QueryRow("SELECT COUNT(*) FROM (SELECT lower(json_extract(payload,'$.username')) FROM objects WHERE kind='access_account' GROUP BY lower(json_extract(payload,'$.username')) HAVING COUNT(*) > 1) duplicates").Scan(&duplicate); err != nil {
				return err
			}
			if duplicate > 0 {
				return fmt.Errorf("migration 11: %d duplicate access-account usernames differ only by case; resolve them before upgrading", duplicate)
			}
		}
		raw, e := files.ReadFile("postgres/" + m.name)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(string(raw)); e != nil {
			return fmt.Errorf("postgres migration %d: %w", m.version, e)
		}
		if _, e = tx.Exec("INSERT INTO schema_migrations VALUES($1)", m.version); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// Apply runs migrations transactionally. New migrations must use a new version.
func Apply(db *sql.DB) error {
	if _, e := db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON; CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY)"); e != nil {
		return e
	}
	var version int
	if e := db.QueryRow("SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&version); e != nil {
		return e
	}
	if version > CurrentVersion {
		return fmt.Errorf("database schema newer than this binary")
	}
	for _, migration := range []struct {
		version int
		name    string
	}{{1, "001_initial.sql"}, {2, "002_runtime_indexes.sql"}, {3, "003_diagnostics.sql"}, {4, "004_diagnostics_detail.sql"}, {5, "005_diagnostic_context.sql"}, {6, "006_session_clients.sql"}, {7, "007_access_subscriptions.sql"}, {8, "008_self_service.sql"}, {9, "009_device_identity.sql"}, {10, "010_agent_diagnostics.sql"}, {11, "011_cleanup_indexes.sql"}, {12, "012_usage_retention.sql"}} {
		if version >= migration.version {
			continue
		}
		if migration.version == 11 {
			var duplicate int
			if e := db.QueryRow("SELECT COUNT(*) FROM (SELECT lower(json_extract(payload,'$.username')) FROM objects WHERE kind='access_account' GROUP BY lower(json_extract(payload,'$.username')) HAVING COUNT(*) > 1)").Scan(&duplicate); e != nil {
				return e
			}
			if duplicate > 0 {
				return fmt.Errorf("migration 11: %d duplicate access-account usernames differ only by case; resolve them before upgrading", duplicate)
			}
		}
		b, e := files.ReadFile(migration.name)
		if e != nil {
			return e
		}
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		if _, e = tx.Exec(string(b)); e == nil {
			_, e = tx.Exec("INSERT INTO schema_migrations VALUES(?)", migration.version)
		}
		if e != nil {
			tx.Rollback()
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	return nil
}
