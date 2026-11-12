package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// serverError logs the cause and returns a generic 500, so internals never leak to clients.
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func (s *server) handleReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slots []Slot `json:"slots"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Slots == nil {
		writeError(w, http.StatusBadRequest, "body must be {\"slots\": [...]} with RFC 3339 start and end times")
		return
	}
	for _, sl := range in.Slots {
		if sl.ID == "" || sl.Provider == "" || sl.Start.IsZero() || sl.End.IsZero() {
			writeError(w, http.StatusBadRequest, "every slot needs id, provider, start and end")
			return
		}
	}
	if err := s.resetSlots(r.Context(), in.Slots); err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusBadRequest, "slot ids must be unique")
			return
		}
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handleListSlots(w http.ResponseWriter, r *http.Request) {
	slots, err := s.listSlots(r.Context(), r.URL.Query().Get("provider"))
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"slots": slots})
}

type bookingRequest struct {
	SlotID string `json:"slot_id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
}

// hash identifies a request body by its field values, so formatting and key order don't matter.
func (in bookingRequest) hash() string {
	sum := sha256.Sum256([]byte(in.SlotID + "\x00" + in.Name + "\x00" + in.Email))
	return hex.EncodeToString(sum[:])
}

func (s *server) handleCreateBooking(w http.ResponseWriter, r *http.Request) {
	var in bookingRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with string fields slot_id, name and email")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 255 {
		writeError(w, http.StatusBadRequest, "Idempotency-Key must be at most 255 characters")
		return
	}

	var status int
	var body []byte
	var err error
	if key == "" {
		status, body, err = s.book(r.Context(), s.db, in)
	} else {
		status, body, err = s.once(r.Context(), key, in.hash(), func(q querier) (int, []byte, error) {
			return s.book(r.Context(), q, in)
		})
	}
	switch {
	case errors.Is(err, errKeyReused):
		writeError(w, http.StatusUnprocessableEntity, "Idempotency-Key was already used with a different body")
	case err != nil:
		serverError(w, r, err)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
	}
}

// book validates and creates a booking, returning the status and JSON body to send.
// The error is only for unexpected failures (500).
func (s *server) book(ctx context.Context, q querier, in bookingRequest) (int, []byte, error) {
	slotID, name := strings.TrimSpace(in.SlotID), strings.TrimSpace(in.Name)
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if slotID == "" || name == "" || email == "" {
		return errorBody(http.StatusBadRequest, "slot_id, name and email are required")
	}
	if !validEmail(email) {
		return errorBody(http.StatusBadRequest, "email is not valid")
	}

	b, err := createBooking(ctx, q, slotID, name, email)
	switch {
	case errors.Is(err, errNotFound):
		return errorBody(http.StatusNotFound, "slot not found")
	case errors.Is(err, errSlotStarted):
		return errorBody(http.StatusBadRequest, "slot has already started")
	case errors.Is(err, errSlotTaken):
		return errorBody(http.StatusConflict, "slot already booked")
	case err != nil:
		return 0, nil, err
	}
	body, err := json.Marshal(b)
	return http.StatusCreated, body, err
}

func errorBody(status int, msg string) (int, []byte, error) {
	body, err := json.Marshal(map[string]string{"error": msg})
	return status, body, err
}

func (s *server) handleListBookings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	email := strings.ToLower(strings.TrimSpace(q.Get("email")))
	if email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	limit := 10
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > 50 {
			writeError(w, http.StatusBadRequest, "limit must be an integer from 1 to 50")
			return
		}
		limit = n
	}
	var after int64
	if c := q.Get("cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		if err == nil {
			after, err = strconv.ParseInt(string(raw), 10, 64)
		}
		if err != nil || after < 1 {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}

	bookings, next, err := s.listBookings(r.Context(), email, after, limit)
	if err != nil {
		serverError(w, r, err)
		return
	}
	var nextCursor *string // nil encodes as null on the last page
	if next > 0 {
		c := base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(next, 10)))
		nextCursor = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"bookings": bookings, "next_cursor": nextCursor})
}

func (s *server) handleCancelBooking(w http.ResponseWriter, r *http.Request) {
	b, err := s.cancelBooking(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "booking not found")
	case errors.Is(err, errCancelled):
		writeError(w, http.StatusConflict, "booking already cancelled")
	case err != nil:
		serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, b)
	}
}

// validEmail accepts a bare address (no display name) whose domain has a dot, e.g. ada@example.com.
func validEmail(e string) bool {
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e || len(e) > 254 {
		return false
	}
	domain := e[strings.LastIndex(e, "@")+1:]
	return strings.Contains(domain, ".") && !strings.HasSuffix(domain, ".")
}

func (s *server) handleGetBooking(w http.ResponseWriter, r *http.Request) {
	b, err := s.getBooking(r.Context(), r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "booking not found")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func serveSpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(openapiJSON)
}

// Same Swagger UI build the official spec page uses, with "Try it out" and "Authorize" enabled.
const docsHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Clinic Booking API docs</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css">
<style>body{margin:0}.swagger-ui .topbar{display:none}</style>
</head>
<body>
<div id="ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
<script>
SwaggerUIBundle({url: "/openapi.json", dom_id: "#ui", docExpansion: "list",
  defaultModelsExpandDepth: 0, deepLinking: true, persistAuthorization: true});
</script>
</body>
</html>`

func serveDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(docsHTML))
}
