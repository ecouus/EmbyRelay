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
	wrapBatch     func([]string) ([]string, error)
}

var uriAttribute = regexp.MustCompile(`URI="([^"]+)"`)

const wrapBatchSize = 512

// wrapAll protects every target with as few control calls as possible. Without
// a batch function (or for one target) it falls back to single calls.
func (p accessProtection) wrapAll(targets []string) ([]string, error) {
	out := make([]string, 0, len(targets))
	if p.wrapBatch == nil || len(targets) < 2 {
		for _, target := range targets {
			wrapped, e := p.wrap(target)
			if e != nil {
				return nil, e
			}
			out = append(out, wrapped)
		}
		return out, nil
	}
	for len(targets) > 0 {
		part := targets[:min(len(targets), wrapBatchSize)]
		wrapped, e := p.wrapBatch(part)
		if e != nil {
			return nil, e
		}
		if len(wrapped) != len(part) {
			return nil, fmt.Errorf("invalid batch wrap response")
		}
		out = append(out, wrapped...)
		targets = targets[len(part):]
	}
	return out, nil
}

// wrapSet collects URLs first so a whole response is wrapped in one batch.
type wrapSet struct {
	index   map[string]int
	targets []string
	apply   []func([]string)
}

func (w *wrapSet) add(target string, set func(string)) {
	if w.index == nil {
		w.index = map[string]int{}
	}
	i, ok := w.index[target]
	if !ok {
		i = len(w.targets)
		w.index[target] = i
		w.targets = append(w.targets, target)
	}
	w.apply = append(w.apply, func(wrapped []string) { set(wrapped[i]) })
}
func (w *wrapSet) run(p accessProtection) error {
	if len(w.targets) == 0 {
		return nil
	}
	wrapped, e := p.wrapAll(w.targets)
	if e != nil {
		return e
	}
	for _, apply := range w.apply {
		apply(wrapped)
	}
	return nil
}

type accessProtectionKey struct{}
type forceMediaKey struct{}

func WithForcedMedia(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceMediaKey{}, true)
}
func WithAccessProtection(ctx context.Context, source, token string, wrap func(string) (string, error), wrapBatch func([]string) ([]string, error)) context.Context {
	return context.WithValue(ctx, accessProtectionKey{}, accessProtection{source, token, wrap, wrapBatch})
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
		var set wrapSet
		parts := make([][]string, len(lines))
		for i, line := range lines {
			text := strings.TrimSpace(line)
			if text != "" && !strings.HasPrefix(text, "#") {
				u, e := resp.Request.URL.Parse(text)
				if e != nil {
					return e
				}
				set.add(u.String(), func(wrapped string) { lines[i] = wrapped })
				continue
			}
			matches := uriAttribute.FindAllStringSubmatchIndex(line, -1)
			if len(matches) == 0 {
				continue
			}
			last := 0
			for _, m := range matches {
				u, e := resp.Request.URL.Parse(line[m[2]:m[3]])
				if e != nil {
					return e
				}
				parts[i] = append(parts[i], line[last:m[2]], "")
				k := len(parts[i]) - 1
				set.add(u.String(), func(wrapped string) { parts[i][k] = wrapped })
				last = m[3]
			}
			parts[i] = append(parts[i], line[last:])
		}
		if e := set.run(p); e != nil {
			return e
		}
		for i, pieces := range parts {
			if pieces != nil {
				lines[i] = strings.Join(pieces, "")
			}
		}
		body = []byte(strings.Join(lines, "\n"))
	} else {
		var data any
		if json.Unmarshal(raw, &data) != nil {
			return fmt.Errorf("invalid subscription JSON")
		}
		var set wrapSet
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
						set.add(u.String(), func(wrapped string) { x[key] = wrapped })
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
		if e := set.run(p); e != nil {
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
