// Package api is the config service's REST API.
//
// /v1/...           Flow Studio and scripts: flows, drafts, publishing, numbers.
//
//	"Authorization: Bearer <token>" where the token is either a Keycloak access
//	token of a signed-in person (their roles and teams decide what they may do)
//	or ADMIN_TOKEN (scripts and CI; acts as a platform admin).
//
// /internal/v1/...  the voice agent: which flow a number runs; call history.
//
//	Needs "Authorization: Bearer <RUNTIME_TOKEN>".
//
// /healthz, /readyz no token.
//
// Who may do what: every flow belongs to a workspace, and people belong to
// workspaces (managed in Flow Studio, stored here):
//
//	member          see the workspace's flows, drafts, versions and calls
//	maker           create flows in it, save drafts, submit them for review
//	checker         approve (publishes) or reject other people's submissions
//	owner           add and remove members, set their roles
//	platform-admin  (a Keycloak role) all of the above in every workspace;
//	                workspaces, phone numbers, model catalog, audit log
//
// People never publish directly: a maker submits, a different person who is a
// checker approves. Only ADMIN_TOKEN (scripts, CI) may publish a draft directly.
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
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/builder"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/flowspec"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/livekit"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

const maxBody = 1 << 20 // 1 MB; a large flow is ~20 KB

var (
	identifierRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	workspaceRE  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	e164RE       = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
)

type Server struct {
	Store        *store.Store
	AdminToken   string
	RuntimeToken string
	Users        *auth.Verifier   // nil: only ADMIN_TOKEN is accepted on /v1
	LiveKit      livekit.Config   // for browser test calls; empty: switched off
	Builder      *builder.Builder // turns a written description into a flow; nil: switched off

	tests  rateLimiter
	builds buildJobs
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
	v1("PUT /v1/flows/{flow_id}/workspace", s.setWorkspace)
	v1("PUT /v1/flows/{flow_id}/layout", s.putLayout)
	v1("POST /v1/flows/{flow_id}/test-sessions", s.startTestSession)
	v1("GET /v1/flows/{flow_id}/versions", s.listVersions)
	v1("GET /v1/flows/{flow_id}/versions/{version}", s.getVersion)
	v1("GET /v1/numbers", s.listNumbers)
	v1("PUT /v1/numbers/{number}", s.putNumber)
	v1("DELETE /v1/numbers/{number}", s.deleteNumber)
	v1("GET /v1/audit", s.audit)
	v1("GET /v1/flows/{flow_id}/calls", s.listCalls)
	v1("GET /v1/flows/{flow_id}/calls/summary", s.callSummary)
	v1("GET /v1/workspaces", s.listWorkspaces)
	v1("PUT /v1/workspaces/{workspace}", s.putWorkspace)
	v1("DELETE /v1/workspaces/{workspace}", s.deleteWorkspace)
	v1("GET /v1/workspaces/{workspace}/members", s.listMembers)
	v1("PUT /v1/workspaces/{workspace}/members/{email}", s.putMember)
	v1("DELETE /v1/workspaces/{workspace}/members/{email}", s.removeMember)
	v1("POST /v1/flows/{flow_id}/reviews", s.submitReview)
	v1("GET /v1/flows/{flow_id}/reviews", s.listReviews)
	v1("GET /v1/flows/{flow_id}/reviews/{review_id}", s.getReview)
	v1("POST /v1/flows/{flow_id}/reviews/{review_id}/approve", s.approveReview)
	v1("POST /v1/flows/{flow_id}/reviews/{review_id}/reject", s.rejectReview)
	v1("POST /v1/flows/{flow_id}/reviews/{review_id}/withdraw", s.withdrawReview)
	v1("GET /v1/reviews", s.pendingReviews)
	v1("GET /v1/integrations", s.listIntegrations)
	v1("PUT /v1/integrations/{integration_id}", s.putIntegration)
	v1("DELETE /v1/integrations/{integration_id}", s.deleteIntegration)
	v1("POST /v1/builds", s.startBuild)
	v1("GET /v1/builds/{build_id}", s.getBuild)
	v1("GET /v1/catalog", s.listCatalog)
	v1("PUT /v1/catalog/{kind}/{model_id}", s.putCatalogEntry)
	v1("DELETE /v1/catalog/{kind}/{model_id}", s.deleteCatalogEntry)

	mux.Handle("GET /internal/v1/resolve", requireToken(s.RuntimeToken, http.HandlerFunc(s.resolve)))
	mux.Handle("GET /internal/v1/flows/{flow_id}/draft", requireToken(s.RuntimeToken, http.HandlerFunc(s.internalDraft)))
	mux.Handle("POST /internal/v1/calls", requireToken(s.RuntimeToken, http.HandlerFunc(s.addCall)))
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
			if err := s.loadMemberships(r.Context(), &p); err != nil {
				s.fail(w, err)
				return
			}
		default:
			writeError(w, http.StatusUnauthorized, "missing or wrong token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// loadMemberships fills in p's workspaces. The first time someone signs in
// with Keycloak team groups, those become their first memberships.
func (s *Server) loadMemberships(ctx context.Context, p *auth.Principal) error {
	var teams []string
	for _, t := range p.Teams {
		if workspaceRE.MatchString(t) {
			teams = append(teams, t)
		}
	}
	if len(teams) > 0 {
		if err := s.Store.ImportDirectoryMemberships(ctx, p.Email, teams, p.HasRole(auth.RoleEditor), p.HasRole(auth.RolePublisher)); err != nil {
			return err
		}
	}
	m, err := s.Store.Memberships(ctx, p.Email)
	if err != nil {
		return err
	}
	p.Workspaces = m
	return nil
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

// What a request needs to do with a flow.
type need int

const (
	toView    need = iota // any member of the workspace
	toEdit                // a maker
	toApprove             // a checker
)

// flowFor loads a flow and checks the caller may act on it. Flows of
// workspaces the caller isn't in answer 404, so their names don't leak.
func (s *Server) flowFor(w http.ResponseWriter, r *http.Request, n need) (*store.FlowSummary, bool) {
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
	if !p.IsAdmin() && !p.Sees(f.Workspace) {
		writeError(w, http.StatusNotFound, "not found")
		return nil, false
	}
	switch {
	case n == toEdit && !p.CanEdit(f.Workspace):
		writeError(w, http.StatusForbidden, "you need to be a maker in workspace "+f.Workspace)
		return nil, false
	case n == toApprove && !p.CanApprove(f.Workspace):
		writeError(w, http.StatusForbidden, "you need to be a checker in workspace "+f.Workspace)
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
	case errors.Is(err, store.ErrNoWorkspace):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrNoDraft), errors.Is(err, store.ErrNotPublished), errors.Is(err, store.ErrConflict),
		errors.Is(err, store.ErrLastOwner), errors.Is(err, store.ErrWorkspaceNotEmpty),
		errors.Is(err, store.ErrUnchangedDraft), errors.Is(err, store.ErrNotPending):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrSelfApproval), errors.Is(err, store.ErrNotSubmitter), errors.Is(err, store.ErrOwnRejection):
		writeError(w, http.StatusForbidden, err.Error())
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

// GET /v1/me — who is signed in: global roles and workspace memberships.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, principal(r))
}

// POST /v1/validate[?workspace=x] — check a flow without saving it (for the UI,
// as you type). With workspace, the model catalog is checked as it would be there.
func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	_, problems, err := s.checkFlow(r.Context(), body, "", workspaceParam(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, "not valid JSON: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": len(problems) == 0, "problems": problems})
}

// workspaceParam reads ?workspace= (or the older ?team=).
func workspaceParam(r *http.Request) string {
	if ws := r.URL.Query().Get("workspace"); ws != "" {
		return ws
	}
	return r.URL.Query().Get("team")
}

// GET /v1/flows[?workspace=x] — the flows of the caller's workspaces (admins: all).
func (s *Server) listFlows(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var workspaces []string // nil: every flow
	if !p.IsAdmin() {
		workspaces = p.WorkspaceIDs()
	}
	if ws := workspaceParam(r); ws != "" {
		if !p.Sees(ws) {
			writeJSON(w, http.StatusOK, map[string]any{"flows": []store.FlowSummary{}})
			return
		}
		workspaces = []string{ws}
	}
	flows, err := s.Store.ListFlows(r.Context(), workspaces)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flows": flows})
}

func (s *Server) getFlow(w http.ResponseWriter, r *http.Request) {
	if f, ok := s.flowFor(w, r, toView); ok {
		writeJSON(w, http.StatusOK, f)
	}
}

// PUT /v1/flows/{flow_id}/draft — save the flow being edited (creates the flow
// on first save). Saving works even with problems, so work isn't lost; the
// problems come back in the response. Publishing needs zero problems.
// A new flow goes in ?workspace=<id> (optional if you're a maker in just one).
func (s *Server) putDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	workspace, ok := s.draftWorkspace(w, r, id)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	catalogWorkspace := workspace // a new flow's workspace; an existing flow's own one
	if catalogWorkspace == "" {
		ws, err := s.Store.FlowWorkspace(r.Context(), id)
		if err != nil {
			s.fail(w, err)
			return
		}
		catalogWorkspace = ws
	}
	decoded, problems, err := s.checkFlow(r.Context(), body, id, catalogWorkspace)
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
	if err := s.Store.SaveDraft(r.Context(), id, name, workspace, body, problems, actor(r), strings.Trim(r.Header.Get("If-Match"), `" `)); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"flow_id": id, "valid": len(problems) == 0, "problems": problems, "checksum": store.Checksum(body),
	})
}

// draftWorkspace checks the caller may save a draft of flow id, and returns the
// workspace a new flow will go in ("" for an existing flow).
func (s *Server) draftWorkspace(w http.ResponseWriter, r *http.Request, id string) (string, bool) {
	p := principal(r)
	if _, err := s.Store.GetFlow(r.Context(), id); err == nil {
		if r.Header.Get("If-None-Match") == "*" { // "create only" (Flow Studio's New flow)
			writeError(w, http.StatusConflict, fmt.Sprintf("a flow called %s already exists", id))
			return "", false
		}
		_, ok := s.flowFor(w, r, toEdit)
		return "", ok
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return "", false
	}
	ws := workspaceParam(r)
	if ws == "" {
		var makerIn []string
		for _, m := range p.Workspaces {
			if m.Maker {
				makerIn = append(makerIn, m.WorkspaceID)
			}
		}
		if len(makerIn) == 1 {
			ws = makerIn[0]
		}
	}
	switch {
	case ws == "" && p.IsAdmin():
		return "", true // a platform admin may create an unassigned flow
	case ws == "":
		writeError(w, http.StatusBadRequest, "say which workspace the new flow goes in: ?workspace=<id>")
		return "", false
	case !workspaceRE.MatchString(ws):
		writeError(w, http.StatusBadRequest, "workspace ids are lower-case letters, digits, - and _")
		return "", false
	case !p.CanEdit(ws):
		if !p.Sees(ws) {
			writeError(w, http.StatusNotFound, "there's no such workspace")
		} else {
			writeError(w, http.StatusForbidden, "you need to be a maker in workspace "+ws)
		}
		return "", false
	}
	if _, err := s.Store.GetWorkspace(r.Context(), ws); err != nil {
		s.fail(w, err)
		return "", false
	}
	return ws, true
}

func (s *Server) getDraft(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, toView)
	if !ok {
		return
	}
	d, err := s.Store.GetDraft(r.Context(), f.FlowID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Check again rather than show the problems found when it was saved: the
	// model catalog may have changed since (e.g. a model switched off).
	if _, problems, err := s.checkFlow(r.Context(), d.Spec, f.FlowID, f.Workspace); err == nil {
		d.Problems = problems
	}
	writeJSON(w, http.StatusOK, d)
}

// POST /v1/flows/{flow_id}/publish — the draft becomes the next version
// directly. Only for ADMIN_TOKEN (scripts, CI): people go through review
// (POST /v1/flows/{id}/reviews, then a checker approves). Numbers that follow
// the latest version use it from their next call.
func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Script {
		writeError(w, http.StatusForbidden, "changes are published by approval: submit the draft for review, and a checker in the workspace approves it")
		return
	}
	f, ok := s.flowFor(w, r, toApprove)
	if !ok {
		return
	}
	id := f.FlowID
	v, unchanged, err := s.Store.Publish(r.Context(), id, actor(r), func(spec []byte) []string {
		_, problems, err := s.checkFlow(r.Context(), spec, id, f.Workspace)
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
	f, ok := s.flowFor(w, r, toEdit)
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

// PUT /v1/flows/{flow_id}/workspace {"workspace": "emi"} — move a flow to another workspace.
func (s *Server) setWorkspace(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "move flows between workspaces") {
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
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(body, &req); err != nil || !workspaceRE.MatchString(req.Workspace) {
		writeError(w, http.StatusBadRequest, `body must be {"workspace": "<id>"} with a lower-case workspace id`)
		return
	}
	if err := s.Store.SetWorkspace(r.Context(), id, req.Workspace, actor(r)); err != nil {
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
	f, ok := s.flowFor(w, r, toView)
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
	f, ok := s.flowFor(w, r, toView)
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
	// The integrations the flow uses come with it; a change to them (a new
	// URL, a tool switched off) changes the ETag, so the agent picks it up.
	ws, err := s.Store.FlowWorkspace(r.Context(), res.FlowID)
	if err != nil {
		s.fail(w, err)
		return
	}
	integrations, digest, err := s.runtimeIntegrations(r.Context(), res.Spec, ws, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	etag := fmt.Sprintf(`"%s.v%d.%s"`, res.FlowID, res.Version, res.Checksum[:16])
	if digest != "" {
		etag = fmt.Sprintf(`"%s.v%d.%s.%s"`, res.FlowID, res.Version, res.Checksum[:16], digest)
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache") // always revalidate
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"number": res.Number, "flow_id": res.FlowID, "version": res.Version, "checksum": res.Checksum,
		"spec": res.Spec, "integrations": integrations,
	})
}
