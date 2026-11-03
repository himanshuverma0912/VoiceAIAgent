package api

import (
	"context"
	"database/sql"
	"encoding/base64"
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

// Drop-resume points: only the agent may keep or claim them; each is returned
// once, for its own flow, within 15 minutes.
func TestResumePoints(t *testing.T) {
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

	key := strings.Repeat("ab", 32)
	sealed := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 60)))
	point := func(flow, s string) []byte {
		return []byte(fmt.Sprintf(`{"flow_id": %q, "flow_version": 3, "sealed": %q}`, flow, s))
	}
	claim := func(flow string) []byte { return []byte(fmt.Sprintf(`{"flow_id": %q}`, flow)) }
	put := "/internal/v1/resume/" + key
	take := put + "/claim"

	// Only the agent's token.
	c.json("PUT", put, adminToken, point("credit_card_helpline", sealed), 401)
	c.json("POST", take, adminToken, claim("credit_card_helpline"), 401)
	c.json("PUT", put, runtimeToken, point("credit_card_helpline", sealed), 201)

	// Bad input is refused: not a hashed key (a phone number), not sealed, extra fields.
	c.json("PUT", "/internal/v1/resume/919876543210", runtimeToken, point("credit_card_helpline", sealed), 400)
	c.json("PUT", put, runtimeToken, point("credit_card_helpline", "not base64!"), 422)
	c.json("PUT", put, runtimeToken, point("Credit Card", sealed), 422)
	c.json("PUT", put, runtimeToken, []byte(`{"flow_id": "credit_card_helpline", "sealed": "`+sealed+`", "caller_number": "+919876543210"}`), 400)

	// Another flow doesn't get it (and doesn't use it up); the right flow does, once.
	c.json("POST", take, runtimeToken, claim("emi_payment_issue"), 404)
	got := c.json("POST", take, runtimeToken, claim("credit_card_helpline"), 200)
	if got["sealed"] != sealed || got["flow_version"] != 3.0 {
		t.Fatalf("claim: %v", got)
	}
	c.json("POST", take, runtimeToken, claim("credit_card_helpline"), 404)

	// Expired points aren't returned, and the purge removes them.
	c.json("PUT", put, runtimeToken, point("credit_card_helpline", sealed), 201)
	if _, err := db.ExecContext(ctx, `UPDATE resume_points SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	c.json("POST", take, runtimeToken, claim("credit_card_helpline"), 404)
	if n, err := st.PurgeResume(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("purge: %d, %v", n, err)
	}
	// A new point is kept for 15 minutes.
	res := c.json("PUT", put, runtimeToken, point("credit_card_helpline", sealed), 201)
	exp, _ := time.Parse(time.RFC3339, fmt.Sprint(res["expires_at"]))
	if d := time.Until(exp); d < 14*time.Minute || d > 15*time.Minute {
		t.Fatalf("expires in %v", d)
	}

	// Call history counts resumes (and nothing about where or what).
	c.json("PUT", "/v1/workspaces/emi", adminToken, []byte(`{"name": "EMI"}`), 201)
	c.json("PUT", "/v1/flows/emi_payment_issue/draft?workspace=emi", adminToken, spec(t, "emi-payment-issue.json"), 200)
	now := time.Now()
	dropped := strings.Replace(string(callRecord("AJ_r1", "caller_hung_up", now.Add(-3*time.Minute), 2, "greet", "ask_loan")), `"turns":`, `"resume_saved": true, "turns":`, 1)
	resumed := strings.Replace(string(callRecord("AJ_r2", "completed", now.Add(-time.Minute), 2, "ask_loan", "goodbye")), `"turns":`, `"resumed": true, "turns":`, 1)
	c.json("POST", "/internal/v1/calls", runtimeToken, []byte(dropped), 201)
	c.json("POST", "/internal/v1/calls", runtimeToken, []byte(resumed), 201)
	c.json("POST", "/internal/v1/calls", runtimeToken, callRecord("AJ_r3", "completed", now.Add(-time.Minute), 2, "greet"), 201)
	vera := kc.Token(t, "vera@demo.corp", []string{"viewer"}, []string{"emi"}, nil)
	sum := c.json("GET", "/v1/flows/emi_payment_issue/calls/summary?days=7&channel=all", vera, nil, 200)
	if got := toJSON(sum["resume"]); got != `{"resumed":1,"saved":1}` {
		t.Fatalf("resume summary: %s", got)
	}
}
