package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/flowspec"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/secrets"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

// Integrations: the systems flows may call during a call, a bank's REST API or
// an MCP server. Flows use their tools as tools the AI may call on a step, or
// in API steps that always run.
//
// Two kinds:
//   - platform integrations, added by platform admins, apply at once and can
//     be offered to any workspace;
//   - workspace integrations, added by a workspace's makers (an MCP server
//     their team runs, wherever it is). A new one, or a change, waits for a
//     checker of that workspace: drafts and test calls can use it right away,
//     but a flow using it can be submitted, approved and called live only
//     once it's approved. Only that workspace's flows can use it, its
//     addresses must pass the host policy (INTEGRATION_ALLOWED_HOSTS), and its
//     token is sealed for the voice agent the moment it arrives.
//
// Security rules, checked on every draft (so a flow breaking them can't be
// test-called or submitted):
//   - only integrations switched on and offered to the flow's workspace;
//   - a tool's "locked" inputs must be set by the flow (from verified values),
//     never chosen by the AI;
//   - "sensitive" tools only on steps that can't be reached without passing
//     an API step that verifies the caller, and never on every step.
// The voice agent enforces the last two again during the call.

var (
	integrationIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	toolNameRE      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	inputNameRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	headerNameRE    = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	// Only variables made for integrations, so an entry can't send the agent's
	// own keys (LiveKit, LiteLLM...) to another server.
	authEnvRE   = regexp.MustCompile(`^INTEGRATION_[A-Z0-9_]{1,60}$`)
	pathParamRE = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	inputTypes  = map[string]bool{"string": true, "number": true, "integer": true, "boolean": true}
	// Headers the agent sets itself, or that would change where a call goes.
	reservedHeaders = map[string]bool{
		"host": true, "content-type": true, "content-length": true, "accept": true, "connection": true,
		"transfer-encoding": true, "mcp-session-id": true, "mcp-protocol-version": true, "cookie": true,
	}
)

const defaultMaxCalls = 5

// authInput is how a person describes the sign-in. Secret is the token
// (header) or client secret (oauth) in the clear; it is sealed at once.
// Empty keeps the one already saved.
type authInput struct {
	Kind     string `json:"kind"` // "" (none), "header" or "oauth"
	Header   string `json:"header"`
	TokenURL string `json:"token_url"`
	ClientID string `json:"client_id"`
	Scope    string `json:"scope"`
	Secret   string `json:"secret"`
}

type integrationInput struct {
	Kind       string                  `json:"kind"`
	Label      string                  `json:"label"`
	Notes      string                  `json:"notes"`
	URL        string                  `json:"url"`
	TestURL    string                  `json:"test_url"`
	AuthHeader string                  `json:"auth_header"`
	AuthEnv    string                  `json:"auth_env"`
	Auth       *authInput              `json:"auth"`
	TimeoutMs  int                     `json:"timeout_ms"`
	Workspaces []string                `json:"workspaces"`
	Enabled    *bool                   `json:"enabled"`
	Tools      []store.IntegrationTool `json:"tools"`
	Workspace  string                  `json:"workspace"` // the owning workspace (workspace integrations)
	Note       string                  `json:"note"`      // for the checker
}

const integrationShape = `{"kind", "label", "notes", "url", "test_url", "auth", "timeout_ms", "workspaces", "enabled", "tools", "workspace", "note"} (platform admins also "auth_header", "auth_env")`

func checkURL(field, raw string, required bool) []string {
	if raw == "" {
		if required {
			return []string{field + ": required"}
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return []string{field + ": must be an http(s) URL like https://bank.example/api (no user name or password in it)"}
	}
	return nil
}

func checkAuth(a *authInput) []string {
	var p []string
	bad := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	a.Header, a.TokenURL = strings.TrimSpace(a.Header), strings.TrimSpace(a.TokenURL)
	a.ClientID, a.Scope = strings.TrimSpace(a.ClientID), strings.TrimSpace(a.Scope)
	if len(a.Secret) > 4096 {
		bad("auth.secret: at most 4096 characters")
	}
	if strings.ContainsAny(a.Secret, "\r\n") {
		bad("auth.secret: must be on one line")
	}
	switch a.Kind {
	case "":
		if a.Header != "" || a.TokenURL != "" || a.ClientID != "" || a.Secret != "" {
			bad("auth.kind: header (a token sent in a header) or oauth (client credentials), to use the details given")
		}
	case "header":
		if a.Header == "" {
			a.Header = "Authorization"
		}
		if !headerNameRE.MatchString(a.Header) || reservedHeaders[strings.ToLower(a.Header)] {
			bad("auth.header: a header name like Authorization or X-Api-Key")
		}
		a.TokenURL, a.ClientID, a.Scope = "", "", ""
	case "oauth":
		p = append(p, checkURL("auth.token_url", a.TokenURL, true)...)
		if a.ClientID == "" || len(a.ClientID) > 200 {
			bad("auth.client_id: 1 to 200 characters")
		}
		if len(a.Scope) > 500 {
			bad("auth.scope: at most 500 characters")
		}
		a.Header = ""
	default:
		bad("auth.kind: none, header or oauth")
	}
	return p
}

func checkIntegration(in *integrationInput) []string {
	var p []string
	bad := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	in.Label, in.Notes = strings.TrimSpace(in.Label), strings.TrimSpace(in.Notes)
	in.URL, in.TestURL = strings.TrimSpace(in.URL), strings.TrimSpace(in.TestURL)
	in.Note = strings.TrimSpace(in.Note)
	if in.Kind != "mcp" && in.Kind != "http" {
		bad("kind: must be mcp (an MCP server) or http (a REST API)")
	}
	if in.Label == "" || len(in.Label) > 80 {
		bad("label: 1 to 80 characters")
	}
	if len(in.Notes) > 500 {
		bad("notes: at most 500 characters")
	}
	if len(in.Note) > 2000 {
		bad("note: at most 2000 characters")
	}
	p = append(p, checkURL("url", in.URL, true)...)
	p = append(p, checkURL("test_url", in.TestURL, false)...)
	if (in.AuthHeader == "") != (in.AuthEnv == "") {
		bad("auth_header and auth_env: set both (the header, and the agent's variable holding its value) or neither")
	}
	if in.AuthHeader != "" && !headerNameRE.MatchString(in.AuthHeader) {
		bad("auth_header: a header name like Authorization or X-Api-Key")
	}
	if in.AuthEnv != "" && !authEnvRE.MatchString(in.AuthEnv) {
		bad("auth_env: must be INTEGRATION_ followed by capitals, digits and _, e.g. INTEGRATION_DEMO_BANK_TOKEN")
	}
	if in.Auth == nil {
		in.Auth = &authInput{}
	}
	p = append(p, checkAuth(in.Auth)...)
	if in.AuthEnv != "" && in.Auth.Kind != "" {
		bad("auth: use either auth_env (a variable of the agent) or auth (a saved token), not both")
	}
	if in.TimeoutMs == 0 {
		in.TimeoutMs = 5000
	}
	if in.TimeoutMs < 500 || in.TimeoutMs > 30000 {
		bad("timeout_ms: between 500 and 30000 (a caller is waiting)")
	}
	for _, ws := range in.Workspaces {
		if !workspaceRE.MatchString(ws) {
			bad("workspaces: '%s' isn't a workspace id", ws)
		}
	}
	if len(in.Tools) == 0 {
		bad("tools: add at least one tool")
	}
	if len(in.Tools) > 50 {
		bad("tools: at most 50")
	}
	seen := map[string]bool{}
	for i := range in.Tools {
		t := &in.Tools[i]
		loc := fmt.Sprintf("tools[%d]", i)
		t.Description = strings.TrimSpace(t.Description)
		if !toolNameRE.MatchString(t.Name) {
			bad("%s.name: letters, digits, _ and - (max 64), e.g. get_card_balance", loc)
		} else if seen[t.Name] {
			bad("%s.name: another tool is also called '%s'", loc, t.Name)
		}
		seen[t.Name] = true
		if t.Description == "" || len(t.Description) > 1000 {
			bad("%s.description: 1 to 1000 characters; the AI reads it to decide when to use the tool", loc)
		}
		inputs := map[string]bool{}
		for k := range t.Inputs {
			ti := &t.Inputs[k]
			iloc := fmt.Sprintf("%s.inputs[%d]", loc, k)
			ti.Description = strings.TrimSpace(ti.Description)
			if ti.Type == "" {
				ti.Type = "string"
			}
			switch {
			case !inputNameRE.MatchString(ti.Name):
				bad("%s.name: letters, digits and _, starting with a letter", iloc)
			case inputs[ti.Name]:
				bad("%s.name: '%s' is listed twice", iloc, ti.Name)
			}
			inputs[ti.Name] = true
			if !inputTypes[ti.Type] {
				bad("%s.type: string, number, integer or boolean", iloc)
			}
			if ti.Description == "" || len(ti.Description) > 300 {
				bad("%s.description: 1 to 300 characters", iloc)
			}
			if len(ti.Enum) > 0 && ti.Type != "string" {
				bad("%s.enum: only string inputs can have a list of values", iloc)
			}
		}
		if t.Outputs == nil {
			t.Outputs = []string{}
		}
		outs := map[string]bool{}
		for k, o := range t.Outputs {
			if !identifierRE.MatchString(o) {
				bad("%s.outputs[%d]: lower-case letters, digits and _ (it becomes a flow variable)", loc, k)
			} else if outs[o] {
				bad("%s.outputs[%d]: '%s' is listed twice", loc, k, o)
			}
			outs[o] = true
		}
		if t.Locked == nil {
			t.Locked = []string{}
		}
		for k, l := range t.Locked {
			if !inputs[l] {
				bad("%s.locked[%d]: '%s' isn't one of the tool's inputs", loc, k, l)
			}
		}
		if t.MaxCalls == 0 {
			t.MaxCalls = defaultMaxCalls
		}
		if t.MaxCalls < 1 || t.MaxCalls > 20 {
			bad("%s.max_calls: between 1 and 20 per call", loc)
		}
		if in.Kind == "http" {
			if t.Method == "" {
				t.Method = "POST"
			}
			if t.Method != "GET" && t.Method != "POST" {
				bad("%s.method: GET or POST", loc)
			}
			if !strings.HasPrefix(t.Path, "/") || strings.ContainsAny(t.Path, "?# ") {
				bad("%s.path: the path after the URL, starting with / (inputs in it as {name}), e.g. /cards/{card_last4}/balance", loc)
			}
			for _, m := range pathParamRE.FindAllStringSubmatch(t.Path, -1) {
				if !inputs[m[1]] {
					bad("%s.path: {%s} isn't one of the tool's inputs", loc, m[1])
				}
			}
			if t.Method == "GET" && t.Sensitive {
				// GET puts inputs in the URL, where proxies and servers log them.
				bad("%s.method: sensitive tools must use POST, so card digits and dates of birth never appear in URLs", loc)
			}
		} else {
			t.Method, t.Path = "", ""
		}
		if t.Inputs == nil {
			t.Inputs = []store.ToolInput{}
		}
	}
	return p
}

// ── Who sees what ─────────────────────────────────────────────────────────────

// hideSecrets: a saved token is never shown, only that there is one.
func hideSecrets(d store.IntegrationDef) store.IntegrationDef {
	d.Auth.HasSecret = d.Auth.Sealed != ""
	d.Auth.Sealed = ""
	return d
}

// integrationView is an integration as p may see it, or false if p may not
// see it at all. Platform admins and the owning workspace's members see
// everything but secrets (members need the details to change it); others see
// the approved definition of integrations offered to their workspaces,
// without addresses or sign-in settings.
func integrationView(p auth.Principal, i store.Integration) (store.Integration, bool) {
	i.IntegrationDef = hideSecrets(i.IntegrationDef)
	if i.Pending != nil {
		pending := hideSecrets(*i.Pending)
		i.Pending = &pending
	}
	if p.IsAdmin() || (i.OwnerWorkspace != "" && p.Sees(i.OwnerWorkspace)) {
		return i, true
	}
	if i.LiveDef() == nil {
		return i, false
	}
	ok := len(i.Workspaces) == 0
	for _, ws := range i.Workspaces {
		ok = ok || p.Sees(ws)
	}
	if !ok {
		return i, false
	}
	i.URL, i.TestURL, i.AuthHeader, i.AuthEnv, i.TimeoutMs = "", "", "", "", 0
	i.Auth = store.IntegrationAuth{Kind: i.Auth.Kind}
	tools := make([]store.IntegrationTool, len(i.Tools))
	for k, t := range i.Tools {
		t.Method, t.Path = "", ""
		tools[k] = t
	}
	i.Tools = tools
	i.Pending, i.PendingBy, i.PendingAt, i.PendingNote = nil, "", nil, ""
	i.RejectedBy, i.RejectedAt, i.RejectedNote = "", nil, ""
	return i, true
}

// GET /v1/integrations — every integration the caller may see (see integrationView).
func (s *Server) listIntegrations(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListIntegrations(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	p := principal(r)
	visible := []store.Integration{}
	for _, i := range list {
		if v, ok := integrationView(p, i); ok {
			visible = append(visible, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"integrations": visible})
}

func integrationPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("integration_id")
	if !integrationIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "integration id: lower-case letters, digits and _ (max 32), e.g. core_banking")
		return "", false
	}
	return id, true
}

// existingIntegration reads one, nil if there's none.
func (s *Server) existingIntegration(ctx context.Context, id string) (*store.Integration, error) {
	i, err := s.Store.GetIntegration(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return i, err
}

// sealAuth turns what was typed into what is saved: the secret sealed for
// the voice agent, or the saved one kept when none was typed. A saved secret
// is kept only while every address it could be sent to stays the same, so
// changing where calls go always needs the secret typed again.
func (s *Server) sealAuth(id string, in *integrationInput, before *store.IntegrationDef) (store.IntegrationAuth, []string) {
	a := in.Auth
	out := store.IntegrationAuth{Kind: a.Kind, Header: a.Header, TokenURL: a.TokenURL, ClientID: a.ClientID, Scope: a.Scope}
	if a.Kind == "" {
		return out, nil
	}
	if a.Secret != "" {
		if s.SecretsKey == nil {
			return out, []string{"auth.secret: this platform can't save tokens yet: INTEGRATION_SECRETS_PUBLIC_KEY isn't set (python -m flow_engine.secrets keygen)"}
		}
		sealed, err := secrets.Seal(s.SecretsKey, id, a.Secret)
		if err != nil {
			return out, []string{"auth.secret: couldn't be sealed: " + err.Error()}
		}
		out.Sealed = sealed
		return out, nil
	}
	if before != nil && before.Auth.Sealed != "" && before.Auth.Kind == a.Kind && before.URL == in.URL &&
		before.TestURL == in.TestURL && before.Auth.TokenURL == a.TokenURL {
		out.Sealed = before.Auth.Sealed
		return out, nil
	}
	what := "auth.secret: enter the token"
	if a.Kind == "oauth" {
		what = "auth.secret: enter the client secret"
	}
	if before != nil && before.Auth.Sealed != "" {
		return out, []string{what + " again: the address changed, and a saved one is only ever sent where it was first meant to go"}
	}
	return out, []string{what}
}

// PUT /v1/integrations/{integration_id} — add or change one.
//
// Platform admins: applies at once. Makers: "workspace" names the workspace
// that owns it; the change waits for one of its checkers.
func (s *Server) putIntegration(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id, ok := integrationPath(w, r)
	if !ok {
		return
	}
	var in integrationInput
	if !decodeStrict(w, r, &in, integrationShape) {
		return
	}
	existing, err := s.existingIntegration(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	owner := strings.TrimSpace(in.Workspace)
	if existing != nil {
		if owner != "" && owner != existing.OwnerWorkspace {
			if existing.OwnerWorkspace == "" || !p.Sees(existing.OwnerWorkspace) {
				writeError(w, http.StatusConflict, fmt.Sprintf("there's already an integration called '%s'; pick another id", id))
			} else {
				writeError(w, http.StatusBadRequest, "an integration can't move to another workspace")
			}
			return
		}
		owner = existing.OwnerWorkspace
	}
	switch {
	case owner == "" && !p.IsAdmin():
		writeError(w, http.StatusForbidden, "only platform admins change platform integrations; to add your own, give your workspace")
		return
	case owner != "" && !p.CanEdit(owner):
		writeError(w, http.StatusForbidden, fmt.Sprintf("you need to be a maker in workspace %s to change its integrations", owner))
		return
	}
	if owner != "" && existing == nil {
		if !workspaceRE.MatchString(owner) {
			writeError(w, http.StatusBadRequest, "workspace: not a workspace id")
			return
		}
		if _, err := s.Store.GetWorkspace(r.Context(), owner); err != nil {
			s.fail(w, err)
			return
		}
	}
	problems := checkIntegration(&in)
	if owner != "" {
		// A workspace's integration is for its own flows, signs in with a
		// sealed token (never one of the agent's variables) and may only point
		// at hosts the platform allows.
		in.Workspaces = []string{owner}
		if in.AuthHeader != "" || in.AuthEnv != "" {
			problems = append(problems, "auth_env: only platform integrations can use the agent's variables; enter the token under auth instead (it's sealed so only the voice agent can read it)")
		}
		problems = append(problems, s.Hosts.check("url", in.URL)...)
		problems = append(problems, s.Hosts.check("test_url", in.TestURL)...)
		if in.Auth.Kind == "oauth" {
			problems = append(problems, s.Hosts.check("auth.token_url", in.Auth.TokenURL)...)
		}
	}
	var before *store.IntegrationDef
	if existing != nil {
		before = existing.DraftDef()
		if before == nil { // turned down before it was ever approved
			before = &existing.IntegrationDef
		}
	}
	sealed, authProblems := s.sealAuth(id, &in, before)
	problems = append(problems, authProblems...)
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the integration has problems", "problems": problems})
		return
	}
	d := store.IntegrationDef{
		Kind: in.Kind, Label: in.Label, Notes: in.Notes, URL: in.URL, TestURL: in.TestURL,
		AuthHeader: in.AuthHeader, AuthEnv: in.AuthEnv, Auth: sealed, TimeoutMs: in.TimeoutMs,
		Workspaces: nonNil(in.Workspaces), Enabled: in.Enabled == nil || *in.Enabled, Tools: in.Tools,
	}
	var created bool
	pending := !p.IsAdmin()
	if pending {
		created, err = s.Store.ProposeIntegration(r.Context(), id, owner, d, in.Note, actor(r))
	} else {
		created, err = s.Store.PutIntegration(r.Context(), id, owner, d, actor(r))
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.integrationAnswer(w, r, id, status, map[string]any{"created": created, "pending": pending})
}

// integrationAnswer replies with the integration as it is now and the flows using it.
func (s *Server) integrationAnswer(w http.ResponseWriter, r *http.Request, id string, status int, extra map[string]any) {
	saved, err := s.Store.GetIntegration(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	used, err := s.Store.IntegrationUsage(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	view, _ := integrationView(principal(r), *saved)
	body := map[string]any{"integration": view, "used_by": used}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

// integrationFor reads the integration in the path, nil after replying.
func (s *Server) integrationFor(w http.ResponseWriter, r *http.Request) *store.Integration {
	id, ok := integrationPath(w, r)
	if !ok {
		return nil
	}
	i, err := s.Store.GetIntegration(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return nil
	}
	if _, ok := integrationView(principal(r), *i); !ok {
		writeError(w, http.StatusNotFound, "not found")
		return nil
	}
	return i
}

// canReview: checkers of the owning workspace; platform admins for platform ones.
func canReview(p auth.Principal, i *store.Integration) bool {
	if i.OwnerWorkspace == "" {
		return p.IsAdmin()
	}
	return p.CanApprove(i.OwnerWorkspace)
}

func reviewNote(w http.ResponseWriter, r *http.Request, required bool) (string, bool) {
	var b struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 && !decodeStrict(w, r, &b, `{"note": "..."}`) {
		return "", false
	}
	b.Note = strings.TrimSpace(b.Note)
	switch {
	case required && b.Note == "":
		writeError(w, http.StatusUnprocessableEntity, "note: say what needs to change, for the maker")
		return "", false
	case len(b.Note) > 2000:
		writeError(w, http.StatusUnprocessableEntity, "note: at most 2000 characters")
		return "", false
	}
	return b.Note, true
}

// POST /v1/integrations/{integration_id}/approve {"note"} — checkers: the change goes live.
func (s *Server) approveIntegration(w http.ResponseWriter, r *http.Request) {
	i := s.integrationFor(w, r)
	if i == nil {
		return
	}
	if !canReview(principal(r), i) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("you need to be a checker in workspace %s to approve its integrations", orUnassigned(i.OwnerWorkspace)))
		return
	}
	if i.Pending != nil && i.OwnerWorkspace != "" {
		// The host policy may have changed since it was proposed.
		var problems []string
		problems = append(problems, s.Hosts.check("url", i.Pending.URL)...)
		problems = append(problems, s.Hosts.check("test_url", i.Pending.TestURL)...)
		if i.Pending.Auth.Kind == "oauth" {
			problems = append(problems, s.Hosts.check("auth.token_url", i.Pending.Auth.TokenURL)...)
		}
		if len(problems) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the change can't be approved", "problems": problems})
			return
		}
	}
	note, ok := reviewNote(w, r, false)
	if !ok {
		return
	}
	if err := s.Store.ApproveIntegration(r.Context(), i.IntegrationID, actor(r), note); err != nil {
		s.fail(w, err)
		return
	}
	s.integrationAnswer(w, r, i.IntegrationID, http.StatusOK, nil)
}

// POST /v1/integrations/{integration_id}/reject {"note"} — checkers: send it back.
func (s *Server) rejectIntegration(w http.ResponseWriter, r *http.Request) {
	i := s.integrationFor(w, r)
	if i == nil {
		return
	}
	if !canReview(principal(r), i) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("you need to be a checker in workspace %s to turn down its integrations", orUnassigned(i.OwnerWorkspace)))
		return
	}
	note, ok := reviewNote(w, r, true)
	if !ok {
		return
	}
	if err := s.Store.RejectIntegration(r.Context(), i.IntegrationID, actor(r), note); err != nil {
		s.fail(w, err)
		return
	}
	s.integrationAnswer(w, r, i.IntegrationID, http.StatusOK, nil)
}

// POST /v1/integrations/{integration_id}/withdraw — the maker takes the change back.
func (s *Server) withdrawIntegration(w http.ResponseWriter, r *http.Request) {
	i := s.integrationFor(w, r)
	if i == nil {
		return
	}
	if err := s.Store.WithdrawIntegration(r.Context(), i.IntegrationID, actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	if !i.IsLive {
		w.WriteHeader(http.StatusNoContent) // a new one: gone
		return
	}
	s.integrationAnswer(w, r, i.IntegrationID, http.StatusOK, nil)
}

// POST /v1/integrations/{integration_id}/enabled {"enabled": bool} — switch it
// off at once (the workspace's makers and checkers: the way to stop a
// misbehaving server), or on again (checkers).
func (s *Server) setIntegrationEnabled(w http.ResponseWriter, r *http.Request) {
	i := s.integrationFor(w, r)
	if i == nil {
		return
	}
	var b struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeStrict(w, r, &b, `{"enabled": true|false}`) {
		return
	}
	if b.Enabled == nil {
		writeError(w, http.StatusBadRequest, `body must be {"enabled": true|false}`)
		return
	}
	p := principal(r)
	allowed := canReview(p, i) || (!*b.Enabled && i.OwnerWorkspace != "" && p.CanEdit(i.OwnerWorkspace))
	if !allowed {
		writeError(w, http.StatusForbidden, "makers can switch their workspace's integrations off; switching one on needs a checker")
		return
	}
	if err := s.Store.SetIntegrationEnabled(r.Context(), i.IntegrationID, *b.Enabled, actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	s.integrationAnswer(w, r, i.IntegrationID, http.StatusOK, nil)
}

// DELETE /v1/integrations/{integration_id} — remove one nothing uses (platform
// admins; a workspace's checkers for its own).
func (s *Server) deleteIntegration(w http.ResponseWriter, r *http.Request) {
	i := s.integrationFor(w, r)
	if i == nil {
		return
	}
	if !canReview(principal(r), i) {
		writeError(w, http.StatusForbidden, "only platform admins, or checkers of the workspace that owns it, can remove an integration")
		return
	}
	used, err := s.Store.IntegrationUsage(r.Context(), i.IntegrationID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(used) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   fmt.Sprintf("still used by %s; take its tools out of those flows first, or switch it off instead", strings.Join(used, ", ")),
			"used_by": used,
		})
		return
	}
	if err := s.Store.DeleteIntegration(r.Context(), i.IntegrationID, actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Checking a flow's tools ───────────────────────────────────────────────────

// toolUse is one place a flow uses a tool.
type toolUse struct {
	loc      string // e.g. nodes.balance.tools[0].tool
	inputLoc string // e.g. nodes.balance.tools[0].inputs
	ref      string // "<integration>.<tool>"
	inputs   []flowspec.Input
	node     *flowspec.Node // nil: the flow's tools (every step)
	apiStep  bool
}

func toolUses(f *flowspec.Flow) []toolUse {
	var out []toolUse
	for i, t := range f.Tools {
		loc := fmt.Sprintf("tools[%d]", i)
		out = append(out, toolUse{loc + ".tool", loc + ".inputs", t.Tool, t.Inputs, nil, false})
	}
	for _, n := range f.Nodes {
		base := "nodes." + n.ID
		switch n.Type {
		case "agent":
			for i, t := range n.Tools {
				loc := fmt.Sprintf("%s.tools[%d]", base, i)
				out = append(out, toolUse{loc + ".tool", loc + ".inputs", t.Tool, t.Inputs, n, false})
			}
		case "api":
			out = append(out, toolUse{base + ".tool", base + ".inputs", n.Tool, n.Inputs, n, true})
		}
	}
	return out
}

// unverified returns the nodes a caller can reach without passing an API step
// that verifies them (an API step only verifies when its call works).
func unverified(f *flowspec.Flow) map[string]bool {
	byID := map[string]*flowspec.Node{}
	for _, n := range f.Nodes {
		byID[n.ID] = n
	}
	seen := map[string]bool{}
	queue := []string{f.StartNode}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		n := byID[id]
		if n == nil || seen[id] {
			continue
		}
		seen[id] = true
		next := flowspec.Successors(n)
		if n.Type == "api" && n.Verifies {
			next = []string{n.OnError}
		}
		queue = append(queue, next...)
	}
	return seen
}

// integrationProblems checks a flow's tools against the integrations. live:
// against what live calls use (to submit, approve or publish); otherwise
// against what drafts and test calls use, which includes changes waiting for
// review.
func integrationProblems(list []store.Integration, f *flowspec.Flow, workspace string, live bool) []string {
	byID := map[string]*store.Integration{}
	for k := range list {
		byID[list[k].IntegrationID] = &list[k]
	}
	open := unverified(f)
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	for _, u := range toolUses(f) {
		integrationID, name, _ := strings.Cut(u.ref, ".")
		i := byID[integrationID]
		if i == nil {
			add("%s: there's no integration called '%s'; pick a tool from the list (a platform admin can add integrations)", u.loc, integrationID)
			continue
		}
		d := i.DraftDef()
		if live {
			d = i.LiveDef()
		}
		switch {
		case d == nil && i.Pending != nil:
			add("%s: the integration '%s' is waiting for a checker to approve it; you can test with it now, and submit once it's approved", u.loc, integrationID)
			continue
		case d == nil && i.RejectedBy != "":
			add("%s: the integration '%s' was turned down (%s); change it and send it for review again", u.loc, integrationID, i.RejectedNote)
			continue
		case d == nil:
			add("%s: the integration '%s' hasn't been approved yet", u.loc, integrationID)
			continue
		case !d.Enabled:
			add("%s: the integration '%s' has been switched off", u.loc, integrationID)
			continue
		case !d.AllowedFor(workspace):
			add("%s: the integration '%s' isn't offered to workspace %s", u.loc, integrationID, orUnassigned(workspace))
			continue
		}
		t := d.Tool(name)
		if t == nil {
			if live && i.Pending != nil && i.Pending.Tool(name) != nil {
				add("%s: '%s' gets the tool '%s' only once its change waiting for review is approved", u.loc, integrationID, name)
			} else {
				add("%s: '%s' has no tool called '%s'", u.loc, integrationID, name)
			}
			continue
		}
		given := map[string]bool{}
		for _, in := range u.inputs {
			given[in.Name] = true
			if t.Input(in.Name) == nil {
				add("%s.%s: '%s' has no input called '%s'", u.inputLoc, in.Name, name, in.Name)
			}
		}
		for _, l := range t.Locked {
			if !given[l] {
				add("%s: '%s' needs the flow to set its input '%s' (the AI may not choose it); add it under inputs, e.g. from a collected value", u.loc, name, l)
			}
		}
		if u.apiStep {
			for _, in := range t.Inputs {
				if in.Required && !given[in.Name] && !contains(t.Locked, in.Name) {
					add("%s: '%s' needs its input '%s'; an API step has no AI to fill it in, so set it under inputs", u.loc, name, in.Name)
				}
			}
			for k, sv := range u.node.Save {
				if !contains(t.Outputs, sv) {
					outs := "nothing"
					if len(t.Outputs) > 0 {
						outs = strings.Join(t.Outputs, ", ")
					}
					add("nodes.%s.save[%d]: '%s' doesn't return '%s'; it returns %s", u.node.ID, k, name, sv, outs)
				}
			}
		}
		if t.Sensitive {
			switch {
			case u.node == nil:
				add("%s: '%s' needs a verified caller, so it can't be on every step; add it to the steps after the caller is verified", u.loc, name)
			case open[u.node.ID]:
				add("%s: '%s' needs a verified caller, but this step can be reached without passing an API step that verifies the caller", u.loc, name)
			}
		}
	}
	return problems
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ── What the voice agent gets with a flow ─────────────────────────────────────

// runtimeAuth: how the agent signs in. Sealed only opens with the agent's key.
type runtimeAuth struct {
	Kind     string `json:"kind"`
	Header   string `json:"header,omitempty"`
	TokenURL string `json:"token_url,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Sealed   string `json:"sealed"`
}

// runtimeIntegration is an integration as the voice agent needs it: where to
// call, how to sign in, and the tools this flow uses. Restricted: a
// workspace's own integration, which the agent refuses to call on its own
// machine or the cloud's metadata addresses.
type runtimeIntegration struct {
	Kind       string                  `json:"kind"`
	URL        string                  `json:"url"`
	AuthHeader string                  `json:"auth_header,omitempty"`
	AuthEnv    string                  `json:"auth_env,omitempty"`
	Auth       *runtimeAuth            `json:"auth,omitempty"`
	Restricted bool                    `json:"restricted,omitempty"`
	TimeoutMs  int                     `json:"timeout_ms"`
	Tools      []store.IntegrationTool `json:"tools"`
}

// runtimeIntegrations lists the integrations a flow uses that are switched on
// and offered to its workspace. testCall: what drafts use (changes waiting for
// review included), with each one's test URL if it has one; otherwise only
// what's approved. The digest changes whenever the answer does (for the
// resolve ETag).
func (s *Server) runtimeIntegrations(ctx context.Context, spec []byte, workspace string, testCall bool) (map[string]runtimeIntegration, string, error) {
	out := map[string]runtimeIntegration{}
	decoded, err := flowspec.Decode(spec)
	if err != nil {
		return out, "", nil
	}
	f := flowspec.Parse(decoded)
	if f == nil {
		return out, "", nil
	}
	uses := toolUses(f)
	if len(uses) == 0 {
		return out, "", nil
	}
	list, err := s.Store.ListIntegrations(ctx)
	if err != nil {
		return nil, "", err
	}
	byID := map[string]*store.Integration{}
	for k := range list {
		byID[list[k].IntegrationID] = &list[k]
	}
	wanted := map[string]map[string]bool{}
	for _, u := range uses {
		id, name, _ := strings.Cut(u.ref, ".")
		if wanted[id] == nil {
			wanted[id] = map[string]bool{}
		}
		wanted[id][name] = true
	}
	for id, names := range wanted {
		i := byID[id]
		if i == nil {
			continue
		}
		d := i.LiveDef()
		if testCall {
			d = i.DraftDef()
		}
		if d == nil || !d.AllowedFor(workspace) {
			continue
		}
		ri := runtimeIntegration{
			Kind: d.Kind, URL: d.URL, AuthHeader: d.AuthHeader, AuthEnv: d.AuthEnv, TimeoutMs: d.TimeoutMs,
			Restricted: i.OwnerWorkspace != "", Tools: []store.IntegrationTool{},
		}
		if d.Auth.Kind != "" {
			ri.Auth = &runtimeAuth{Kind: d.Auth.Kind, Header: d.Auth.Header, TokenURL: d.Auth.TokenURL, ClientID: d.Auth.ClientID, Scope: d.Auth.Scope, Sealed: d.Auth.Sealed}
		}
		if testCall && d.TestURL != "" {
			ri.URL = d.TestURL
		}
		for _, t := range d.Tools {
			if names[t.Name] {
				ri.Tools = append(ri.Tools, t)
			}
		}
		sort.Slice(ri.Tools, func(a, b int) bool { return ri.Tools[a].Name < ri.Tools[b].Name })
		out[id] = ri
	}
	b, _ := json.Marshal(out) // map keys are sorted: the same answer gives the same digest
	sum := sha256.Sum256(b)
	return out, hex.EncodeToString(sum[:])[:12], nil
}
