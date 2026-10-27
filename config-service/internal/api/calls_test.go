package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth/authtest"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

func callRecord(id, outcome string, started time.Time, version any, steps ...string) []byte {
	var st []string
	for _, s := range steps {
		st = append(st, fmt.Sprintf(`{"node":%q,"ms":2000}`, s))
	}
	v, _ := json.Marshal(version)
	return []byte(fmt.Sprintf(`{
		"call_id": %q, "flow_id": "emi_payment_issue", "flow_version": %s, "flow_source": "config-service",
		"channel": "phone", "runtime": "livekit", "started_at": %q, "duration_ms": %d,
		"outcome": %q, "steps": [%s], "turns": {"caller": 3, "agent": 4},
		"latency_ms": {"p50": 900, "p95": 1400, "count": 4},
		"usage": {"llm_input_tokens": 1000, "llm_output_tokens": 100, "stt_audio_seconds": 30.5,
		          "tts_characters": 500, "tts_audio_seconds": 28.25}
	}`, id, v, started.UTC().Format(time.RFC3339Nano), 2000*len(steps), outcome, strings.Join(st, ",")))
}

func TestCallHistory(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	kc := authtest.New(t)
	st := store.New(db)
	srv := httptest.NewServer((&Server{
		Store: st, AdminToken: adminToken, RuntimeToken: runtimeToken,
		Users: &auth.Verifier{Issuer: authtest.Issuer, Audience: "config-api", JWKSURL: kc.JWKSURL()},
	}).Handler())
	defer srv.Close()
	c := client{t, srv.URL}

	vera := kc.Token(t, "vera@demo.corp", []string{"viewer"}, []string{"emi"}, nil)
	ravi := kc.Token(t, "ravi@demo.corp", []string{"viewer", "flow-editor"}, []string{"complaints"}, nil)
	c.json("PUT", "/v1/workspaces/emi", adminToken, []byte(`{"name": "EMI"}`), 201)
	c.json("PUT", "/v1/flows/emi_payment_issue/draft?workspace=emi", adminToken, spec(t, "emi-payment-issue.json"), 200)

	now := time.Now()
	// Only the agent's token may send records.
	rec := callRecord("AJ_one", "completed", now.Add(-time.Minute), 2, "greet", "ask_loan", "goodbye")
	c.json("POST", "/internal/v1/calls", adminToken, rec, 401)
	c.json("POST", "/internal/v1/calls", vera, rec, 401)
	if res := c.json("POST", "/internal/v1/calls", runtimeToken, rec, 201); res["created"] != true {
		t.Fatalf("first send: %v", res)
	}
	// Sending the same call again (a retry) is fine and doesn't count twice.
	if res := c.json("POST", "/internal/v1/calls", runtimeToken, rec, 200); res["created"] != false {
		t.Fatalf("second send: %v", res)
	}
	c.json("POST", "/internal/v1/calls", runtimeToken, callRecord("AJ_two", "caller_hung_up", now.Add(-2*time.Minute), 2, "greet", "ask_loan"), 201)
	c.json("POST", "/internal/v1/calls", runtimeToken, callRecord("AJ_three", "caller_hung_up", now.Add(-26*time.Hour), 1, "greet"), 201)
	c.json("POST", "/internal/v1/calls", runtimeToken, callRecord("AJ_four", "completed", now.Add(-3*time.Minute), nil, "greet", "goodbye"), 201)

	// Anything that isn't on the list of allowed fields is refused, so nothing
	// personal can be slipped in.
	for name, change := range map[string][2]string{
		"caller number":   {"{", `{"caller_number": "+919876543210",`},
		"transcript":      {"{", `{"transcript": ["hello"],`},
		"room name":       {"{", `{"room": "_inbound_+919876543210_abc",`},
		"value in a step": {`{"node":"greet","ms":2000}`, `{"node":"greet","ms":2000,"value":"Asha"}`},
		"words in usage":  {`"tts_characters": 500`, `"tts_characters": 500, "tts_text": "Namaste Asha"`},
	} {
		b := []byte(strings.Replace(strings.Replace(string(rec), change[0], change[1], 1), "AJ_one", "AJ_bad", 1))
		if res := c.json("POST", "/internal/v1/calls", runtimeToken, b, 400); !strings.Contains(fmt.Sprint(res["error"]), "unknown field") {
			t.Fatalf("%s: %v", name, res)
		}
	}
	// Values out of range or of the wrong kind.
	bad := map[string][2]string{
		"room name as call id": {`"AJ_one"`, `"_inbound_+919876543210_abc"`},
		"unknown outcome":      {`"completed"`, `"great"`},
		"unknown channel":      {`"phone"`, `"whatsapp"`},
		"step not a node id":   {`"ask_loan"`, `"Asha Rao"`},
		"started long ago":     {now.Add(-time.Minute).UTC().Format(time.RFC3339Nano), now.Add(-72 * time.Hour).UTC().Format(time.RFC3339Nano)},
		"negative duration":    {`"duration_ms": 6000`, `"duration_ms": -1`},
	}
	for name, r := range bad {
		b := strings.Replace(string(rec), r[0], r[1], 1)
		if name != "room name as call id" {
			b = strings.Replace(b, "AJ_one", "AJ_bad", 1)
		}
		c.json("POST", "/internal/v1/calls", runtimeToken, []byte(b), 422)
	}

	// Members of the workspace see the calls; other workspaces don't see the flow at all.
	list := c.json("GET", "/v1/flows/emi_payment_issue/calls", vera, nil, 200)
	calls := list["calls"].([]any)
	if len(calls) != 4 || calls[0].(map[string]any)["call_id"] != "AJ_one" {
		t.Fatalf("list: %s", toJSON(list))
	}
	c.json("GET", "/v1/flows/emi_payment_issue/calls", ravi, nil, 404)
	c.json("GET", "/v1/flows/emi_payment_issue/calls/summary", ravi, nil, 404)
	c.json("GET", "/v1/flows/emi_payment_issue/calls?days=0", vera, nil, 400)
	c.json("GET", "/v1/flows/emi_payment_issue/calls?channel=sms", vera, nil, 400)
	if got := c.json("GET", "/v1/flows/emi_payment_issue/calls?channel=studio_test", vera, nil, 200)["calls"].([]any); len(got) != 0 {
		t.Fatalf("studio calls: %v", got)
	}
	if got := c.json("GET", "/v1/flows/emi_payment_issue/calls?days=1", vera, nil, 200)["calls"].([]any); len(got) != 3 {
		t.Fatalf("last day: %d calls", len(got))
	}

	sum := c.json("GET", "/v1/flows/emi_payment_issue/calls/summary?days=7", vera, nil, 200)
	if sum["calls"] != 4.0 || toJSON(sum["outcomes"]) != `{"caller_hung_up":2,"completed":2,"ended_early":0,"error":0,"other":0,"time_limit":0}` {
		t.Fatalf("summary: %s", toJSON(sum))
	}
	steps := map[string]map[string]any{}
	for _, s := range sum["steps"].([]any) {
		m := s.(map[string]any)
		steps[m["node"].(string)] = m
	}
	if steps["greet"]["reached"] != 4.0 || steps["ask_loan"]["hung_up_here"] != 1.0 || steps["greet"]["hung_up_here"] != 1.0 || steps["goodbye"]["ended_here"] != 2.0 {
		t.Fatalf("steps: %s", toJSON(sum["steps"]))
	}
	if len(sum["versions"].([]any)) != 3 || len(sum["per_day"].([]any)) != 7 {
		t.Fatalf("versions / days: %s", toJSON(sum))
	}
	if u := sum["usage"].(map[string]any); u["llm_input_tokens"] != 4000.0 || u["tts_audio_seconds"] != 113.0 {
		t.Fatalf("usage: %v", u)
	}

	// Old history is deleted.
	if n, err := st.PurgeCalls(ctx, now.Add(-24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
}

func TestSummariseEmpty(t *testing.T) {
	s := summarise(nil, 3, "", time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC))
	if s.Calls != 0 || s.Duration != nil || s.Latency != nil || s.Channel != "all" || len(s.Steps) != 0 {
		t.Fatalf("%+v", s)
	}
	// 20:00 UTC is already the 27th in India.
	if len(s.PerDay) != 3 || s.PerDay[2].Date != "2026-10-27" || s.PerDay[0].Date != "2026-10-25" {
		t.Fatalf("days: %+v", s.PerDay)
	}
}
