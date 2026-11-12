package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/mail"
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

func (s *server) handleCreateBooking(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SlotID string `json:"slot_id"`
		Name   string `json:"name"`
		Email  string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with string fields slot_id, name and email")
		return
	}
	slotID, name := strings.TrimSpace(in.SlotID), strings.TrimSpace(in.Name)
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if slotID == "" || name == "" || email == "" {
		writeError(w, http.StatusBadRequest, "slot_id, name and email are required")
		return
	}
	if !validEmail(email) {
		writeError(w, http.StatusBadRequest, "email is not valid")
		return
	}

	b, err := s.createBooking(r.Context(), slotID, name, email)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "slot not found")
	case errors.Is(err, errSlotStarted):
		writeError(w, http.StatusBadRequest, "slot has already started")
	case err != nil:
		serverError(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, b)
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
