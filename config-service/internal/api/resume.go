package api

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"time"
)

// ── Drop-resume points (for the voice agent) ──────────────────────────────────
//
// A caller who hangs up mid-flow can pick up from the same step by calling the
// same flow back within resumeTTL. The agent seals the point (step, and only
// the values the flow chose to keep) with a key this service never sees, and
// files it under an HMAC of the caller's number: nothing here can be read or
// tied to a number. A point is returned once, then deleted.

// resumeTTL: how long a point is kept (the agent's RESUME_MINUTES).
const resumeTTL = 15 * time.Minute

var resumeKeyRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type resumeBody struct {
	FlowID      string `json:"flow_id"`
	FlowVersion *int   `json:"flow_version"`
	Sealed      string `json:"sealed"`
}

func resumeKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.PathValue("key")
	if !resumeKeyRE.MatchString(key) {
		writeError(w, http.StatusBadRequest, "key: must be 64 lower-case hex characters")
		return "", false
	}
	return key, true
}

// PUT /internal/v1/resume/{key} {"flow_id", "flow_version", "sealed"} — the agent,
// when a caller hangs up mid-flow. Replaces any earlier point under the key.
func (s *Server) putResume(w http.ResponseWriter, r *http.Request) {
	key, ok := resumeKey(w, r)
	if !ok {
		return
	}
	var in resumeBody
	if !decodeStrict(w, r, &in, `{"flow_id", "flow_version", "sealed"}`) {
		return
	}
	var problems []string
	if !identifierRE.MatchString(in.FlowID) {
		problems = append(problems, "flow_id: not a flow id")
	}
	if v := in.FlowVersion; v != nil && (*v < 1 || *v > 1_000_000) {
		problems = append(problems, "flow_version: must be between 1 and 1000000")
	}
	// Nonce (12 bytes) + AES-GCM tag (16) at least; a few KB at most.
	if raw, err := base64.StdEncoding.DecodeString(in.Sealed); err != nil || len(raw) < 28 || len(raw) > 8<<10 {
		problems = append(problems, "sealed: must be the agent's sealed point (base64, at most 8 KB)")
	}
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "resume point has problems", "problems": problems})
		return
	}
	expires := time.Now().Add(resumeTTL)
	if err := s.Store.PutResume(r.Context(), key, in.FlowID, in.FlowVersion, in.Sealed, expires); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"expires_at": expires.UTC().Format(time.RFC3339)})
}

// POST /internal/v1/resume/{key}/claim {"flow_id"} — the agent, as a call starts:
// the caller's point for this flow, if there is one and it hasn't expired. It's
// deleted as it's returned, so it's used once.
func (s *Server) claimResume(w http.ResponseWriter, r *http.Request) {
	key, ok := resumeKey(w, r)
	if !ok {
		return
	}
	var in struct {
		FlowID string `json:"flow_id"`
	}
	if !decodeStrict(w, r, &in, `{"flow_id": "<flow id>"}`) {
		return
	}
	if !identifierRE.MatchString(in.FlowID) {
		writeError(w, http.StatusBadRequest, "flow_id: not a flow id")
		return
	}
	sealed, version, found, err := s.Store.ClaimResume(r.Context(), key, in.FlowID, time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no resume point")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sealed": sealed, "flow_version": version})
}
