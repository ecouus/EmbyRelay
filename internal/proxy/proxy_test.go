package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "0123456789abcdef0123456789abcdef"

func setup(t *testing.T, h http.HandlerFunc) (*httptest.Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	u, _ := ValidateURL(up.URL)
	gw := httptest.NewServer(New(Config{Upstream: u, AccessKey: testKey, Network: NetworkPolicy{Allowed: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}, MaxResumes: 3}))
	t.Cleanup(gw.Close)
	return up, gw
}
func fetch(t *testing.T, base, path, rangeHeader string) (*http.Response, []byte, error) {
	t.Helper()
	r, _ := http.NewRequest("GET", base+path, nil)
	r.Header.Set("X-Relay-Key", testKey)
	if rangeHeader != "" {
		r.Header.Set("Range", rangeHeader)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	return resp, b, e
}
func TestRanges(t *testing.T) {
	data := strings.Repeat("abcdefghij", 200)
	_, gw := setup(t, func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "video", time.Time{}, strings.NewReader(data))
	})
	for _, tc := range []struct {
		rangeHeader string
		status      int
		body        string
	}{{"", 200, data}, {"bytes=0-999", 206, data[:1000]}, {"bytes=500-", 206, data[500:]}, {"bytes=-100", 206, data[len(data)-100:]}, {"bytes=9000-", 416, ""}} {
		t.Run(tc.rangeHeader, func(t *testing.T) {
			resp, b, e := fetch(t, gw.URL, "/Videos/1/stream", tc.rangeHeader)
			if e != nil || resp.StatusCode != tc.status {
				t.Fatalf("status=%d err=%v", resp.StatusCode, e)
			}
			if tc.status != 416 && string(b) != tc.body {
				t.Fatal("body mismatch")
			}
			if tc.status == 416 && resp.Header.Get("Content-Range") != "bytes */2000" {
				t.Fatal("lost range header")
			}
		})
	}
}
func TestResumeValidation(t *testing.T) {
	for _, mode := range []string{"valid", "offset", "version", "ignored", "no-validator"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			_, gw := setup(t, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				tag := "\"v1\""
				if mode == "version" && n > 1 {
					tag = "\"v2\""
				}
				if mode != "no-validator" {
					w.Header().Set("ETag", tag)
				}
				if n == 1 {
					w.Header().Set("Content-Length", "10")
					w.WriteHeader(200)
					w.Write([]byte("01234"))
					return
				}
				if r.Header.Get("Range") != "bytes=5-9" || r.Header.Get("If-Range") != "\"v1\"" {
					t.Error("wrong resume request")
				}
				if mode == "ignored" {
					w.Header().Set("Content-Length", "10")
					w.Write([]byte("0123456789"))
					return
				}
				cr := "bytes 5-9/10"
				if mode == "offset" {
					cr = "bytes 4-8/10"
				}
				w.Header().Set("Content-Range", cr)
				w.Header().Set("Content-Length", "5")
				w.WriteHeader(206)
				w.Write([]byte("56789"))
			})
			_, b, err := fetch(t, gw.URL, "/Videos/1/stream", "")
			if mode == "valid" {
				if err != nil || string(b) != "0123456789" || calls.Load() != 2 {
					t.Fatalf("body=%q err=%v calls=%d", b, err, calls.Load())
				}
			} else if err == nil || string(b) != "01234" {
				t.Fatalf("unsafe response: %q %v", b, err)
			}
		})
	}
}
func TestIgnoredRange(t *testing.T) {
	_, gw := setup(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("entire resource")) })
	resp, b, e := fetch(t, gw.URL, "/Videos/1/stream", "bytes=5-")
	if e != nil || resp.StatusCode != 200 || string(b) != "entire resource" {
		t.Fatal("must preserve 200")
	}
}
func TestAuthAndPost(t *testing.T) {
	var calls atomic.Int32
	_, gw := setup(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Relay-Key") != "" {
			t.Error("key leaked")
		}
		io.Copy(w, r.Body)
	})
	resp, e := http.Get(gw.URL + "/Users")
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || calls.Load() != 0 {
		t.Fatal("auth bypass")
	}
	r, _ := http.NewRequest("POST", gw.URL+"/Users/AuthenticateByName", strings.NewReader("payload"))
	r.Header.Set("X-Relay-Key", testKey)
	resp, e = http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "payload" || calls.Load() != 1 {
		t.Fatal("POST mismatch")
	}
}
func TestRedirectDenied(t *testing.T) {
	_, gw := setup(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.1/private", 302) })
	for _, p := range []string{"/Users", "/Videos/1/stream"} {
		r, _, _ := fetch(t, gw.URL, p, "")
		if r.StatusCode != 502 || r.Header.Get("Location") != "" {
			t.Fatal("redirect escaped policy")
		}
	}
}
func TestNetworkPolicy(t *testing.T) {
	p := NetworkPolicy{}
	for _, s := range []string{"127.0.0.1", "::1", "10.1.2.3", "169.254.169.254", "100.64.0.1", "::ffff:127.0.0.1"} {
		if p.permits(netip.MustParseAddr(s)) {
			t.Errorf("allowed %s", s)
		}
	}
	p.Allowed = []netip.Prefix{netip.MustParsePrefix("10.1.2.0/24")}
	if !p.permits(netip.MustParseAddr("10.1.2.3")) {
		t.Fatal("explicit allow ignored")
	}
}
func TestBadRange(t *testing.T) {
	for _, s := range []string{"bytes 2-1/10", "bytes 0-10/10", "bytes 0-1/*", "bytes 0-9223372036854775807/9", "garbage"} {
		if _, _, _, e := rangeInfo(s); e == nil {
			t.Error(s)
		}
	}
}
func TestCancellation(t *testing.T) {
	done := make(chan struct{})
	_, gw := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(done)
	})
	r, _ := http.NewRequest("GET", gw.URL+"/Videos/1/stream", nil)
	r.Header.Set("X-Relay-Key", testKey)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, e := client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream not cancelled")
	}
}
func TestConcurrent(t *testing.T) {
	_, gw := setup(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	done := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		go func() {
			req, _ := http.NewRequest("GET", gw.URL+"/Videos/1/stream", nil)
			req.Header.Set("X-Relay-Key", testKey)
			r, e := http.DefaultClient.Do(req)
			if e != nil {
				done <- false
				return
			}
			b, e := io.ReadAll(r.Body)
			r.Body.Close()
			done <- e == nil && r.StatusCode == 200 && string(b) == "ok"
		}()
	}
	for i := 0; i < 100; i++ {
		if !<-done {
			t.Error("concurrent request failed")
		}
	}
}

func TestInitialWrongOffset(t *testing.T) {
	_, gw := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 1-3/10")
		w.Header().Set("Content-Length", "3")
		w.WriteHeader(206)
		w.Write([]byte("123"))
	})
	resp, _, _ := fetch(t, gw.URL, "/Videos/1/stream", "bytes=0-2")
	if resp.StatusCode != 502 {
		t.Fatal("accepted wrong initial offset")
	}
}

func TestReadTimeoutResumes(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "\"v1\"")
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "10")
			w.Write([]byte("01234"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Range", "bytes 5-9/10")
		w.Header().Set("Content-Length", "5")
		w.WriteHeader(206)
		w.Write([]byte("56789"))
	}))
	defer up.Close()
	u, _ := ValidateURL(up.URL)
	gw := httptest.NewServer(New(Config{Upstream: u, AccessKey: testKey, Network: NetworkPolicy{Allowed: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}, MaxResumes: 3, ReadTimeout: 40 * time.Millisecond}))
	defer gw.Close()
	_, b, e := fetch(t, gw.URL, "/Videos/1/stream", "")
	if e != nil || string(b) != "0123456789" {
		t.Fatalf("timeout resume failed %q %v", b, e)
	}
}

func TestAllowedRedirectDoesNotForwardSecrets(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Emby-Token") != "" || r.Header.Get("X-MediaBrowser-Authorization") != "" {
			t.Error("cross-origin secrets leaked")
		}
		w.Write([]byte("media"))
	}))
	defer target.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/file", 302) }))
	defer up.Close()
	u, _ := ValidateURL(up.URL)
	targetURL, _ := ValidateURL(target.URL)
	for _, mode := range []string{"proxy", "redirect", "auto"} {
		t.Run(mode, func(t *testing.T) {
			g := httptest.NewServer(New(Config{Upstream: u, AccessKey: testKey, Mode: mode, RedirectHosts: []string{targetURL.Host}, Network: NetworkPolicy{Allowed: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}}))
			defer g.Close()
			r, _ := http.NewRequest("GET", g.URL+"/Videos/1/stream", nil)
			r.Header.Set("X-Relay-Key", testKey)
			r.Header.Set("Authorization", "secret")
			r.Header.Set("Cookie", "secret")
			r.Header.Set("X-Emby-Token", "secret")
			r.Header.Set("X-MediaBrowser-Authorization", `MediaBrowser Token="secret"`)
			client := &http.Client{CheckRedirect: func(r *http.Request, v []*http.Request) error { return http.ErrUseLastResponse }}
			resp, e := client.Do(r)
			if e != nil {
				t.Fatal(e)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if mode == "proxy" {
				if resp.StatusCode != 200 || string(b) != "media" {
					t.Fatal(resp.StatusCode, string(b))
				}
			} else if resp.StatusCode != 302 || resp.Header.Get("Location") != target.URL+"/file" {
				t.Fatal("direct redirect missing")
			}
		})
	}
}

func TestMediaStatusRetry(t *testing.T) {
	var calls atomic.Int32
	_, gw := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte("ok"))
	})
	resp, b, e := fetch(t, gw.URL, "/Videos/1/stream", "")
	if e != nil || resp.StatusCode != 200 || string(b) != "ok" || calls.Load() != 3 {
		t.Fatal("media GET not retried")
	}
}
func TestUnsafePostNeverRetried(t *testing.T) {
	var calls atomic.Int32
	_, gw := setup(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) })
	r, _ := http.NewRequest("POST", gw.URL+"/Videos/1/stream", strings.NewReader("state"))
	r.Header.Set("X-Relay-Key", testKey)
	resp, e := http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if calls.Load() != 1 {
		t.Fatal("POST retried")
	}
}
