package proxy

import (
	"context"
	"fmt"
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

func TestAccessPlaylistUsesOneBatch(t *testing.T) {
	base, _ := url.Parse("https://source.example/emby/Videos/1/master.m3u8")
	request := &http.Request{URL: base}
	var singles, batches, sizes int
	p := accessProtection{source: "real-token", token: "proxy-token",
		wrap: func(s string) (string, error) { singles++; return "/relay/single", nil },
		wrapBatch: func(targets []string) ([]string, error) {
			batches++
			sizes += len(targets)
			out := make([]string, len(targets))
			for i, target := range targets {
				out[i] = "/relay/" + target[strings.LastIndex(target, "/")+1:]
			}
			return out, nil
		}}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXT-X-MAP:URI=\"init.mp4\",BYTERANGE=\"1@0\"\n")
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&b, "#EXTINF:2,\nseg%d.ts\n", i)
	}
	resp := &http.Response{Request: request, Header: http.Header{"Content-Type": []string{"application/vnd.apple.mpegurl"}}, Body: io.NopCloser(strings.NewReader(b.String()))}
	if err := protectAccessResponse(resp, "/relay", p); err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if singles != 0 || sizes != 1202 || batches != 3 {
		t.Fatalf("single=%d batches=%d targets=%d", singles, batches, sizes)
	}
	if !strings.Contains(text, "URI=\"/relay/key.bin\"") || !strings.Contains(text, "URI=\"/relay/init.mp4\",BYTERANGE") || !strings.Contains(text, "\n/relay/seg1199.ts") {
		t.Fatal(text[:300])
	}
}
