package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/valkey-io/valkey-go"
)

func fixture(t *testing.T, handler http.HandlerFunc) (*relay, *miniredis.Miniredis) {
	t.Helper()
	db := miniredis.RunT(t)
	c, err := loadConfig(func(k string) string {
		if k == "TMDB_READ_ACCESS_TOKEN" {
			return "operator-secret-token"
		}
		if k == "VALKEY_URL" {
			return "redis://" + db.Addr()
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := valkey.NewClient(c.valkey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := newRelay(ctx, c, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.base = upstream.URL
	return s, db
}
func request(s *relay, method, path string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for i := 0; i < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func jsonReply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, body)
}

func TestConfigEndpoint(t *testing.T) {
	for _, limits := range [][2]int{{40, 200}, {7, 12}} {
		c, err := loadConfig(func(key string) string {
			switch key {
			case "TMDB_READ_ACCESS_TOKEN":
				return "operator-secret-token"
			case "TMDB_REQUESTS_PER_SECOND":
				return fmt.Sprint(limits[0])
			case "TMDB_QUEUE_SIZE":
				return fmt.Sprint(limits[1])
			case "CACHE_TTL_SEARCH_SECONDS":
				return fmt.Sprint(limits[0])
			default:
				return ""
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		s := &relay{cfg: c}
		w := request(s, "GET", "/config")
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unexpected response: %d %v", w.Code, w.Header())
		}
		want := fmt.Sprintf(`{"cache_ttl_seconds":{"details":86400,"lists":900,"reference":604800,"search":%d,"trending":300},"rate_limit":{"queue_size":%d,"requests_per_second":%d}}`, limits[0], limits[1], limits[0])
		if strings.TrimSpace(w.Body.String()) != want {
			t.Fatalf("unexpected config: %s", w.Body)
		}
		if w := request(s, "POST", "/config"); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET" {
			t.Fatalf("unexpected method response: %d %v", w.Code, w.Header())
		}
	}
}

func TestCacheRefreshAndNoStale(t *testing.T) {
	var calls atomic.Int32
	var mode atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	s, db := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer operator-secret-token" || r.Header.Get("Cookie") != "" {
			t.Error("incorrect upstream credentials")
		}
		if mode.Load() == 1 {
			entered <- struct{}{}
			<-release
		}
		if mode.Load() == 2 {
			w.Header().Set("Retry-After", "42")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":"limited"}`)
			return
		}
		jsonReply(w, fmt.Sprintf("{ \"generation\": %d }\n", n))
	})
	first := request(s, "GET", "/3/movie/550?language=fr&page=1")
	if first.Code != 200 || first.Body.String() != "{ \"generation\": 1 }\n" {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	hit := request(s, "GET", "/3/movie/550?page=1&language=fr")
	if hit.Body.String() != first.Body.String() || calls.Load() != 1 || !strings.Contains(hit.Header().Get("Cache-Control"), "must-revalidate") {
		t.Fatal("cache miss or bad freshness headers")
	}
	if len(db.Keys()) != 1 || db.TTL(db.Keys()[0]) <= 0 {
		t.Fatal("missing Valkey TTL")
	}
	db.FastForward(25 * time.Hour)
	mode.Store(1)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(s, "GET", "/3/movie/550?language=fr&page=1") }()
	<-entered
	select {
	case <-done:
		t.Fatal("refresh did not block")
	default:
	}
	close(release)
	fresh := <-done
	if fresh.Body.String() != "{ \"generation\": 2 }\n" {
		t.Fatal("refresh returned old bytes")
	}
	db.FastForward(25 * time.Hour)
	mode.Store(2)
	for i := 0; i < 2; i++ {
		// Clear cooldown to verify each upstream error is returned without caching.
		s.limiter = newUpstreamLimiter(s.cfg)
		w := request(s, "GET", "/3/movie/550?language=fr&page=1")
		if w.Code != 429 || w.Body.String() != `{"error":"limited"}` || w.Header().Get("Retry-After") != "42" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("error preservation: %d %s", w.Code, w.Body)
		}
	}
	if calls.Load() != 4 {
		t.Fatal("errors were cached")
	}
}

func TestConcurrentFetch(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		jsonReply(w, `{"ok":true}`)
	})
	const n = 30
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := request(s, "GET", "/3/movie/550")
			if w.Code != 200 {
				t.Errorf("status %d", w.Code)
			}
		}()
	}
	close(start)
	<-entered
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("got %d upstream requests", calls.Load())
	}
}

func TestQueryAndVersionIsolation(t *testing.T) {
	var calls atomic.Int32
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); jsonReply(w, `{"public":true}`) })
	paths := []string{"/3/list/1?language=en&region=US&page=1", "/3/list/1?language=fr&region=US&page=1", "/3/list/1?language=en&region=FR&page=1", "/3/list/1?language=en&region=US&page=2", "/4/list/1?language=en&region=US&page=1", "/3/list/1?language=en&region=US&page=1&custom=a", "/3/list/1?custom=a&language=en&region=US&page=1"}
	for _, path := range paths {
		if w := request(s, "GET", path); w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if calls.Load() != 6 {
		t.Fatalf("variant calls %d", calls.Load())
	}
}

func TestRoutesAndCredentials(t *testing.T) {
	var calls atomic.Int32
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); jsonReply(w, `{"public":true}`) })
	for _, path := range []string{"/3/movie/550", "/3/search/multi?query=hello", "/3/tv/1/season/0/episode/1", "/3/trending/all/day", "/3/find/tt0137523?external_source=imdb_id", "/4/list/1", "/3/movie/550?append_to_response=credits,images"} {
		if w := request(s, "GET", path); w.Code != 200 {
			t.Errorf("allowed %s: %d", path, w.Code)
		}
	}
	before := calls.Load()
	for _, path := range []string{"/4/account/1/movie/recommendations", "/4/list/1/item_status", "/4/movie/550", "/3/account/1", "/3/authentication", "/3/movie/1/account_states", "/3/guest_session/1/rated/movies", "/3/movie/1/unknown", "/3/trending/all/year", "/3/movie/../account/1", "/3/movie/%35%35%30", "/3/movie/550?api_key=bad", "/3/movie/550?API-Key=bad", "/3/movie/550?session_id=bad", "/3/movie/550?access_token=bad", "/3/movie/550?append_to_response=account_states", "/3/movie/550?append_to_response=credits,../account_states", "/3/movie/550?append_to_response=credits&append_to_response=account_states", "/3/movie/550?query=%zz"} {
		if w := request(s, "GET", path); w.Code < 400 {
			t.Errorf("accepted %s", path)
		}
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD"} {
		if w := request(s, method, "/3/movie/550"); w.Code != 405 {
			t.Errorf("accepted method %s", method)
		}
	}
	for _, h := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Auth-Token", "Cookie"} {
		if w := request(s, "GET", "/3/movie/550", h, "caller-secret"); w.Code != 400 {
			t.Errorf("accepted %s", h)
		}
	}
	if calls.Load() != before {
		t.Fatal("rejected request reached upstream")
	}
	s.cfg.versions = map[string]bool{"3": true}
	if w := request(s, "GET", "/4/list/1"); w.Code != 404 {
		t.Fatal("disabled version accepted")
	}
}

func TestFailuresAndLimits(t *testing.T) {
	t.Run("cache unavailable", func(t *testing.T) {
		var calls atomic.Int32
		s, db := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); jsonReply(w, `{"ok":true}`) })
		db.SetError("ERR unavailable")
		for i := 0; i < 2; i++ {
			w := request(s, "GET", "/3/movie/1")
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("cache failure broke successful response")
			}
		}
		if calls.Load() != 2 {
			t.Fatal("unexpected cache hit")
		}
		if w := request(s, "GET", "/readyz"); w.Code != 503 {
			t.Fatal("incorrect readiness")
		}
		if w := request(s, "GET", "/healthz"); w.Code != 200 {
			t.Fatal("incorrect liveness")
		}
	})
	t.Run("write failure", func(t *testing.T) {
		var db *miniredis.Miniredis
		s, cache := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			db.SetError("ERR write failed")
			jsonReply(w, `{"ok":true}`)
		})
		db = cache
		w := request(s, "GET", "/3/movie/1")
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("write failure broke response")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		s.cfg.tmdbTimeout = 30 * time.Millisecond
		if w := request(s, "GET", "/3/movie/1"); w.Code != 504 {
			t.Fatalf("timeout: %d", w.Code)
		}
	})
	t.Run("size", func(t *testing.T) {
		s, db := fixture(t, func(w http.ResponseWriter, r *http.Request) { jsonReply(w, `{"large":"payload"}`) })
		s.cfg.maxBytes = 5
		if w := request(s, "GET", "/3/movie/1"); w.Code != 502 || len(db.Keys()) != 0 {
			t.Fatal("response limit not applied")
		}
	})
	for _, body := range []string{`{"public":false,"secret":"private"}`, `{"id":1}`} {
		t.Run("private list", func(t *testing.T) {
			s, db := fixture(t, func(w http.ResponseWriter, r *http.Request) { jsonReply(w, body) })
			w := request(s, "GET", "/4/list/1")
			if w.Code != 404 || strings.Contains(w.Body.String(), "private") || len(db.Keys()) != 0 {
				t.Fatal("private list leaked")
			}
		})
	}
}

func TestNonCacheableAndRedirects(t *testing.T) {
	for _, mode := range []string{"invalid JSON", "non JSON", "no-store", "redirect", "secret"} {
		t.Run(mode, func(t *testing.T) {
			s, db := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "invalid JSON":
					jsonReply(w, `{`)
				case "non JSON":
					w.Header().Set("Content-Type", "text/plain")
					_, _ = io.WriteString(w, "hello")
				case "no-store":
					w.Header().Set("Cache-Control", "no-store")
					jsonReply(w, `{}`)
				case "redirect":
					w.Header().Set("Location", "http://127.0.0.1:1/private")
					w.WriteHeader(302)
				case "secret":
					jsonReply(w, `{"token":"operator-secret-token"}`)
				}
			})
			w := request(s, "GET", "/3/movie/1")
			if len(db.Keys()) != 0 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("unsafe caching")
			}
			if mode == "redirect" && w.Code != 302 {
				t.Fatal("redirect was followed")
			}
			if mode == "secret" && (w.Code != 502 || strings.Contains(w.Body.String(), "operator-secret-token")) {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestConfigAndExample(t *testing.T) {
	defaults, err := loadConfig(func(key string) string {
		if key == "TMDB_READ_ACCESS_TOKEN" {
			return "test-token"
		}
		return ""
	})
	if err != nil || len(defaults.valkey.InitAddress) != 1 || defaults.valkey.InitAddress[0] != "valkey:6379" {
		t.Fatal("default cache address must match the bundled Compose service")
	}
	data, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(k, "#") {
			env[k] = v
		}
	}
	env["TMDB_READ_ACCESS_TOKEN"] = "test-token"
	get := func(k string) string { return env[k] }
	if _, err := loadConfig(get); err != nil {
		t.Fatalf("example: %v", err)
	}
	for key, bad := range map[string]string{"TMDB_READ_ACCESS_TOKEN": "", "LISTEN_ADDR": "invalid", "VALKEY_URL": "https://user:secret@host", "TMDB_API_VERSIONS": "3,5", "CACHE_TTL_DETAILS_SECONDS": "0", "CACHE_TTL_SEARCH_SECONDS": "no", "CACHE_TTL_LISTS_SECONDS": "-1", "CACHE_TTL_TRENDING_SECONDS": "0", "CACHE_TTL_REFERENCE_SECONDS": "999999999999999", "TMDB_TIMEOUT_SECONDS": "0", "VALKEY_TIMEOUT_SECONDS": "0", "MAX_RESPONSE_BYTES": "0", "TMDB_REQUESTS_PER_SECOND": "0", "TMDB_QUEUE_SIZE": "-1", "CORS_ALLOWED_ORIGINS": "https://host/path", "LOG_LEVEL": "secret"} {
		old := env[key]
		env[key] = bad
		if _, err := loadConfig(get); err == nil {
			t.Errorf("accepted %s", key)
		} else if strings.Contains(err.Error(), "user:secret") {
			t.Fatal("secret in error")
		}
		env[key] = old
	}
	env["VALKEY_URL"] = "rediss://user:password@localhost:6380/1"
	c, err := loadConfig(get)
	if err != nil || c.valkey.TLSConfig == nil || c.valkey.TLSConfig.InsecureSkipVerify {
		t.Fatal("TLS config")
	}
}

func TestCORS(t *testing.T) {
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { jsonReply(w, `{}`) })
	s.cfg.origins["https://client.example"] = true
	w := request(s, "OPTIONS", "/3/movie/1", "Origin", "https://client.example", "Access-Control-Request-Method", "GET")
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "https://client.example" || w.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("CORS preflight")
	}
	if w = request(s, "OPTIONS", "/3/movie/1", "Origin", "https://other.example", "Access-Control-Request-Method", "GET"); w.Code != 405 {
		t.Fatal("unapproved origin")
	}
}

func TestResponsePreservation(t *testing.T) {
	for _, status := range []int{200, 201, 400, 401, 404, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			body := "{ \"unchanged\": \"/image.jpg\" }\n"
			s, db := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Language") != "" || r.Header.Get("If-None-Match") != "" {
					t.Error("caller headers forwarded")
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Retry-After", "Wed, 01 Jan 2031 00:00:00 GMT")
				w.Header().Set("ETag", `"version1"`)
				w.Header().Set("Set-Cookie", "private-cookie=secret")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			})
			for i := 0; i < 2; i++ {
				s.limiter = newUpstreamLimiter(s.cfg)
				w := request(s, "GET", "/3/movie/1", "Accept-Language", "fr", "If-None-Match", `"version1"`)
				if w.Code != status || w.Body.String() != body || w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("Retry-After") != "Wed, 01 Jan 2031 00:00:00 GMT" || w.Header().Get("ETag") != `"version1"` || w.Header().Get("Set-Cookie") != "" {
					t.Fatalf("response not preserved: %d %s", w.Code, w.Body)
				}
			}
			if status >= 300 && len(db.Keys()) != 0 {
				t.Fatal("error cached")
			}
		})
	}
}

func TestTTLCategories(t *testing.T) {
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { jsonReply(w, `{}`) })
	for path, want := range map[string]time.Duration{
		"/3/movie/1": s.cfg.ttlDetails, "/3/search/movie": s.cfg.ttlSearch, "/4/list/1": s.cfg.ttlLists, "/3/discover/tv": s.cfg.ttlLists, "/3/tv/popular": s.cfg.ttlLists, "/3/movie/1/lists": s.cfg.ttlLists, "/3/trending/all/day": s.cfg.ttlTrending, "/3/configuration": s.cfg.ttlReference, "/3/watch/providers/tv": s.cfg.ttlReference,
	} {
		if got := s.cfg.ttl(path); got != want {
			t.Errorf("%s TTL %v want %v", path, got, want)
		}
	}
}

func TestRemainingFreshness(t *testing.T) {
	s, db := fixture(t, func(w http.ResponseWriter, r *http.Request) { jsonReply(w, `{}`) })
	request(s, "GET", "/3/movie/1")
	key := db.Keys()[0]
	raw, _ := db.Get(key)
	var cached response
	if err := json.Unmarshal([]byte(raw), &cached); err != nil {
		t.Fatal(err)
	}
	cached.Expires = time.Now().Add(2500 * time.Millisecond)
	rawBytes, _ := json.Marshal(cached)
	_ = db.Set(key, string(rawBytes))
	db.SetTTL(key, 3*time.Second)
	w := request(s, "GET", "/3/movie/1")
	if w.Header().Get("Cache-Control") != "public, max-age=2, must-revalidate, proxy-revalidate" {
		t.Fatalf("freshness extended: %s", w.Header().Get("Cache-Control"))
	}
	cached.Expires = time.Now().Add(500 * time.Millisecond)
	rawBytes, _ = json.Marshal(cached)
	_ = db.Set(key, string(rawBytes))
	db.SetTTL(key, time.Second)
	w = request(s, "GET", "/3/movie/1")
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("subsecond freshness extended")
	}
}

func TestDotEnv(t *testing.T) {
	path := t.TempDir() + "/test.env"
	const key = "TMDB_RELAY_DOTENV_TEST"
	t.Setenv(key, "")
	_ = os.Unsetenv(key)
	if err := os.WriteFile(path, []byte("# comment\n"+key+"='literal$VALUE'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotEnv(path); err != nil || os.Getenv(key) != "literal$VALUE" {
		t.Fatal("literal dotenv not loaded")
	}
	t.Setenv(key, "process-value")
	if err := loadDotEnv(path); err != nil || os.Getenv(key) != "process-value" {
		t.Fatal("process environment was overwritten")
	}
	if err := os.WriteFile(path, []byte("invalid-secret-line"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotEnv(path); err == nil || strings.Contains(err.Error(), "invalid-secret-line") {
		t.Fatal("dotenv validation leaked a value")
	}
	if err := loadDotEnv(path + ".missing"); err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamFreshness(t *testing.T) {
	for _, control := range []string{"private", `private="Set-Cookie"`, "no-cache", "no-store", "max-age=0", "max-age=5", "max-age=100, s-maxage=5"} {
		t.Run(control, func(t *testing.T) {
			s, db := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", control)
				w.Header().Set("Age", "2")
				jsonReply(w, `{}`)
			})
			w := request(s, "GET", "/3/movie/1")
			if strings.Contains(control, "5") {
				if len(db.Keys()) != 1 || db.TTL(db.Keys()[0]) > 3*time.Second || (!strings.Contains(w.Header().Get("Cache-Control"), "max-age=2") && !strings.Contains(w.Header().Get("Cache-Control"), "max-age=3")) {
					t.Fatalf("incorrect bounded TTL: %s", w.Header().Get("Cache-Control"))
				}
			} else if len(db.Keys()) != 0 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("upstream cache restriction ignored")
			}
		})
	}
}
