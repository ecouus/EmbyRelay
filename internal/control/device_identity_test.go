package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeviceIdentityModesAndValidation(t *testing.T) {
	a, _ := testApp(t)
	account := AccessAccount{ID: "account-a"}
	upstream := Upstream{ID: "source-a"}
	in := AccessCommand{Device: "phone-1", DeviceName: "Phone", ClientName: "RealPlayer", ClientVersion: "2.3"}
	legacy, _, err := a.resolveDeviceIdentity(account, upstream, in)
	if err != nil || legacy.Applied.ID != account.ID+Hash(in.Device)[:16] || legacy.Scope != "default" {
		t.Fatal("legacy login identity changed", legacy, err)
	}
	upstream.DevicePolicy = &DevicePolicy{Mode: "original"}
	original, _, err := a.resolveDeviceIdentity(account, upstream, in)
	if err != nil || original.Applied != original.Original {
		t.Fatal("original identity lost", original, err)
	}
	upstream.DevicePolicy = &DevicePolicy{Mode: "stable"}
	stable, _, _ := a.resolveDeviceIdentity(account, upstream, in)
	repeated, _, _ := a.resolveDeviceIdentity(account, upstream, in)
	if stable.Applied.ID != repeated.Applied.ID || stable.Applied.Client != "RealPlayer" {
		t.Fatal("unstable identity or client replaced")
	}
	otherAccount, _, _ := a.resolveDeviceIdentity(AccessAccount{ID: "account-b"}, upstream, in)
	otherSource := upstream
	otherSource.ID = "source-b"
	otherUpstream, _, _ := a.resolveDeviceIdentity(account, otherSource, in)
	if stable.Applied.ID == otherAccount.Applied.ID || stable.Applied.ID == otherUpstream.Applied.ID {
		t.Fatal("scope not isolated")
	}
	upstream.DevicePolicy = &DevicePolicy{Mode: "custom", DeviceFields: DeviceFields{ID: "living-room", Name: "TV"}}
	custom, _, _ := a.resolveDeviceIdentity(account, upstream, in)
	second := in
	second.Device = "phone-2"
	shared, _, _ := a.resolveDeviceIdentity(account, upstream, second)
	if custom.Applied.ID != shared.Applied.ID || custom.RecordID == shared.RecordID || custom.Applied.Name != "TV" {
		t.Fatal("custom mapping/local separation failed")
	}
	upstream.DevicePolicy = &DevicePolicy{Mode: "grouped", Groups: []DeviceGroup{{Key: "room", DeviceFields: DeviceFields{Name: "Room"}, Devices: []string{"phone-1", "phone-2"}}}, Exceptions: []DeviceException{{Device: "phone-2", Mode: "original"}}}
	grouped, _, _ := a.resolveDeviceIdentity(account, upstream, in)
	exception, _, _ := a.resolveDeviceIdentity(account, upstream, second)
	third := in
	third.Device = "laptop"
	unassigned, _, _ := a.resolveDeviceIdentity(account, upstream, third)
	if grouped.Mode != "grouped" || grouped.Group != "room" || grouped.Applied.Name != "Room" || exception.Applied.ID != "phone-2" || unassigned.Mode != "stable" || unassigned.Applied.ID == grouped.Applied.ID {
		t.Fatal("group/exception/fallback failed")
	}
	for _, p := range []*DevicePolicy{
		{Mode: "wrong"}, {Mode: "custom", DeviceFields: DeviceFields{Name: "bad\r\nheader"}},
		{Mode: "custom", DeviceFields: DeviceFields{Client: `quoted"client`}},
		{Mode: "grouped", Groups: []DeviceGroup{{Key: "room", Devices: []string{"same"}}, {Key: "other", Devices: []string{"same"}}}},
		{Mode: "stable", Exceptions: []DeviceException{{Device: "phone", Mode: "grouped", Group: "missing"}}},
	} {
		if validateDevicePolicy(p) == nil {
			t.Fatal("invalid policy accepted", p)
		}
	}
}

func TestDeviceRequestExtractionAndRewriting(t *testing.T) {
	r := httptest.NewRequest("POST", "http://relay/Sessions/Playing/Progress?DeviceId=conflict&X-Emby-Client=old", strings.NewReader(`{"DeviceId":"conflict","DeviceName":"Wrong","PlaySessionId":"session-a","PositionTicks":123,"DeviceProfile":{"Name":"OriginalProfile","CodecProfiles":[{"Codec":"h264"}]}}`))
	r.Header.Set("Authorization", `MediaBrowser Client="Player", Device="Phone", DeviceId="header-device", Version="3.0", Token="local-token"`)
	r.Header.Set("X-Emby-Device-Id", "explicit-device")
	extracted := deviceRequestFields(r)
	if extracted.ID != "explicit-device" || extracted.Name != "Phone" || extracted.Client != "old" || extracted.Version != "3.0" {
		t.Fatal(extracted)
	}
	identity := &DeviceIdentity{Applied: DeviceFields{ID: "mapped", Name: "Mapped", Client: "Player", Version: "3.0"}}
	replaceAccessAuth(r, "source-token")
	if err := applyDeviceIdentity(r, identity, "source-token"); err != nil {
		t.Fatal(err)
	}
	if accessToken(r) != "source-token" || deviceRequestFields(r).ID != "mapped" || r.URL.Query().Get("DeviceId") != "mapped" {
		t.Fatal("auth or query mapping wrong")
	}
	var body map[string]json.RawMessage
	json.NewDecoder(r.Body).Decode(&body)
	if string(body["DeviceId"]) != `"mapped"` || string(body["PlaySessionId"]) != `"session-a"` || !strings.Contains(string(body["DeviceProfile"]), "OriginalProfile") || string(body["PositionTicks"]) != "123" {
		t.Fatal("playback fields changed", body)
	}
	malformed := httptest.NewRequest("POST", "http://relay/Sessions/Playing", strings.NewReader("not json"))
	if applyDeviceIdentity(malformed, identity, "") == nil {
		t.Fatal("malformed progress accepted")
	}
}

func TestDevicePoliciesPinnedAcrossPlaybackAndLogout(t *testing.T) {
	a, admin, c, prefix := subscriptionFixture(t)
	upstreams, _ := list[Upstream](a.Store, "upstream")
	u := upstreams[0]
	var mu sync.Mutex
	observed := []DeviceFields{}
	bodies := []map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		observed = append(observed, deviceRequestFields(r))
		mu.Unlock()
		if strings.EqualFold(r.URL.Path, "/Users/AuthenticateByName") {
			writeJSON(w, 200, map[string]any{"AccessToken": "source-token", "User": map[string]any{"Id": "source-user", "Policy": map[string]bool{"IsAdministrator": false}}, "SessionInfo": map[string]any{"DeviceId": deviceRequestFields(r).ID}})
			return
		}
		if r.Header.Get("X-Emby-Token") != "source-token" {
			t.Error("upstream auth missing")
		}
		if r.Method == "POST" {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	w := call(t, a, admin, "PATCH", "/api/v1/upstreams/"+u.ID, map[string]any{"base_url": server.URL})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	setPolicy := func(p any) {
		t.Helper()
		w := call(t, a, admin, "PATCH", "/api/v1/upstreams/"+u.ID+"/device-policy", map[string]any{"policy": p})
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	setPolicy(DevicePolicy{Mode: "custom", DeviceFields: DeviceFields{ID: "shared", Name: "Media Room"}})
	request := func(path, token, device, playSession string) *httptest.ResponseRecorder {
		body := map[string]any{"Username": "viewer", "Pw": "viewer-password"}
		if token != "" {
			body = map[string]any{"DeviceId": device, "PlaySessionId": playSession, "PositionTicks": 42, "DeviceProfile": map[string]string{"Name": "NativeProfile"}}
		}
		r := httptest.NewRequest("POST", prefix+path+"?DeviceId="+url.QueryEscape(device), bytes.NewReader(Encode(body)))
		r.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Native", Device="Phone", DeviceId="`+device+`", Version="8.0"`)
		if token != "" {
			r.Header.Set("X-Emby-Token", token)
		}
		r.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		a.ServeHTTP(recorder, r)
		return recorder
	}
	login := func(device string) string {
		t.Helper()
		w := request("/Users/AuthenticateByName", "", device, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var out struct {
			AccessToken string
			SessionInfo map[string]any
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		if out.SessionInfo["DeviceId"] != device {
			t.Fatal("client identity was replaced")
		}
		return out.AccessToken
	}
	first, second := login("phone-a"), login("phone-b")
	if first == second {
		t.Fatal("proxy tokens were shared")
	}
	mu.Lock()
	mapped := observed[0].ID
	if mapped != observed[1].ID {
		t.Fatal("group identity not shared")
	}
	mu.Unlock()
	setPolicy(DevicePolicy{Mode: "original"})
	for _, v := range []struct{ token, device, session string }{{first, "phone-a", "play-a"}, {second, "phone-b", "play-b"}} {
		w := request("/Sessions/Playing/Progress", v.token, v.device, v.session)
		if w.Code != 204 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	mu.Lock()
	if observed[2].ID != mapped || observed[3].ID != mapped || bodies[0]["DeviceId"] != mapped || bodies[0]["PlaySessionId"] != "play-a" || bodies[1]["PlaySessionId"] != "play-b" {
		t.Fatal("identity not pinned or session IDs overwritten")
	}
	mu.Unlock()
	third := login("phone-c")
	mu.Lock()
	if observed[len(observed)-1].ID != "phone-c" {
		t.Fatal("new login did not adopt new policy")
	}
	mu.Unlock()
	// Two shared-identity players still have independent local playback leases.
	fresh, _ := get[AccessAccount](a.Store, "access_account", c.ID)
	fresh.Plan.MaxConcurrent = 2
	a.Store.put("access_account", fresh.ID, fresh, "test")
	beginA := AccessCommand{Action: "begin", AccountID: c.ID, KeyID: c.KeyID, Token: first, Stream: true}
	beginB := beginA
	beginB.Token = second
	leaseA, errA := a.accessCall(context.Background(), beginA)
	leaseB, errB := a.accessCall(context.Background(), beginB)
	if errA != nil || errB != nil || leaseA.Lease == leaseB.Lease {
		t.Fatal("independent playback leases failed", errA, errB)
	}
	finishA := beginA
	finishA.Action = "finish"
	finishA.Lease = leaseA.Lease
	finishA.Status = 204
	if _, err := a.accessCall(context.Background(), finishA); err != nil {
		t.Fatal(err)
	}
	checkB := beginB
	checkB.Action = "check"
	checkB.Lease = leaseB.Lease
	if _, err := a.accessCall(context.Background(), checkB); err != nil {
		t.Fatal("stopping first player invalidated second", err)
	}
	checkB.Action = "finish"
	a.accessCall(context.Background(), checkB)
	w = call(t, a, nil, "POST", prefix+"/Sessions/Logout?api_key="+first, nil)
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("/Sessions/Playing/Stopped", second, "phone-b", "play-b")
	if w.Code != 204 {
		t.Fatal("logout affected other local session", w.Code)
	}
	if _, err := a.accessCall(context.Background(), AccessCommand{Action: "info", AccountID: c.ID, KeyID: c.KeyID, Token: third}); err != nil {
		t.Fatal(err)
	}
	records := call(t, a, admin, "GET", "/api/v1/device-records?upstream_id="+u.ID, nil)
	if records.Code != 200 || strings.Contains(records.Body.String(), first) || strings.Contains(records.Body.String(), "source-token") {
		t.Fatal("records leaked credentials", records.Body.String())
	}
	var list []map[string]any
	json.Unmarshal(records.Body.Bytes(), &list)
	if len(list) != 3 {
		t.Fatal("original devices not recorded separately", records.Body.String())
	}
	for _, record := range list {
		if record["last_request_status"] == float64(204) {
			return
		}
	}
	t.Fatal("request diagnostics missing")
}

func TestDevicePolicyOverridesAndAccessControl(t *testing.T) {
	a, admin, c, _ := subscriptionFixture(t)
	us, _ := list[Upstream](a.Store, "upstream")
	u := us[0]
	sourcePath := "/api/v1/upstreams/" + u.ID + "/device-policy"
	accountPath := "/api/v1/access-accounts/" + c.ID + "/device-policy?upstream_id=" + u.ID
	for _, item := range []struct {
		path   string
		policy DevicePolicy
	}{{sourcePath, DevicePolicy{Mode: "custom"}}, {accountPath, DevicePolicy{Mode: "stable"}}} {
		w := call(t, a, admin, "PATCH", item.path, map[string]any{"policy": item.policy})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	u, _ = get[Upstream](a.Store, "upstream", u.ID)
	identity, _, err := a.resolveDeviceIdentity(c, u, AccessCommand{Device: "phone"})
	if err != nil || identity.Scope != "account" || identity.Mode != "stable" {
		t.Fatal(identity, err)
	}
	w := call(t, a, admin, "PATCH", accountPath, map[string]any{"policy": nil})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	identity, _, err = a.resolveDeviceIdentity(c, u, AccessCommand{Device: "phone"})
	if err != nil || identity.Scope != "source" || identity.Mode != "custom" {
		t.Fatal("inherit not restored", identity, err)
	}
	w = call(t, a, admin, "PATCH", sourcePath, map[string]any{"policy": DevicePolicy{Mode: "custom", DeviceFields: DeviceFields{Name: "bad\r\nfield"}}})
	if w.Code != 400 {
		t.Fatal("unsafe policy accepted", w.Code)
	}
	w = call(t, a, nil, "GET", "/api/v1/device-records", nil)
	if w.Code != 401 {
		t.Fatal("anonymous device access", w.Code)
	}
	w = call(t, a, nil, "POST", "/api/v1/portal/login", map[string]string{"username": "viewer", "password": "viewer-password"})
	var portal *http.Cookie
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "relay_portal" {
			portal = cookie
		}
	}
	if portal == nil {
		t.Fatal("portal login failed", w.Body.String())
	}
	now := time.Now().Unix()
	other := DeviceIdentity{RecordID: "other-record", Original: DeviceFields{ID: "private-other-device"}, Applied: DeviceFields{ID: "private-other-upstream"}}
	a.Store.DB.Exec("INSERT INTO access_devices(id,account_id,upstream_id,identity,last_seen) VALUES(?,?,?,?,?)", other.RecordID, "other-account", u.ID, string(Encode(other)), now)
	w = call(t, a, portal, "GET", "/api/v1/portal/devices?account_id=other-account", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-other-device") {
		t.Fatal("portal scope isolation failed", w.Code, w.Body.String())
	}
	w = call(t, a, portal, "PATCH", sourcePath, map[string]any{"policy": nil})
	if w.Code != 401 {
		t.Fatal("subscriber could edit admin policy", w.Code)
	}
}
