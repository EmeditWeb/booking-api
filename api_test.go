package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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
