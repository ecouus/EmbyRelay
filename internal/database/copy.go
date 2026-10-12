package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Copy transfers business data into an initialized empty database. The source
// is read in one snapshot and the target is committed atomically. Callers must
// stop all control servers and Agents before switching the database afterward.
func Copy(ctx context.Context, source, target *DB, migrationID string) (map[string]int64, error) {
	tables := []string{"users", "login_sessions", "objects", "settings", "traffic_daily", "audit_logs", "stream_sessions", "quota_leases", "remote_sessions", "enroll_tokens", "nonces", "relay_reports", "request_diagnostics", "access_usage", "access_tokens", "access_leases", "access_rate", "access_targets", "portal_sessions", "access_devices"}
	options := &sql.TxOptions{ReadOnly: true}
	if source.Driver == "postgres" {
		options.Isolation = sql.LevelRepeatableRead
	}
	from, err := source.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	defer from.Rollback()
	to, err := target.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer to.Rollback()
	if migrationID != "" {
		var previous string
		err = to.QueryRow("SELECT value FROM settings WHERE key='database_copy_complete'").Scan(&previous)
		if err == nil && previous == migrationID {
			return map[string]int64{}, nil
		}
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
	}
	var version int
	if err = from.QueryRow("SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&version); err != nil || version != 10 {
		return nil, fmt.Errorf("source must be upgraded to schema version 10 before copying")
	}
	for _, table := range tables {
		var count int64
		if err = to.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			return nil, err
		}
		if table == "settings" {
			var initial int
			if err = to.QueryRow("SELECT COUNT(*) FROM settings WHERE key='version' AND value='1'").Scan(&initial); err != nil {
				return nil, err
			}
			if count != 1 || initial != 1 {
				return nil, fmt.Errorf("target settings are not empty defaults")
			}
		} else if count != 0 {
			return nil, fmt.Errorf("target table %s is not empty; refusing overwrite", table)
		}
	}
	if _, err = to.Exec("DELETE FROM settings"); err != nil {
		return nil, err
	}
	counts := make(map[string]int64)
	for _, table := range tables {
		rows, e := from.Query("SELECT * FROM " + table)
		if e != nil {
			return nil, e
		}
		columns, e := rows.Columns()
		if e != nil {
			rows.Close()
			return nil, e
		}
		quoted := make([]string, len(columns))
		for i, col := range columns {
			quoted[i] = `"` + strings.ReplaceAll(col, `"`, `""`) + `"`
		}
		query := "INSERT INTO " + table + "(" + strings.Join(quoted, ",") + ") VALUES(" + strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",") + ")"
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if e = rows.Scan(pointers...); e != nil {
				rows.Close()
				return nil, e
			}
			for i, v := range values {
				if raw, ok := v.([]byte); ok {
					values[i] = string(raw)
				}
			}
			if _, e = to.ExecContext(ctx, query, values...); e != nil {
				rows.Close()
				return nil, fmt.Errorf("copy %s: %w", table, e)
			}
			counts[table]++
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		var actual int64
		if e = to.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&actual); e != nil {
			return nil, e
		}
		if actual != counts[table] {
			return nil, fmt.Errorf("row count mismatch for %s", table)
		}
	}
	if target.Driver == "postgres" {
		if _, err = to.Exec(`SELECT setval(pg_get_serial_sequence('audit_logs','id'), COALESCE(MAX(id),1), COUNT(*)>0) FROM audit_logs`); err != nil {
			return nil, err
		}
	}
	if migrationID != "" {
		if _, err = to.Exec("INSERT INTO settings VALUES('database_copy_complete',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", migrationID); err != nil {
			return nil, err
		}
	}
	if err = to.Commit(); err != nil {
		return nil, err
	}
	return counts, nil
}
