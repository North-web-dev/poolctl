// Package server exposes the pool over HTTP.
package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/North-web-dev/poolctl/internal/pool"
)

// Server serves take/release/status/reload over HTTP.
type Server struct {
	pool   *pool.Pool
	apiKey string
	reload func() error
}

// New builds a Server. reload may be nil.
func New(p *pool.Pool, apiKey string, reload func() error) *Server {
	return &Server{pool: p, apiKey: apiKey, reload: reload}
}

// Handler returns the configured HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/take", s.auth(s.handleTake))
	mux.HandleFunc("/release", s.auth(s.handleRelease))
	mux.HandleFunc("/status", s.auth(s.handleStatus))
	mux.HandleFunc("/reload", s.auth(s.handleReload))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" && r.Header.Get("X-API-Key") != s.apiKey {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleTake(w http.ResponseWriter, r *http.Request) {
	l, ok := s.pool.Take()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no token available"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": l.ID, "token": l.Value})
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
		OK bool   `json:"ok"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "need {id, ok}"})
		return
	}
	s.pool.Release(body.ID, body.OK)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.pool.Status())
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if s.reload == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if err := s.reload(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}
