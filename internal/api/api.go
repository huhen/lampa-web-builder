// Package api is the HTTP surface of the builder (spec §3): everything but
// /healthz requires the X-API-Key header compared in constant time.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/huhen/lampa-web-builder/internal/builder"
	"github.com/huhen/lampa-web-builder/internal/config"
)

// maxBodyBytes bounds request bodies (a domain is tiny; anything larger is a
// client bug).
const maxBodyBytes = 64 << 10

// Server holds the builder and the reported version.
type Server struct {
	B       *builder.Builder
	Version string
}

// New returns the fully routed handler. Authentication wraps the whole mux
// (except /healthz) so the key is checked before any route existence — or its
// method — is revealed. Each path gets exactly one method wrapper (a second
// one on the same pattern panics); a path needing more than one method must
// branch inside its handler.
func New(b *builder.Builder, version string) http.Handler {
	s := &Server{B: b, Version: version}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", only(http.MethodGet, s.health))
	mux.HandleFunc("/api/v1/status", only(http.MethodGet, s.status))
	mux.HandleFunc("/api/v1/check", only(http.MethodPost, s.check))
	mux.HandleFunc("/api/v1/builds", only(http.MethodPost, s.createBuild))
	mux.HandleFunc("/api/v1/builds/{id}", only(http.MethodGet, s.getBuild))
	mux.HandleFunc("/api/v1/builds/{id}/archive", only(http.MethodGet, s.archive))
	mux.HandleFunc("/api/v1/builds/{id}/logs", only(http.MethodGet, s.logs))
	mux.HandleFunc("/", notFound)
	return s.auth(mux)
}

// only restricts a handler to one method, answering 405 (with Allow) in the
// API's {"error": ...} convention instead of the mux's text/plain. HEAD is
// accepted wherever GET is, matching net/http's method-pattern behaviour.
func only(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		head := r.Method == http.MethodHead && method == http.MethodGet
		if r.Method != method && !head {
			allow := method
			if method == http.MethodGet {
				allow = "GET, HEAD" // HEAD is served by the GET handler
			}
			w.Header().Set("Allow", allow)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		h(w, r)
	}
}

// notFound answers unknown paths in the API's JSON error convention.
func notFound(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

// auth guards everything but /healthz: the X-API-Key header must equal the
// configured key, compared in constant time.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-API-Key")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.B.APIKey())) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Write([]byte("ok"))
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	st := s.B.Status()
	st.Version = s.Version
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	triggered, err := s.B.CheckNow(r.Context())
	switch {
	case errors.Is(err, builder.ErrTestInProgress), errors.Is(err, builder.ErrCheckBusy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "busy"})
		return
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"test_triggered": triggered})
}

func (s *Server) createBuild(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Domain string `json:"domain"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if !config.ValidDomain(req.Domain) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "domain must match ^[a-z0-9.-]+$"})
		return
	}
	id, cached, err := s.B.OrderBuild(req.Domain)
	switch {
	case errors.Is(err, builder.ErrTestInProgress):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "test_build_in_progress"})
		return
	case errors.Is(err, builder.ErrNoCommit):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no_available_commit"})
		return
	case errors.Is(err, builder.ErrQueueFull):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "queue_full"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"build_id": id, "cached": cached})
}

func (s *Server) getBuild(w http.ResponseWriter, r *http.Request) {
	bd, ok := s.B.Build(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "build not found"})
		return
	}
	writeJSON(w, http.StatusOK, bd)
}

func (s *Server) archive(w http.ResponseWriter, r *http.Request) {
	// The file is opened under the builder lock (issue #27): eviction can no
	// longer remove it between the check and the open, and an open descriptor
	// keeps serving even if the directory goes away afterwards.
	f, err := s.B.OpenArchive(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "archive not found"})
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Explicit headers survive ServeContent; it adds Content-Length plus
	// Range and If-* handling on top.
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="archive.tar.gz"`)
	http.ServeContent(w, r, "archive.tar.gz", fi.ModTime(), f)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	f, err := s.B.OpenLog(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "logs not found"})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Stream instead of reading the whole log into memory; the write error is
	// ignored exactly as the previous w.Write call ignored it.
	io.Copy(w, f)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
