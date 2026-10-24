// Package api is the config service's REST API.
//
// /v1/...           flow teams and (later) the UI: drafts, publishing, numbers.
//
//	Needs "Authorization: Bearer <ADMIN_TOKEN>".
//
// /internal/v1/...  the voice agent: which flow a number runs.
//
//	Needs "Authorization: Bearer <RUNTIME_TOKEN>".
//
// /healthz, /readyz no token.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/flowspec"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

const maxBody = 1 << 20 // 1 MB; a large flow is ~20 KB

var (
	identifierRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	e164RE       = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
)

type Server struct {
	Store        *store.Store
	AdminToken   string
	RuntimeToken string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.ready)

	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, requireToken(s.AdminToken, "admin", h)) }
	admin("POST /v1/validate", s.validate)
	admin("GET /v1/flows", s.listFlows)
	admin("GET /v1/flows/{flow_id}", s.getFlow)
	admin("PUT /v1/flows/{flow_id}/draft", s.putDraft)
	admin("GET /v1/flows/{flow_id}/draft", s.getDraft)
	admin("POST /v1/flows/{flow_id}/publish", s.publish)
	admin("GET /v1/flows/{flow_id}/versions", s.listVersions)
	admin("GET /v1/flows/{flow_id}/versions/{version}", s.getVersion)
	admin("GET /v1/numbers", s.listNumbers)
	admin("PUT /v1/numbers/{number}", s.putNumber)
	admin("DELETE /v1/numbers/{number}", s.deleteNumber)
	admin("GET /v1/audit", s.audit)

	mux.Handle("GET /internal/v1/resolve", requireToken(s.RuntimeToken, "runtime", http.HandlerFunc(s.resolve)))
	return logRequests(mux)
}

// ── Middleware ────────────────────────────────────────────────────────────────

type actorKey struct{}

// actor is who made a change, for the audit log. With only tokens it is the
// token's role; with Keycloak (Phase 5) it becomes the signed-in user.
func actor(r *http.Request) string {
	a, _ := r.Context().Value(actorKey{}).(string)
	return a
}

func requireToken(token, role string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or wrong token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, role+"-token")))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// logRequests logs one line per request: the route pattern (not the full URL),
// status and duration.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" {
			return
		}
		route := r.Pattern
		if route == "" {
			route = r.Method + " (no route)"
		}
		slog.Info("request", "route", route, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var invalid *store.InvalidError
	switch {
	case errors.As(err, &invalid):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "the draft has problems; fix them before publishing", "problems": invalid.Problems,
		})
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrNoDraft), errors.Is(err, store.ErrNotPublished):
		writeError(w, http.StatusConflict, err.Error())
	default:
		slog.Error("request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body is larger than 1 MB")
		return nil, false
	}
	return body, true
}

func flowID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("flow_id")
	if !identifierRE.MatchString(id) {
		writeError(w, http.StatusBadRequest,
			"flow_id must be lower-case letters, digits and _ , starting with a letter (max 48 characters)")
		return "", false
	}
	return id, true
}

// NormalizeNumber turns "+91 80-4567 8901", "918045678901" or "00918045678901"
// into E.164 ("+918045678901"). ok is false if it isn't a phone number.
func NormalizeNumber(raw string) (string, bool) {
	n := strings.Map(func(r rune) rune {
		if strings.ContainsRune(" -().\t", r) {
			return -1
		}
		return r
	}, raw)
	n = strings.TrimPrefix(n, "tel:")
	if strings.HasPrefix(n, "00") {
		n = "+" + n[2:]
	}
	if !strings.HasPrefix(n, "+") {
		n = "+" + n
	}
	return n, e164RE.MatchString(n)
}

func number(w http.ResponseWriter, raw string) (string, bool) {
	n, ok := NormalizeNumber(raw)
	if !ok {
		writeError(w, http.StatusBadRequest, "not a phone number; use E.164, e.g. +918045678901")
	}
	return n, ok
}

// checkSpec validates a flow body. The flow_id inside must match the URL's.
func checkSpec(body []byte, urlFlowID string) (decoded any, problems []string, err error) {
	decoded, err = flowspec.Decode(body)
	if err != nil {
		return nil, nil, err
	}
	problems = flowspec.Validate(decoded).Problems
	if root, ok := decoded.(*flowspec.Object); ok && urlFlowID != "" {
		if id, ok := root.Values["flow_id"].(string); ok && id != urlFlowID {
			problems = append(problems, fmt.Sprintf("flow_id: must be '%s', the flow this draft belongs to", urlFlowID))
		}
	}
	if problems == nil {
		problems = []string{}
	}
	return decoded, problems, nil
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database not reachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// POST /v1/validate — check a flow without saving it (for the UI, as you type).
func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	_, problems, err := checkSpec(body, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, "not valid JSON: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": len(problems) == 0, "problems": problems})
}

func (s *Server) listFlows(w http.ResponseWriter, r *http.Request) {
	flows, err := s.Store.ListFlows(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flows": flows})
}

func (s *Server) getFlow(w http.ResponseWriter, r *http.Request) {
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	f, err := s.Store.GetFlow(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// PUT /v1/flows/{flow_id}/draft — save the flow being edited (creates the flow
// on first save). Saving works even with problems, so work isn't lost; the
// problems come back in the response. Publishing needs zero problems.
func (s *Server) putDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	decoded, problems, err := checkSpec(body, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, "not valid JSON: "+err.Error())
		return
	}
	name := id
	if root, ok := decoded.(*flowspec.Object); ok {
		if n, ok := root.Values["name"].(string); ok && n != "" {
			name = n
		}
	}
	if err := s.Store.SaveDraft(r.Context(), id, name, body, problems, actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"flow_id": id, "valid": len(problems) == 0, "problems": problems, "checksum": store.Checksum(body),
	})
}

func (s *Server) getDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	d, err := s.Store.GetDraft(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// POST /v1/flows/{flow_id}/publish — the draft becomes the next version. Numbers
// that follow the latest version use it from their next call.
func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	v, unchanged, err := s.Store.Publish(r.Context(), id, actor(r), func(spec []byte) []string {
		_, problems, err := checkSpec(spec, id)
		if err != nil {
			return []string{"(top level): not valid JSON: " + err.Error()}
		}
		return problems
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusCreated
	if unchanged {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"flow_id": v.FlowID, "version": v.Version, "checksum": v.Checksum, "unchanged": unchanged,
		"published_at": v.PublishedAt, "published_by": v.PublishedBy,
	})
}

func (s *Server) listVersions(w http.ResponseWriter, r *http.Request) {
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	versions, err := s.Store.ListVersions(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || version < 1 {
		writeError(w, http.StatusBadRequest, "version must be a whole number from 1")
		return
	}
	v, err := s.Store.GetVersion(r.Context(), id, version)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listNumbers(w http.ResponseWriter, r *http.Request) {
	numbers, err := s.Store.ListNumbers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"numbers": numbers})
}

// PUT /v1/numbers/{number} {"flow_id": "...", "version_pin": null | 3}
func (s *Server) putNumber(w http.ResponseWriter, r *http.Request) {
	n, ok := number(w, r.PathValue("number"))
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		FlowID     string `json:"flow_id"`
		VersionPin *int   `json:"version_pin"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, `body must be {"flow_id": "...", "version_pin": null or a version}: `+err.Error())
		return
	}
	if !identifierRE.MatchString(req.FlowID) {
		writeError(w, http.StatusBadRequest, "flow_id is missing or not a valid id")
		return
	}
	saved, err := s.Store.PutNumber(r.Context(), n, req.FlowID, req.VersionPin, actor(r))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such flow, or no such version of it")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) deleteNumber(w http.ResponseWriter, r *http.Request) {
	n, ok := number(w, r.PathValue("number"))
	if !ok {
		return
	}
	if err := s.Store.DeleteNumber(r.Context(), n, actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	entries, err := s.Store.Audit(r.Context(), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// GET /internal/v1/resolve?number=+918045678901 — called by the voice agent at
// the start of every call. The ETag changes whenever the answer changes, so the
// agent sends If-None-Match and usually gets a tiny 304.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	n, ok := number(w, r.URL.Query().Get("number"))
	if !ok {
		return
	}
	res, err := s.Store.Resolve(r.Context(), n)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "this number isn't mapped to a published flow")
			return
		}
		s.fail(w, err)
		return
	}
	etag := fmt.Sprintf(`"%s.v%d.%s"`, res.FlowID, res.Version, res.Checksum[:16])
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache") // always revalidate
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
