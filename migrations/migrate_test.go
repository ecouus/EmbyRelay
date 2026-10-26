package migrations

import (
	"database/sql"
	_ "modernc.org/sqlite"
	"strings"
	"testing"
)

func TestDeviceIdentityUpgradePreservesOldTokens(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"001_initial.sql", "002_runtime_indexes.sql", "003_diagnostics.sql", "004_diagnostics_detail.sql", "005_diagnostic_context.sql", "006_session_clients.sql", "007_access_subscriptions.sql", "008_self_service.sql"} {
		raw, e := files.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec(string(raw)); e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec("INSERT INTO schema_migrations VALUES(?)", i+1); e != nil {
			t.Fatal(e)
		}
	}
	if _, err = db.Exec(`INSERT INTO access_tokens(hash,account_id,source_token,user_id,created_at,expires_at,device,client_ip,user_agent,key_id) VALUES('old-token','account','encrypted-source','user',1,9999999999,'device','127.0.0.1','old-player','key')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO request_diagnostics(id,status,reason,created_at,method,path,source,details) VALUES('old-diagnostic',401,'missing token',1,'GET','/r/test/***/Items','local','{}')"); err != nil {
		t.Fatal(err)
	}
	if err = Apply(db); err != nil {
		t.Fatal(err)
	}
	var identity, upstream, source string
	if err = db.QueryRow("SELECT device_identity,upstream_id,source_token FROM access_tokens WHERE hash='old-token'").Scan(&identity, &upstream, &source); err != nil {
		t.Fatal(err)
	}
	if identity != "" || upstream != "" || source != "encrypted-source" {
		t.Fatal("old token was changed during migration")
	}
	if err = Apply(db); err != nil {
		t.Fatal("repeat migration failed", err)
	}
	var version int
	db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version)
	var nodeID, nodeName string
	var uploaded int
	if err = db.QueryRow("SELECT node_id,node_name,uploaded FROM request_diagnostics WHERE id='old-diagnostic'").Scan(&nodeID, &nodeName, &uploaded); err != nil || nodeID != "" || nodeName != "" || uploaded != 0 {
		t.Fatal("legacy diagnostic migration", err)
	}
	if version != CurrentVersion {
		t.Fatal(version)
	}
}

func TestUsageRetentionUpgradePreservesLegacyAccounting(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	names := []string{"001_initial.sql", "002_runtime_indexes.sql", "003_diagnostics.sql", "004_diagnostics_detail.sql", "005_diagnostic_context.sql", "006_session_clients.sql", "007_access_subscriptions.sql", "008_self_service.sql", "009_device_identity.sql", "010_agent_diagnostics.sql", "011_cleanup_indexes.sql"}
	for i, name := range names {
		raw, e := files.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec(string(raw)); e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec("INSERT INTO schema_migrations VALUES(?)", i+1); e != nil {
			t.Fatal(e)
		}
	}
	for _, query := range []string{
		"INSERT INTO access_usage VALUES('account','old-cycle',123456789)",
		"INSERT INTO access_leases VALUES('legacy-lease','account','hash','node',9999999999,1)",
		"INSERT INTO relay_reports VALUES('node','old-report',1)",
	} {
		if _, err = db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err = Apply(db); err != nil {
		t.Fatal(err)
	}
	var bytes, retired, charged int64
	var cycle string
	if err = db.QueryRow("SELECT bytes,retired_at FROM access_usage WHERE account_id='account'").Scan(&bytes, &retired); err != nil || bytes != 123456789 || retired != 0 {
		t.Fatal("legacy usage was changed or immediately retired", bytes, retired, err)
	}
	if err = db.QueryRow("SELECT charged_cycle,charged_bytes FROM access_leases WHERE id='legacy-lease'").Scan(&cycle, &charged); err != nil || cycle != "" || charged != 0 {
		t.Fatal("legacy lease was removed or given an invented charge", cycle, charged, err)
	}
	if _, err = db.Exec("SELECT * FROM relay_reports"); err == nil {
		t.Fatal("obsolete reports table still exists")
	}
	if err = Apply(db); err != nil {
		t.Fatal("repeat migration", err)
	}
}

func TestCleanupScanIndexesAreIdempotentAndUsed(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err = Apply(db); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("DELETE FROM schema_migrations WHERE version=16"); err != nil {
		t.Fatal(err)
	}
	if err = Apply(db); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stream_sessions_ended", "access_targets_token", "remote_sessions_started"} {
		var n int
		if err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s: %d %v", name, n, err)
		}
	}
	rows, err := db.Query("EXPLAIN QUERY PLAN SELECT id FROM remote_sessions ORDER BY CAST(json_extract(payload,'$.started_at') AS BIGINT) DESC LIMIT 5")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var a, b, c int
		var d string
		rows.Scan(&a, &b, &c, &d)
		plan += d + ";"
	}
	if !strings.Contains(plan, "remote_sessions_started") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("plan = %s", plan)
	}
}
