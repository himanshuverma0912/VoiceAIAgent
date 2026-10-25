package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/flowspec"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

const (
	adminToken   = "admin-token-for-tests-0123456789"
	runtimeToken = "runtime-token-for-tests-0123456789"
)

// These tests need a Postgres database they may wipe:
//
//	TEST_DATABASE_URL=postgres://...?sslmode=disable go test ./...
func newTestServer(t *testing.T) *httptest.Server {
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
	for i := 0; i < 2; i++ { // running migrations twice must be harmless
		if err := store.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer((&Server{Store: store.New(db), AdminToken: adminToken, RuntimeToken: runtimeToken}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

type client struct {
	t   *testing.T
	url string
}

func (c client) do(method, path, token string, body []byte, header ...string) (int, http.Header, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.url+path, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, out
}

func (c client) json(method, path, token string, body []byte, wantStatus int) map[string]any {
	c.t.Helper()
	status, _, out := c.do(method, path, token, body)
	if wantStatus == 0 && status == 204 {
		return nil
	}
	if status != wantStatus {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, wantStatus, out)
	}
	var v map[string]any
	if len(out) > 0 {
		if err := json.Unmarshal(out, &v); err != nil {
			c.t.Fatalf("%s %s: %v: %s", method, path, err, out)
		}
	}
	return v
}

func flowFile(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "flows", "complaint-helpline.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTokens(t *testing.T) {
	c := client{t, newTestServer(t).URL}
	c.json("GET", "/healthz", "", nil, 200)
	c.json("GET", "/readyz", "", nil, 200)
	c.json("GET", "/v1/flows", "", nil, 401)
	c.json("GET", "/v1/flows", "wrong", nil, 401)
	c.json("GET", "/v1/flows", runtimeToken, nil, 401) // the agent can't change flows
	c.json("GET", "/internal/v1/resolve?number=%2B918045678901", adminToken, nil, 401)
	c.json("GET", "/v1/flows", adminToken, nil, 200)
}

func TestDraftPublishResolve(t *testing.T) {
	c := client{t, newTestServer(t).URL}
	spec := flowFile(t)
	const flow = "/v1/flows/complaint_helpline"

	// An invalid draft is saved (work isn't lost) but can't be published.
	broken := bytes.Replace(spec, []byte(`"next": "register"`), []byte(`"next": "regster"`), 1)
	res := c.json("PUT", flow+"/draft", adminToken, broken, 200)
	if res["valid"] != false || !strings.Contains(toJSON(res["problems"]), "'regster' is not a node") {
		t.Fatalf("broken draft: %v", res)
	}
	res = c.json("POST", flow+"/publish", adminToken, nil, 422)
	if !strings.Contains(toJSON(res["problems"]), "regster") {
		t.Fatalf("publish of broken draft: %v", res)
	}

	// The flow_id inside must match the URL.
	res = c.json("PUT", "/v1/flows/other_flow/draft", adminToken, spec, 200)
	if !strings.Contains(toJSON(res["problems"]), "flow_id: must be 'other_flow'") {
		t.Fatalf("mismatched flow_id: %v", res)
	}
	c.json("PUT", flow+"/draft", adminToken, []byte(`{not json`), 400)
	c.json("PUT", "/v1/flows/Bad-Id/draft", adminToken, spec, 400)

	// A number can't point at a flow with nothing published yet.
	c.json("PUT", "/v1/numbers/+918045678901", adminToken, []byte(`{"flow_id":"complaint_helpline"}`), 409)

	// Valid draft -> version 1. Publishing the same draft again changes nothing.
	res = c.json("PUT", flow+"/draft", adminToken, spec, 200)
	if res["valid"] != true {
		t.Fatalf("valid draft: %v", res)
	}
	res = c.json("POST", flow+"/publish", adminToken, nil, 201)
	if res["version"] != 1.0 {
		t.Fatalf("publish: %v", res)
	}
	res = c.json("POST", flow+"/publish", adminToken, nil, 200)
	if res["version"] != 1.0 || res["unchanged"] != true {
		t.Fatalf("re-publish: %v", res)
	}

	// Map the number, written loosely; it's stored in E.164.
	res = c.json("PUT", "/v1/numbers/+91%2080-4567%208901", adminToken, []byte(`{"flow_id":"complaint_helpline"}`), 200)
	if res["number"] != "+918045678901" {
		t.Fatalf("number: %v", res)
	}
	c.json("PUT", "/v1/numbers/12", adminToken, []byte(`{"flow_id":"complaint_helpline"}`), 400)
	c.json("PUT", "/v1/numbers/+918045678901", adminToken, []byte(`{"flow_id":"complaint_helpline","version_pin":9}`), 404)
	c.json("PUT", "/v1/numbers/+918045678901", adminToken, []byte(`{"flow_id":"nope"}`), 404)
	c.json("PUT", "/v1/numbers/+918045678901", adminToken, []byte(`{"flow":"x"}`), 400)

	// The agent resolves the number. "number=918045678901" (no +) works too.
	status, hdr, body := c.do("GET", "/internal/v1/resolve?number=918045678901", runtimeToken, nil)
	if status != 200 {
		t.Fatalf("resolve: %d %s", status, body)
	}
	etag1 := hdr.Get("ETag")
	var resolved struct {
		FlowID  string          `json:"flow_id"`
		Version int             `json:"version"`
		Spec    json.RawMessage `json:"spec"`
	}
	json.Unmarshal(body, &resolved) //nolint:errcheck
	if resolved.FlowID != "complaint_helpline" || resolved.Version != 1 || etag1 == "" {
		t.Fatalf("resolved %s / etag %q", body, etag1)
	}
	// Key order survives the database round trip (Postgres "json", not "jsonb").
	assertSameKeyOrder(t, spec, resolved.Spec)

	// Unchanged -> 304 with no body.
	status, _, body = c.do("GET", "/internal/v1/resolve?number=%2B918045678901", runtimeToken, nil, "If-None-Match", etag1)
	if status != 304 || len(body) != 0 {
		t.Fatalf("revalidate: %d %q", status, body)
	}

	// Publish a change -> the next resolve gets version 2.
	changed := bytes.Replace(spec, []byte(`"settings": {`), []byte(`"settings": {"llm_temperature": 0.3, `), 1)
	if bytes.Equal(changed, spec) {
		t.Fatal("test flow edit didn't apply")
	}
	c.json("PUT", flow+"/draft", adminToken, changed, 200)
	c.json("POST", flow+"/publish", adminToken, nil, 201)
	status, hdr, body = c.do("GET", "/internal/v1/resolve?number=%2B918045678901", runtimeToken, nil, "If-None-Match", etag1)
	json.Unmarshal(body, &resolved) //nolint:errcheck
	if status != 200 || resolved.Version != 2 || hdr.Get("ETag") == etag1 {
		t.Fatalf("after publish: %d version %d etag %q", status, resolved.Version, hdr.Get("ETag"))
	}

	// Pin to version 1 (rollback), then follow latest again.
	c.json("PUT", "/v1/numbers/+918045678901", adminToken, []byte(`{"flow_id":"complaint_helpline","version_pin":1}`), 200)
	if v := c.json("GET", "/internal/v1/resolve?number=%2B918045678901", runtimeToken, nil, 200)["version"]; v != 1.0 {
		t.Fatalf("pinned: version %v", v)
	}
	c.json("PUT", "/v1/numbers/+918045678901", adminToken, []byte(`{"flow_id":"complaint_helpline","version_pin":null}`), 200)
	if v := c.json("GET", "/internal/v1/resolve?number=%2B918045678901", runtimeToken, nil, 200)["version"]; v != 2.0 {
		t.Fatalf("unpinned: version %v", v)
	}

	// Read-back endpoints.
	if f := c.json("GET", flow, adminToken, nil, 200); f["latest_version"] != 2.0 || toJSON(f["numbers"]) != `["+918045678901"]` || f["draft_valid"] != true {
		t.Fatalf("flow summary: %v", f)
	}
	if v := c.json("GET", flow+"/versions", adminToken, nil, 200); len(v["versions"].([]any)) != 2 {
		t.Fatalf("versions: %v", v)
	}
	if v := c.json("GET", flow+"/versions/1", adminToken, nil, 200); v["spec"] == nil {
		t.Fatalf("version 1: %v", v)
	}
	c.json("GET", flow+"/versions/7", adminToken, nil, 404)
	if d := c.json("GET", flow+"/draft", adminToken, nil, 200); toJSON(d["problems"]) != "[]" {
		t.Fatalf("draft: %v", d)
	}
	if l := c.json("GET", "/v1/flows", adminToken, nil, 200); len(l["flows"].([]any)) != 2 { // + other_flow
		t.Fatalf("flows: %v", l)
	}
	audit := toJSON(c.json("GET", "/v1/audit", adminToken, nil, 200)["entries"])
	for _, action := range []string{"flow.create", "draft.save", "flow.publish", "number.map"} {
		if !strings.Contains(audit, action) {
			t.Errorf("audit log has no %s", action)
		}
	}
	if strings.Contains(audit, "persona") {
		t.Error("audit log should not copy flow contents")
	}

	// Unmap -> the agent gets 404 and uses its fallback flow.
	if status, _, _ := c.do("DELETE", "/v1/numbers/+918045678901", adminToken, nil); status != 204 {
		t.Fatalf("delete: %d", status)
	}
	c.json("DELETE", "/v1/numbers/+918045678901", adminToken, nil, 404)
	c.json("GET", "/internal/v1/resolve?number=%2B918045678901", runtimeToken, nil, 404)
}

func TestValidateEndpoint(t *testing.T) {
	c := client{t, newTestServer(t).URL}
	if res := c.json("POST", "/v1/validate", adminToken, flowFile(t), 200); res["valid"] != true {
		t.Fatalf("%v", res)
	}
	res := c.json("POST", "/v1/validate", adminToken, []byte(`{"schema_version": 1}`), 200)
	if res["valid"] != false || !strings.Contains(toJSON(res["problems"]), "persona: Field required") {
		t.Fatalf("%v", res)
	}
	c.json("POST", "/v1/validate", adminToken, []byte(`[1,`), 400)
}

func TestNormalizeNumber(t *testing.T) {
	for raw, want := range map[string]string{
		"+918045678901":      "+918045678901",
		"918045678901":       "+918045678901",
		"0091 80 4567 8901":  "+918045678901",
		"+91 (80) 4567-8901": "+918045678901",
		"tel:+918045678901":  "+918045678901",
	} {
		if got, ok := NormalizeNumber(raw); !ok || got != want {
			t.Errorf("%q -> %q %v, want %q", raw, got, ok, want)
		}
	}
	for _, bad := range []string{"", "12345", "+0918045678901", "abc", "+91804567890123456"} {
		if got, ok := NormalizeNumber(bad); ok {
			t.Errorf("%q should be rejected, got %q", bad, got)
		}
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func assertSameKeyOrder(t *testing.T, a, b []byte) {
	t.Helper()
	da, err1 := flowspec.Decode(a)
	db, err2 := flowspec.Decode(b)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	ka := strings.Join(da.(*flowspec.Object).Values["nodes"].(*flowspec.Object).Keys, ",")
	kb := strings.Join(db.(*flowspec.Object).Values["nodes"].(*flowspec.Object).Keys, ",")
	if ka != kb || ka == "" {
		t.Fatalf("node order changed: %q vs %q", ka, kb)
	}
}
