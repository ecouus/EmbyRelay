package control

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestOverviewTotalIncludesRowsBeyondTrafficPage(t *testing.T) {
	a, admin := testApp(t)
	tx, err := a.Store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	for i := 0; i < 1200; i++ {
		if _, err = tx.Exec("INSERT INTO traffic_daily VALUES(?,?,?,?)", day, fmt.Sprintf("route-%04d", i), 2, 3); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec("INSERT INTO stream_reservations VALUES(?,?,?)", day, "route-0000", 1); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rr := call(t, a, admin, "GET", "/api/v1/overview", nil)
	var out struct {
		Traffic []Traffic        `json:"traffic"`
		Total   map[string]int64 `json:"traffic_total"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err, rr.Body.String())
	}
	if len(out.Traffic) != 1000 || out.Total["ingress_bytes"] != 2400 || out.Total["egress_bytes"] != 3600-1 {
		t.Fatalf("rows=%d total=%v", len(out.Traffic), out.Total)
	}
}

func TestUnchangedHeartbeatRowsSkipWrites(t *testing.T) {
	a, admin := testApp(t)
	route, _ := configured(t, a, admin, "http://127.0.0.1:8096")
	node := Node{ID: "test-node", Enabled: true}
	if err := a.Store.put("node", node.ID, node, "test"); err != nil {
		t.Fatal(err)
	}
	route.RelayID = node.ID
	if err := a.Store.put("route", route.ID, route, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	hb := Heartbeat{Traffic: []Traffic{{Date: day, RouteID: route.ID, Ingress: 10, Egress: 20}}}
	if err := a.report(node, hb); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.DB.Exec("DELETE FROM traffic_daily"); err != nil {
		t.Fatal(err)
	}
	if err := a.report(node, hb); err != nil {
		t.Fatal(err)
	}
	if rows, _ := a.Store.Traffic(); len(rows) != 0 {
		t.Fatalf("unchanged row was written again: %v", rows)
	}
	hb.Traffic[0].Egress = 50
	if err := a.report(node, hb); err != nil {
		t.Fatal(err)
	}
	if rows, _ := a.Store.Traffic(); len(rows) != 1 || rows[0].Egress != 30 || rows[0].Ingress != 0 {
		t.Fatalf("changed row delta wrong: %v", rows)
	}
}

func TestCleanupKeepsNewestDiagnosticsAndLiveTargets(t *testing.T) {
	s, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, _ := s.DB.Begin()
	for i := 0; i < 1010; i++ {
		// Ten distinct timestamps force the (created_at,id) tie break.
		if _, err = tx.Exec("INSERT INTO request_diagnostics(id,status,reason,created_at) VALUES(?,?,?,?)", fmt.Sprintf("d%04d", i), 500, "x", 1000+i/101); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, q := range []string{
		fmt.Sprintf("INSERT INTO access_tokens(hash,account_id,source_token,user_id,created_at,expires_at,device,client_ip,user_agent) VALUES('live','a','s','u',1,%d,'','','')", now+3600),
		fmt.Sprintf("INSERT INTO access_targets VALUES('t1','a','live','x',%d)", now+3600),
		fmt.Sprintf("INSERT INTO access_targets VALUES('t2','a','live','x',%d)", now-5),
		fmt.Sprintf("INSERT INTO access_targets VALUES('t3','a','gone','x',%d)", now+3600),
		fmt.Sprintf("INSERT INTO stream_sessions(id,route_id,started_at,ended_at,bytes_sent,status) VALUES('old','r',1,%d,0,'done')", now-31*86400),
		fmt.Sprintf("INSERT INTO stream_sessions(id,route_id,started_at,ended_at,bytes_sent,status) VALUES('new','r',1,%d,0,'done')", now-86400),
	} {
		if _, err = s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Cleanup()
	var n int
	var oldest string
	s.DB.QueryRow("SELECT COUNT(*) FROM request_diagnostics").Scan(&n)
	s.DB.QueryRow("SELECT id FROM request_diagnostics ORDER BY created_at,id LIMIT 1").Scan(&oldest)
	if n != 1000 || oldest != "d0010" {
		t.Fatalf("diagnostics kept %d, oldest %s", n, oldest)
	}
	var targets []string
	rows, _ := s.DB.Query("SELECT id FROM access_targets")
	for rows.Next() {
		var id string
		rows.Scan(&id)
		targets = append(targets, id)
	}
	rows.Close()
	if strings.Join(targets, ",") != "t1" {
		t.Fatalf("targets = %v", targets)
	}
	s.DB.QueryRow("SELECT COUNT(*) FROM stream_sessions").Scan(&n)
	if n != 1 {
		t.Fatalf("sessions = %d", n)
	}
}

func TestSessionsMergesLocalAndRemoteNewestFirst(t *testing.T) {
	s, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 1; i <= 5; i++ {
		s.DB.Exec("INSERT INTO stream_sessions(id,route_id,started_at,bytes_sent,status) VALUES(?,?,?,0,'active')", fmt.Sprintf("l%d", i), "r", i*10)
		s.DB.Exec("INSERT INTO remote_sessions VALUES('n',?,?,?)", fmt.Sprintf("r%d", i), fmt.Sprintf(`{"route_id":"r","started_at":%d,"bytes_sent":1,"status":"done"}`, i*10+5), time.Now().Unix())
	}
	got, err := s.Sessions(4)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, x := range got {
		ids = append(ids, x.ID)
	}
	if strings.Join(ids, ",") != "r5,l5,r4,l4" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestStoreStringReportsDriver(t *testing.T) {
	s, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := "SQLite control store"
	if s.DB.Driver == "postgres" {
		want = "PostgreSQL control store"
	}
	if s.String() != want {
		t.Fatal(s.String())
	}
}

func TestResetAdminPasswordIgnoresCase(t *testing.T) {
	s, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Bootstrap("Admin", "test-password-123"); err != nil {
		t.Fatal(err)
	}
	if err = s.ResetAdminPassword("aDMIN", "another-password-456"); err != nil {
		t.Fatal(err)
	}
}
