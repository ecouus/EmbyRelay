package migrations

import (
	"database/sql"
	_ "modernc.org/sqlite"
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
	if version != 10 {
		t.Fatal(version)
	}
}
