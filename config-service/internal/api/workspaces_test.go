package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth/authtest"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/livekit"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

// peopleServer is a config service with Keycloak sign-in (a fake Keycloak),
// on a fresh database.
func peopleServer(t *testing.T, change func(*Server)) (client, *authtest.Keycloak) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	kc := authtest.New(t)
	s := &Server{
		Store: store.New(db), AdminToken: adminToken, RuntimeToken: runtimeToken,
		Users:   &auth.Verifier{Issuer: authtest.Issuer, Audience: "config-api", JWKSURL: kc.JWKSURL()},
		LiveKit: livekit.Config{URL: "wss://demo.livekit.cloud", APIKey: "APIkey", APISecret: "a-secret-of-some-length", AgentName: "VoiceAIAgentInbound"},
	}
	if change != nil {
		change(s)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return client{t, srv.URL}, kc
}

func member(maker, checker, owner bool) []byte {
	b, _ := json.Marshal(map[string]bool{"maker": maker, "checker": checker, "owner": owner})
	return b
}

func workspaceIDs(list map[string]any, key string) string {
	var ids []string
	for _, w := range list[key].([]any) {
		ids = append(ids, w.(map[string]any)["workspace_id"].(string))
	}
	return strings.Join(ids, ",")
}

func TestWorkspacesAndMembers(t *testing.T) {
	c, kc := peopleServer(t, nil)
	admin := kc.Token(t, "admin@demo.corp", []string{"viewer", "platform-admin"}, nil, nil)
	asha := kc.Token(t, "Asha@Demo.corp", []string{"viewer"}, nil, nil) // email case doesn't matter
	ravi := kc.Token(t, "ravi@demo.corp", []string{"viewer"}, nil, nil)
	vera := kc.Token(t, "vera@demo.corp", []string{"viewer"}, nil, nil)
	bo := kc.Token(t, "bo@demo.corp", []string{"viewer"}, nil, nil)
	norole := kc.Token(t, "x@demo.corp", nil, nil, nil)
	emi := bytes.ReplaceAll(spec(t, "emi-payment-issue.json"), []byte("\r"), nil)
	complaint := spec(t, "complaint-helpline.json")

	// Signed in, but in no workspace yet: sees nothing.
	if me := c.json("GET", "/v1/me", asha, nil, 200); me["email"] != "asha@demo.corp" || toJSON(me["workspaces"]) != `[]` {
		t.Fatalf("me: %v", me)
	}
	if got := c.json("GET", "/v1/workspaces", asha, nil, 200); toJSON(got["workspaces"]) != `[]` {
		t.Fatalf("workspaces: %v", got)
	}
	c.json("GET", "/v1/me", norole, nil, 403)
	c.json("GET", "/v1/me", runtimeToken, nil, 401) // the agent's token can't use /v1

	// Only platform admins create workspaces.
	c.json("PUT", "/v1/workspaces/emi", asha, []byte(`{"name": "EMI"}`), 403)
	if ws := c.json("PUT", "/v1/workspaces/emi", admin, []byte(`{"name": "EMI"}`), 201); ws["workspace_id"] != "emi" || ws["name"] != "EMI" {
		t.Fatalf("create: %v", ws)
	}
	c.json("PUT", "/v1/workspaces/emi", admin, []byte(`{"name": "EMI collections"}`), 200) // rename
	c.json("PUT", "/v1/workspaces/Bad%20Id", admin, []byte(`{"name": "x"}`), 400)
	c.json("PUT", "/v1/workspaces/emi", admin, []byte(`{"name": ""}`), 422)
	c.json("PUT", "/v1/workspaces/emi", admin, []byte(`{"name": "x", "team": "y"}`), 400)

	// The admin makes Asha its owner and a maker; owners then manage the rest.
	c.json("PUT", "/v1/workspaces/emi/members/asha@demo.corp", admin, member(true, false, true), 201)
	c.json("PUT", "/v1/workspaces/emi/members/not-an-email", admin, member(true, false, false), 400)
	me := c.json("GET", "/v1/me", asha, nil, 200)
	if toJSON(me["workspaces"]) != `[{"checker":false,"maker":true,"name":"EMI collections","owner":true,"workspace_id":"emi"}]` {
		t.Fatalf("asha's workspaces: %v", me["workspaces"])
	}
	c.json("PUT", "/v1/workspaces/emi/members/RAVI@demo.corp", asha, member(false, true, false), 201)
	c.json("PUT", "/v1/workspaces/emi/members/vera@demo.corp", asha, member(false, false, false), 201) // view only
	c.json("PUT", "/v1/workspaces/emi/members/bo@demo.corp", vera, member(true, false, false), 403)    // not an owner
	c.json("PUT", "/v1/workspaces/emi/members/bo@demo.corp", bo, member(true, false, false), 404)      // not even a member
	c.json("PUT", "/v1/workspaces/emi", asha, []byte(`{"name": "EMI team"}`), 200)                     // owners rename
	c.json("PUT", "/v1/workspaces/emi", vera, []byte(`{"name": "x"}`), 403)
	members := c.json("GET", "/v1/workspaces/emi/members", vera, nil, 200)["members"].([]any)
	if len(members) != 3 || members[1].(map[string]any)["email"] != "ravi@demo.corp" {
		t.Fatalf("members: %v", members)
	}
	c.json("GET", "/v1/workspaces/emi/members", bo, nil, 404)

	// A workspace keeps at least one owner.
	c.json("PUT", "/v1/workspaces/emi/members/asha@demo.corp", asha, member(true, false, false), 409)
	c.json("DELETE", "/v1/workspaces/emi/members/asha@demo.corp", asha, nil, 409)
	c.json("PUT", "/v1/workspaces/emi/members/ravi@demo.corp", asha, member(false, true, true), 200)
	c.json("PUT", "/v1/workspaces/emi/members/asha@demo.corp", asha, member(true, false, false), 200) // now fine
	c.json("PUT", "/v1/workspaces/emi/members/asha@demo.corp", ravi, member(true, false, true), 200)
	c.json("DELETE", "/v1/workspaces/emi/members/nobody@demo.corp", asha, nil, 404)

	// Flows: makers create them (their only workspace is the default); viewers can't.
	if res := c.json("PUT", "/v1/flows/emi_payment_issue/draft", asha, emi, 200); res["valid"] != true {
		t.Fatalf("asha draft: %v", res)
	}
	c.json("PUT", "/v1/flows/other/draft?workspace=emi", vera, complaint, 403)
	if f := c.json("GET", "/v1/flows/emi_payment_issue", vera, nil, 200); f["workspace"] != "emi" || f["pending_review"] != nil {
		t.Fatalf("flow: %v", f)
	}

	// Another workspace: its flows are invisible to emi's members, and the reverse.
	c.json("PUT", "/v1/workspaces/complaints", admin, []byte(`{"name": "Complaints"}`), 201)
	c.json("PUT", "/v1/workspaces/complaints/members/bo@demo.corp", admin, member(true, false, true), 201)
	c.json("PUT", "/v1/flows/complaint_helpline/draft?workspace=complaints", bo, complaint, 200)
	c.json("PUT", "/v1/flows/x_flow/draft?workspace=emi", bo, complaint, 404)
	if got := flowIDs(c.json("GET", "/v1/flows", asha, nil, 200)); got != "emi_payment_issue" {
		t.Fatalf("asha sees %s", got)
	}
	if got := flowIDs(c.json("GET", "/v1/flows", bo, nil, 200)); got != "complaint_helpline" {
		t.Fatalf("bo sees %s", got)
	}
	if got := flowIDs(c.json("GET", "/v1/flows", admin, nil, 200)); got != "complaint_helpline,emi_payment_issue" {
		t.Fatalf("admin sees %s", got)
	}
	if got := flowIDs(c.json("GET", "/v1/flows?workspace=complaints", asha, nil, 200)); got != "" {
		t.Fatalf("asha filtering on another workspace sees %s", got)
	}
	c.json("GET", "/v1/flows/complaint_helpline", asha, nil, 404)
	c.json("GET", "/v1/flows/complaint_helpline/draft", asha, nil, 404)
	if got := workspaceIDs(c.json("GET", "/v1/workspaces", admin, nil, 200), "workspaces"); got != "complaints,emi" {
		t.Fatalf("admin's workspaces: %s", got)
	}
	if got := workspaceIDs(c.json("GET", "/v1/workspaces", bo, nil, 200), "workspaces"); got != "complaints" {
		t.Fatalf("bo's workspaces: %s", got)
	}

	// Moving a flow (platform admins), e.g. one created before workspaces.
	c.json("PUT", "/v1/flows/legacy_flow/draft", adminToken, bytes.Replace(complaint, []byte(`"complaint_helpline"`), []byte(`"legacy_flow"`), 1), 200)
	c.json("GET", "/v1/flows/legacy_flow", asha, nil, 404)
	c.json("PUT", "/v1/flows/legacy_flow/workspace", asha, []byte(`{"workspace":"emi"}`), 403)
	c.json("PUT", "/v1/flows/legacy_flow/workspace", admin, []byte(`{"workspace":"nowhere"}`), 404)
	if f := c.json("PUT", "/v1/flows/legacy_flow/workspace", admin, []byte(`{"workspace":"emi"}`), 200); f["workspace"] != "emi" {
		t.Fatalf("move: %v", f)
	}
	c.json("GET", "/v1/flows/legacy_flow", asha, nil, 200)

	// Only empty workspaces can be removed.
	c.json("DELETE", "/v1/workspaces/emi", admin, nil, 409)
	c.json("PUT", "/v1/workspaces/temp", admin, []byte(`{"name": "Temp"}`), 201)
	c.json("DELETE", "/v1/workspaces/temp", asha, nil, 403)
	c.json("DELETE", "/v1/workspaces/temp", admin, nil, 0)

	// Canvas positions and test calls: makers (and checkers can test); viewers can't.
	c.json("PUT", "/v1/flows/emi_payment_issue/layout", asha, []byte(`{"positions": {"verify_customer": {"x": 10, "y": 20}}}`), 0)
	c.json("PUT", "/v1/flows/emi_payment_issue/layout", vera, []byte(`{"positions": {}}`), 403)
	ts := c.json("POST", "/v1/flows/emi_payment_issue/test-sessions", asha, nil, 201)
	if meta := decodeLiveKitMetadata(t, ts["token"].(string)); meta["flow_id"] != "emi_payment_issue" || meta["requested_by"] != "asha@demo.corp" {
		t.Fatalf("test session: %v", meta)
	}
	c.json("POST", "/v1/flows/emi_payment_issue/test-sessions", ravi, nil, 201) // checker
	c.json("POST", "/v1/flows/emi_payment_issue/test-sessions", vera, nil, 403)
	c.json("POST", "/v1/flows/emi_payment_issue/test-sessions", bo, nil, 404)

	// Two people editing at once: the second save, based on an old copy, is refused.
	opened := c.json("GET", "/v1/flows/emi_payment_issue/draft", asha, nil, 200)["checksum"].(string)
	edited := bytes.Replace(emi, []byte(`"settings": {`), []byte(`"settings": {"llm_temperature": 0.2, `), 1)
	if status, _, _ := c.do("PUT", "/v1/flows/emi_payment_issue/draft", asha, edited, "If-Match", `"`+opened+`"`); status != 200 {
		t.Fatalf("save: %d", status)
	}
	if status, _, _ := c.do("PUT", "/v1/flows/emi_payment_issue/draft", asha, emi, "If-Match", `"`+opened+`"`); status != 409 {
		t.Fatalf("stale save: %d", status)
	}
	if status, _, _ := c.do("PUT", "/v1/flows/emi_payment_issue/draft", asha, emi, "If-None-Match", "*"); status != 409 {
		t.Fatalf("create-only over an existing flow: %d", status)
	}

	// Phone numbers and the audit log are for platform admins.
	c.json("PUT", "/v1/numbers/+918045678901", asha, []byte(`{"flow_id":"emi_payment_issue"}`), 403)
	c.json("GET", "/v1/audit", asha, nil, 403)
	audit := toJSON(c.json("GET", "/v1/audit?limit=200", admin, nil, 200)["entries"])
	for _, a := range []string{"workspace.create", "workspace.rename", "member.add", "member.update", "flow.workspace", "workspace.delete"} {
		if !strings.Contains(audit, a) {
			t.Errorf("audit log has no %s", a)
		}
	}

	// A token for another audience, or an expired one, is refused.
	c.json("GET", "/v1/me", kc.Token(t, "asha@demo.corp", []string{"viewer"}, nil, map[string]any{"aud": "account"}), nil, 401)
	c.json("GET", "/v1/me", kc.Token(t, "asha@demo.corp", []string{"viewer"}, nil, map[string]any{"exp": 1}), nil, 401)
}

// People who already had Keycloak teams get those as their first workspace
// memberships, once; after that Flow Studio decides.
func TestFirstSignInImportsDirectoryTeams(t *testing.T) {
	c, kc := peopleServer(t, nil)
	lee := kc.Token(t, "lee@demo.corp", []string{"viewer", "flow-editor", "flow-publisher"}, []string{"emi", "cards"}, nil)
	ana := kc.Token(t, "ana@demo.corp", []string{"viewer"}, []string{"emi"}, nil)

	me := c.json("GET", "/v1/me", lee, nil, 200)
	if toJSON(me["workspaces"]) != `[{"checker":true,"maker":true,"name":"Cards","owner":false,"workspace_id":"cards"},{"checker":true,"maker":true,"name":"Emi","owner":false,"workspace_id":"emi"}]` {
		t.Fatalf("lee: %v", me["workspaces"])
	}
	if toJSON(c.json("GET", "/v1/me", ana, nil, 200)["workspaces"]) != `[{"checker":false,"maker":false,"name":"Emi","owner":false,"workspace_id":"emi"}]` {
		t.Fatal("ana should be a viewer in emi")
	}
	// Removed in Flow Studio: stays removed, although Keycloak still lists the team.
	c.json("DELETE", "/v1/workspaces/emi/members/ana@demo.corp", adminToken, nil, 0)
	if toJSON(c.json("GET", "/v1/me", ana, nil, 200)["workspaces"]) != `[]` {
		t.Fatal("ana was imported again")
	}
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

func decodeLiveKitMetadata(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		RoomConfig struct {
			Agents []struct {
				Metadata string `json:"metadata"`
			} `json:"agents"`
		} `json:"roomConfig"`
	}
	json.Unmarshal(payload, &claims) //nolint:errcheck
	var meta map[string]any
	json.Unmarshal([]byte(claims.RoomConfig.Agents[0].Metadata), &meta) //nolint:errcheck
	return meta
}
