package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterSlidingWindow(t *testing.T) {
	l := &limiter{limit: 5, window: time.Minute, hits: map[string][]time.Time{}}
	t0 := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		if ok, _ := l.allow("a", t0.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	ok, wait := l.allow("a", t0.Add(10*time.Second))
	if ok || wait != 50*time.Second {
		t.Fatalf("6th attempt: ok=%v wait=%v, want blocked for 50s", ok, wait)
	}
	if ok, _ := l.allow("b", t0.Add(10*time.Second)); !ok {
		t.Error("other keys must not be affected")
	}
	// rejected attempts aren't recorded, so the first slot frees exactly when the oldest attempt expires
	if ok, _ := l.allow("a", t0.Add(59*time.Second)); ok {
		t.Error("still inside the window at 59s")
	}
	if ok, _ := l.allow("a", t0.Add(60*time.Second+time.Millisecond)); !ok {
		t.Error("oldest attempt expired at 60s, one more should be allowed")
	}
	if ok, _ := l.allow("a", t0.Add(60*time.Second+2*time.Millisecond)); ok {
		t.Error("only one attempt freed up")
	}

	l.sweep(t0.Add(time.Hour))
	if len(l.hits) != 0 {
		t.Errorf("sweep left %d stale keys", len(l.hits))
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	for wait, want := range map[time.Duration]string{
		50 * time.Second: "50", 49*time.Second + time.Millisecond: "50", time.Millisecond: "1", 0: "1",
	} {
		if got := retryAfterSeconds(wait); got != want {
			t.Errorf("%v: got %s, want %s", wait, got, want)
		}
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	if got := clientIP(r); got != "10.0.0.9" {
		t.Errorf("no proxy: %s", got)
	}
	r.Header.Add("X-Forwarded-For", "6.6.6.6, 1.2.3.4") // first entry is client-supplied and can be forged
	if got := clientIP(r); got != "1.2.3.4" {
		t.Errorf("proxy: %s", got)
	}
}
