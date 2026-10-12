package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

type accessProtection struct {
	source, token string
	wrap          func(string) (string, error)
}
type accessProtectionKey struct{}
type forceMediaKey struct{}

func WithForcedMedia(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceMediaKey{}, true)
}
func WithAccessProtection(ctx context.Context, source, token string, wrap func(string) (string, error)) context.Context {
	return context.WithValue(ctx, accessProtectionKey{}, accessProtection{source, token, wrap})
}
func protectAccessResponse(resp *http.Response, prefix string, p accessProtection) error {
	resp.Header.Del("Set-Cookie")
	if loc := resp.Header.Get("Location"); loc != "" {
		target, e := resp.Request.URL.Parse(loc)
		if e != nil {
			return e
		}
		wrapped, e := p.wrap(target.String())
		if e != nil {
			return e
		}
		resp.Header.Set("Location", wrapped)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	playlist := strings.Contains(ct, "mpegurl") || (!strings.Contains(ct, "json") && strings.HasSuffix(strings.ToLower(resp.Request.URL.Path), ".m3u8"))
	if !strings.Contains(ct, "json") && !playlist {
		return nil
	}
	if resp.Header.Get("Content-Encoding") != "" {
		return fmt.Errorf("encoded subscription metadata cannot be protected")
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	resp.Body.Close()
	if e != nil || len(raw) > 8<<20 {
		return fmt.Errorf("subscription metadata exceeds safe rewrite limit")
	}
	var body []byte
	if playlist {
		lines := strings.Split(string(raw), "\n")
		attr := regexp.MustCompile(`URI="([^"]+)"`)
		for i, line := range lines {
			text := strings.TrimSpace(line)
			if text != "" && !strings.HasPrefix(text, "#") {
				u, e := resp.Request.URL.Parse(text)
				if e != nil {
					return e
				}
				lines[i], e = p.wrap(u.String())
				if e != nil {
					return e
				}
			} else {
				var rewriteErr error
				lines[i] = attr.ReplaceAllStringFunc(line, func(m string) string {
					val := attr.FindStringSubmatch(m)[1]
					u, e := resp.Request.URL.Parse(val)
					if e != nil {
						rewriteErr = e
						return m
					}
					wrapped, e := p.wrap(u.String())
					if e != nil {
						rewriteErr = e
						return m
					}
					return "URI=\"" + wrapped + "\""
				})
				if rewriteErr != nil {
					return rewriteErr
				}
			}
		}
		body = []byte(strings.Join(lines, "\n"))
	} else {
		var data any
		if json.Unmarshal(raw, &data) != nil {
			return fmt.Errorf("invalid subscription JSON")
		}
		var walk func(any) error
		walk = func(v any) error {
			switch x := v.(type) {
			case []any:
				for _, v := range x {
					if e := walk(v); e != nil {
						return e
					}
				}
			case map[string]any:
				for key, v := range x {
					str, ok := v.(string)
					if !ok {
						if e := walk(v); e != nil {
							return e
						}
						continue
					}
					switch strings.ToLower(key) {
					case "directstreamurl", "transcodingurl", "deliveryurl":
						u, e := resp.Request.URL.Parse(str)
						if e != nil {
							return e
						}
						if u.Scheme != "http" && u.Scheme != "https" {
							return fmt.Errorf("unsupported media URL")
						}
						wrapped, e := p.wrap(u.String())
						if e != nil {
							return e
						}
						x[key] = wrapped
					case "servertoken", "accesstoken":
						x[key] = p.token
					case "serveraddress", "localaddress", "wanaddress":
						x[key] = prefix
					default:
						if p.source != "" {
							str = strings.ReplaceAll(str, p.source, p.token)
							str = strings.ReplaceAll(str, url.QueryEscape(p.source), url.QueryEscape(p.token))
						}
						x[key] = str
					}
				}
			}
			return nil
		}
		if e := walk(data); e != nil {
			return e
		}
		body, e = json.Marshal(data)
		if e != nil {
			return e
		}
	}
	resp.Header.Del("ETag")
	resp.Header.Del("Content-MD5")
	resp.Header.Del("Content-Encoding")
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", fmt.Sprint(len(body)))
	return nil
}
