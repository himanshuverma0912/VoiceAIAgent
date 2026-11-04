package api

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/secrets"
)

// secretsFixture: the key pair shared with the Python tests.
func secretsFixture(t *testing.T) (priv, pub string) {
	t.Helper()
	b, err := os.ReadFile("../../../tests/secrets_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		PrivateKey string `json:"private_key"`
		PublicKey  string `json:"public_key"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.PrivateKey, f.PublicKey
}

// openSealed does what the voice agent does (flow_engine/secrets.py), to
// check what this service sealed.
func openSealed(t *testing.T, privB64, aad, sealed string) (string, error) {
	t.Helper()
	raw, _ := base64.StdEncoding.DecodeString(privB64)
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, "v1."))
	if err != nil || len(blob) < 60 {
		return "", fmt.Errorf("not sealed: %q", sealed)
	}
	eph, err := ecdh.X25519().NewPublicKey(blob[:32])
	if err != nil {
		return "", err
	}
	shared, err := priv.ECDH(eph)
	if err != nil {
		return "", err
	}
	k, _ := hkdf.Key(sha256.New, shared, append(append([]byte{}, blob[:32]...), priv.PublicKey().Bytes()...), "flow-studio integration secret v1", 32)
	block, _ := aes.NewCipher(k)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, blob[32:44], blob[44:], []byte(aad))
	return string(plain), err
}

func TestHostPolicy(t *testing.T) {
	if _, err := ParseHostPolicy("mcp.bank.internal, *.cards.bank.internal, 10.20.0.0/16, 10.9.8.7"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"10.0.0.0/33", "http://x", "a*b"} {
		if _, err := ParseHostPolicy(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	h, _ := ParseHostPolicy("mcp.bank.internal, *.cards.bank.internal, 10.20.0.0/16")
	ok := []string{"https://mcp.bank.internal/mcp", "http://pay.cards.bank.internal:8080/mcp", "http://10.20.3.4/mcp"}
	for _, u := range ok {
		if p := h.check("url", u); p != nil {
			t.Errorf("%s: %v", u, p)
		}
	}
	refused := map[string]string{
		"http://127.0.0.1:8080/mcp":        "this machine",
		"http://[::1]/mcp":                 "this machine",
		"http://169.254.169.254/latest":    "link-local",
		"http://localhost:8080/mcp":        "platform's own address",
		"http://db.localhost/mcp":          "platform's own address",
		"http://config-service:8080/x":     "platform's own address",
		"http://metadata.google.internal/": "platform's own address",
		"http://evil.example/mcp":          "isn't one of the hosts",
		"http://10.21.0.1/mcp":             "isn't one of the hosts",
		"http://cards.bank.internal/mcp":   "isn't one of the hosts", // *.x is under x, not x
	}
	for u, want := range refused {
		if p := h.check("url", u); len(p) != 1 || !strings.Contains(p[0], want) {
			t.Errorf("%s: want %q, got %v", u, want, p)
		}
	}
	// No allow list: anything but this machine and the platform's own services.
	open, _ := ParseHostPolicy("")
	if p := open.check("url", "https://anything.example/mcp"); p != nil {
		t.Error(p)
	}
	if p := open.check("url", "http://[fd00:ec2::254]/"); p == nil {
		t.Error("AWS IPv6 metadata address allowed")
	}
}

// A name that resolves to this machine is refused when dialled, and a
// redirect isn't followed.
func TestOutboundClientRefusesThisMachine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
	}))
	defer srv.Close()
	_, err := outboundClient(true, 2*time.Second, nil).Get(srv.URL)
	if err == nil || !strings.Contains(plainNetError(err), "this machine") {
		t.Fatalf("restricted client reached %s: %v", srv.URL, err)
	}
	resp, err := outboundClient(false, 2*time.Second, nil).Get(srv.URL)
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("unrestricted: %v", err)
	}
	resp.Body.Close()
}

// fakeMCP is a small MCP server: Authorization must be "Bearer <token>"; tool
// lists come in two pages, the second as a server-sent event stream.
func fakeMCP(t *testing.T, token string) *httptest.Server {
	t.Helper()
	tools1 := `[
		{"name": "get_emi_schedule", "description": "The caller's EMI schedule.", "inputSchema": {"type": "object",
			"properties": {"loan_id": {"type": "string", "description": "Loan number"}, "months": {"type": ["integer", "null"]},
				"plan": {"type": "string", "enum": ["basic", "plus"]}, "filters": {"type": "object"}},
			"required": ["loan_id"]}},
		{"name": "bulk_update", "description": "Updates many loans.", "inputSchema": {"type": "object",
			"properties": {"loan_ids": {"type": "array", "items": {"type": "string"}}}, "required": ["loan_ids"]}}
	]`
	tools2 := `[{"name": "ping", "title": "Checks the server", "inputSchema": {"type": "object"}}, {"name": "bad name!", "inputSchema": {}}]`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&msg) //nolint:errcheck
		answer := func(result string) string {
			return fmt.Sprintf(`{"jsonrpc": "2.0", "id": %d, "result": %s}`, *msg.ID, result)
		}
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s-1")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, answer(`{"protocolVersion": "2025-06-18", "capabilities": {"tools": {}}, "serverInfo": {"name": "loans", "version": "2.1"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "s-1" || r.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !bytes.Contains(msg.Params, []byte("page2")) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, answer(`{"tools": `+tools1+`, "nextCursor": "page2"}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\n")
			fmt.Fprint(w, "event: message\ndata: "+strings.ReplaceAll(answer(`{"tools": `+tools2+`}`), "\n", " ")+"\n\n")
		default:
			fmt.Fprintf(w, `{"jsonrpc": "2.0", "id": %d, "error": {"code": -32601, "message": "no such method"}}`, *msg.ID)
		}
	}))
}

// Makers add their workspace's MCP servers; a checker approves them before
// flows using them can be submitted or called live; tokens are sealed for
// the agent and never shown.
func TestWorkspaceIntegrations(t *testing.T) {
	priv, pub := secretsFixture(t)
	key, _ := secrets.PublicKey(pub)
	mcp := fakeMCP(t, "loans-token")
	defer mcp.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(mcp.URL, "http://"))
	c, kc := peopleServer(t, func(s *Server) {
		s.SecretsKey = key
		s.Hosts, _ = ParseHostPolicy("*.cards.test")
		// mcp.cards.test is the fake server.
		s.testDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasSuffix(addr, ".cards.test:"+port) {
				addr = "127.0.0.1:" + port
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	})
	asha := kc.Token(t, "asha@demo.corp", []string{"viewer"}, nil, nil) // maker in cards
	carl := kc.Token(t, "carl@demo.corp", []string{"viewer"}, nil, nil) // maker and checker in cards
	vera := kc.Token(t, "vera@demo.corp", []string{"viewer"}, nil, nil) // maker in emi
	c.json("PUT", "/v1/workspaces/cards", adminToken, []byte(`{"name": "Credit cards"}`), 201)
	c.json("PUT", "/v1/workspaces/emi", adminToken, []byte(`{"name": "EMI"}`), 201)
	c.json("PUT", "/v1/workspaces/cards/members/asha@demo.corp", adminToken, member(true, false, false), 201)
	c.json("PUT", "/v1/workspaces/cards/members/carl@demo.corp", adminToken, member(true, true, false), 201)
	c.json("PUT", "/v1/workspaces/emi/members/vera@demo.corp", adminToken, member(true, false, false), 201)
	c.json("PUT", "/v1/integrations/demo_bank", adminToken, toBody(integrationFile(t, "demo_bank.json")), 201)

	// ── Loading an MCP server's tools ──
	mcpURL := fmt.Sprintf("http://mcp.cards.test:%s/mcp", port)
	discover := func(token, ws, url, secret string, want int) map[string]any {
		t.Helper()
		body := map[string]any{"workspace": ws, "url": url, "auth": map[string]any{"kind": "header", "header": "Authorization", "secret": secret}}
		return c.json("POST", "/v1/integrations/discover", token, toBody(body), want)
	}
	if res := discover(asha, "cards", mcpURL, "Bearer wrong", 502); !strings.Contains(toJSON(res), "refused the sign-in (HTTP 401)") {
		t.Fatalf("wrong token: %v", res)
	}
	res := discover(asha, "cards", mcpURL, "Bearer loans-token", 200)
	if toJSON(res["server"]) != `{"name":"loans","version":"2.1"}` {
		t.Fatalf("server: %v", res["server"])
	}
	got := toJSON(res["tools"])
	for _, want := range []string{
		`"name":"get_emi_schedule"`, `{"description":"Loan number","name":"loan_id","required":true,"type":"string"}`,
		`{"description":"months","name":"months","required":false,"type":"integer"}`, `"enum":["basic","plus"]`,
		`"description":"Checks the server"`, `"max_calls":5`, `"sensitive":false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tools: want %s in %s", want, got)
		}
	}
	if strings.Contains(got, "filters") || strings.Contains(got, "bulk_update") {
		t.Errorf("unusable inputs or tools offered: %s", got)
	}
	if s := toJSON(res["skipped"]); !strings.Contains(s, `"bulk_update","reason":"its input 'loan_ids' is a list, which a flow can't fill"`) || !strings.Contains(s, "bad name!") {
		t.Errorf("skipped: %s", s)
	}
	for url, want := range map[string]string{
		"http://localhost:" + port + "/mcp": "platform's own address",
		"http://127.0.0.1:" + port + "/mcp": "this machine",
		"http://mcp.example.com/mcp":        "isn't one of the hosts",
	} {
		if res := discover(asha, "cards", url, "Bearer loans-token", 422); !strings.Contains(problemText(res), want) {
			t.Errorf("%s: %s", url, problemText(res))
		}
	}
	discover(vera, "cards", mcpURL, "Bearer loans-token", 403) // not her workspace
	c.json("POST", "/v1/integrations/discover", asha, toBody(map[string]any{"workspace": "cards", "url": mcpURL, "auth": map[string]any{"kind": "header"}}), 422)

	// ── Proposing ──
	bank := integrationFile(t, "demo_bank.json") // the same tools, as the cards team's own server
	delete(bank, "auth_header")
	delete(bank, "auth_env")
	bank["url"], bank["workspace"], bank["note"] = mcpURL, "cards", "Our loans MCP server"
	bank["auth"] = map[string]any{"kind": "header", "header": "Authorization", "secret": "Bearer loans-token"}
	cardsMCP := "/v1/integrations/cards_mcp"

	wrong := map[string]any{}
	json.Unmarshal(toBody(bank), &wrong) //nolint:errcheck
	wrong["auth_header"], wrong["auth_env"], wrong["test_url"] = "Authorization", "INTEGRATION_DEMO_BANK_TOKEN", "http://169.254.169.254/"
	res = c.json("PUT", cardsMCP, asha, toBody(wrong), 422)
	for _, want := range []string{"auth_env: only platform integrations", "test_url: can't point at this machine"} {
		if !strings.Contains(problemText(res), want) {
			t.Errorf("want %q in %s", want, problemText(res))
		}
	}
	c.json("PUT", cardsMCP, vera, toBody(bank), 403)                     // not a maker in cards
	c.json("PUT", "/v1/integrations/demo_bank", asha, toBody(bank), 409) // a platform integration's id
	delete(bank, "workspace")
	c.json("PUT", "/v1/integrations/other", asha, toBody(bank), 403) // no workspace: a platform integration
	bank["workspace"] = "cards"

	res = c.json("PUT", cardsMCP, asha, toBody(bank), 201)
	if res["pending"] != true || res["created"] != true {
		t.Fatalf("propose: %v", res)
	}
	view := toJSON(res["integration"])
	if strings.Contains(view, "v1.") || strings.Contains(view, "loans-token") || !strings.Contains(view, `"has_secret":true`) ||
		!strings.Contains(view, `"live":false`) || !strings.Contains(view, `"pending_by":"asha@demo.corp"`) || !strings.Contains(view, `"workspaces":["cards"]`) {
		t.Fatalf("view: %s", view)
	}
	if all := toJSON(c.json("GET", "/v1/integrations", vera, nil, 200)); strings.Contains(all, "cards_mcp") {
		t.Fatalf("another workspace sees a proposal: %s", all)
	}
	if all := toJSON(c.json("GET", "/v1/integrations", adminToken, nil, 200)); !strings.Contains(all, "cards_mcp") || strings.Contains(all, "v1.") {
		t.Fatalf("admin view: %s", all)
	}

	// A draft can use it, and test calls get it with the sealed token...
	flow := bytes.ReplaceAll(spec(t, "credit-card-helpline.json"), []byte(`"demo_bank.`), []byte(`"cards_mcp.`))
	if saved := c.json("PUT", "/v1/flows/credit_card_helpline/draft?workspace=cards", asha, flow, 200); saved["valid"] != true {
		t.Fatalf("draft: %v", saved["problems"])
	}
	draft := c.json("GET", "/internal/v1/flows/credit_card_helpline/draft", runtimeToken, nil, 200)
	ri := draft["integrations"].(map[string]any)["cards_mcp"].(map[string]any)
	sealed := ri["auth"].(map[string]any)["sealed"].(string)
	if plain, err := openSealed(t, priv, "cards_mcp", sealed); err != nil || plain != "Bearer loans-token" || ri["restricted"] != true || ri["url"] != mcpURL {
		t.Fatalf("test-call integration: %v (%v)", ri, err)
	}
	if _, err := openSealed(t, priv, "demo_bank", sealed); err == nil {
		t.Fatal("a sealed token opened for another integration")
	}
	// ...but the flow can't be submitted until it's approved.
	res = c.json("POST", "/v1/flows/credit_card_helpline/reviews", asha, []byte(`{}`), 422)
	if !strings.Contains(problemText(res), "the integration 'cards_mcp' is waiting for a checker to approve it") {
		t.Fatalf("submit before approval: %s", problemText(res))
	}

	// ── Reviewing ──
	c.json("POST", cardsMCP+"/approve", asha, nil, 403) // not a checker
	c.json("POST", cardsMCP+"/reject", carl, []byte(`{"note": " "}`), 422)
	res = c.json("POST", cardsMCP+"/reject", carl, []byte(`{"note": "Use the sandbox for test calls"}`), 200)
	if v := toJSON(res["integration"]); !strings.Contains(v, `"rejected_note":"Use the sandbox for test calls"`) || strings.Contains(v, `"pending":{`) {
		t.Fatalf("rejected: %s", v)
	}
	res = c.json("POST", "/v1/validate?workspace=cards", asha, flow, 200)
	if !strings.Contains(problemText(res), "was turned down (Use the sandbox for test calls)") {
		t.Fatalf("after rejection: %s", problemText(res))
	}
	c.json("POST", cardsMCP+"/approve", carl, nil, 409) // nothing waiting

	sandbox := fmt.Sprintf("http://sandbox.cards.test:%s/mcp", port)
	bank["test_url"] = sandbox
	delete(bank["auth"].(map[string]any), "secret")
	if res := c.json("PUT", cardsMCP, asha, toBody(bank), 422); !strings.Contains(problemText(res), "enter the token again: the address changed") {
		t.Fatalf("changed address kept the token: %s", problemText(res))
	}
	bank["auth"].(map[string]any)["secret"] = "Bearer loans-token"
	c.json("PUT", cardsMCP, asha, toBody(bank), 200)
	res = c.json("POST", cardsMCP+"/approve", carl, []byte(`{"note": "OK"}`), 200)
	if v := toJSON(res["integration"]); !strings.Contains(v, `"live":true`) || !strings.Contains(v, `"approved_by":"carl@demo.corp"`) || strings.Contains(v, `"pending":{`) {
		t.Fatalf("approved: %s", v)
	}
	// Others in the workspace's flows... no: only this workspace's flows may use it.
	if all := toJSON(c.json("GET", "/v1/integrations", vera, nil, 200)); strings.Contains(all, "cards_mcp") {
		t.Fatalf("emi sees cards' own integration: %s", all)
	}

	// Submitted, approved, called live: the live URL and the sealed token.
	r1 := c.json("POST", "/v1/flows/credit_card_helpline/reviews", asha, []byte(`{}`), 201)
	c.json("POST", fmt.Sprintf("/v1/flows/credit_card_helpline/reviews/%v/approve", r1["review_id"]), carl, nil, 201)
	c.json("PUT", "/v1/numbers/+918045678902", adminToken, []byte(`{"flow_id": "credit_card_helpline"}`), 200)
	_, hdr, body := c.do("GET", "/internal/v1/resolve?number=%2B918045678902", runtimeToken, nil)
	var resolved struct {
		Integrations map[string]runtimeIntegration `json:"integrations"`
	}
	json.Unmarshal(body, &resolved) //nolint:errcheck
	live := resolved.Integrations["cards_mcp"]
	if live.URL != mcpURL || !live.Restricted || live.Auth == nil || live.Auth.Header != "Authorization" {
		t.Fatalf("resolve: %s", body)
	}
	etag := hdr.Get("ETag")

	// A change waiting for review doesn't touch live calls; a tool only in it
	// can be tested but not submitted. Keeping the address keeps the token.
	delete(bank["auth"].(map[string]any), "secret")
	tools := bank["tools"].([]any)
	extra := map[string]any{}
	json.Unmarshal(toBody(tools[0]), &extra) //nolint:errcheck
	extra["name"] = "verify_customer_v2"
	bank["tools"] = append(tools, extra)
	bank["note"] = "Adds verify_customer_v2"
	c.json("PUT", cardsMCP, asha, toBody(bank), 200)
	if status, _, _ := c.do("GET", "/internal/v1/resolve?number=%2B918045678902", runtimeToken, nil, "If-None-Match", etag); status != 304 {
		t.Fatalf("a pending change changed live calls: %d", status)
	}
	all := c.json("GET", "/v1/integrations", asha, nil, 200)["integrations"].([]any)
	for _, i := range all {
		i := i.(map[string]any)
		if i["integration_id"] == "cards_mcp" && i["pending"].(map[string]any)["auth"].(map[string]any)["has_secret"] != true {
			t.Fatalf("the token wasn't kept: %v", i["pending"])
		}
	}
	v2 := bytes.Replace(flow, []byte(`"cards_mcp.verify_customer"`), []byte(`"cards_mcp.verify_customer_v2"`), 1)
	if saved := c.json("PUT", "/v1/flows/credit_card_helpline/draft", asha, v2, 200); saved["valid"] != true {
		t.Fatalf("draft with the new tool: %v", saved["problems"])
	}
	res = c.json("POST", "/v1/flows/credit_card_helpline/reviews", asha, []byte(`{}`), 422)
	if !strings.Contains(problemText(res), "'cards_mcp' gets the tool 'verify_customer_v2' only once its change waiting for review is approved") {
		t.Fatalf("submit with a pending tool: %s", problemText(res))
	}
	c.json("POST", cardsMCP+"/withdraw", carl, nil, 403) // asha's change
	c.json("POST", cardsMCP+"/withdraw", asha, nil, 200)

	// Carl can't approve his own change.
	bank["note"] = "carl's change"
	c.json("PUT", cardsMCP, carl, toBody(bank), 200)
	if res := c.json("POST", cardsMCP+"/approve", carl, nil, 403); !strings.Contains(toJSON(res), "someone else has to approve it") {
		t.Fatalf("self-approval: %v", res)
	}
	c.json("POST", cardsMCP+"/withdraw", carl, nil, 200)

	// Switching off: makers may (to stop a misbehaving server now); switching
	// on needs a checker.
	c.json("POST", cardsMCP+"/enabled", asha, []byte(`{"enabled": false}`), 200)
	_, _, body = c.do("GET", "/internal/v1/resolve?number=%2B918045678902", runtimeToken, nil)
	if strings.Contains(string(body), "cards_mcp\":{") {
		t.Fatalf("switched off but still sent: %s", body)
	}
	c.json("POST", cardsMCP+"/enabled", asha, []byte(`{"enabled": true}`), 403)
	c.json("POST", cardsMCP+"/enabled", carl, []byte(`{"enabled": true}`), 200)

	// Removing: checkers of the workspace, once nothing uses it.
	c.json("DELETE", cardsMCP, asha, nil, 403)
	c.json("DELETE", cardsMCP, carl, nil, 409)
	c.json("DELETE", cardsMCP, vera, nil, 404)

	entries := toJSON(c.json("GET", "/v1/audit", adminToken, nil, 200)["entries"])
	for _, want := range []string{"integration.propose", "integration.approve", "integration.reject", "integration.withdraw", "integration.enabled"} {
		if !strings.Contains(entries, want) {
			t.Errorf("%s isn't in the audit log", want)
		}
	}
	if strings.Contains(entries, "loans-token") || strings.Contains(entries, sealed) {
		t.Error("a token is in the audit log")
	}
}

// Without the public key, tokens can't be saved (platform admins can still
// use the agent's variables).
func TestIntegrationTokensNeedTheKey(t *testing.T) {
	c, kc := peopleServer(t, nil)
	asha := kc.Token(t, "asha@demo.corp", []string{"viewer"}, nil, nil)
	c.json("PUT", "/v1/workspaces/cards", adminToken, []byte(`{"name": "Credit cards"}`), 201)
	c.json("PUT", "/v1/workspaces/cards/members/asha@demo.corp", adminToken, member(true, false, false), 201)
	bank := integrationFile(t, "demo_bank.json")
	delete(bank, "auth_header")
	delete(bank, "auth_env")
	bank["url"], bank["workspace"] = "https://mcp.cards.example/mcp", "cards"
	bank["auth"] = map[string]any{"kind": "header", "secret": "Bearer x"}
	if res := c.json("PUT", "/v1/integrations/cards_mcp", asha, toBody(bank), 422); !strings.Contains(problemText(res), "INTEGRATION_SECRETS_PUBLIC_KEY isn't set") {
		t.Fatalf("%s", problemText(res))
	}
	delete(bank, "auth") // a server without sign-in is fine
	c.json("PUT", "/v1/integrations/cards_mcp", asha, toBody(bank), 201)
}
