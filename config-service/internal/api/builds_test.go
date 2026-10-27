package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/builder"
)

// The AI builder through the API: a maker starts a build, polls it, and gets a
// valid flow back (an LLM stand-in writes the flow).
func TestFlowBuilds(t *testing.T) {
	flow := strings.ReplaceAll(string(spec(t, "complaint-helpline.json")), "\r", "")
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply, _ := json.Marshal(map[string]any{"flow": json.RawMessage(flow), "notes": []string{"Assumed Hindi-first callers."}})
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"choices": []any{map[string]any{"message": map[string]any{"content": string(reply)}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 18000, "completion_tokens": 3000},
		})
	}))
	defer llm.Close()
	c, kc := peopleServer(t, func(s *Server) {
		s.Builder = builder.New(builder.Config{URL: llm.URL, APIKey: "k", Model: "flow-builder"})
	})
	asha := kc.Token(t, "asha@demo.corp", []string{"viewer"}, nil, nil)
	vera := kc.Token(t, "vera@demo.corp", []string{"viewer"}, nil, nil)
	c.json("PUT", "/v1/workspaces/complaints", adminToken, []byte(`{"name": "Complaints"}`), 201)
	c.json("PUT", "/v1/workspaces/complaints/members/asha@demo.corp", adminToken, member(true, false, false), 201)
	c.json("PUT", "/v1/workspaces/complaints/members/vera@demo.corp", adminToken, member(false, false, false), 201)

	description := strings.Repeat("When a citizen calls about a water problem, ask since when and where. ", 20)
	body := func(ws string) []byte {
		b, _ := json.Marshal(map[string]any{"workspace": ws, "flow_id": "water_line", "name": "Water complaints", "description": description})
		return b
	}
	c.json("POST", "/v1/builds", vera, body("complaints"), 403) // not a maker
	c.json("POST", "/v1/builds", asha, body("emi"), 404)        // not her workspace
	short, _ := json.Marshal(map[string]any{"workspace": "complaints", "flow_id": "water_line", "name": "W", "description": "Too short"})
	c.json("POST", "/v1/builds", asha, short, 422)

	job := c.json("POST", "/v1/builds", asha, body("complaints"), 202)
	id := job["build_id"].(string)
	if job["status"] != "running" {
		t.Fatalf("start: %v", job)
	}
	var done map[string]any
	for i := 0; i < 100; i++ {
		done = c.json("GET", "/v1/builds/"+id, asha, nil, 200)
		if done["status"] != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	res, _ := done["result"].(map[string]any)
	if done["status"] != "done" || res == nil || toJSON(res["problems"]) != `[]` || res["steps"] != 4.0 {
		t.Fatalf("build: %v", done)
	}
	f := res["flow"].(map[string]any)
	if f["flow_id"] != "water_line" || f["name"] != "Water complaints" {
		t.Fatalf("flow id and name: %v %v", f["flow_id"], f["name"])
	}
	// Only whoever started it can read it.
	c.json("GET", "/v1/builds/"+id, vera, nil, 404)

	// The flow saves as a normal draft.
	saved := c.json("PUT", "/v1/flows/water_line/draft?workspace=complaints", asha, []byte(toJSON(res["flow"])), 200)
	if saved["valid"] != true {
		t.Fatalf("saving the built flow: %v", saved)
	}
	if !strings.Contains(toJSON(c.json("GET", "/v1/audit", adminToken, nil, 200)["entries"]), "build.start") {
		t.Error("build not in the audit log")
	}
}

func TestFlowBuilderOff(t *testing.T) {
	c, kc := peopleServer(t, nil)
	asha := kc.Token(t, "asha@demo.corp", []string{"viewer"}, nil, nil)
	c.json("POST", "/v1/builds", asha, []byte(`{"workspace": "x", "flow_id": "y", "name": "z", "description": "d"}`), 503)
}
