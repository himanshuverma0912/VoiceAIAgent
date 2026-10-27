package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func integrationFile(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../../infra/integrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func toBody(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func problemText(res map[string]any) string {
	b, _ := json.Marshal(res["problems"])
	return string(b)
}

// Integrations: admins register them; flows use their tools; the rules about
// verification and locked inputs are checked on every draft; the agent gets
// what it needs with the flow.
func TestIntegrations(t *testing.T) {
	c, kc := peopleServer(t, nil)
	asha := kc.Token(t, "asha@demo.corp", []string{"viewer"}, nil, nil) // maker in cards
	vera := kc.Token(t, "vera@demo.corp", []string{"viewer"}, nil, nil) // in emi only
	c.json("PUT", "/v1/workspaces/cards", adminToken, []byte(`{"name": "Credit cards"}`), 201)
	c.json("PUT", "/v1/workspaces/emi", adminToken, []byte(`{"name": "EMI"}`), 201)
	c.json("PUT", "/v1/workspaces/cards/members/asha@demo.corp", adminToken, member(true, false, false), 201)
	c.json("PUT", "/v1/workspaces/emi/members/vera@demo.corp", adminToken, member(true, false, false), 201)

	bank := integrationFile(t, "demo_bank.json")
	bank["workspaces"] = []string{"cards"}
	bank["test_url"] = "http://demo-bank-sandbox:8000/mcp"

	// Only admins add them, and the entry is checked.
	c.json("PUT", "/v1/integrations/demo_bank", asha, toBody(bank), 403)
	bad := integrationFile(t, "demo_bank_rest.json")
	bad["auth_env"] = "LIVEKIT_API_SECRET"
	bad["tools"].([]any)[1].(map[string]any)["method"] = "GET"
	bad["tools"].([]any)[0].(map[string]any)["locked"] = []string{"pin"}
	res := c.json("PUT", "/v1/integrations/demo_bank_rest", adminToken, toBody(bad), 422)
	for _, want := range []string{"auth_env: must be INTEGRATION_", "tools[1].method: sensitive tools must use POST", "tools[0].locked[0]: 'pin' isn't one of the tool's inputs"} {
		if !strings.Contains(problemText(res), want) {
			t.Errorf("want %q in %s", want, problemText(res))
		}
	}
	res = c.json("PUT", "/v1/integrations/demo_bank", adminToken, toBody(bank), 201)
	if res["created"] != true {
		t.Fatalf("put: %v", res)
	}

	// Makers see the integrations offered to their workspaces, without the URL
	// or which variable holds the credential.
	list := c.json("GET", "/v1/integrations", asha, nil, 200)["integrations"].([]any)
	if len(list) != 1 {
		t.Fatalf("asha's list: %v", list)
	}
	seen := toJSON(list[0])
	if strings.Contains(seen, "demo-bank:8000") || strings.Contains(seen, "INTEGRATION_") || !strings.Contains(seen, "get_card_balance") {
		t.Fatalf("redacted view: %s", seen)
	}
	if list := c.json("GET", "/v1/integrations", vera, nil, 200)["integrations"].([]any); len(list) != 0 {
		t.Fatalf("vera shouldn't see a cards-only integration: %v", list)
	}
	if all := toJSON(c.json("GET", "/v1/integrations", adminToken, nil, 200)); !strings.Contains(all, "http://demo-bank:8000/mcp") {
		t.Fatalf("admins see everything: %s", all)
	}

	// The credit card flow is valid in its workspace.
	flow := spec(t, "credit-card-helpline.json")
	saved := c.json("PUT", "/v1/flows/credit_card_helpline/draft?workspace=cards", asha, flow, 200)
	if saved["valid"] != true {
		t.Fatalf("credit card flow: %v", saved["problems"])
	}

	// The same flow in a workspace the integration isn't offered to.
	other := bytes.Replace(flow, []byte(`"credit_card_helpline"`), []byte(`"cc_copy"`), 1)
	res = c.json("PUT", "/v1/flows/cc_copy/draft?workspace=emi", vera, other, 200)
	if !strings.Contains(problemText(res), "the integration 'demo_bank' isn't offered to workspace emi") {
		t.Fatalf("other workspace: %s", problemText(res))
	}

	// Breaking the rules, one at a time.
	check := func(name string, change func(f map[string]any), want ...string) {
		t.Helper()
		var f map[string]any
		json.Unmarshal(flow, &f) //nolint:errcheck
		change(f)
		res := c.json("POST", "/v1/validate?workspace=cards", asha, toBody(f), 200)
		got := problemText(res)
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: want %q in\n%s", name, w, got)
			}
		}
	}
	nodes := func(f map[string]any) map[string]any { return f["nodes"].(map[string]any) }
	node := func(f map[string]any, id string) map[string]any { return nodes(f)[id].(map[string]any) }

	check("not verified first", func(f map[string]any) { node(f, "verify")["verifies"] = false },
		"nodes.account_help.tools[0].tool: 'get_card_balance' needs a verified caller, but this step can be reached without passing an API step that verifies the caller",
		"nodes.dispute.tool: 'raise_dispute' needs a verified caller")
	check("verification failure leads to the balance", func(f map[string]any) { node(f, "verify")["on_error"] = "account_help" },
		"nodes.account_help.tools[0].tool: 'get_card_balance' needs a verified caller")
	check("sensitive tool on every step", func(f map[string]any) {
		f["tools"] = []any{map[string]any{"tool": "demo_bank.recent_transactions", "inputs": map[string]any{"card_last4": "{{card_last4}}", "date_of_birth": "{{date_of_birth}}"}}}
	}, "tools[0].tool: 'recent_transactions' needs a verified caller, so it can't be on every step")
	check("the AI would choose the card", func(f map[string]any) {
		node(f, "account_help")["tools"].([]any)[0].(map[string]any)["inputs"] = map[string]any{"date_of_birth": "{{date_of_birth}}"}
	}, "nodes.account_help.tools[0].tool: 'get_card_balance' needs the flow to set its input 'card_last4' (the AI may not choose it)")
	check("unknown input, unknown output, unknown tool", func(f map[string]any) {
		node(f, "verify")["inputs"].(map[string]any)["pin"] = "1234"
		node(f, "dispute")["save"] = []any{"reference", "refund"}
		f["tools"] = []any{map[string]any{"tool": "demo_bank.block_card"}, map[string]any{"tool": "crm.lookup"}}
	},
		"nodes.verify.inputs.pin: 'verify_customer' has no input called 'pin'",
		"nodes.dispute.save[1]: 'raise_dispute' doesn't return 'refund'; it returns reference, callback_within",
		"tools[0].tool: 'demo_bank' has no tool called 'block_card'",
		"tools[1].tool: there's no integration called 'crm'")
	check("api step missing a required input", func(f map[string]any) { delete(node(f, "dispute")["inputs"].(map[string]any), "merchant") },
		"nodes.dispute.tool: 'raise_dispute' needs its input 'merchant'; an API step has no AI to fill it in")

	// Test calls get the integration's test URL; phone calls its real URL.
	draft := c.json("GET", "/internal/v1/flows/credit_card_helpline/draft", runtimeToken, nil, 200)
	ri := draft["integrations"].(map[string]any)["demo_bank"].(map[string]any)
	if ri["url"] != "http://demo-bank-sandbox:8000/mcp" || ri["auth_env"] != "INTEGRATION_DEMO_BANK_TOKEN" || len(ri["tools"].([]any)) != 5 { // only the tools the flow uses
		t.Fatalf("draft integrations: %v", ri)
	}

	// Published: resolve brings the integrations; a change to one changes the ETag.
	c.json("POST", "/v1/flows/credit_card_helpline/publish", adminToken, nil, 201)
	c.json("PUT", "/v1/numbers/+918045678902", adminToken, []byte(`{"flow_id": "credit_card_helpline"}`), 200)
	status, hdr, body := c.do("GET", "/internal/v1/resolve?number=%2B918045678902", runtimeToken, nil)
	var resolved map[string]any
	json.Unmarshal(body, &resolved) //nolint:errcheck
	ri = resolved["integrations"].(map[string]any)["demo_bank"].(map[string]any)
	if status != 200 || ri["url"] != "http://demo-bank:8000/mcp" || ri["kind"] != "mcp" {
		t.Fatalf("resolve: %d %s", status, body)
	}
	etag := hdr.Get("ETag")
	if status, _, _ := c.do("GET", "/internal/v1/resolve?number=%2B918045678902", runtimeToken, nil, "If-None-Match", etag); status != 304 {
		t.Fatalf("unchanged: %d", status)
	}
	bank["url"] = "http://core-banking.internal/mcp"
	c.json("PUT", "/v1/integrations/demo_bank", adminToken, toBody(bank), 200)
	status, hdr, body = c.do("GET", "/internal/v1/resolve?number=%2B918045678902", runtimeToken, nil, "If-None-Match", etag)
	if status != 200 || hdr.Get("ETag") == etag || !strings.Contains(string(body), "core-banking.internal") {
		t.Fatalf("after changing the URL: %d %s", status, body)
	}
	// Switched off: the agent no longer gets it, so its tools stop at once.
	bank["enabled"] = false
	res = c.json("PUT", "/v1/integrations/demo_bank", adminToken, toBody(bank), 200)
	if toJSON(res["used_by"]) != `["cc_copy","credit_card_helpline"]` {
		t.Fatalf("used by: %v", res["used_by"])
	}
	_, _, body = c.do("GET", "/internal/v1/resolve?number=%2B918045678902", runtimeToken, nil)
	if strings.Contains(string(body), "demo_bank\":{") {
		t.Fatalf("switched off but still sent: %s", body)
	}
	c.json("DELETE", "/v1/integrations/demo_bank", adminToken, nil, 409)

	// Call history: which tools a call used, nothing else.
	record := map[string]any{
		"call_id": "AJ_tools1", "flow_id": "credit_card_helpline", "flow_version": 1, "flow_source": "config-service",
		"channel": "phone", "runtime": "livekit", "started_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"duration_ms": 60000, "outcome": "completed", "steps": []any{map[string]any{"node": "triage", "ms": 1000}},
		"tools": []any{
			map[string]any{"tool": "demo_bank.verify_customer", "ok": true, "ms": 120},
			map[string]any{"tool": "demo_bank.get_card_balance", "ok": true, "ms": 300},
			map[string]any{"tool": "demo_bank.get_card_balance", "ok": false, "ms": 5000},
		},
		"turns": map[string]any{"caller": 4, "agent": 5}, "latency_ms": nil,
		"usage": map[string]any{"llm_input_tokens": 1, "llm_output_tokens": 1, "stt_audio_seconds": 1, "tts_characters": 1, "tts_audio_seconds": 1},
	}
	c.json("POST", "/internal/v1/calls", runtimeToken, toBody(record), 201)
	record["call_id"], record["tools"] = "AJ_tools2", []any{map[string]any{"tool": "get balance", "ok": true, "ms": 1}}
	c.json("POST", "/internal/v1/calls", runtimeToken, toBody(record), 422)
	sum := c.json("GET", "/v1/flows/credit_card_helpline/calls/summary?days=1", asha, nil, 200)
	tools := toJSON(sum["tools"])
	if !strings.Contains(tools, `{"avg_ms":2650,"failed":1,"p95_ms":4765,"tool":"demo_bank.get_card_balance","uses":2}`) {
		t.Fatalf("tool stats: %s", tools)
	}
	calls := c.json("GET", "/v1/flows/credit_card_helpline/calls?days=1", asha, nil, 200)["calls"].([]any)
	if got := fmt.Sprint(len(calls[0].(map[string]any)["tools"].([]any))); got != "3" {
		t.Fatalf("call tools: %v", calls[0])
	}
	if !strings.Contains(toJSON(c.json("GET", "/v1/audit", adminToken, nil, 200)["entries"]), "integration.update") {
		t.Error("integration changes aren't in the audit log")
	}
}
