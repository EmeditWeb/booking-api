package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testKey = "test-key"

// newTestServer returns a running server backed by TEST_DATABASE_URL with an empty database.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := openDB(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), "TRUNCATE slots, bookings, idempotency_keys"); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer((&server{db: db, apiKey: testKey}).routes())
	t.Cleanup(func() { ts.Close(); db.Close() })
	return ts
}

// call sends a request with the test API key and returns the status and decoded JSON body.
func call(t *testing.T, ts *httptest.Server, method, path, body string, headers ...string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", testKey)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	if len(raw) > 0 && json.Unmarshal(raw, &out) != nil {
		t.Fatalf("%s %s: non-JSON body %q", method, path, raw)
	}
	return res.StatusCode, out
}

func TestScaffold(t *testing.T) {
	ts := newTestServer(t)

	if code, body := call(t, ts, "GET", "/nope", ""); code != 404 || body["error"] != "not found" {
		t.Errorf("unknown route: got %d %v", code, body)
	}
	if code, body := call(t, ts, "GET", "/openapi.json", ""); code != 200 || body["openapi"] != "3.0.3" {
		t.Errorf("spec: got %d", code)
	}
	res, err := http.Get(ts.URL + "/docs")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("docs: %v %v", err, res)
	}
	res.Body.Close()
}

// seed resets the API to three future slots and returns their start times by id.
func seed(t *testing.T, ts *httptest.Server) map[string]string {
	t.Helper()
	at := func(h int) string {
		return time.Now().UTC().Add(time.Duration(h) * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	}
	starts := map[string]string{"s1": at(48), "s2": at(24), "s3": at(72)}
	body := `{"slots":[
		{"id":"s1","provider":"Dr Ade","start":"` + starts["s1"] + `","end":"` + at(49) + `"},
		{"id":"s2","provider":"Dr Bisi","start":"` + starts["s2"] + `","end":"` + at(25) + `"},
		{"id":"s3","provider":"Dr Ade","start":"` + starts["s3"] + `","end":"` + at(73) + `"}]}`
	if code, out := call(t, ts, "POST", "/reset", body); code != 200 {
		t.Fatalf("reset: %d %v", code, out)
	}
	return starts
}

// slotIDs returns the ids of a /slots response in order, with availability as a suffix ("s1:true").
func slotIDs(body map[string]any) []string {
	var ids []string
	for _, v := range body["slots"].([]any) {
		m := v.(map[string]any)
		ids = append(ids, m["id"].(string)+":"+map[bool]string{true: "free", false: "taken"}[m["available"].(bool)])
	}
	return ids
}

func TestLevel1(t *testing.T) {
	ts := newTestServer(t)
	starts := seed(t, ts)

	_, body := call(t, ts, "GET", "/slots", "")
	if got := strings.Join(slotIDs(body), ","); got != "s2:free,s1:free,s3:free" {
		t.Errorf("slots not sorted by start: %s", got)
	}
	first := body["slots"].([]any)[1].(map[string]any)
	if first["provider"] != "Dr Ade" || first["start"] != starts["s1"] {
		t.Errorf("slot fields: %v", first)
	}

	_, body = call(t, ts, "GET", "/slots?provider=Dr%20Ade", "")
	if got := strings.Join(slotIDs(body), ","); got != "s1:free,s3:free" {
		t.Errorf("provider filter: %s", got)
	}
	_, body = call(t, ts, "GET", "/slots?provider=Dr", "")
	if got := len(body["slots"].([]any)); got != 0 {
		t.Errorf("provider filter must be exact, got %d slots", got)
	}

	code, booking := call(t, ts, "POST", "/bookings", `{"slot_id":"s1","name":"Ada Obi","email":"ada@example.com"}`)
	if code != 201 || booking["status"] != "confirmed" || booking["slot_id"] != "s1" || booking["id"] == "" || booking["created_at"] == nil {
		t.Fatalf("create booking: %d %v", code, booking)
	}

	_, body = call(t, ts, "GET", "/slots", "")
	if got := strings.Join(slotIDs(body), ","); got != "s2:free,s1:taken,s3:free" {
		t.Errorf("availability after booking: %s", got)
	}

	code, got := call(t, ts, "GET", "/bookings/"+booking["id"].(string), "")
	if code != 200 || got["id"] != booking["id"] || got["email"] != "ada@example.com" {
		t.Errorf("get booking: %d %v", code, got)
	}
	if code, body := call(t, ts, "GET", "/bookings/bk_missing", ""); code != 404 || body["error"] == nil {
		t.Errorf("missing booking: %d %v", code, body)
	}

	// reset wipes bookings and replaces slots
	seed(t, ts)
	if code, _ := call(t, ts, "GET", "/bookings/"+booking["id"].(string), ""); code != 404 {
		t.Errorf("booking survived reset: %d", code)
	}
}

func TestResetValidation(t *testing.T) {
	ts := newTestServer(t)
	for name, body := range map[string]string{
		"not json":      `nope`,
		"no slots key":  `{}`,
		"missing field": `{"slots":[{"id":"s1","provider":"Dr Ade","start":"2026-12-10T09:00:00Z"}]}`,
		"bad time":      `{"slots":[{"id":"s1","provider":"Dr Ade","start":"tomorrow","end":"2026-12-10T09:30:00Z"}]}`,
		"duplicate id":  `{"slots":[{"id":"s1","provider":"A","start":"2026-12-10T09:00:00Z","end":"2026-12-10T09:30:00Z"},{"id":"s1","provider":"B","start":"2026-12-10T10:00:00Z","end":"2026-12-10T10:30:00Z"}]}`,
	} {
		if code, out := call(t, ts, "POST", "/reset", body); code != 400 || out["error"] == nil {
			t.Errorf("%s: got %d %v", name, code, out)
		}
	}
	// past slots are allowed, and an empty list is valid
	if code, _ := call(t, ts, "POST", "/reset", `{"slots":[{"id":"old","provider":"A","start":"2020-01-01T09:00:00Z","end":"2020-01-01T09:30:00Z"}]}`); code != 200 {
		t.Errorf("past slot rejected: %d", code)
	}
	// a realistic clinic schedule (500 slots, ~60 KB) must fit in the body limit
	var many []string
	for i := 0; i < 500; i++ {
		many = append(many, fmt.Sprintf(`{"id":"x%d","provider":"Dr Ade","start":"2027-01-01T09:00:00Z","end":"2027-01-01T09:30:00Z"}`, i))
	}
	if code, out := call(t, ts, "POST", "/reset", `{"slots":[`+strings.Join(many, ",")+`]}`); code != 200 {
		t.Errorf("500-slot reset: %d %v", code, out)
	}
	if code, _ := call(t, ts, "POST", "/reset", `{"slots":[]}`); code != 200 {
		t.Errorf("empty slot list rejected: %d", code)
	}
	if _, body := call(t, ts, "GET", "/slots", ""); body["slots"] == nil {
		t.Errorf("empty slot list must be [], got %v", body)
	}
}

func TestLevel2APIKey(t *testing.T) {
	ts := newTestServer(t)
	seed(t, ts)
	routes := [][2]string{
		{"POST", "/reset"}, {"GET", "/slots"}, {"POST", "/bookings"},
		{"GET", "/bookings/bk_x"}, {"DELETE", "/bookings/bk_x"}, {"GET", "/bookings?email=a@b.co"},
	}
	for _, key := range []string{"", "wrong-key", testKey + "x"} {
		for _, rt := range routes {
			code, body := call(t, ts, rt[0], rt[1], `{}`, "X-API-Key", key)
			if code != 401 || body["error"] == nil {
				t.Errorf("%s %s with key %q: got %d %v", rt[0], rt[1], key, code, body)
			}
		}
	}
	for _, path := range []string{"/docs", "/openapi.json"} {
		res, err := http.Get(ts.URL + path)
		if err != nil || res.StatusCode != 200 {
			t.Errorf("%s must be public: %v %v", path, err, res)
		}
		res.Body.Close()
	}
}

func TestLevel2Validation(t *testing.T) {
	ts := newTestServer(t)
	seed(t, ts)
	cases := []struct {
		name, body string
		want       int
	}{
		{"not json", `nope`, 400},
		{"empty object", `{}`, 400},
		{"missing slot_id", `{"name":"Ada","email":"ada@example.com"}`, 400},
		{"missing name", `{"slot_id":"s1","email":"ada@example.com"}`, 400},
		{"missing email", `{"slot_id":"s1","name":"Ada"}`, 400},
		{"empty name", `{"slot_id":"s1","name":"","email":"ada@example.com"}`, 400},
		{"whitespace name", `{"slot_id":"s1","name":"   ","email":"ada@example.com"}`, 400},
		{"whitespace email", `{"slot_id":"s1","name":"Ada","email":"  "}`, 400},
		{"wrong type", `{"slot_id":"s1","name":42,"email":"ada@example.com"}`, 400},
		{"email no at", `{"slot_id":"s1","name":"Ada","email":"ada.example.com"}`, 400},
		{"email no domain", `{"slot_id":"s1","name":"Ada","email":"ada@"}`, 400},
		{"email no local", `{"slot_id":"s1","name":"Ada","email":"@example.com"}`, 400},
		{"email no tld", `{"slot_id":"s1","name":"Ada","email":"ada@example"}`, 400},
		{"email with space", `{"slot_id":"s1","name":"Ada","email":"ada obi@example.com"}`, 400},
		{"email display name", `{"slot_id":"s1","name":"Ada","email":"Ada <ada@example.com>"}`, 400},
		{"email two ats", `{"slot_id":"s1","name":"Ada","email":"a@b@example.com"}`, 400},
		{"invalid email beats unknown slot", `{"slot_id":"nope","name":"Ada","email":"bad"}`, 400},
		{"unknown slot", `{"slot_id":"nope","name":"Ada","email":"ada@example.com"}`, 404},
	}
	for _, c := range cases {
		if code, body := call(t, ts, "POST", "/bookings", c.body); code != c.want || body["error"] == nil {
			t.Errorf("%s: got %d %v, want %d", c.name, code, body, c.want)
		}
	}

	// slot that has already started (and one in progress) → 400, but only after the 404 check
	past := `{"slots":[
		{"id":"old","provider":"Dr Ade","start":"2020-01-01T09:00:00Z","end":"2020-01-01T09:30:00Z"},
		{"id":"now","provider":"Dr Ade","start":"` + time.Now().UTC().Add(-time.Minute).Format(time.RFC3339) + `","end":"` + time.Now().UTC().Add(time.Hour).Format(time.RFC3339) + `"}]}`
	call(t, ts, "POST", "/reset", past)
	for _, id := range []string{"old", "now"} {
		if code, body := call(t, ts, "POST", "/bookings", `{"slot_id":"`+id+`","name":"Ada","email":"ada@example.com"}`); code != 400 || body["error"] == nil {
			t.Errorf("started slot %s: got %d %v", id, code, body)
		}
	}

	// trimming and lower-casing
	seed(t, ts)
	code, b := call(t, ts, "POST", "/bookings", `{"slot_id":"s1","name":"  Ada Obi  ","email":"  Ada.Obi@Example.COM "}`)
	if code != 201 || b["name"] != "Ada Obi" || b["email"] != "ada.obi@example.com" {
		t.Errorf("normalisation: %d %v", code, b)
	}
	if _, got := call(t, ts, "GET", "/bookings/"+b["id"].(string), ""); got["email"] != "ada.obi@example.com" {
		t.Errorf("stored email not normalised: %v", got)
	}
}
