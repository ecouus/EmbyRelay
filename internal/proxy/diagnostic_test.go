package proxy

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestDiagnosticClassification(t *testing.T) {
	var reason string
	ctx := WithDiagnostic(context.Background(), func(s string) { reason = s })
	reportError(ctx, errors.New("redirect target denied"))
	if !strings.Contains(reason, "白名单") {
		t.Fatal(reason)
	}
	reportStatus(ctx, 502)
	if reason != "回源：源站返回 HTTP 502" {
		t.Fatal(reason)
	}
	reportError(ctx, errors.New("https://secret.example/token?api_key=secret"))
	if strings.Contains(reason, "secret") {
		t.Fatal("raw error leaked")
	}
}

func TestRedirectDetailSafety(t *testing.T) {
	upstream, _ := url.Parse("https://emby.example")
	for _, tc := range []struct{ target, code string }{
		{"https://cdn.example:8443/video?token=secret", "redirect_host_denied"},
		{"http://cdn.example/video", "redirect_downgrade"},
		{"https://user:secret@cdn.example/video", "redirect_invalid"},
	} {
		var d Detail
		ctx := WithDetail(context.Background(), func(v Detail) { d = v })
		target, _ := url.Parse(tc.target)
		redirectDenied(ctx, target, upstream)
		if d.Code != tc.code {
			t.Fatal(d)
		}
		if strings.Contains(d.Target, "secret") || strings.Contains(d.Target, "video") {
			t.Fatal("sensitive redirect leaked")
		}
	}
}
