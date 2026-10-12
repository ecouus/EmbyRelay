package proxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAccessMetadataProtection(t *testing.T) {
	base, _ := url.Parse("https://source.example/emby/Videos/1/master.m3u8")
	request := &http.Request{URL: base}
	targets := []string{}
	p := accessProtection{source: "real-token", token: "proxy-token", wrap: func(s string) (string, error) { targets = append(targets, s); return "/relay/wrapped", nil }}
	resp := &http.Response{Request: request, Header: http.Header{"Content-Type": []string{"application/vnd.apple.mpegurl"}}, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\nsegment.ts?api_key=real-token\n"))}
	if err := protectAccessResponse(resp, "/relay", p); err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "real-token") || len(targets) != 2 || targets[0] != "https://source.example/emby/Videos/1/key.bin" {
		t.Fatal(string(raw), targets)
	}
	resp = &http.Response{Request: request, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"MediaSources":[{"DirectStreamUrl":"https://cdn.example/video?signature=abc","TranscodingUrl":"/emby/Videos/1/master.m3u8?api_key=real-token"}],"AccessToken":"real-token","Other":"url?api_key=real-token"}`))}
	if err := protectAccessResponse(resp, "/relay", p); err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "real-token") || strings.Contains(string(raw), "cdn.example") || !strings.Contains(string(raw), "proxy-token") {
		t.Fatal(string(raw))
	}
}
func TestProtectedRedirectIsMedia(t *testing.T) {
	ctx := WithForcedMedia(context.Background())
	v, _ := ctx.Value(forceMediaKey{}).(bool)
	if !v {
		t.Fatal("media context missing")
	}
}
