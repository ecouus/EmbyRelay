package control

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestUserSourceRequestPolicy(t *testing.T) {
	for _, tc := range []struct {
		method, path, upgrade string
		allowed               bool
	}{
		{"GET", "/emby/System/Info/Public", "", true}, {"POST", "/Users/AuthenticateByName", "", true},
		{"GET", "/Users/viewer/Items", "", true}, {"GET", "/Items/123/Images/Primary/0", "", true},
		{"POST", "/Items/123/PlaybackInfo", "", true}, {"GET", "/Videos/123/stream.mkv", "", true},
		{"GET", "/Videos/123/hls1/main/0.ts", "", true}, {"GET", "/Videos/123/source/Subtitles/1/Stream.srt", "", true},
		{"POST", "/Sessions/Playing/Progress", "", true}, {"DELETE", "/Users/viewer/FavoriteItems/123", "", true},
		{"GET", "/__access_media/abcdef_123", "", true}, {"GET", "/subscription/info", "", true},
		{"GET", "/trojan", "", false}, {"GET", "/socket", "websocket", false},
		{"GET", "/System/Info/Public", "websocket", false}, {"CONNECT", "/Items", "", false},
		{"POST", "/Videos/123/stream", "", false}, {"GET", "/Items/../trojan", "", false},
		{"GET", "/Items/%252e%252e/trojan", "", false}, {"GET", "/Users/viewer/Password", "", false},
		{"POST", "/Sessions/Capabilities/trojan", "", false}, {"GET", "/Videos/123/hls1/main/..", "", false},
	} {
		t.Run(tc.method+tc.path+tc.upgrade, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Upgrade", tc.upgrade)
			if allowedUserSourceRequest(r) != tc.allowed {
				t.Fatal("unexpected policy decision")
			}
		})
	}
	r := httptest.NewRequest("GET", "/System/Info/Public", nil)
	r.Header.Add("Connection", "keep-alive, Upgrade")
	if allowedUserSourceRequest(r) {
		t.Fatal("connection upgrade accepted")
	}
}

func TestUserSourcePolicyOnControlAndAgent(t *testing.T) {
	a, _, c, prefix := subscriptionFixture(t)
	routes, _ := list[Route](a.Store, "route")
	route := routes[0]
	route.OwnerAccountID = c.ID
	route.RelayID = "auto"
	if err := a.Store.put("route", route.ID, route, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	a.AccessRPC = func(ctx context.Context, command AccessCommand) (AccessResult, error) {
		return a.accessCommand(ctx, "test-node", command)
	}
	for _, agent := range []bool{false, true} {
		a.AgentMode = agent
		for _, path := range []string{"/trojan", "/socket", "/Items/../trojan"} {
			if w := call(t, a, nil, "GET", prefix+path, nil); w.Code != 403 {
				t.Fatal(agent, path, w.Code, w.Body.String())
			}
		}
		if w := call(t, a, nil, "GET", prefix+"/System/Info/Public", nil); w.Code != 200 {
			t.Fatal(agent, w.Code, w.Body.String())
		}
		token := subscriptionLogin(t, a, prefix)
		if w := call(t, a, nil, "GET", prefix+"/Videos/1/stream?api_key="+token, nil); w.Code != 200 || w.Body.Len() != 1024 {
			t.Fatal("normal playback rejected", agent, w.Code, w.Body.String())
		}
		request := httptest.NewRequest("GET", prefix+"/System/Info/Public", nil)
		request.Header.Set("Upgrade", "websocket")
		request.Header.Set("Connection", "Upgrade")
		response := httptest.NewRecorder()
		a.ServeHTTP(response, request)
		if response.Code != 403 {
			t.Fatal("upgrade forwarded", agent, response.Code)
		}
	}
}
