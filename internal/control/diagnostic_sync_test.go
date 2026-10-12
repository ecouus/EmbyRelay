package control

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRemoteDiagnosticValidationAndRetention(t *testing.T) {
	app, _ := testApp(t)
	now := time.Now().Unix()
	makeRecord := func(id string) Diagnostic {
		return Diagnostic{ID: id, Status: 401, Reason: "missing token", CreatedAt: now, Method: "GET", Path: "/r/demo/secret/Items?api_key=token", Source: "local", Details: `{"auth":{"inbound_fields":["Cookie","query:api_key","Authorization: secret"],"outbound_fields":["X-Emby-Token"],"redirect_origins":["https://user:pass@example.com/path?api_key=token"]}}`}
	}
	d, err := cleanRemoteDiagnostic(makeRecord(ID()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Details, "query:api_key") || !strings.Contains(d.Details, "Cookie") || strings.Contains(d.Details, "secret") || strings.Contains(d.Details, "pass") || strings.Contains(d.Path, "secret") {
		t.Fatal(d)
	}
	bad := makeRecord(ID())
	bad.Status = 200
	if _, err = app.receiveDiagnostics(Node{ID: "bad"}, DiagnosticBatch{Records: []Diagnostic{makeRecord(ID()), bad}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	var n int
	app.Store.DB.QueryRow("SELECT COUNT(*) FROM request_diagnostics WHERE node_id='bad'").Scan(&n)
	if n != 0 {
		t.Fatal("partially committed invalid batch")
	}
	other := Node{ID: "other", Name: "Other relay"}
	if _, err = app.receiveDiagnostics(other, DiagnosticBatch{Records: []Diagnostic{makeRecord(ID())}}); err != nil {
		t.Fatal(err)
	}
	if err = app.Store.RecordDiagnostic(makeRecord(ID())); err != nil {
		t.Fatal(err)
	}
	node := Node{ID: "noisy", Name: "Noisy relay"}
	for i := 0; i < 11; i++ {
		batch := DiagnosticBatch{}
		for j := 0; j < 100; j++ {
			batch.Records = append(batch.Records, makeRecord(ID()))
		}
		if _, err = app.receiveDiagnostics(node, batch); err != nil {
			t.Fatal(err)
		}
	}
	app.Store.DB.QueryRow("SELECT COUNT(*) FROM request_diagnostics WHERE node_id=?", node.ID).Scan(&n)
	if n != 1000 {
		t.Fatal(n)
	}
	app.Store.DB.QueryRow("SELECT COUNT(*) FROM request_diagnostics WHERE node_id=?", other.ID).Scan(&n)
	if n != 1 {
		t.Fatal("another node's record evicted")
	}
	app.Store.DB.QueryRow("SELECT COUNT(*) FROM request_diagnostics WHERE node_id='' ").Scan(&n)
	if n != 1 {
		t.Fatal("local record evicted")
	}
}

func TestSubscriptionLoginDiagnosticDoesNotPersistRawErrors(t *testing.T) {
	if got := subscriptionLoginDiagnostic(fmt.Errorf("用户名或密码错误")); !strings.Contains(got, "用户名或密码错误") {
		t.Fatal(got)
	}
	if got := subscriptionLoginDiagnostic(fmt.Errorf("postgres://admin:secret@private.example")); got != "订阅登录被拒绝" {
		t.Fatal("raw error leaked", got)
	}
}
