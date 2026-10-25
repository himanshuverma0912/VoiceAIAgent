// Package api is the config service's REST API.
//
// /v1/...           Flow Studio and scripts: flows, drafts, publishing, numbers.
//
//	"Authorization: Bearer <token>" where the token is either a Keycloak access
//	token of a signed-in person (their roles and teams decide what they may do)
//	or ADMIN_TOKEN (scripts and CI; acts as a platform admin).
//
// /internal/v1/...  the voice agent: which flow a number runs.
//
//	Needs "Authorization: Bearer <RUNTIME_TOKEN>".
//
// /healthz, /readyz no token.
//
// Who may do what:
//
//	viewer          see the flows of their teams (everyone signed in has it)
//	flow-editor     create flows for their teams, save drafts
//	flow-publisher  publish their teams' flows
//	platform-admin  everything, in every team; phone numbers; audit log
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

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/flowspec"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

const maxBody = 1 << 20 // 1 MB; a large flow is ~20 KB

var (
	identifierRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	teamRE       = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	e164RE       = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
)

type Server struct {
	Store        *store.Store
	AdminToken   string
	RuntimeToken string
	Users        *auth.Verifier // nil: only ADMIN_TOKEN is accepted on /v1
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.ready)

	v1 := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.authenticate(h)) }
	v1("GET /v1/me", s.me)
	v1("POST /v1/validate", s.validate)
	v1("GET /v1/flows", s.listFlows)
	v1("GET /v1/flows/{flow_id}", s.getFlow)
	v1("PUT /v1/flows/{flow_id}/draft", s.putDraft)
	v1("GET /v1/flows/{flow_id}/draft", s.getDraft)
	v1("POST /v1/flows/{flow_id}/publish", s.publish)
	v1("PUT /v1/flows/{flow_id}/team", s.setTeam)
	v1("PUT /v1/flows/{flow_id}/layout", s.putLayout)
	v1("GET /v1/flows/{flow_id}/versions", s.listVersions)
	v1("GET /v1/flows/{flow_id}/versions/{version}", s.getVersion)
	v1("GET /v1/numbers", s.listNumbers)
	v1("PUT /v1/numbers/{number}", s.putNumber)
	v1("DELETE /v1/numbers/{number}", s.deleteNumber)
	v1("GET /v1/audit", s.audit)

	mux.Handle("GET /internal/v1/resolve", requireToken(s.RuntimeToken, http.HandlerFunc(s.resolve)))
	return logRequests(mux)
}

// ── Middleware ────────────────────────────────────────────────────────────────

type principalKey struct{}

func principal(r *http.Request) auth.Principal {
	p, _ := r.Context().Value(principalKey{}).(auth.Principal)
	return p
}

// actor is who made a change, for the audit log: the person's email, or
// "admin-token" for scripts.
func actor(r *http.Request) string { return principal(r).Actor() }

func bearer(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return strings.TrimSpace(token)
}

// authenticate accepts ADMIN_TOKEN or a Keycloak access token.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		var p auth.Principal
		switch {
		case token == "":
			writeError(w, http.StatusUnauthorized, "sign in first")
			return
		case subtle.ConstantTimeCompare([]byte(token), []byte(s.AdminToken)) == 1:
			p = auth.ScriptAdmin()
		case s.Users != nil:
			var err error
			if p, err = s.Users.Verify(r.Context(), token); err != nil {
				slog.Info("rejected token", "reason", err.Error())
				writeError(w, http.StatusUnauthorized, "your sign-in has expired or isn't valid; sign in again")
				return
			}
			if len(p.Roles) == 0 {
				writeError(w, http.StatusForbidden, "your account has no Flow Studio role")
				return
			}
		default:
			writeError(w, http.StatusUnauthorized, "missing or wrong token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or wrong token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requireAdmin(w http.ResponseWriter, r *http.Request, what string) bool {
	if !principal(r).IsAdmin() {
		writeError(w, http.StatusForbidden, "only platform admins can "+what)
		return false
	}
	return true
}

// flowFor loads a flow and checks the caller may use role on it. Flows of other
// teams answer 404, so their names don't leak.
func (s *Server) flowFor(w http.ResponseWriter, r *http.Request, role string) (*store.FlowSummary, bool) {
	id, ok := flowID(w, r)
	if !ok {
		return nil, false
	}
	f, err := s.Store.GetFlow(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	p := principal(r)
	if !p.IsAdmin() && (f.Team == "" || !p.InTeam(f.Team)) {
		writeError(w, http.StatusNotFound, "not found")
		return nil, false
	}
	if !p.Can(role, f.Team) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("you need the %s role for team %s", role, f.Team))
		return nil, false
	}
	return f, true
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
	case errors.Is(err, store.ErrNoDraft), errors.Is(err, store.ErrNotPublished), errors.Is(err, store.ErrConflict):
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

// GET /v1/me — who is signed in, with their roles and teams (for Flow Studio).
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, principal(r))
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
	p := principal(r)
	var teams []string // nil: every flow
	if !p.IsAdmin() {
		teams = p.Teams
	}
	flows, err := s.Store.ListFlows(r.Context(), teams)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flows": flows})
}

func (s *Server) getFlow(w http.ResponseWriter, r *http.Request) {
	if f, ok := s.flowFor(w, r, auth.RoleViewer); ok {
		writeJSON(w, http.StatusOK, f)
	}
}

// PUT /v1/flows/{flow_id}/draft — save the flow being edited (creates the flow
// on first save). Saving works even with problems, so work isn't lost; the
// problems come back in the response. Publishing needs zero problems.
// A new flow belongs to ?team=<name> (optional if you're in exactly one team).
func (s *Server) putDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	team, ok := s.draftTeam(w, r, id)
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
	if err := s.Store.SaveDraft(r.Context(), id, name, team, body, problems, actor(r), strings.Trim(r.Header.Get("If-Match"), `" `)); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"flow_id": id, "valid": len(problems) == 0, "problems": problems, "checksum": store.Checksum(body),
	})
}

// draftTeam checks the caller may save a draft of flow id, and returns the team
// a new flow will belong to.
func (s *Server) draftTeam(w http.ResponseWriter, r *http.Request, id string) (string, bool) {
	p := principal(r)
	if _, err := s.Store.GetFlow(r.Context(), id); err == nil {
		if r.Header.Get("If-None-Match") == "*" { // "create only" (Flow Studio's New flow)
			writeError(w, http.StatusConflict, fmt.Sprintf("a flow called %s already exists", id))
			return "", false
		}
		_, ok := s.flowFor(w, r, auth.RoleEditor)
		return "", ok
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return "", false
	}
	team := r.URL.Query().Get("team")
	if team == "" && len(p.Teams) == 1 {
		team = p.Teams[0]
	}
	switch {
	case team == "" && p.IsAdmin():
		return "", true // a platform admin may create an unassigned flow
	case team == "":
		writeError(w, http.StatusBadRequest, "say which of your teams owns the new flow: ?team=<name>")
		return "", false
	case !teamRE.MatchString(team):
		writeError(w, http.StatusBadRequest, "team must be lower-case letters, digits, - and _")
		return "", false
	case !p.Can(auth.RoleEditor, team):
		writeError(w, http.StatusForbidden, fmt.Sprintf("you need the %s role for team %s", auth.RoleEditor, team))
		return "", false
	}
	return team, true
}

func (s *Server) getDraft(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, auth.RoleViewer)
	if !ok {
		return
	}
	d, err := s.Store.GetDraft(r.Context(), f.FlowID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// POST /v1/flows/{flow_id}/publish — the draft becomes the next version. Numbers
// that follow the latest version use it from their next call.
func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, auth.RolePublisher)
	if !ok {
		return
	}
	id := f.FlowID
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

// PUT /v1/flows/{flow_id}/layout {"positions": {"<node id>": {"x": 10, "y": 20}}}
// — where Flow Studio draws the nodes.
func (s *Server) putLayout(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, auth.RoleEditor)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var layout struct {
		Positions map[string]struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		} `json:"positions"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&layout); err != nil || layout.Positions == nil || len(layout.Positions) > 500 {
		writeError(w, http.StatusBadRequest, `body must be {"positions": {"<node id>": {"x": <number>, "y": <number>}}}`)
		return
	}
	clean, _ := json.Marshal(layout)
	if err := s.Store.SetLayout(r.Context(), f.FlowID, clean); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PUT /v1/flows/{flow_id}/team {"team": "emi"} — give a flow to a team.
func (s *Server) setTeam(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "move flows between teams") {
		return
	}
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Team string `json:"team"`
	}
	if err := json.Unmarshal(body, &req); err != nil || !teamRE.MatchString(req.Team) {
		writeError(w, http.StatusBadRequest, `body must be {"team": "<name>"} with a lower-case team name`)
		return
	}
	if err := s.Store.SetTeam(r.Context(), id, req.Team, actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	f, err := s.Store.GetFlow(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) listVersions(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, auth.RoleViewer)
	if !ok {
		return
	}
	versions, err := s.Store.ListVersions(r.Context(), f.FlowID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, auth.RoleViewer)
	if !ok {
		return
	}
	id := f.FlowID
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
	if !requireAdmin(w, r, "see phone numbers") {
		return
	}
	numbers, err := s.Store.ListNumbers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"numbers": numbers})
}

// PUT /v1/numbers/{number} {"flow_id": "...", "version_pin": null | 3}
func (s *Server) putNumber(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "route phone numbers") {
		return
	}
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
	if !requireAdmin(w, r, "route phone numbers") {
		return
	}
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
	if !requireAdmin(w, r, "read the audit log") {
		return
	}
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
