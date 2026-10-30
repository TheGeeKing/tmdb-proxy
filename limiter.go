package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// shortcut: this budget is per process; divide it across replicas sharing a token.
type upstreamLimiter struct {
	mu             sync.Mutex
	next, cooldown time.Time
	interval       time.Duration
	queue          chan struct{}
	changed        chan struct{}
	cooldownDate   string
}

func newUpstreamLimiter(c config) *upstreamLimiter {
	return &upstreamLimiter{interval: time.Second / time.Duration(c.tmdbRPS), queue: make(chan struct{}, c.tmdbQueueSize), changed: make(chan struct{})}
}

func (l *upstreamLimiter) wait(ctx context.Context) error {
	select {
	case l.queue <- struct{}{}:
		defer func() { <-l.queue }()
	default:
		return errors.New("queue full")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		now := time.Now()
		due := l.next
		if l.cooldown.After(now) {
			l.mu.Unlock()
			return errors.New("upstream cooldown")
		}
		changed := l.changed
		if !now.Before(due) {
			l.next = now.Add(l.interval)
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		timer := time.NewTimer(time.Until(due))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		case <-changed:
			timer.Stop()
		}
	}
}

func (l *upstreamLimiter) pause(value string) {
	now := time.Now()
	until := now.Add(time.Second)
	dateValue := ""
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		// Clamp before converting to a duration to avoid overflow on malformed headers.
		if seconds > 2592000 {
			seconds = 2592000
		}
		if seconds > 0 {
			until = now.Add(time.Duration(seconds) * time.Second)
		}
	} else if date, err := http.ParseTime(value); err == nil && date.After(until) {
		until = date
		dateValue = value
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if until.After(l.cooldown) {
		l.cooldown = until
		l.cooldownDate = dateValue
		close(l.changed)
		l.changed = make(chan struct{})
	}
}

// Estimate when the current queue drains; new arrivals can change this.
func (l *upstreamLimiter) retryAfter() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	due := l.next
	if l.cooldown.After(now) {
		if l.cooldownDate != "" {
			return l.cooldownDate
		}
		return strconv.FormatInt(int64((l.cooldown.Sub(now)+time.Second-1)/time.Second), 10)
	}
	if due.Before(now) {
		due = now
	}
	wait := due.Sub(now) + time.Duration(len(l.queue))*l.interval
	seconds := int64(wait / time.Second)
	if wait%time.Second > 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}
