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
	"sync"
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

func TestLevel3NoDoubleBooking(t *testing.T) {
	ts := newTestServer(t)
	seed(t, ts)

	const n = 50
	codes := make(chan int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all requests at the same moment
			code, _ := call(t, ts, "POST", "/bookings", fmt.Sprintf(`{"slot_id":"s1","name":"P%d","email":"p%d@example.com"}`, i, i))
			codes <- code
		}(i)
	}
	close(start)
	wg.Wait()
	close(codes)

	count := map[int]int{}
	for c := range codes {
		count[c]++
	}
	if count[201] != 1 || count[409] != n-1 {
		t.Fatalf("want exactly one 201 and %d 409s, got %v", n-1, count)
	}
	if code, body := call(t, ts, "POST", "/bookings", `{"slot_id":"s1","name":"Late","email":"late@example.com"}`); code != 409 || body["error"] == nil {
		t.Errorf("booking a taken slot: %d %v", code, body)
	}
}

func TestLevel3Cancel(t *testing.T) {
	ts := newTestServer(t)
	seed(t, ts)

	_, b := call(t, ts, "POST", "/bookings", `{"slot_id":"s1","name":"Ada","email":"ada@example.com"}`)
	id := b["id"].(string)

	code, got := call(t, ts, "DELETE", "/bookings/"+id, "")
	if code != 200 || got["status"] != "cancelled" || got["id"] != id || got["created_at"] != b["created_at"] {
		t.Fatalf("cancel: %d %v", code, got)
	}
	if _, got := call(t, ts, "GET", "/bookings/"+id, ""); got["status"] != "cancelled" {
		t.Errorf("cancel not persisted: %v", got)
	}
	if _, body := call(t, ts, "GET", "/slots", ""); !strings.Contains(strings.Join(slotIDs(body), ","), "s1:free") {
		t.Errorf("slot not freed: %v", slotIDs(body))
	}
	if code, body := call(t, ts, "DELETE", "/bookings/"+id, ""); code != 409 || body["error"] == nil {
		t.Errorf("second cancel: %d %v", code, body)
	}
	if code, body := call(t, ts, "DELETE", "/bookings/bk_missing", ""); code != 404 || body["error"] == nil {
		t.Errorf("cancel unknown: %d %v", code, body)
	}

	// the freed slot can be booked again, and the old booking stays cancelled
	code, again := call(t, ts, "POST", "/bookings", `{"slot_id":"s1","name":"Bo","email":"bo@example.com"}`)
	if code != 201 || again["id"] == id {
		t.Fatalf("rebook freed slot: %d %v", code, again)
	}
	if _, got := call(t, ts, "GET", "/bookings/"+id, ""); got["status"] != "cancelled" {
		t.Errorf("old booking changed: %v", got)
	}
}

func TestLevel3ConcurrentCancel(t *testing.T) {
	ts := newTestServer(t)
	seed(t, ts)
	_, b := call(t, ts, "POST", "/bookings", `{"slot_id":"s1","name":"Ada","email":"ada@example.com"}`)

	const n = 20
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := call(t, ts, "DELETE", "/bookings/"+b["id"].(string), "")
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for c := range codes {
		count[c]++
	}
	if count[200] != 1 || count[409] != n-1 {
		t.Fatalf("want one 200 and %d 409s, got %v", n-1, count)
	}
}

// manySlots resets the API to n future slots s0..s(n-1).
func manySlots(t *testing.T, ts *httptest.Server, n int) {
	t.Helper()
	var slots []string
	for i := 0; i < n; i++ {
		start := time.Now().UTC().Add(time.Duration(24+i) * time.Hour).Format(time.RFC3339)
		end := time.Now().UTC().Add(time.Duration(25+i) * time.Hour).Format(time.RFC3339)
		slots = append(slots, fmt.Sprintf(`{"id":"s%d","provider":"Dr Ade","start":"%s","end":"%s"}`, i, start, end))
	}
	if code, out := call(t, ts, "POST", "/reset", `{"slots":[`+strings.Join(slots, ",")+`]}`); code != 200 {
		t.Fatalf("reset: %d %v", code, out)
	}
}

func TestLevel4Idempotency(t *testing.T) {
	ts := newTestServer(t)
	seed(t, ts)
	body := `{"slot_id":"s1","name":"Ada","email":"ada@example.com"}`

	code, first := call(t, ts, "POST", "/bookings", body, "Idempotency-Key", "k1")
	if code != 201 {
		t.Fatalf("first: %d %v", code, first)
	}
	for i := 0; i < 3; i++ {
		code, again := call(t, ts, "POST", "/bookings", body, "Idempotency-Key", "k1")
		if code != 201 || again["id"] != first["id"] || again["created_at"] != first["created_at"] {
			t.Errorf("retry %d: %d %v, want 201 %v", i, code, again, first["id"])
		}
	}
	// same body, whitespace and key order differ: still the same request
	if code, again := call(t, ts, "POST", "/bookings", `{ "email":"ada@example.com", "name":"Ada", "slot_id":"s1" }`, "Idempotency-Key", "k1"); code != 201 || again["id"] != first["id"] {
		t.Errorf("reformatted body: %d %v", code, again)
	}
	if code, out := call(t, ts, "POST", "/bookings", `{"slot_id":"s2","name":"Ada","email":"ada@example.com"}`, "Idempotency-Key", "k1"); code != 422 || out["error"] == nil {
		t.Errorf("same key, different body: %d %v", code, out)
	}
	if _, list := call(t, ts, "GET", "/bookings?email=ada@example.com", ""); len(list["bookings"].([]any)) != 1 {
		t.Errorf("retries created extra bookings: %v", list)
	}

	// failures replay too: same status, no new booking even if the slot later frees up
	if code, _ := call(t, ts, "POST", "/bookings", `{"slot_id":"nope","name":"Ada","email":"ada@example.com"}`, "Idempotency-Key", "k2"); code != 404 {
		t.Errorf("k2 first: %d", code)
	}
	if code, _ := call(t, ts, "POST", "/bookings", `{"slot_id":"nope","name":"Ada","email":"ada@example.com"}`, "Idempotency-Key", "k2"); code != 404 {
		t.Errorf("k2 replay: %d", code)
	}

	// a different key is a different request
	if code, _ := call(t, ts, "POST", "/bookings", body, "Idempotency-Key", "k3"); code != 409 {
		t.Errorf("new key for taken slot: %d", code)
	}
	if code, _ := call(t, ts, "POST", "/bookings", body, "Idempotency-Key", strings.Repeat("x", 256)); code != 400 {
		t.Errorf("oversized key: %d", code)
	}
}

func TestLevel4IdempotencyConcurrent(t *testing.T) {
	ts := newTestServer(t)
	seed(t, ts)
	const n = 20
	ids := make(chan string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			code, b := call(t, ts, "POST", "/bookings", `{"slot_id":"s1","name":"Ada","email":"ada@example.com"}`, "Idempotency-Key", "same")
			if code != 201 {
				t.Errorf("concurrent retry: %d %v", code, b)
				return
			}
			ids <- b["id"].(string)
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("want one booking id across concurrent retries, got %v", seen)
	}
}

func TestLevel4Pagination(t *testing.T) {
	ts := newTestServer(t)
	manySlots(t, ts, 26)
	var want []string
	for i := 0; i < 23; i++ {
		email := "ada@example.com"
		if i%2 == 1 {
			email = "ADA@Example.com" // stored lower-cased, so same person
		}
		_, b := call(t, ts, "POST", "/bookings", fmt.Sprintf(`{"slot_id":"s%d","name":"Ada","email":"%s"}`, i, email))
		want = append(want, b["id"].(string))
	}
	call(t, ts, "POST", "/bookings", `{"slot_id":"s23","name":"Bo","email":"bo@example.com"}`)
	call(t, ts, "DELETE", "/bookings/"+want[5], "") // cancelled bookings are still listed

	var got []string
	cursor, pages := "", 0
	for {
		path := "/bookings?email=" + "Ada@Example.COM" + "&limit=10"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		code, page := call(t, ts, "GET", path, "")
		if code != 200 {
			t.Fatalf("page %d: %d %v", pages, code, page)
		}
		pages++
		for _, b := range page["bookings"].([]any) {
			got = append(got, b.(map[string]any)["id"].(string))
		}
		next, present := page["next_cursor"]
		if !present {
			t.Fatal("next_cursor key missing")
		}
		if next == nil {
			break
		}
		cursor = next.(string)
	}
	if pages != 3 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("pages=%d\n got  %v\n want %v", pages, got, want)
	}

	if _, page := call(t, ts, "GET", "/bookings?email=ada@example.com", ""); len(page["bookings"].([]any)) != 10 {
		t.Errorf("default limit should be 10, got %d", len(page["bookings"].([]any)))
	}
	if _, page := call(t, ts, "GET", "/bookings?email=ada@example.com&limit=23", ""); len(page["bookings"].([]any)) != 23 || page["next_cursor"] != nil {
		t.Errorf("exact-fit page must end with null cursor: %v", page["next_cursor"])
	}
	if _, page := call(t, ts, "GET", "/bookings?email=nobody@example.com", ""); page["bookings"] == nil || len(page["bookings"].([]any)) != 0 || page["next_cursor"] != nil {
		t.Errorf("unknown email: %v", page)
	}
	for _, q := range []string{"limit=0", "limit=51", "limit=-1", "limit=abc", "limit=1.5", "limit=", "cursor=%21%21", "cursor=bm9wZQ"} {
		if code, body := call(t, ts, "GET", "/bookings?email=ada@example.com&"+q, ""); code != 400 || body["error"] == nil {
			t.Errorf("%s: got %d %v", q, code, body)
		}
	}
	if code, _ := call(t, ts, "GET", "/bookings", ""); code != 400 {
		t.Errorf("missing email: %d", code)
	}
	for _, q := range []string{"limit=1", "limit=50"} {
		if code, _ := call(t, ts, "GET", "/bookings?email=ada@example.com&"+q, ""); code != 200 {
			t.Errorf("%s should be allowed: %d", q, code)
		}
	}
}
