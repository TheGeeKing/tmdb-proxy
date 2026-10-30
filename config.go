package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
)

type config struct {
	token, listen                                              string
	valkey                                                     valkey.ClientOption
	versions                                                   map[string]bool
	ttlDetails, ttlSearch, ttlLists, ttlTrending, ttlReference time.Duration
	tmdbTimeout, valkeyTimeout                                 time.Duration
	maxBytes                                                   int64
	origins                                                    map[string]bool
	level                                                      slog.Level
}

func loadConfig(get func(string) string) (config, error) {
	c := config{versions: map[string]bool{}, origins: map[string]bool{}}
	value := func(k, fallback string) string {
		if v := get(k); v != "" {
			return v
		}
		return fallback
	}
	c.token = get("TMDB_READ_ACCESS_TOKEN")
	if c.token == "" || strings.ContainsAny(c.token, " \t\r\n") || c.token == "replace-me" {
		return c, errors.New("TMDB_READ_ACCESS_TOKEN must contain a TMDB API Read Access Token without whitespace")
	}
	c.listen = value("LISTEN_ADDR", ":8080")
	_, port, err := net.SplitHostPort(c.listen)
	if err != nil {
		return c, errors.New("LISTEN_ADDR must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return c, errors.New("LISTEN_ADDR port must be 1..65535")
	}
	raw := value("VALKEY_URL", "redis://valkey:6379/0")
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("VALKEY_URL must be redis:// or rediss://host[:port]/db (optional username/password; no query or fragment)")
	}
	c.valkey, err = valkey.ParseURL(raw)
	if err != nil {
		return c, errors.New("VALKEY_URL has invalid connection settings")
	}
	c.valkey.DisableCache = true
	c.valkey.ForceSingleClient = true
	c.valkey.DisableRetry = true
	for _, v := range strings.Split(value("TMDB_API_VERSIONS", "3,4"), ",") {
		v = strings.TrimSpace(v)
		if v != "3" && v != "4" {
			return c, errors.New("TMDB_API_VERSIONS must be a comma-separated subset of 3,4")
		}
		c.versions[v] = true
	}
	for _, item := range []struct {
		k, def string
		dst    *time.Duration
	}{
		{"CACHE_TTL_DETAILS_SECONDS", "86400", &c.ttlDetails}, {"CACHE_TTL_SEARCH_SECONDS", "300", &c.ttlSearch}, {"CACHE_TTL_LISTS_SECONDS", "900", &c.ttlLists}, {"CACHE_TTL_TRENDING_SECONDS", "300", &c.ttlTrending}, {"CACHE_TTL_REFERENCE_SECONDS", "604800", &c.ttlReference}, {"TMDB_TIMEOUT_SECONDS", "15", &c.tmdbTimeout}, {"VALKEY_TIMEOUT_SECONDS", "2", &c.valkeyTimeout},
	} {
		n, e := strconv.ParseInt(value(item.k, item.def), 10, 64)
		if e != nil || n < 1 || n > 2592000 {
			return c, fmt.Errorf("%s must be an integer between 1 and 2592000", item.k)
		}
		*item.dst = time.Duration(n) * time.Second
	}
	c.maxBytes, err = strconv.ParseInt(value("MAX_RESPONSE_BYTES", "10485760"), 10, 64)
	if err != nil || c.maxBytes < 1 || c.maxBytes > 1073741824 {
		return c, errors.New("MAX_RESPONSE_BYTES must be an integer between 1 and 1073741824")
	}
	for _, o := range strings.Split(get("CORS_ALLOWED_ORIGINS"), ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o != "*" {
			u, e := url.Parse(o)
			if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				return c, errors.New("CORS_ALLOWED_ORIGINS must contain comma-separated http(s) origins or *")
			}
		}
		c.origins[o] = true
	}
	level := strings.ToLower(value("LOG_LEVEL", "info"))
	if level != "debug" && level != "info" && level != "warn" && level != "error" {
		return c, errors.New("LOG_LEVEL must be debug, info, warn, or error")
	}
	_ = c.level.UnmarshalText([]byte(level))
	c.valkey.Dialer.Timeout = c.valkeyTimeout
	c.valkey.ConnWriteTimeout = c.valkeyTimeout
	return c, nil
}

func envConfig() (config, error) { return loadConfig(os.Getenv) }
