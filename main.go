package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

//go:embed docs/openapi.json
var openapiJSON []byte

type server struct {
	db     *pgxpool.Pool
	apiKey string
}

func main() {
	dbURL, apiKey := os.Getenv("DATABASE_URL"), os.Getenv("API_KEY")
	if dbURL == "" || apiKey == "" {
		log.Fatal("DATABASE_URL and API_KEY must be set")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := openDB(ctx, dbURL)
	if err != nil {
		log.Fatal(err)
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           (&server{db: db, apiKey: apiKey}).routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("listening on :%s", port)
	log.Fatal(srv.ListenAndServe())
}

// openDB connects and applies the schema.
func openDB(ctx context.Context, url string) (*pgxpool.Pool, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if _, err := db.Exec(ctx, schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return db, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reset", s.handleReset)
	mux.HandleFunc("GET /slots", s.handleListSlots)
	mux.HandleFunc("POST /bookings", s.handleCreateBooking)
	mux.HandleFunc("GET /bookings/{id}", s.handleGetBooking)
	mux.HandleFunc("GET /docs", serveDocs)
	mux.HandleFunc("GET /openapi.json", serveSpec)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	return http.MaxBytesHandler(s.requireAPIKey(mux), 1<<20) // 1 MB: room for a large /reset, still bounded
}

// publicPaths are the web page and API docs; every other path, including unknown ones, needs the key.
var publicPaths = map[string]bool{"/": true, "/docs": true, "/openapi.json": true}

func (s *server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if !publicPaths[r.URL.Path] && subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}
