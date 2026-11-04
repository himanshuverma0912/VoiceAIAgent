package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

// Loading an MCP server's tools, so a maker picks from what the server offers
// instead of typing them. The token typed for this is used for these few
// requests and forgotten; it is never stored or logged.

const (
	discoverTimeout   = 12 * time.Second // under the server's 15 s write timeout
	discoversPerHour  = 60
	mcpProtocol       = "2025-06-18"
	maxDiscoverAnswer = 4 << 20
)

type discoverInput struct {
	Workspace string     `json:"workspace"`
	URL       string     `json:"url"`
	Auth      *authInput `json:"auth"`
}

type skippedTool struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// discoverError is a problem to show the person as it is.
type discoverError struct{ msg string }

func (e *discoverError) Error() string { return e.msg }

func discoverFailed(format string, args ...any) error {
	return &discoverError{fmt.Sprintf(format, args...)}
}

// POST /v1/integrations/discover — connect to an MCP server and list its tools.
func (s *Server) discoverTools(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in discoverInput
	if !decodeStrict(w, r, &in, `{"workspace", "url", "auth"}`) {
		return
	}
	restricted := !p.IsAdmin()
	if restricted && (in.Workspace == "" || !p.CanEdit(in.Workspace)) {
		writeError(w, http.StatusForbidden, "you need to be a maker in the workspace to load an MCP server's tools")
		return
	}
	if !s.discovers.allow(p.Subject, discoversPerHour, time.Now()) {
		writeError(w, http.StatusTooManyRequests, "you've loaded tools a lot in the last hour; try again later")
		return
	}
	in.URL = strings.TrimSpace(in.URL)
	if in.Auth == nil {
		in.Auth = &authInput{}
	}
	problems := checkURL("url", in.URL, true)
	problems = append(problems, checkAuth(in.Auth)...)
	if restricted {
		problems = append(problems, s.Hosts.check("url", in.URL)...)
		if in.Auth.Kind == "oauth" {
			problems = append(problems, s.Hosts.check("auth.token_url", in.Auth.TokenURL)...)
		}
	}
	if in.Auth.Kind != "" && in.Auth.Secret == "" {
		problems = append(problems, "auth.secret: enter it to load the tools; a saved one is sealed for the voice agent, so the platform can't use it")
	}
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "can't connect yet", "problems": problems})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
	defer cancel()
	client := outboundClient(restricted, discoverTimeout, s.testDial)
	headers := http.Header{}
	switch in.Auth.Kind {
	case "header":
		headers.Set(in.Auth.Header, in.Auth.Secret)
	case "oauth":
		token, err := clientCredentialsToken(ctx, client, in.Auth)
		if err != nil {
			discoverReply(w, err)
			return
		}
		headers.Set("Authorization", "Bearer "+token)
	}
	mcp := &mcpClient{http: client, url: in.URL, headers: headers}
	info, tools, err := mcp.listTools(ctx)
	mcp.close()
	if err != nil {
		discoverReply(w, err)
		return
	}
	usable, skipped := fromMCPTools(tools)
	writeJSON(w, http.StatusOK, map[string]any{"server": info, "tools": usable, "skipped": skipped})
}

func discoverReply(w http.ResponseWriter, err error) {
	var de *discoverError
	if errors.As(err, &de) {
		writeError(w, http.StatusBadGateway, de.msg)
		return
	}
	writeError(w, http.StatusBadGateway, "couldn't reach the server: "+plainNetError(err))
}

// plainNetError says what went wrong without the request details.
func plainNetError(err error) string {
	switch {
	case errors.Is(err, errBlockedAddress):
		return "its address is this machine or a link-local address, which isn't allowed"
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer in time"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// clientCredentialsToken signs in with OAuth client credentials (RFC 6749 4.4).
func clientCredentialsToken(ctx context.Context, client *http.Client, a *authInput) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if a.Scope != "" {
		form.Set("scope", a.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(a.ClientID), url.QueryEscape(a.Secret))
	resp, err := client.Do(req)
	if err != nil {
		return "", discoverFailed("couldn't reach the token URL: %s", plainNetError(err))
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		reason := body.Error
		if reason == "" {
			reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", discoverFailed("the token URL didn't give a token (%s); check the client id, secret and scope", reason)
	}
	return body.AccessToken, nil
}

// ── A small MCP client (Streamable HTTP) ──────────────────────────────────────

type mcpClient struct {
	http    *http.Client
	url     string
	headers http.Header
	session string
	version string
	nextID  int
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcAnswer struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (c *mcpClient) request(ctx context.Context, method string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.version != "" {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	return c.http.Do(req)
}

// call sends one JSON-RPC request (notify: a notification, no answer).
func (c *mcpClient) call(ctx context.Context, method string, params any, notify bool) (json.RawMessage, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	id := 0
	if !notify {
		c.nextID++
		id = c.nextID
		msg["id"] = id
	}
	body, _ := json.Marshal(msg)
	resp, err := c.request(ctx, http.MethodPost, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, discoverFailed("the server refused the sign-in (HTTP %d); check the token", resp.StatusCode)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, discoverFailed("the server redirects elsewhere (HTTP %d); use the address it redirects to", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return nil, discoverFailed("no MCP server answers at that address (HTTP %d); MCP servers usually listen on a path like /mcp", resp.StatusCode)
	case resp.StatusCode >= 400:
		return nil, discoverFailed("the server answered HTTP %d to %s", resp.StatusCode, method)
	}
	if notify {
		return nil, nil
	}
	limited := io.LimitReader(resp.Body, maxDiscoverAnswer)
	var answer *rpcAnswer
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		answer, err = readEventAnswer(limited, id)
	} else {
		answer = &rpcAnswer{}
		err = json.NewDecoder(limited).Decode(answer)
	}
	if err != nil || answer == nil {
		return nil, discoverFailed("that doesn't look like an MCP server: its answer to %s wasn't JSON-RPC", method)
	}
	if answer.Error != nil {
		return nil, discoverFailed("the server said: %s", strings.TrimSpace(answer.Error.Message))
	}
	return answer.Result, nil
}

// readEventAnswer finds the answer with this id in a server-sent event stream.
func readEventAnswer(r io.Reader, id int) (*rpcAnswer, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxDiscoverAnswer)
	var data strings.Builder
	want := fmt.Sprint(id)
	flush := func() *rpcAnswer {
		defer data.Reset()
		if data.Len() == 0 {
			return nil
		}
		var a rpcAnswer
		if json.Unmarshal([]byte(data.String()), &a) == nil && strings.TrimSpace(string(a.ID)) == want {
			return &a
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if a := flush(); a != nil {
				return a, nil
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if a := flush(); a != nil {
		return a, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("no answer in the stream")
}

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpTool struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	InputSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	} `json:"inputSchema"`
}

func (c *mcpClient) listTools(ctx context.Context) (mcpServerInfo, []mcpTool, error) {
	var info mcpServerInfo
	res, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "flow-studio", "version": "1"},
	}, false)
	if err != nil {
		return info, nil, err
	}
	var init struct {
		ProtocolVersion string        `json:"protocolVersion"`
		ServerInfo      mcpServerInfo `json:"serverInfo"`
		Capabilities    struct {
			Tools *json.RawMessage `json:"tools"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(res, &init) != nil || init.ProtocolVersion == "" {
		return info, nil, discoverFailed("that doesn't look like an MCP server: it didn't say which protocol version it speaks")
	}
	info, c.version = init.ServerInfo, init.ProtocolVersion
	if init.Capabilities.Tools == nil {
		return info, nil, discoverFailed("the MCP server %s doesn't offer tools", info.Name)
	}
	if _, err := c.call(ctx, "notifications/initialized", nil, true); err != nil {
		return info, nil, err
	}
	var tools []mcpTool
	cursor := ""
	for page := 0; page < 20; page++ {
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		res, err := c.call(ctx, "tools/list", params, false)
		if err != nil {
			return info, nil, err
		}
		var list struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &list); err != nil {
			return info, nil, discoverFailed("the server's list of tools isn't readable")
		}
		tools = append(tools, list.Tools...)
		if list.NextCursor == "" || len(tools) > 500 {
			break
		}
		cursor = list.NextCursor
	}
	return info, tools, nil
}

// close ends the session (best effort).
func (c *mcpClient) close() {
	if c.session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if resp, err := c.request(ctx, http.MethodDelete, nil); err == nil {
		resp.Body.Close()
	}
}

// fromMCPTools turns a server's tools into the platform's, with the safe
// defaults (not sensitive, nothing locked, nothing saved); the maker then
// says which tools need a verified caller and which inputs the flow sets.
// Tools whose required inputs a flow can't fill (lists, objects) are skipped.
func fromMCPTools(tools []mcpTool) ([]store.IntegrationTool, []skippedTool) {
	usable := []store.IntegrationTool{}
	skipped := []skippedTool{}
	for _, t := range tools {
		if !toolNameRE.MatchString(t.Name) {
			skipped = append(skipped, skippedTool{t.Name, "its name has characters the platform doesn't allow"})
			continue
		}
		desc := strings.TrimSpace(t.Description)
		if desc == "" {
			desc = strings.TrimSpace(t.Title)
		}
		if len(desc) > 1000 {
			desc = desc[:1000]
		}
		required := map[string]bool{}
		for _, r := range t.InputSchema.Required {
			required[r] = true
		}
		out := store.IntegrationTool{Name: t.Name, Description: desc, Inputs: []store.ToolInput{}, Outputs: []string{}, Locked: []string{}, MaxCalls: defaultMaxCalls}
		reason := ""
		names := make([]string, 0, len(t.InputSchema.Properties))
		for name := range t.InputSchema.Properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			var prop struct {
				Type        json.RawMessage `json:"type"`
				Description string          `json:"description"`
				Title       string          `json:"title"`
				Enum        []any           `json:"enum"`
			}
			_ = json.Unmarshal(t.InputSchema.Properties[name], &prop)
			typ := schemaType(prop.Type)
			switch {
			case !inputNameRE.MatchString(name):
				reason = fmt.Sprintf("its input '%s' has a name the platform doesn't allow", name)
			case !inputTypes[typ]:
				if required[name] {
					reason = fmt.Sprintf("its input '%s' is a %s, which a flow can't fill", name, orWord(typ, "value of no simple type"))
				}
				continue // optional: left out
			}
			if reason != "" {
				break
			}
			d := strings.TrimSpace(prop.Description)
			if d == "" {
				d = strings.TrimSpace(prop.Title)
			}
			if d == "" {
				d = strings.ReplaceAll(name, "_", " ")
			}
			if len(d) > 300 {
				d = d[:300]
			}
			in := store.ToolInput{Name: name, Type: typ, Description: d, Required: required[name]}
			if typ == "string" {
				for _, e := range prop.Enum {
					if s, ok := e.(string); ok {
						in.Enum = append(in.Enum, s)
					}
				}
			}
			out.Inputs = append(out.Inputs, in)
		}
		if reason != "" {
			skipped = append(skipped, skippedTool{t.Name, reason})
			continue
		}
		usable = append(usable, out)
	}
	return usable, skipped
}

// schemaType reads a JSON Schema "type": "string", or ["string", "null"].
func schemaType(raw json.RawMessage) string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		var picked []string
		for _, t := range many {
			if t != "null" {
				picked = append(picked, t)
			}
		}
		if len(picked) == 1 {
			return picked[0]
		}
	}
	return ""
}

func orWord(s, def string) string {
	switch s {
	case "":
		return def
	case "array":
		return "list"
	}
	return s
}
