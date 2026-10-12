package control

import (
	"context"
	"strings"
	"testing"
)

func TestPublicImageScopedLoginMeteringAndNoCredentialFallback(t *testing.T) {
	a, _, account, prefix := subscriptionFixture(t)
	path := "/emby/Items/1/Images/Primary"
	if w := call(t, a, nil, "GET", prefix+path, nil); w.Code == 200 {
		t.Fatal("image allowed before subscription login")
	}
	token := subscriptionLogin(t, a, prefix)
	account, err := get[AccessAccount](a.Store, "access_account", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w := call(t, a, nil, "GET", prefix+path, nil); w.Code != 200 || w.Body.String() != "public-image" {
		t.Fatal("anonymous source image failed or source token borrowed", w.Code, w.Body.String())
	}
	var billed int64
	if err := a.Store.DB.QueryRow("SELECT COALESCE(SUM(bytes),0) FROM access_usage WHERE account_id=?", account.ID).Scan(&billed); err != nil || billed != 2*int64(len("public-image")) {
		t.Fatal("public image not billed", billed, err)
	}
	if w := call(t, a, nil, "GET", prefix+"/emby/Items/2/Images/Primary", nil); w.Code != 401 {
		t.Fatal("private image gained source credentials", w.Code)
	}
	account.Enabled = false
	if err := a.Store.put("access_account", account.ID, account, "test"); err != nil {
		t.Fatal(err)
	}
	if w := call(t, a, nil, "GET", prefix+path, nil); w.Code == 200 {
		t.Fatal("disabled account accessed public image")
	}
	account.Enabled = true
	account.Plan.TrafficBytes = billed
	if err := a.Store.put("access_account", account.ID, account, "test"); err != nil {
		t.Fatal(err)
	}
	if w := call(t, a, nil, "GET", prefix+path, nil); w.Code == 200 {
		t.Fatal("public image bypassed exhausted subscription quota")
	}
	account.Plan.TrafficBytes = 100000
	if err := a.Store.put("access_account", account.ID, account, "test"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/Items/1", "/Videos/1/stream", "/emby/Items/../Images/Primary", "/emby/Items/1/Images/../Users"} {
		if w := call(t, a, nil, "GET", prefix+path, nil); w.Code == 200 {
			t.Fatal("public image authorization widened", path)
		}
	}
	keys, _ := list[Key](a.Store, "key")
	command := AccessCommand{Action: "begin", AccountID: account.ID, KeyID: keys[0].ID, PublicImage: true, Target: path}
	out, err := a.accessCommand(context.Background(), "local", command)
	if err != nil || out.SourceToken != "" || out.DeviceIdentity != nil || out.Lease == "" {
		t.Fatal("public image grant exposes source identity", err)
	}
	for _, action := range []string{"wrap", "resolve", "login", "logout", "info"} {
		forged := command
		forged.Action = action
		if _, err := a.accessCommand(context.Background(), "local", forged); err == nil {
			t.Fatal("public image mode accepted", action)
		}
	}
	forged := command
	forged.Target = "/Videos/1/stream"
	if _, err := a.accessCommand(context.Background(), "local", forged); err == nil {
		t.Fatal("public image grant accepted media path")
	}
	forged = command
	forged.Stream = true
	if _, err := a.accessCommand(context.Background(), "local", forged); err == nil {
		t.Fatal("public image grant accepted media lease")
	}
	command.Action, command.Lease = "finish", out.Lease
	if _, err := a.accessCommand(context.Background(), "local", command); err != nil {
		t.Fatal(err)
	}
	// A valid login under another key must not authorize this entry.
	other := keys[0]
	other.ID, other.Hash = ID(), Hash("other-image-entry")
	if err := a.Store.put("key", other.ID, other, "test"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := a.Store.Snapshot()
	if err := a.Apply(snapshot); err != nil {
		t.Fatal(err)
	}
	otherPrefix := prefix[:strings.LastIndex(prefix, "/")+1] + "other-image-entry"
	if w := call(t, a, nil, "GET", otherPrefix+path, nil); w.Code == 200 {
		t.Fatal("login crossed key boundary")
	}
	if w := call(t, a, nil, "POST", prefix+"/Sessions/Logout?api_key="+token, nil); w.Code != 204 {
		t.Fatal("logout failed", w.Code)
	}
	if w := call(t, a, nil, "GET", prefix+path, nil); w.Code == 200 {
		t.Fatal("public image allowed after final session revoked")
	}
}
