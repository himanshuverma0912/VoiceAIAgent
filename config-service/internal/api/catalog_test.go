package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth/authtest"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

// withSettings returns a flow file with some settings changed (nil removes one).
func withSettings(t *testing.T, file string, changes map[string]any) []byte {
	t.Helper()
	var flow map[string]any
	if err := json.Unmarshal(spec(t, file), &flow); err != nil {
		t.Fatal(err)
	}
	settings, _ := flow["settings"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
	}
	for k, v := range changes {
		if v == nil {
			delete(settings, k)
		} else {
			settings[k] = v
		}
	}
	flow["settings"] = settings
	b, _ := json.Marshal(flow)
	return b
}

func problemsOf(res map[string]any) string {
	return toJSON(res["problems"])
}

func TestModelCatalog(t *testing.T) {
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
	srv := httptest.NewServer((&Server{
		Store: store.New(db), AdminToken: adminToken, RuntimeToken: runtimeToken,
		Users: &auth.Verifier{Issuer: authtest.Issuer, Audience: "config-api", JWKSURL: kc.JWKSURL()},
	}).Handler())
	defer srv.Close()
	c := client{t, srv.URL}

	asha := kc.Token(t, "asha@demo.corp", []string{"viewer", "flow-editor", "flow-publisher"}, []string{"emi"}, nil)
	ravi := kc.Token(t, "ravi@demo.corp", []string{"viewer", "flow-editor", "flow-publisher"}, []string{"complaints"}, nil)
	admin := kc.Token(t, "admin@demo.corp", []string{"viewer", "platform-admin"}, nil, nil)

	// The seeded entries: what the agent already uses.
	list := c.json("GET", "/v1/catalog", asha, nil, 200)["entries"].([]any)
	if len(list) != 4 {
		t.Fatalf("seeded catalog: %s", toJSON(list))
	}
	c.json("PUT", "/v1/flows/emi_payment_issue/draft?workspace=emi", asha, spec(t, "emi-payment-issue.json"), 200)
	c.json("PUT", "/v1/flows/complaint_helpline/draft?workspace=complaints", ravi, spec(t, "complaint-helpline.json"), 200)

	// Only platform admins change it, and entries are checked.
	smart := []byte(`{"label": "Voice smart", "notes": "Better reasoning, slower and costlier.", "workspaces": ["emi"]}`)
	c.json("PUT", "/v1/catalog/llm/voice-smart", asha, smart, 403)
	c.json("PUT", "/v1/catalog/llm/voice-smart", admin, []byte(`{"label": "x", "price": 3}`), 400)
	c.json("PUT", "/v1/catalog/llm/voice-smart", admin, []byte(`{"label": "", "languages": ["hindi"]}`), 422)
	c.json("PUT", "/v1/catalog/tts/x", admin, smart, 400)
	c.json("PUT", "/v1/catalog/llm/bad%20id", admin, smart, 400)
	if res := c.json("PUT", "/v1/catalog/llm/voice-smart", admin, smart, 201); res["created"] != true {
		t.Fatalf("add: %v", res)
	}
	c.json("PUT", "/v1/catalog/llm/voice-smart", admin, smart, 200) // again: an update

	// Offered to emi only: Asha sees it, Ravi doesn't.
	ids := func(token string) string {
		var out []string
		for _, e := range c.json("GET", "/v1/catalog", token, nil, 200)["entries"].([]any) {
			out = append(out, e.(map[string]any)["model_id"].(string))
		}
		return strings.Join(out, ",")
	}
	if got := ids(asha); !strings.Contains(got, "voice-smart") {
		t.Fatalf("asha sees %s", got)
	}
	if got := ids(ravi); strings.Contains(got, "voice-smart") {
		t.Fatalf("ravi sees %s", got)
	}

	// Drafts: a model from the catalog is fine, anything else is a problem.
	if res := c.json("PUT", "/v1/flows/emi_payment_issue/draft", asha, withSettings(t, "emi-payment-issue.json", map[string]any{"llm_model": "voice-smart", "stt_model": "nova-2"}), 200); res["valid"] != true {
		t.Fatalf("emi with voice-smart: %v", res)
	}
	res := c.json("PUT", "/v1/flows/complaint_helpline/draft", ravi, withSettings(t, "complaint-helpline.json", map[string]any{"llm_model": "voice-smart"}), 200)
	if !strings.Contains(problemsOf(res), "isn't offered to workspace complaints") {
		t.Fatalf("complaints with voice-smart: %v", res)
	}
	c.json("POST", "/v1/flows/complaint_helpline/reviews", ravi, nil, 422) // can't even be submitted
	res = c.json("PUT", "/v1/flows/complaint_helpline/draft", ravi, withSettings(t, "complaint-helpline.json", map[string]any{"llm_model": "gpt-9", "tts_voice_id": "made-up-voice"}), 200)
	if p := problemsOf(res); !strings.Contains(p, "settings.llm_model: 'gpt-9' isn't in the model catalog") || !strings.Contains(p, "settings.tts_voice_id: 'made-up-voice' isn't in the model catalog") {
		t.Fatalf("unknown models: %v", p)
	}
	// Empty settings mean the agent's defaults: always fine.
	if res := c.json("PUT", "/v1/flows/complaint_helpline/draft", ravi, withSettings(t, "complaint-helpline.json", map[string]any{"llm_model": nil}), 200); res["valid"] != true {
		t.Fatalf("defaults: %v", res)
	}
	// Checking as you type uses the flow's workspace.
	body := withSettings(t, "complaint-helpline.json", map[string]any{"llm_model": "voice-smart"})
	if res := c.json("POST", "/v1/validate?workspace=complaints", ravi, body, 200); res["valid"] != false {
		t.Fatalf("validate as complaints: %v", res)
	}
	if res := c.json("POST", "/v1/validate?workspace=emi", asha, withSettings(t, "emi-payment-issue.json", map[string]any{"llm_model": "voice-smart"}), 200); res["valid"] != true {
		t.Fatalf("validate as emi: %v", res)
	}

	// A voice limited to English, in a Hindi flow.
	c.json("PUT", "/v1/catalog/tts_voice/en-only-voice", admin, []byte(`{"label": "English voice", "languages": ["en"]}`), 201)
	res = c.json("PUT", "/v1/flows/emi_payment_issue/draft", asha, withSettings(t, "emi-payment-issue.json", map[string]any{"tts_voice_id": "en-only-voice"}), 200)
	if !strings.Contains(problemsOf(res), "supports en, not this flow's language (hi)") {
		t.Fatalf("language: %v", res)
	}

	// Back to voice-smart, then publish it.
	c.json("PUT", "/v1/flows/emi_payment_issue/draft", asha, withSettings(t, "emi-payment-issue.json", map[string]any{"llm_model": "voice-smart"}), 200)
	c.json("POST", "/v1/flows/emi_payment_issue/publish", adminToken, nil, 201)

	// In use: can't be deleted, but can be switched off.
	res = c.json("DELETE", "/v1/catalog/llm/voice-smart", admin, nil, 409)
	if toJSON(res["used_by"]) != `["emi_payment_issue"]` {
		t.Fatalf("used by: %v", res)
	}
	off := []byte(`{"label": "Voice smart", "workspaces": ["emi"], "enabled": false}`)
	if res := c.json("PUT", "/v1/catalog/llm/voice-smart", admin, off, 200); toJSON(res["used_by"]) != `["emi_payment_issue"]` {
		t.Fatalf("switch off: %v", res)
	}
	// Still listed (so Studio can say it's switched off), marked as off.
	for _, e := range c.json("GET", "/v1/catalog", asha, nil, 200)["entries"].([]any) {
		if m := e.(map[string]any); m["model_id"] == "voice-smart" && m["enabled"] != false {
			t.Fatalf("voice-smart should be off: %v", m)
		}
	}
	if got := ids(ravi); strings.Contains(got, "voice-smart") {
		t.Fatalf("ravi sees %s", got)
	}
	// The published version keeps running; the draft can't be published again until it's changed.
	c.json("POST", "/v1/flows/emi_payment_issue/reviews", asha, nil, 422)
	if res := c.json("POST", "/v1/flows/emi_payment_issue/publish", adminToken, nil, 422); !strings.Contains(problemsOf(res), "switched off") {
		t.Fatalf("publish after switch off: %v", res)
	}
	c.json("GET", "/internal/v1/flows/emi_payment_issue/draft", runtimeToken, nil, 422) // a test call won't start either
	// Opening the draft shows the new problem, not the ones from when it was saved.
	if d := c.json("GET", "/v1/flows/emi_payment_issue/draft", asha, nil, 200); !strings.Contains(problemsOf(d), "switched off") {
		t.Fatalf("draft problems: %v", d["problems"])
	}

	// Switched to the default and republished, nothing uses it: it can go.
	c.json("PUT", "/v1/flows/emi_payment_issue/draft", asha, spec(t, "emi-payment-issue.json"), 200)
	c.json("POST", "/v1/flows/emi_payment_issue/publish", adminToken, nil, 201)
	c.json("DELETE", "/v1/catalog/llm/voice-smart", admin, nil, 0)
	c.json("DELETE", "/v1/catalog/llm/voice-smart", admin, nil, 404)

	// Every change is in the audit log.
	audit := toJSON(c.json("GET", "/v1/audit?limit=50", admin, nil, 200))
	for _, a := range []string{"catalog.add", "catalog.update", "catalog.delete"} {
		if !strings.Contains(audit, a) {
			t.Fatalf("audit has no %s", a)
		}
	}
}
