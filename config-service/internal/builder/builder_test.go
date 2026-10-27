package builder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/flowspec"
)

// A small valid flow, as the model might write it (with a wrong id and an
// extra fence around it, which the builder must cope with).
const goodFlow = `{
  "flow": {
    "schema_version": 1,
    "flow_id": "whatever",
    "name": "Card help",
    "persona": "You are a polite card helpline agent.",
    "guardrails": ["Never ask for a full card number."],
    "greeting": "Hello, card helpline.",
    "settings": {"language": "en", "llm_temperature": 0},
    "start_node": "listen",
    "nodes": {
      "listen": {"type": "agent", "on_enter": "wait", "instructions": "Find out what the caller needs.",
                 "transitions": [{"to": "goodbye", "when": "The caller has nothing more to ask."}]},
      "goodbye": {"type": "end", "instructions": "Thank the caller."}
    }
  },
  "notes": ["Card blocking needs a real system; the agent tells callers to use the app."]
}`

// The same flow with a broken link, for the first attempt.
var brokenFlow = strings.Replace(goodFlow, `"to": "goodbye"`, `"to": "good_bye"`, 1)

type fakeLLM struct {
	mu      sync.Mutex
	replies []string
	got     []map[string]any
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(body, &req) //nolint:errcheck
	f.mu.Lock()
	f.got = append(f.got, req)
	reply := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	f.mu.Unlock()
	if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
		http.Error(w, "bad request", 400)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"choices": []any{map[string]any{"message": map[string]any{"content": reply}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 200},
	})
}

func check(flow []byte) []string {
	res, err := flowspec.ValidateJSON(flow)
	if err != nil {
		return []string{err.Error()}
	}
	return res.Problems
}

func TestBuildFixesProblems(t *testing.T) {
	llm := &fakeLLM{replies: []string{"```json\n" + brokenFlow + "\n```", goodFlow}}
	srv := httptest.NewServer(llm)
	defer srv.Close()
	b := New(Config{URL: srv.URL + "/", APIKey: "test-key", Model: "flow-builder"})

	var steps []string
	res, err := b.Build(context.Background(), Request{FlowID: "card_help", Name: "Card help", Description: "A long description..."},
		check, func(s string) { steps = append(steps, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 0 || res.Attempts != 2 || res.Steps != 2 || res.InputTokens != 2000 || res.OutputTokens != 400 {
		t.Fatalf("result: %+v", res)
	}
	var flow map[string]any
	if err := json.Unmarshal(res.Flow, &flow); err != nil {
		t.Fatal(err)
	}
	if flow["flow_id"] != "card_help" || flow["name"] != "Card help" {
		t.Fatalf("id and name must be ours: %v %v", flow["flow_id"], flow["name"])
	}
	if !strings.HasPrefix(string(res.Flow), "{\n  \"schema_version\": 1,\n  \"flow_id\": \"card_help\",\n  \"name\": \"Card help\",\n  \"persona\"") {
		t.Fatalf("key order not kept:\n%s", res.Flow[:120])
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "Card blocking") {
		t.Fatalf("notes: %v", res.Notes)
	}

	// What the LLM was sent: the guide with the example, JSON mode, the
	// description; then the problems on the second attempt.
	first, second := llm.got[0], llm.got[1]
	if first["model"] != "flow-builder" || toJSON(first["response_format"]) != `{"type":"json_object"}` {
		t.Fatalf("request: %v", first["model"])
	}
	msgs := first["messages"].([]any)
	system := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(system, `"flow_id": "emi_payment_issue"`) || strings.Contains(system, "%EXAMPLE%") {
		t.Fatal("the example flow isn't in the guide")
	}
	if !strings.Contains(msgs[1].(map[string]any)["content"].(string), "A long description...") {
		t.Fatal("description not sent")
	}
	fix := second["messages"].([]any)
	if len(fix) != 4 || !strings.Contains(fix[3].(map[string]any)["content"].(string), "'good_bye' is not a node") {
		t.Fatalf("second attempt should carry the problems: %v", fix[len(fix)-1])
	}
	if len(steps) < 3 || !strings.Contains(steps[len(steps)-2], "Fixing 1 problem(s)") {
		t.Fatalf("progress: %v", steps)
	}
}

func TestBuildGivesUpWithProblemsLeft(t *testing.T) {
	srv := httptest.NewServer(&fakeLLM{replies: []string{brokenFlow}})
	defer srv.Close()
	b := New(Config{URL: srv.URL, APIKey: "test-key", Model: "m", MaxAttempts: 2})
	res, err := b.Build(context.Background(), Request{FlowID: "card_help", Name: "Card help", Description: "x"}, check, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	// The flow comes back anyway, with its problems, for the maker to fix in the editor.
	if res.Attempts != 2 || len(res.Problems) == 0 || res.Flow == nil {
		t.Fatalf("%+v", res)
	}
}

func TestBuildErrors(t *testing.T) {
	srv := httptest.NewServer(&fakeLLM{replies: []string{"not json at all"}})
	defer srv.Close()
	b := New(Config{URL: srv.URL, APIKey: "test-key", Model: "m", MaxAttempts: 2})
	if _, err := b.Build(context.Background(), Request{FlowID: "f", Name: "F", Description: "x"}, check, func(string) {}); err == nil {
		t.Fatal("no usable flow should be an error")
	}
	bad := New(Config{URL: srv.URL, APIKey: "wrong", Model: "m"})
	if _, err := bad.Build(context.Background(), Request{FlowID: "f", Name: "F", Description: "x"}, check, func(string) {}); err == nil || !strings.Contains(err.Error(), "answered 400") {
		t.Fatalf("auth error: %v", err)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
