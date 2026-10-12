package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDiagnosticsRedaction(t *testing.T) {
	a, c := testApp(t)
	r := call(t, a, nil, "GET", "/r/missing/private-secret?api_key=private-token", nil)
	if r.Code != 404 {
		t.Fatal(r.Code)
	}
	logs := call(t, a, c, "GET", "/api/v1/diagnostics", nil)
	if !strings.Contains(logs.Body.String(), "路由不存在") || strings.Contains(logs.Body.String(), "private-") {
		t.Fatal(logs.Body.String())
	}
}
func TestRecoverableKeyAndSnapshot(t *testing.T) {
	a, _ := testApp(t)
	cipher, err := a.seal("test-access-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Store.put("key", "k", Key{ID: "k", RouteID: "r", Hash: Hash("test-access-secret"), EncryptedSecret: cipher}, "test"); err != nil {
		t.Fatal(err)
	}
	k, err := get[Key](a.Store, "key", "k")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := a.unseal(k.EncryptedSecret)
	if err != nil || plain != "test-access-secret" {
		t.Fatal("recovery failed")
	}
	snap, err := a.Store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(snap)
	if strings.Contains(string(b), cipher) || strings.Contains(string(b), "test-access-secret") {
		t.Fatal("snapshot leaked recoverable key")
	}
}
