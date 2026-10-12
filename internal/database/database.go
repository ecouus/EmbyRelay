// Package database contains the small SQL dialect boundary shared by the
// control server and the SQLite-only Agent. Business queries remain parameterized.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
	Driver string
}
type Tx struct {
	*sql.Tx
	db *DB
}

func (tx *Tx) ForUpdate() string {
	if tx.db.Driver == "postgres" {
		return " FOR UPDATE"
	}
	return ""
}

func Open(driver, source string) (*DB, error) {
	if driver != "sqlite" && driver != "postgres" {
		return nil, fmt.Errorf("unsupported database driver %q", driver)
	}
	name := driver
	if name == "postgres" {
		name = "pgx"
	} else {
		query := url.Values{}
		query.Add("_pragma", "busy_timeout(5000)")
		query.Add("_pragma", "foreign_keys(ON)")
		source += "?" + query.Encode()
	}
	raw, err := sql.Open(name, source)
	if err != nil {
		return nil, err
	}
	db := &DB{DB: raw, Driver: driver}
	if driver == "sqlite" {
		raw.SetMaxOpenConns(1)
	} else {
		raw.SetMaxOpenConns(16)
		raw.SetMaxIdleConns(4)
	}
	return db, nil
}

// SQL adapts only the scalar MAX expressions used by this application and
// bind markers. JSON extraction is supplied by the PostgreSQL migration.
func (db *DB) SQL(query string) string {
	if db.Driver != "postgres" {
		return query
	}
	query = strings.NewReplacer(
		"MAX(0,granted-reported)", "GREATEST(0,granted-reported)",
		"MAX(quota_leases.granted,excluded.granted)", "GREATEST(quota_leases.granted,excluded.granted)",
		"MAX(reported,?)", "GREATEST(reported,?)",
		"MAX(0,ingress_bytes+?)", "GREATEST(0,ingress_bytes+?)",
		"MAX(0,egress_bytes+?)", "GREATEST(0,egress_bytes+?)",
	).Replace(query)
	var out strings.Builder
	index := 0
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' || ch == '"' {
			quote := ch
			out.WriteByte(ch)
			for i++; i < len(query); i++ {
				out.WriteByte(query[i])
				if query[i] == quote {
					if i+1 < len(query) && query[i+1] == quote {
						i++
						out.WriteByte(query[i])
						continue
					}
					break
				}
			}
		} else if ch == '-' && i+1 < len(query) && query[i+1] == '-' {
			for ; i < len(query) && query[i] != '\n'; i++ {
				out.WriteByte(query[i])
			}
			if i < len(query) {
				out.WriteByte(query[i])
			}
		} else if ch == '/' && i+1 < len(query) && query[i+1] == '*' {
			out.WriteString("/*")
			i += 2
			for ; i < len(query); i++ {
				if query[i] == '*' && i+1 < len(query) && query[i+1] == '/' {
					out.WriteString("*/")
					i++
					break
				}
				out.WriteByte(query[i])
			}
		} else if ch == '?' {
			index++
			fmt.Fprintf(&out, "$%d", index)
		} else {
			out.WriteByte(ch)
		}
	}
	return out.String()
}

func (db *DB) Exec(q string, args ...any) (sql.Result, error) { return db.DB.Exec(db.SQL(q), args...) }
func (db *DB) Query(q string, args ...any) (*sql.Rows, error) { return db.DB.Query(db.SQL(q), args...) }
func (db *DB) QueryRow(q string, args ...any) *sql.Row        { return db.DB.QueryRow(db.SQL(q), args...) }
func (db *DB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return db.DB.ExecContext(ctx, db.SQL(q), args...)
}
func (db *DB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return db.DB.QueryContext(ctx, db.SQL(q), args...)
}
func (db *DB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return db.DB.QueryRowContext(ctx, db.SQL(q), args...)
}
func (db *DB) Begin() (*Tx, error) { return db.BeginTx(context.Background(), nil) }
func (db *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := db.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, db: db}, nil
}
func (tx *Tx) Exec(q string, args ...any) (sql.Result, error) {
	return tx.Tx.Exec(tx.db.SQL(q), args...)
}
func (tx *Tx) Query(q string, args ...any) (*sql.Rows, error) {
	return tx.Tx.Query(tx.db.SQL(q), args...)
}
func (tx *Tx) QueryRow(q string, args ...any) *sql.Row { return tx.Tx.QueryRow(tx.db.SQL(q), args...) }
func (tx *Tx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return tx.Tx.ExecContext(ctx, tx.db.SQL(q), args...)
}
func (tx *Tx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return tx.Tx.QueryRowContext(ctx, tx.db.SQL(q), args...)
}
