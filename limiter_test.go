package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestLimiterPacesConcurrentMisses(t *testing.T) {
	l := newUpstreamLimiter(config{tmdbRPS: 100, tmdbQueueSize: 10})
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 5; i++ {
		wg.Go(func() {
			if err := l.wait(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("requests were not paced")
	}
}

func TestLimiterQueueBoundsAndCancellation(t *testing.T) {
	l := newUpstreamLimiter(config{tmdbRPS: 1, tmdbQueueSize: 1})
	l.queue <- struct{}{}
	if l.wait(context.Background()) == nil {
		t.Fatal("full queue accepted request")
	}
	<-l.queue
	l.next = time.Now().Add(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if l.wait(ctx) == nil {
		t.Fatal("queue deadline ignored")
	}
	if len(l.queue) != 0 {
		t.Fatal("queue slot leaked")
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if l.wait(cancelled) == nil {
		t.Fatal("shutdown ignored")
	}
}

func TestCooldownAndCacheBypass(t *testing.T) {
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/3/movie/2" {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
			return
		}
		jsonReply(w, `{}`)
	})
	if request(s, "GET", "/3/movie/1").Code != 200 {
		t.Fatal("initial miss failed")
	}
	if request(s, "GET", "/3/movie/2").Code != 429 {
		t.Fatal("upstream 429 lost")
	}
	if request(s, "GET", "/3/movie/1").Code != 200 {
		t.Fatal("cache hit was throttled")
	}
	w := request(s, "GET", "/3/movie/3")
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cooldown did not reject miss")
	}
}

func TestRetryAfterParsing(t *testing.T) {
	for _, value := range []string{"5", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), "invalid", "0", "9223372036854775807"} {
		l := newUpstreamLimiter(config{tmdbRPS: 30, tmdbQueueSize: 1})
		before := time.Now()
		l.pause(value)
		if !l.cooldown.After(before) {
			t.Fatalf("missing cooldown for %q", value)
		}
		until := l.cooldown
		l.pause("1")
		if l.cooldown.Before(until) {
			t.Fatal("cooldown shortened")
		}
	}
}

func TestQueueRetryAfter(t *testing.T) {
	l := newUpstreamLimiter(config{tmdbRPS: 1, tmdbQueueSize: 2})
	if l.retryAfter() != "1" {
		t.Fatal("retry delay must be at least one second")
	}
	l.queue <- struct{}{}
	l.queue <- struct{}{}
	if l.retryAfter() != "2" {
		t.Fatal("retry delay must include queued requests")
	}
	l.next = time.Now().Add(500 * time.Millisecond)
	if l.retryAfter() != "3" {
		t.Fatal("retry delay must round up")
	}
}

func TestCooldownReleasesQueuedRequests(t *testing.T) {
	l := newUpstreamLimiter(config{tmdbRPS: 1, tmdbQueueSize: 2})
	l.next = time.Now().Add(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.wait(ctx) }()
	for len(l.queue) == 0 {
		if ctx.Err() != nil {
			t.Fatal("request did not enter queue")
		}
		time.Sleep(time.Millisecond)
	}
	l.pause("30")
	select {
	case err := <-done:
		if err == nil || ctx.Err() != nil {
			t.Fatal("queued request did not reject on cooldown")
		}
	case <-ctx.Done():
		t.Fatal("cooldown did not wake queued request")
	}
	if l.retryAfter() != "30" {
		t.Fatal("cooldown must not include queue delay")
	}
	date := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	l.pause(date)
	if l.retryAfter() != date {
		t.Fatal("TMDB HTTP date was not preserved")
	}
}
