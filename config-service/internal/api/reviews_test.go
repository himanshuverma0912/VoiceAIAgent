package api

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// Maker-checker: a maker submits, a different person who is a checker
// approves (which publishes exactly what was submitted) or rejects.
func TestMakerChecker(t *testing.T) {
	c, kc := peopleServer(t, nil)
	asha := kc.Token(t, "asha@demo.corp", []string{"viewer"}, nil, nil) // maker
	ravi := kc.Token(t, "ravi@demo.corp", []string{"viewer"}, nil, nil) // checker
	mia := kc.Token(t, "mia@demo.corp", []string{"viewer"}, nil, nil)   // maker and checker
	vera := kc.Token(t, "vera@demo.corp", []string{"viewer"}, nil, nil) // viewer
	c.json("PUT", "/v1/workspaces/emi", adminToken, []byte(`{"name": "EMI"}`), 201)
	c.json("PUT", "/v1/workspaces/emi/members/asha@demo.corp", adminToken, member(true, false, true), 201)
	c.json("PUT", "/v1/workspaces/emi/members/ravi@demo.corp", adminToken, member(false, true, false), 201)
	c.json("PUT", "/v1/workspaces/emi/members/mia@demo.corp", adminToken, member(true, true, false), 201)
	c.json("PUT", "/v1/workspaces/emi/members/vera@demo.corp", adminToken, member(false, false, false), 201)

	flow := "/v1/flows/emi_payment_issue"
	emi := bytes.ReplaceAll(spec(t, "emi-payment-issue.json"), []byte("\r"), nil)
	edited := bytes.Replace(emi, []byte(`"settings": {`), []byte(`"settings": {"llm_temperature": 0.2, `), 1)
	c.json("PUT", flow+"/draft?workspace=emi", asha, emi, 200)

	// Nobody publishes directly any more (only scripts with ADMIN_TOKEN).
	if res := c.json("POST", flow+"/publish", asha, nil, 403); !strings.Contains(fmt.Sprint(res["error"]), "submit the draft for review") {
		t.Fatalf("direct publish: %v", res)
	}
	c.json("POST", flow+"/publish", ravi, nil, 403)

	// The maker submits; viewers and checkers-only can't.
	c.json("POST", flow+"/reviews", vera, []byte(`{"note": "x"}`), 403)
	c.json("POST", flow+"/reviews", ravi, []byte(`{"note": "x"}`), 403)
	r1 := c.json("POST", flow+"/reviews", asha, []byte(`{"note": "First version of the EMI line"}`), 201)
	if r1["status"] != "pending" || r1["submitted_by"] != "asha@demo.corp" || r1["base_version"] != nil || r1["spec"] == nil {
		t.Fatalf("submit: %v", r1)
	}
	id1 := fmt.Sprint(r1["review_id"])
	if f := c.json("GET", flow, vera, nil, 200); f["pending_review"].(map[string]any)["submitted_by"] != "asha@demo.corp" {
		t.Fatalf("pending review on the flow: %v", f["pending_review"])
	}

	// The inbox: what's waiting, and whether it's mine to decide.
	inbox := c.json("GET", "/v1/reviews", ravi, nil, 200)["reviews"].([]any)
	if len(inbox) != 1 || inbox[0].(map[string]any)["for_me"] != true || inbox[0].(map[string]any)["flow_name"] == "" {
		t.Fatalf("ravi's inbox: %v", inbox)
	}
	if inbox := c.json("GET", "/v1/reviews", asha, nil, 200)["reviews"].([]any); inbox[0].(map[string]any)["for_me"] != false {
		t.Fatalf("asha's inbox: %v", inbox)
	}

	// Editing the draft after submitting doesn't change what's under review.
	c.json("PUT", flow+"/draft", asha, edited, 200)

	// Approving: checkers only; the submitted flow is what goes live.
	c.json("POST", flow+"/reviews/"+id1+"/approve", asha, nil, 403)
	c.json("POST", flow+"/reviews/"+id1+"/approve", vera, nil, 403)
	v := c.json("POST", flow+"/reviews/"+id1+"/approve", ravi, []byte(`{"note": "Looks good"}`), 201)
	if v["version"] != 1.0 || v["published_by"] != "ravi@demo.corp" || v["submitted_by"] != "asha@demo.corp" || v["checksum"] != r1["checksum"] {
		t.Fatalf("approve: %v", v)
	}
	c.json("POST", flow+"/reviews/"+id1+"/approve", ravi, nil, 409) // already closed
	versions := c.json("GET", flow+"/versions", vera, nil, 200)["versions"].([]any)
	if got := versions[0].(map[string]any); got["submitted_by"] != "asha@demo.corp" || got["published_by"] != "ravi@demo.corp" {
		t.Fatalf("version 1: %v", got)
	}
	if live := c.json("GET", flow+"/versions/1", vera, nil, 200); strings.Contains(toJSON(live["spec"]), "0.2") {
		t.Fatal("the edit made after submitting was published")
	}
	if got := c.json("GET", flow+"/reviews/"+id1, vera, nil, 200); got["status"] != "approved" || got["version"] != 1.0 || got["decided_by"] != "ravi@demo.corp" {
		t.Fatalf("approved review: %v", got)
	}

	// Submitting again replaces an open submission.
	r2 := c.json("POST", flow+"/reviews", asha, []byte(`{"note": "Warmer replies"}`), 201)
	r3 := c.json("POST", flow+"/reviews", asha, []byte(`{"note": "Warmer replies, take 2"}`), 201)
	if r3["base_version"] != 1.0 {
		t.Fatalf("base version: %v", r3["base_version"])
	}
	if got := c.json("GET", flow+"/reviews/"+fmt.Sprint(r2["review_id"]), vera, nil, 200); got["status"] != "superseded" {
		t.Fatalf("older submission: %v", got["status"])
	}

	// Rejecting needs a reason; nothing is published.
	id3 := fmt.Sprint(r3["review_id"])
	c.json("POST", flow+"/reviews/"+id3+"/reject", ravi, nil, 422)
	c.json("POST", flow+"/reviews/"+id3+"/reject", ravi, []byte(`{"note": "  "}`), 422)
	rej := c.json("POST", flow+"/reviews/"+id3+"/reject", ravi, []byte(`{"note": "Temperature 0.2 is too creative for a bank"}`), 200)
	if rej["status"] != "rejected" || rej["decision_note"] != "Temperature 0.2 is too creative for a bank" {
		t.Fatalf("reject: %v", rej)
	}
	if f := c.json("GET", flow, vera, nil, 200); f["latest_version"] != 1.0 || f["pending_review"] != nil {
		t.Fatalf("after reject: %v", f)
	}

	// Someone who is both maker and checker still can't approve their own change.
	r4 := c.json("POST", flow+"/reviews", mia, []byte(`{"note": "mine"}`), 201)
	id4 := fmt.Sprint(r4["review_id"])
	if res := c.json("POST", flow+"/reviews/"+id4+"/approve", mia, nil, 403); !strings.Contains(fmt.Sprint(res["error"]), "someone else") {
		t.Fatalf("self-approval: %v", res)
	}
	c.json("POST", flow+"/reviews/"+id4+"/reject", mia, []byte(`{"note": "x"}`), 403)
	if inbox := c.json("GET", "/v1/reviews", mia, nil, 200)["reviews"].([]any); inbox[0].(map[string]any)["for_me"] != false {
		t.Fatal("mia's own submission shouldn't be hers to decide")
	}
	// Only the person who submitted it can withdraw it.
	c.json("POST", flow+"/reviews/"+id4+"/withdraw", asha, nil, 403)
	if w := c.json("POST", flow+"/reviews/"+id4+"/withdraw", mia, nil, 200); w["status"] != "withdrawn" {
		t.Fatalf("withdraw: %v", w)
	}

	// Nothing to review when the draft is what's live; a broken draft can't be submitted.
	c.json("PUT", flow+"/draft", asha, emi, 200)
	if res := c.json("POST", flow+"/reviews", asha, nil, 409); !strings.Contains(fmt.Sprint(res["error"]), "same as the live version") {
		t.Fatalf("unchanged: %v", res)
	}
	c.json("PUT", flow+"/draft", asha, []byte(`{"schema_version": 1, "flow_id": "emi_payment_issue"}`), 200)
	if res := c.json("POST", flow+"/reviews", asha, nil, 422); res["problems"] == nil {
		t.Fatalf("broken draft: %v", res)
	}

	// The history of reviews, newest first.
	history := c.json("GET", flow+"/reviews", vera, nil, 200)["reviews"].([]any)
	var statuses []string
	for _, r := range history {
		statuses = append(statuses, r.(map[string]any)["status"].(string))
	}
	if strings.Join(statuses, ",") != "withdrawn,rejected,superseded,approved" {
		t.Fatalf("history: %v", statuses)
	}

	// The voice agent gets the approved version.
	c.json("PUT", "/v1/numbers/+918045678901", adminToken, []byte(`{"flow_id":"emi_payment_issue"}`), 200)
	if res := c.json("GET", "/internal/v1/resolve?number=%2B918045678901", runtimeToken, nil, 200); res["version"] != 1.0 {
		t.Fatalf("resolve: %v", res)
	}

	audit := toJSON(c.json("GET", "/v1/audit?limit=200", adminToken, nil, 200)["entries"])
	for _, a := range []string{"review.submit", "review.approve", "review.reject", "review.withdraw", "flow.publish"} {
		if !strings.Contains(audit, a) {
			t.Errorf("audit log has no %s", a)
		}
	}
}
