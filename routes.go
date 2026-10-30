package main

import (
	_ "embed"
	"net/url"
	"regexp"
	"strings"
	"time"
)

//go:embed routes.txt
var routeText string

var routes = compileRoutes()

func compileRoutes() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, line := range strings.Fields(strings.TrimPrefix(routeText, "\ufeff")) {
		parts := strings.Split(line, "/")
		for i, p := range parts {
			if strings.HasPrefix(p, "{") {
				switch p {
				case "{time_window}":
					parts[i] = "(?:day|week)"
				case "{external_id}", "{credit_id}", "{review_id}", "{tv_episode_group_id}":
					parts[i] = "[A-Za-z0-9_:-]+(?:[.][A-Za-z0-9_:-]+)*"
				case "{season_number}", "{episode_number}":
					parts[i] = "[0-9]+"
				default:
					parts[i] = "[1-9][0-9]*"
				}
			} else {
				parts[i] = regexp.QuoteMeta(p)
			}
		}
		out = append(out, regexp.MustCompile("^"+strings.Join(parts, "/")+"$"))
	}
	return out
}
func allowedPath(path string) bool {
	for _, r := range routes {
		if r.MatchString(path) {
			return true
		}
	}
	return false
}
func safeQuery(path string, q url.Values) bool {
	for k, values := range q {
		normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(k))
		if strings.Contains(normalized, "token") || strings.Contains(normalized, "session") || strings.Contains(normalized, "authorization") || strings.Contains(normalized, "apikey") || normalized == "key" || normalized == "password" || normalized == "username" {
			return false
		}
		if strings.EqualFold(k, "append_to_response") {
			if k != "append_to_response" {
				return false
			}
			for _, v := range values {
				for _, sub := range strings.Split(v, ",") {
					if sub == "" || !allowedPath(path+"/"+sub) {
						return false
					}
				}
			}
		}
	}
	return true
}
func (c config) ttl(path string) time.Duration {
	p := strings.Split(path, "/")
	switch p[2] {
	case "search":
		return c.ttlSearch
	case "trending":
		return c.ttlTrending
	case "configuration", "certification", "genre", "watch":
		return c.ttlReference
	case "list", "discover":
		return c.ttlLists
	}
	if strings.HasSuffix(path, "/lists") || (len(p) == 4 && (p[3] == "popular" || p[3] == "top_rated" || p[3] == "now_playing" || p[3] == "upcoming" || p[3] == "airing_today" || p[3] == "on_the_air")) {
		return c.ttlLists
	}
	return c.ttlDetails
}
