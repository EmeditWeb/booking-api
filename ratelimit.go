package main

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// limiter allows at most limit attempts per key in any rolling window.
// shortcut: in-memory, so it is per instance and resets on restart; move to Postgres if we run >1 instance.
type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time // attempt times inside the window, oldest first
}

func newLimiter(limit int, window time.Duration) *limiter {
	l := &limiter{limit: limit, window: window, hits: map[string][]time.Time{}}
	go func() {
		for range time.Tick(window) {
			l.sweep(time.Now())
		}
	}()
	return l
}

// allow records an attempt for key at now if it is within the limit. When it is not,
// the attempt is not recorded and wait is how long until the oldest one leaves the window.
func (l *limiter) allow(key string, now time.Time) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := inWindow(l.hits[key], now.Add(-l.window))
	if len(recent) >= l.limit {
		l.hits[key] = recent
		return false, recent[0].Add(l.window).Sub(now)
	}
	l.hits[key] = append(recent, now)
	return true, 0
}

// sweep drops keys with no attempts left in the window, so memory doesn't grow without bound.
func (l *limiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, times := range l.hits {
		if len(inWindow(times, now.Add(-l.window))) == 0 {
			delete(l.hits, k)
		}
	}
}

func (l *limiter) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits = map[string][]time.Time{}
}

func inWindow(times []time.Time, cutoff time.Time) []time.Time {
	for i, t := range times {
		if t.After(cutoff) {
			return times[i:]
		}
	}
	return nil
}

// retryAfterSeconds rounds up to whole seconds, at least 1, as the Retry-After header needs.
func retryAfterSeconds(wait time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(wait.Seconds()))))
}

// limitByIP is a flood guard on every route. The spec's per-email booking limit is separate.
func (s *server) limitByIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := s.ipLimit.allow(clientIP(r), time.Now()); !ok {
			w.Header().Set("Retry-After", retryAfterSeconds(wait))
			writeError(w, http.StatusTooManyRequests, "too many requests, slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP uses the last X-Forwarded-For entry, which our hosting proxy appends and a
// client cannot forge, falling back to the connection address when there is no proxy.
func clientIP(r *http.Request) string {
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		hops := strings.Split(xff[len(xff)-1], ",")
		if ip := strings.TrimSpace(hops[len(hops)-1]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
