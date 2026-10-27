package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

// Maker-checker. A maker submits the draft for review with a note; a checker
// in the same workspace, who isn't the person who submitted it, approves it
// (which publishes exactly what was submitted) or rejects it with a reason.

func reviewID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("review_id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "review_id must be a whole number")
		return 0, false
	}
	return id, true
}

type noteBody struct {
	Note string `json:"note"`
}

func readNote(w http.ResponseWriter, r *http.Request, required bool) (string, bool) {
	var b noteBody
	if r.ContentLength != 0 {
		if !decodeStrict(w, r, &b, `{"note": "<text>"}`) {
			return "", false
		}
	}
	b.Note = strings.TrimSpace(b.Note)
	switch {
	case required && b.Note == "":
		writeError(w, http.StatusUnprocessableEntity, "say why, so the maker knows what to change")
		return "", false
	case len(b.Note) > 2000:
		writeError(w, http.StatusUnprocessableEntity, "note: at most 2000 characters")
		return "", false
	}
	return b.Note, true
}

// validator re-checks a flow (shape, rules, model catalog) for a workspace.
func (s *Server) validator(r *http.Request, flowID, workspace string) func([]byte) []string {
	return func(spec []byte) []string {
		_, problems, err := s.checkFlow(r.Context(), spec, flowID, workspace)
		if err != nil {
			return []string{"(top level): not valid JSON: " + err.Error()}
		}
		return problems
	}
}

// POST /v1/flows/{flow_id}/reviews {"note": "..."} — submit the draft (makers).
func (s *Server) submitReview(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, toEdit)
	if !ok {
		return
	}
	note, ok := readNote(w, r, false)
	if !ok {
		return
	}
	review, err := s.Store.SubmitReview(r.Context(), f.FlowID, note, actor(r), s.validator(r, f.FlowID, f.Workspace))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, review)
}

// GET /v1/flows/{flow_id}/reviews — the flow's reviews, newest first.
func (s *Server) listReviews(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, toView)
	if !ok {
		return
	}
	reviews, err := s.Store.ListReviews(r.Context(), f.FlowID, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": reviews})
}

// GET /v1/flows/{flow_id}/reviews/{review_id} — one review, with the submitted flow.
func (s *Server) getReview(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, toView)
	if !ok {
		return
	}
	id, ok := reviewID(w, r)
	if !ok {
		return
	}
	review, err := s.Store.GetReview(r.Context(), f.FlowID, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, review)
}

// POST /v1/flows/{flow_id}/reviews/{review_id}/approve {"note": "..."} — checkers.
// Publishes the submitted flow; numbers that follow the latest version use it
// from their next call.
func (s *Server) approveReview(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, toApprove)
	if !ok {
		return
	}
	id, ok := reviewID(w, r)
	if !ok {
		return
	}
	note, ok := readNote(w, r, false)
	if !ok {
		return
	}
	v, err := s.Store.ApproveReview(r.Context(), f.FlowID, id, actor(r), note, s.validator(r, f.FlowID, f.Workspace))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"flow_id": v.FlowID, "version": v.Version, "checksum": v.Checksum,
		"published_at": v.PublishedAt, "published_by": v.PublishedBy, "submitted_by": v.SubmittedBy,
	})
}

// POST /v1/flows/{flow_id}/reviews/{review_id}/reject {"note": "why"} — checkers.
func (s *Server) rejectReview(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, toApprove)
	if !ok {
		return
	}
	id, ok := reviewID(w, r)
	if !ok {
		return
	}
	note, ok := readNote(w, r, true)
	if !ok {
		return
	}
	if err := s.Store.RejectReview(r.Context(), f.FlowID, id, actor(r), note); err != nil {
		s.fail(w, err)
		return
	}
	s.writeReview(w, r, f.FlowID, id)
}

// POST /v1/flows/{flow_id}/reviews/{review_id}/withdraw — the maker who submitted it.
func (s *Server) withdrawReview(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, toView)
	if !ok {
		return
	}
	id, ok := reviewID(w, r)
	if !ok {
		return
	}
	if err := s.Store.WithdrawReview(r.Context(), f.FlowID, id, actor(r), principal(r).IsAdmin()); err != nil {
		s.fail(w, err)
		return
	}
	s.writeReview(w, r, f.FlowID, id)
}

func (s *Server) writeReview(w http.ResponseWriter, r *http.Request, flowID string, id int64) {
	review, err := s.Store.GetReview(r.Context(), flowID, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	review.Spec = nil
	writeJSON(w, http.StatusOK, review)
}

// GET /v1/reviews — open submissions in the workspaces where the caller is a
// checker (platform admins: all), oldest first. "for_me" marks the ones the
// caller may decide (not their own).
func (s *Server) pendingReviews(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var workspaces []string
	if !p.IsAdmin() {
		workspaces = []string{}
		for _, m := range p.Workspaces {
			if m.Checker || m.Maker {
				workspaces = append(workspaces, m.WorkspaceID)
			}
		}
	}
	reviews, err := s.Store.PendingReviews(r.Context(), workspaces)
	if err != nil {
		s.fail(w, err)
		return
	}
	type item struct {
		store.Review
		ForMe bool `json:"for_me"`
	}
	out := []item{}
	for _, rv := range reviews {
		out = append(out, item{Review: rv, ForMe: p.CanApprove(rv.Workspace) && rv.SubmittedBy != p.Actor()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": out})
}
