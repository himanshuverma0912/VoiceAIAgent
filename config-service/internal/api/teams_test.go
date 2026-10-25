package api

import (
	"bytes"
	"context"
	"database/sql"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth/authtest"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

// Signed-in people: their Keycloak roles and teams decide what they may do.
func TestTeamsAndRoles(t *testing.T) {
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
	ravi := kc.Token(t, "ravi@demo.corp", []string{"viewer", "flow-editor"}, []string{"complaints"}, nil)
	vera := kc.Token(t, "vera@demo.corp", []string{"viewer"}, []string{"emi"}, nil) // can only look
	both := kc.Token(t, "bo@demo.corp", []string{"viewer", "flow-editor"}, []string{"complaints", "emi"}, nil)
	admin := kc.Token(t, "admin@demo.corp", []string{"viewer", "platform-admin"}, nil, nil)
	norole := kc.Token(t, "x@demo.corp", nil, []string{"emi"}, nil)

	emi := bytes.ReplaceAll(spec(t, "emi-payment-issue.json"), []byte("\r"), nil)
	complaint := spec(t, "complaint-helpline.json")

	// Who am I.
	me := c.json("GET", "/v1/me", asha, nil, 200)
	if me["email"] != "asha@demo.corp" || toJSON(me["teams"]) != `["emi"]` || toJSON(me["roles"]) != `["flow-editor","flow-publisher","viewer"]` {
		t.Fatalf("me: %v", me)
	}
	c.json("GET", "/v1/me", norole, nil, 403)
	c.json("GET", "/v1/me", "not-a-token", nil, 401)
	c.json("GET", "/v1/me", runtimeToken, nil, 401) // the agent's token can't use /v1

	// Creating flows: an editor's single team is the default; viewers can't create.
	if res := c.json("PUT", "/v1/flows/emi_payment_issue/draft", asha, emi, 200); res["valid"] != true {
		t.Fatalf("asha draft: %v", res)
	}
	c.json("PUT", "/v1/flows/other/draft", vera, complaint, 403)
	c.json("PUT", "/v1/flows/complaint_helpline/draft", both, complaint, 400) // two teams: must say which
	c.json("PUT", "/v1/flows/complaint_helpline/draft?team=sales", both, complaint, 403)
	c.json("PUT", "/v1/flows/complaint_helpline/draft?team=complaints", both, complaint, 200)

	// Each team sees only its own flows; other teams' flows look like they don't exist.
	if got := flowIDs(c.json("GET", "/v1/flows", ravi, nil, 200)); got != "complaint_helpline" {
		t.Fatalf("ravi sees %s", got)
	}
	if got := flowIDs(c.json("GET", "/v1/flows", both, nil, 200)); got != "complaint_helpline,emi_payment_issue" {
		t.Fatalf("bo sees %s", got)
	}
	if got := flowIDs(c.json("GET", "/v1/flows", admin, nil, 200)); got != "complaint_helpline,emi_payment_issue" {
		t.Fatalf("admin sees %s", got)
	}
	c.json("GET", "/v1/flows/emi_payment_issue", ravi, nil, 404)
	c.json("GET", "/v1/flows/emi_payment_issue/draft", ravi, nil, 404)
	c.json("PUT", "/v1/flows/emi_payment_issue/draft", ravi, emi, 404)
	c.json("GET", "/v1/flows/emi_payment_issue/draft", vera, nil, 200) // same team, view only

	// Publishing needs flow-publisher in the flow's team.
	if res := c.json("POST", "/v1/flows/complaint_helpline/publish", ravi, nil, 403); !strings.Contains(res["error"].(string), "flow-publisher role for team complaints") {
		t.Fatalf("ravi publish: %v", res)
	}
	c.json("POST", "/v1/flows/emi_payment_issue/publish", vera, nil, 403)
	res := c.json("POST", "/v1/flows/emi_payment_issue/publish", asha, nil, 201)
	if res["published_by"] != "asha@demo.corp" {
		t.Fatalf("publish: %v", res)
	}

	// Phone numbers and the audit log are for platform admins.
	c.json("PUT", "/v1/numbers/+918045678901", asha, []byte(`{"flow_id":"emi_payment_issue"}`), 403)
	c.json("GET", "/v1/audit", asha, nil, 403)
	c.json("PUT", "/v1/numbers/+918045678901", admin, []byte(`{"flow_id":"emi_payment_issue"}`), 200)
	audit := toJSON(c.json("GET", "/v1/audit", admin, nil, 200)["entries"])
	for _, who := range []string{"asha@demo.corp", "bo@demo.corp", "admin@demo.corp"} {
		if !strings.Contains(audit, who) {
			t.Errorf("audit log doesn't name %s", who)
		}
	}

	// Older flows without a team: only admins see them, until an admin assigns one.
	c.json("PUT", "/v1/flows/legacy_flow/draft", adminToken, bytes.Replace(complaint, []byte(`"complaint_helpline"`), []byte(`"legacy_flow"`), 1), 200)
	c.json("GET", "/v1/flows/legacy_flow", ravi, nil, 404)
	c.json("PUT", "/v1/flows/legacy_flow/team", ravi, []byte(`{"team":"complaints"}`), 403)
	if f := c.json("PUT", "/v1/flows/legacy_flow/team", admin, []byte(`{"team":"complaints"}`), 200); f["team"] != "complaints" {
		t.Fatalf("set team: %v", f)
	}
	c.json("GET", "/v1/flows/legacy_flow", ravi, nil, 200)
	c.json("PUT", "/v1/flows/legacy_flow/team", admin, []byte(`{"team":"Bad Team"}`), 400)

	// "New flow" never overwrites an existing flow, even one of another team.
	if status, _, _ := c.do("PUT", "/v1/flows/emi_payment_issue/draft", ravi, complaint, "If-None-Match", "*"); status != 409 {
		t.Fatalf("create-only over an existing flow: %d", status)
	}
	if status, _, _ := c.do("PUT", "/v1/flows/brand_new/draft", ravi, bytes.Replace(complaint, []byte(`"complaint_helpline"`), []byte(`"brand_new"`), 1), "If-None-Match", "*"); status != 200 {
		t.Fatalf("create-only of a new flow: %d", status)
	}

	// Two people editing at once: the second save, based on an old copy, is refused.
	draft := c.json("GET", "/v1/flows/emi_payment_issue/draft", asha, nil, 200)
	opened := draft["checksum"].(string)
	if status, _, _ := c.do("PUT", "/v1/flows/emi_payment_issue/draft", asha, emi, "If-Match", `"`+opened+`"`); status != 200 {
		t.Fatalf("save with current checksum: %d", status)
	}
	edited := bytes.Replace(emi, []byte(`"settings": {`), []byte(`"settings": {"llm_temperature": 0.2, `), 1)
	if status, _, _ := c.do("PUT", "/v1/flows/emi_payment_issue/draft", asha, edited, "If-Match", `"`+opened+`"`); status != 200 {
		t.Fatalf("edit based on the current draft: %d", status) // nobody else saved in between
	}
	// A second tab still holding the copy opened before that edit:
	if status, _, body := c.do("PUT", "/v1/flows/emi_payment_issue/draft", asha, emi, "If-Match", `"`+opened+`"`); status != 409 || !strings.Contains(string(body), "someone else saved") {
		t.Fatalf("stale save: %d %s", status, body)
	}

	// Canvas positions: editors save them, viewers read them with the draft.
	c.json("PUT", "/v1/flows/emi_payment_issue/layout", asha, []byte(`{"positions": {"verify_customer": {"x": 10, "y": 20.5}}}`), 0)
	if d := c.json("GET", "/v1/flows/emi_payment_issue/draft", vera, nil, 200); toJSON(d["layout"]) != `{"positions":{"verify_customer":{"x":10,"y":20.5}}}` {
		t.Fatalf("layout: %v", d["layout"])
	}
	c.json("PUT", "/v1/flows/emi_payment_issue/layout", vera, []byte(`{"positions": {}}`), 403)
	c.json("PUT", "/v1/flows/emi_payment_issue/layout", asha, []byte(`{"positions": {"a": {"x": "left"}}}`), 400)
	c.json("PUT", "/v1/flows/emi_payment_issue/layout", ravi, []byte(`{"positions": {}}`), 404)

	// A token for another audience, or an expired one, is refused.
	c.json("GET", "/v1/me", kc.Token(t, "asha@demo.corp", []string{"viewer"}, nil, map[string]any{"aud": "account"}), nil, 401)
	c.json("GET", "/v1/me", kc.Token(t, "asha@demo.corp", []string{"viewer"}, nil, map[string]any{"exp": 1}), nil, 401)
}

func spec(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../flows/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func flowIDs(list map[string]any) string {
	var ids []string
	for _, f := range list["flows"].([]any) {
		ids = append(ids, f.(map[string]any)["flow_id"].(string))
	}
	return strings.Join(ids, ",")
}
