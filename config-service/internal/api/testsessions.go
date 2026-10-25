package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth"
)

// Browser test calls ("Talk to this draft"): Flow Studio asks for a session,
// gets a LiveKit token for a fresh room, and the agent is dispatched into that
// room with the flow id in its metadata. The agent then reads the current draft
// through /internal/v1/flows/{flow_id}/draft. Nothing about the call is stored.

const (
	TestSessionSeconds = 300 // the agent ends a test call after 5 minutes
	testsPerHour       = 20  // per person, so a stuck script can't run up a bill
)

type rateLimiter struct {
	mu    sync.Mutex
	times map[string][]time.Time
}

// allow reports whether key may start another session now (at most n per hour).
func (l *rateLimiter) allow(key string, n int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.times == nil {
		l.times = map[string][]time.Time{}
	}
	recent := l.times[key][:0]
	for _, t := range l.times[key] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= n {
		l.times[key] = recent
		return false
	}
	l.times[key] = append(recent, now)
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}

// POST /v1/flows/{flow_id}/test-sessions — talk to the flow's current draft
// from the browser. Needs flow-editor or flow-publisher in the flow's team.
func (s *Server) startTestSession(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, auth.RoleViewer)
	if !ok {
		return
	}
	p := principal(r)
	if !p.Can(auth.RoleEditor, f.Team) && !p.Can(auth.RolePublisher, f.Team) {
		writeError(w, http.StatusForbidden, "testing a draft needs the flow-editor or flow-publisher role for team "+f.Team)
		return
	}
	if !s.LiveKit.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "browser test calls aren't set up: LIVEKIT_URL, LIVEKIT_API_KEY and LIVEKIT_API_SECRET are missing")
		return
	}
	draft, err := s.Store.GetDraft(r.Context(), f.FlowID)
	if err != nil {
		s.fail(w, err)
		return
	}
	_, problems, err := checkSpec(draft.Spec, f.FlowID)
	if err != nil || len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "fix the draft's problems before testing it", "problems": problems})
		return
	}
	now := time.Now()
	if !s.tests.allow(p.Subject, testsPerHour, now) {
		writeError(w, http.StatusTooManyRequests, "that's 20 test calls in the last hour; try again a little later")
		return
	}

	room := "studio-test-" + f.FlowID + "-" + randomHex(4)
	identity := "studio-" + randomHex(6)
	meta, _ := json.Marshal(map[string]any{
		"kind": "studio_test", "flow_id": f.FlowID, "checksum": draft.Checksum,
		"requested_by": p.Actor(), "max_seconds": TestSessionSeconds,
	})
	token, err := s.LiveKit.AgentRoomToken(identity, p.Actor(), room, string(meta), 10*time.Minute, now)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Store.AuditOnly(r.Context(), p.Actor(), "test.start", f.FlowID, map[string]any{"room": room, "checksum": draft.Checksum}); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"url": s.LiveKit.URL, "token": token, "room": room, "identity": identity,
		"max_seconds": TestSessionSeconds, "checksum": draft.Checksum,
	})
}

// GET /internal/v1/flows/{flow_id}/draft — for the agent, in a test call.
func (s *Server) internalDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	d, err := s.Store.GetDraft(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if _, problems, err := checkSpec(d.Spec, id); err != nil || len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the draft has problems", "problems": problems})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flow_id": id, "checksum": d.Checksum, "spec": d.Spec})
}
