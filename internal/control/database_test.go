package control

import (
	"context"
	"embyrelay/internal/database"
	"path/filepath"
	"testing"
)

func TestDatabaseCopyRoundTripAndResume(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	inputs := []string{
		"INSERT INTO users VALUES('u','admin','hash','admin','')",
		"INSERT INTO login_sessions VALUES('token','u',4294967296)",
		`INSERT INTO objects VALUES('route','r','{"alias":"route","upstream_id":"up"}',1)`,
		"INSERT INTO audit_logs VALUES(400,'u','test','r',4294967296)",
		"INSERT INTO traffic_daily VALUES('2026-10-12','r',1099511627776,2199023255552)",
		"INSERT INTO request_diagnostics(id,status,reason,created_at,method,path,source,details,node_id,node_name,uploaded) VALUES('node:diag',401,'missing token',4294967296,'POST','/r/route/***/Users/AuthenticateByName','local','{}','node','Relay B',1)",
		"INSERT INTO access_usage(account_id,cycle,bytes,retired_at) VALUES('a','cycle',4398046511104,4294967296)",
		"INSERT INTO access_leases(id,account_id,token_hash,node_id,expires_at,is_stream,charged_cycle,charged_bytes) VALUES('lease','a','hash','node',4294967296,1,'cycle',10485760)",
		"INSERT INTO access_rate VALUES('a',9000000000000000000)",
		"INSERT INTO stream_sessions(id,route_id,started_at,bytes_sent,status) VALUES('s','r',4294967296,1099511627776,'completed')",
	}
	for _, query := range inputs {
		if _, err = source.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = source.DB.Exec("INSERT INTO settings VALUES('secret',?)", "secret '? $1 ' text"); err != nil {
		t.Fatal(err)
	}
	counts, err := database.Copy(context.Background(), source.DB, target.DB, "test-migration")
	if err != nil {
		t.Fatal(err)
	}
	if counts["audit_logs"] != 1 || counts["users"] != 1 {
		t.Fatal(counts)
	}
	if _, exists := counts["relay_reports"]; exists {
		t.Fatal("obsolete relay reports included in copy")
	}
	if _, err = database.Copy(context.Background(), source.DB, target.DB, "test-migration"); err != nil {
		t.Fatal("resume", err)
	}
	if _, err = database.Copy(context.Background(), source.DB, target.DB, "different-migration"); err == nil {
		t.Fatal("nonempty target overwritten")
	}
	// Generated audit IDs must continue above imported IDs on both engines.
	if _, err = target.DB.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES('u','new','r',1)"); err != nil {
		t.Fatal(err)
	}
	var maximum int64
	if err = target.DB.QueryRow("SELECT MAX(id) FROM audit_logs").Scan(&maximum); err != nil || maximum <= 400 {
		t.Fatal(maximum, err)
	}
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err = database.Copy(context.Background(), target.DB, restored.DB, ""); err != nil {
		t.Fatal("reverse copy", err)
	}
	var diagnosticNode, diagnosticName string
	var uploaded int
	if err = restored.DB.QueryRow("SELECT node_id,node_name,uploaded FROM request_diagnostics WHERE id='node:diag'").Scan(&diagnosticNode, &diagnosticName, &uploaded); err != nil || diagnosticNode != "node" || diagnosticName != "Relay B" || uploaded != 1 {
		t.Fatal("diagnostic provenance lost", err)
	}
	var used, rate, ended int64
	if err = restored.DB.QueryRow("SELECT bytes FROM access_usage").Scan(&used); err != nil || used != 4398046511104 {
		t.Fatal(used, err)
	}
	var retired, charged int64
	var chargedCycle string
	if err = restored.DB.QueryRow("SELECT retired_at FROM access_usage").Scan(&retired); err != nil || retired != 4294967296 {
		t.Fatal("retention metadata lost", retired, err)
	}
	if err = restored.DB.QueryRow("SELECT charged_cycle,charged_bytes FROM access_leases WHERE id='lease'").Scan(&chargedCycle, &charged); err != nil || chargedCycle != "cycle" || charged != 10485760 {
		t.Fatal("settlement metadata lost", chargedCycle, charged, err)
	}
	if err = restored.DB.QueryRow("SELECT next_ns FROM access_rate").Scan(&rate); err != nil || rate != 9000000000000000000 {
		t.Fatal(rate, err)
	}
	if err = restored.DB.QueryRow("SELECT COUNT(*) FROM stream_sessions WHERE ended_at IS NULL").Scan(&ended); err != nil || ended != 1 {
		t.Fatal(ended, err)
	}
	var secret string
	if err = restored.DB.QueryRow("SELECT value FROM settings WHERE key='secret'").Scan(&secret); err != nil || secret != "secret '? $1 ' text" {
		t.Fatal(secret, err)
	}
	if err = source.DB.QueryRow("SELECT COUNT(*) FROM audit_logs").Scan(&maximum); err != nil || maximum != 1 {
		t.Fatal("source changed", maximum, err)
	}
}

func TestConfiguredDatabaseRejectsUnknownDriver(t *testing.T) {
	t.Setenv("RELAY_DB_DRIVER", "unknown")
	if store, err := OpenConfigured(t.TempDir()); err == nil {
		store.Close()
		t.Fatal("unknown driver accepted")
	}
}
