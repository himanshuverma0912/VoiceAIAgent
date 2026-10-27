package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/flowspec"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

// Integrations: the systems flows may call during a call, a bank's REST API or
// an MCP server. Platform admins register each one with its tools; flows use
// them as tools the AI may call on a step, or in API steps that always run.
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
)

const defaultMaxCalls = 5

type integrationInput struct {
	Kind       string                  `json:"kind"`
	Label      string                  `json:"label"`
	Notes      string                  `json:"notes"`
	URL        string                  `json:"url"`
	TestURL    string                  `json:"test_url"`
	AuthHeader string                  `json:"auth_header"`
	AuthEnv    string                  `json:"auth_env"`
	TimeoutMs  int                     `json:"timeout_ms"`
	Workspaces []string                `json:"workspaces"`
	Enabled    *bool                   `json:"enabled"`
	Tools      []store.IntegrationTool `json:"tools"`
}

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

func checkIntegration(in *integrationInput) []string {
	var p []string
	bad := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	in.Label, in.Notes = strings.TrimSpace(in.Label), strings.TrimSpace(in.Notes)
	in.URL, in.TestURL = strings.TrimSpace(in.URL), strings.TrimSpace(in.TestURL)
	if in.Kind != "mcp" && in.Kind != "http" {
		bad("kind: must be mcp (an MCP server) or http (a REST API)")
	}
	if in.Label == "" || len(in.Label) > 80 {
		bad("label: 1 to 80 characters")
	}
	if len(in.Notes) > 500 {
		bad("notes: at most 500 characters")
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

// GET /v1/integrations — admins see everything; others see the integrations
// offered to their workspaces, without URLs or credentials settings.
func (s *Server) listIntegrations(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListIntegrations(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	p := principal(r)
	if !p.IsAdmin() {
		visible := []store.Integration{}
		for _, i := range list {
			ok := len(i.Workspaces) == 0
			for _, ws := range i.Workspaces {
				ok = ok || p.Sees(ws)
			}
			if ok {
				visible = append(visible, redacted(i))
			}
		}
		list = visible
	}
	writeJSON(w, http.StatusOK, map[string]any{"integrations": list})
}

func redacted(i store.Integration) store.Integration {
	i.URL, i.TestURL, i.AuthHeader, i.AuthEnv, i.TimeoutMs = "", "", "", "", 0
	tools := make([]store.IntegrationTool, len(i.Tools))
	for k, t := range i.Tools {
		t.Method, t.Path = "", ""
		tools[k] = t
	}
	i.Tools = tools
	return i
}

func integrationPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("integration_id")
	if !integrationIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "integration id: lower-case letters, digits and _ (max 32), e.g. core_banking")
		return "", false
	}
	return id, true
}

// PUT /v1/integrations/{integration_id} — add or change one (platform admins).
func (s *Server) putIntegration(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "change integrations") {
		return
	}
	id, ok := integrationPath(w, r)
	if !ok {
		return
	}
	var in integrationInput
	if !decodeStrict(w, r, &in, `{"kind", "label", "notes", "url", "test_url", "auth_header", "auth_env", "timeout_ms", "workspaces", "enabled", "tools"}`) {
		return
	}
	if problems := checkIntegration(&in); len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the integration has problems", "problems": problems})
		return
	}
	i := store.Integration{
		IntegrationID: id, Kind: in.Kind, Label: in.Label, Notes: in.Notes, URL: in.URL, TestURL: in.TestURL,
		AuthHeader: in.AuthHeader, AuthEnv: in.AuthEnv, TimeoutMs: in.TimeoutMs, Workspaces: nonNil(in.Workspaces),
		Enabled: in.Enabled == nil || *in.Enabled, Tools: in.Tools,
	}
	created, err := s.Store.PutIntegration(r.Context(), i, actor(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	used, err := s.Store.IntegrationUsage(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	saved, err := s.Store.GetIntegration(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, status, map[string]any{"integration": saved, "created": created, "used_by": used})
}

// DELETE /v1/integrations/{integration_id} — remove one nothing uses (platform admins).
func (s *Server) deleteIntegration(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "change integrations") {
		return
	}
	id, ok := integrationPath(w, r)
	if !ok {
		return
	}
	used, err := s.Store.IntegrationUsage(r.Context(), id)
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
	if err := s.Store.DeleteIntegration(r.Context(), id, actor(r)); err != nil {
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

func integrationProblems(list []store.Integration, f *flowspec.Flow, workspace string) []string {
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
		switch {
		case i == nil:
			add("%s: there's no integration called '%s'; pick a tool from the list (a platform admin can add integrations)", u.loc, integrationID)
			continue
		case !i.Enabled:
			add("%s: the integration '%s' has been switched off", u.loc, integrationID)
			continue
		case !i.AllowedFor(workspace):
			add("%s: the integration '%s' isn't offered to workspace %s", u.loc, integrationID, orUnassigned(workspace))
			continue
		}
		t := i.Tool(name)
		if t == nil {
			add("%s: '%s' has no tool called '%s'", u.loc, integrationID, name)
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

// runtimeIntegration is an integration as the voice agent needs it: where to
// call, which of the agent's variables holds the credential, and the tools
// this flow uses (no secrets: those stay in the agent's environment).
type runtimeIntegration struct {
	Kind       string                  `json:"kind"`
	URL        string                  `json:"url"`
	AuthHeader string                  `json:"auth_header,omitempty"`
	AuthEnv    string                  `json:"auth_env,omitempty"`
	TimeoutMs  int                     `json:"timeout_ms"`
	Tools      []store.IntegrationTool `json:"tools"`
}

// runtimeIntegrations lists the integrations a flow uses that are switched on
// and offered to its workspace. testCall: use each one's test URL if it has one.
// The digest changes whenever the answer does (for the resolve ETag).
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
		if i == nil || !i.AllowedFor(workspace) {
			continue
		}
		ri := runtimeIntegration{Kind: i.Kind, URL: i.URL, AuthHeader: i.AuthHeader, AuthEnv: i.AuthEnv, TimeoutMs: i.TimeoutMs, Tools: []store.IntegrationTool{}}
		if testCall && i.TestURL != "" {
			ri.URL = i.TestURL
		}
		for _, t := range i.Tools {
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
