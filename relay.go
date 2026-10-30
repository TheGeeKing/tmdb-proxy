package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
	"golang.org/x/sync/singleflight"
)

const upstreamHost = "https://api.themoviedb.org"

type response struct {
	Status  int
	Header  http.Header
	Body    []byte
	Expires time.Time
}
type relay struct {
	cfg       config
	cache     valkey.Client
	http      *http.Client
	base      string // Fixed in production; replaced only by in-process tests.
	ctx       context.Context
	flights   singleflight.Group
	log       *slog.Logger
	limiter   *upstreamLimiter
	namespace string
}

func newRelay(ctx context.Context, c config, cache valkey.Client, logger *slog.Logger) *relay {
	sum := sha256.Sum256([]byte(c.token))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 32
	transport.ResponseHeaderTimeout = c.tmdbTimeout
	return &relay{limiter: newUpstreamLimiter(c), cfg: c, cache: cache, ctx: ctx, base: upstreamHost, log: logger, namespace: hex.EncodeToString(sum[:]), http: &http.Client{Timeout: c.tmdbTimeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func problem(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
func (s *relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Add("Vary", "Origin")
		if s.cfg.origins["*"] {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if s.cfg.origins[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		if r.Method == http.MethodOptions && w.Header().Get("Access-Control-Allow-Origin") != "" {
			if r.Header.Get("Access-Control-Request-Method") != "GET" || r.Header.Get("Access-Control-Request-Headers") != "" {
				problem(w, 403, "unsupported preflight")
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if r.Method != "GET" {
		w.Header().Set("Allow", "GET")
		problem(w, 405, "only GET is supported")
		return
	}
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}
	if r.URL.Path == "/readyz" {
		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.valkeyTimeout)
		defer cancel()
		if err := s.cache.Do(ctx, s.cache.B().Ping().Build()).Error(); err != nil {
			problem(w, 503, "cache unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ready"}`)
		return
	}
	path := r.URL.Path
	if r.URL.RawPath != "" || !allowedPath(path) {
		problem(w, 404, "unsupported route")
		return
	}
	version := strings.Split(path, "/")[1]
	if !s.cfg.versions[version] {
		problem(w, 404, "API version disabled")
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || !safeQuery(path, q) {
		problem(w, 400, "invalid query or credentials are not allowed")
		return
	}
	for _, h := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Auth-Token", "Cookie"} {
		if _, ok := r.Header[h]; ok {
			problem(w, 400, "caller credentials are not allowed")
			return
		}
	}
	// Encode sorts names, preserving duplicate-value order because upstream semantics may depend on it.
	canonical := q.Encode()
	sum := sha256.Sum256([]byte(path + "?" + canonical))
	key := "tmdb-relay:v1:" + s.namespace + ":" + hex.EncodeToString(sum[:])
	result := s.flights.DoChan(key, func() (any, error) {
		if cached := s.readCache(key); cached != nil {
			return cached, nil
		}
		return s.fetch(key, path, canonical), nil
	})
	select {
	case <-r.Context().Done():
		return
	case out := <-result:
		resp := out.Val.(*response)
		for k, values := range resp.Header {
			w.Header()[k] = append([]string(nil), values...)
		}
		if !resp.Expires.IsZero() {
			seconds := int64(time.Until(resp.Expires) / time.Second)
			if seconds > 0 {
				w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d, must-revalidate, proxy-revalidate", seconds))
			}
		}
		w.WriteHeader(resp.Status)
		_, _ = w.Write(resp.Body)
	}
}
func (s *relay) readCache(key string) *response {
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.valkeyTimeout)
	defer cancel()
	raw, err := s.cache.Do(ctx, s.cache.B().Get().Key(key).Build()).AsBytes()
	if err != nil {
		if !valkey.IsValkeyNil(err) {
			s.log.Warn("cache read failed; fetching upstream")
		}
		return nil
	}
	var resp response
	if json.Unmarshal(raw, &resp) != nil || resp.Expires.IsZero() || !time.Now().Before(resp.Expires) || resp.Status < 200 || resp.Status >= 300 {
		return nil
	}
	return &resp
}
func localResponse(status int, msg string) *response {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return &response{Status: status, Body: b, Header: http.Header{"Content-Type": []string{"application/json"}}}
}
func (s *relay) fetch(key, path, query string) *response {
	err := s.limiter.wait(s.ctx)
	if err != nil {
		resp := localResponse(429, "upstream rate limit reached; retry later")
		resp.Header.Set("Retry-After", s.limiter.retryAfter())
		return resp
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.tmdbTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", s.base+path+"?"+query, nil)
	if err != nil {
		return localResponse(502, "upstream request failed")
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	upstream, err := s.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return localResponse(504, "upstream timeout")
		}
		return localResponse(502, "upstream request failed")
	}
	defer upstream.Body.Close()
	if upstream.StatusCode == http.StatusTooManyRequests {
		s.limiter.pause(upstream.Header.Get("Retry-After"))
	}
	body, err := io.ReadAll(io.LimitReader(upstream.Body, s.cfg.maxBytes+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return localResponse(504, "upstream timeout")
		}
		return localResponse(502, "upstream read failed")
	}
	if int64(len(body)) > s.cfg.maxBytes {
		return localResponse(502, "upstream response exceeds MAX_RESPONSE_BYTES")
	}
	// A credential should never be echoed by TMDB; fail closed if it is.
	if bytes.Contains(body, []byte(s.cfg.token)) {
		return localResponse(502, "unsafe upstream response")
	}
	resp := &response{Status: upstream.StatusCode, Body: body, Header: make(http.Header)}
	for _, k := range []string{"Content-Type", "Content-Encoding", "Content-Language", "Retry-After", "ETag", "Last-Modified"} {
		for _, v := range upstream.Header.Values(k) {
			if strings.Contains(v, s.cfg.token) {
				return localResponse(502, "unsafe upstream response")
			}
			resp.Header.Add(k, v)
		}
	}
	media, _, _ := mime.ParseMediaType(upstream.Header.Get("Content-Type"))
	success := resp.Status >= 200 && resp.Status < 300 && (media == "application/json" || strings.HasSuffix(media, "+json")) && json.Valid(body) && upstream.Header.Get("Content-Encoding") == ""
	if resp.Status >= 200 && resp.Status < 300 && strings.HasPrefix(path, "/4/list/") {
		var list struct {
			Public *bool `json:"public"`
		}
		if !success || json.Unmarshal(body, &list) != nil || list.Public == nil || !*list.Public {
			return localResponse(404, "public list not found")
		}
	}
	if success {
		ttl := s.cfg.ttl(path)
		age, _ := strconv.ParseInt(upstream.Header.Get("Age"), 10, 64)
		if age < 0 {
			age = 0
		}
		// Honor a stricter upstream freshness or storage restriction, if present.
		for _, directive := range strings.Split(upstream.Header.Get("Cache-Control"), ",") {
			d := strings.TrimSpace(strings.ToLower(directive))
			name, value, _ := strings.Cut(d, "=")
			name = strings.TrimSpace(name)
			if name == "no-store" || name == "private" || name == "no-cache" {
				return resp
			}
			if name == "max-age" || name == "s-maxage" {
				n, e := strconv.ParseInt(strings.Trim(strings.TrimSpace(value), `"`), 10, 64)
				if e != nil || n < 0 || n <= age {
					return resp
				}
				if n-age < int64(ttl/time.Second) {
					ttl = time.Duration(n-age) * time.Second
				}
			}
		}
		if ttl <= 0 {
			return resp
		}
		resp.Expires = time.Now().Add(ttl)
		raw, _ := json.Marshal(resp)
		cacheCtx, cacheCancel := context.WithTimeout(s.ctx, s.cfg.valkeyTimeout)
		defer cacheCancel()
		err = s.cache.Do(cacheCtx, s.cache.B().Set().Key(key).Value(string(raw)).Px(ttl).Build()).Error()
		if err != nil {
			s.log.Warn("cache write failed; returning upstream response")
			resp.Expires = time.Time{}
		}
	}
	return resp
}
