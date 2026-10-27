package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/builder"
)

// The AI flow builder: a maker pastes a written description of the call (it
// can be long), the config service asks an LLM to turn it into a flow, checks
// it and has the LLM fix any problems. It takes a minute or more, so it runs
// in the background: POST /v1/builds starts it, GET /v1/builds/{id} reports
// progress and, at the end, the flow. Nothing is saved: Flow Studio opens the
// flow as a draft for the maker to check, test and submit for review.
//
// Builds (with the description) live only in this process's memory and are
// forgotten an hour after they finish.

const (
	maxDescription    = 400_000 // characters, ~100k tokens; a 20k-token prompt is ~80k
	buildsPerPerson   = 1       // at a time
	buildsAtOnce      = 3
	buildsKeptFor     = time.Hour
	buildTimeoutTotal = 15 * time.Minute
)

type buildJob struct {
	ID        string          `json:"build_id"`
	Owner     string          `json:"-"`
	FlowID    string          `json:"flow_id"`
	Name      string          `json:"name"`
	Workspace string          `json:"workspace"`
	Status    string          `json:"status"` // running | done | failed
	Progress  string          `json:"progress"`
	Error     string          `json:"error,omitempty"`
	Result    *builder.Result `json:"result,omitempty"`
	Started   time.Time       `json:"started_at"`
	Finished  *time.Time      `json:"finished_at"`
}

type buildJobs struct {
	mu   sync.Mutex
	jobs map[string]*buildJob
}

func (b *buildJobs) snapshot(id, owner string) (buildJob, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok || j.Owner != owner {
		return buildJob{}, false
	}
	return *j, true
}

// add registers a new job if the limits allow, forgetting old finished ones.
func (b *buildJobs) add(j *buildJob) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.jobs == nil {
		b.jobs = map[string]*buildJob{}
	}
	running, mine := 0, 0
	for id, old := range b.jobs {
		if old.Finished != nil && time.Since(*old.Finished) > buildsKeptFor {
			delete(b.jobs, id)
			continue
		}
		if old.Status == "running" {
			running++
			if old.Owner == j.Owner {
				mine++
			}
		}
	}
	switch {
	case mine >= buildsPerPerson:
		return "you already have a flow being built; wait for it to finish"
	case running >= buildsAtOnce:
		return "the AI builder is busy with other flows; try again in a few minutes"
	}
	b.jobs[j.ID] = j
	return ""
}

func (b *buildJobs) update(id string, fn func(*buildJob)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if j, ok := b.jobs[id]; ok {
		fn(j)
	}
}

// POST /v1/builds {"workspace": "emi", "flow_id": "card_help", "name": "Card help", "description": "..."}
// — start building a flow from a description (makers of the workspace).
func (s *Server) startBuild(w http.ResponseWriter, r *http.Request) {
	if s.Builder == nil {
		writeError(w, http.StatusServiceUnavailable, "the AI flow builder isn't set up: LITELLM_URL and LITELLM_API_KEY are missing")
		return
	}
	var req struct {
		Workspace   string `json:"workspace"`
		FlowID      string `json:"flow_id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeStrict(w, r, &req, `{"workspace", "flow_id", "name", "description"}`) {
		return
	}
	req.Name, req.Description = strings.TrimSpace(req.Name), strings.TrimSpace(req.Description)
	p := principal(r)
	switch {
	case !identifierRE.MatchString(req.FlowID):
		writeError(w, http.StatusBadRequest, "flow_id must be lower-case letters, digits and _ , starting with a letter (max 48 characters)")
		return
	case !p.Sees(req.Workspace):
		writeError(w, http.StatusNotFound, "there's no such workspace")
		return
	case !p.CanEdit(req.Workspace):
		writeError(w, http.StatusForbidden, "you need to be a maker in workspace "+req.Workspace)
		return
	case req.Name == "" || len(req.Name) > 80:
		writeError(w, http.StatusUnprocessableEntity, "name: 1 to 80 characters")
		return
	case len(req.Description) < 40:
		writeError(w, http.StatusUnprocessableEntity, "describe the call in a few sentences at least")
		return
	case len(req.Description) > maxDescription:
		writeError(w, http.StatusRequestEntityTooLarge, "the description is longer than about 100,000 words' worth of tokens; split it into several flows")
		return
	}
	job := &buildJob{
		ID: "b_" + randomHex(8), Owner: p.Actor(), FlowID: req.FlowID, Name: req.Name, Workspace: req.Workspace,
		Status: "running", Progress: "Starting", Started: time.Now(),
	}
	if msg := s.builds.add(job); msg != "" {
		writeError(w, http.StatusTooManyRequests, msg)
		return
	}
	if err := s.Store.AuditOnly(r.Context(), p.Actor(), "build.start", req.FlowID, map[string]any{
		"workspace": req.Workspace, "characters": len(req.Description), "model": s.Builder.Model(),
	}); err != nil {
		s.fail(w, err)
		return
	}

	go s.runBuild(job.ID, req.Workspace, builder.Request{FlowID: req.FlowID, Name: req.Name, Description: req.Description})
	snap, _ := s.builds.snapshot(job.ID, p.Actor())
	writeJSON(w, http.StatusAccepted, snap)
}

func (s *Server) runBuild(id, workspace string, req builder.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeoutTotal)
	defer cancel()
	check := func(flow []byte) []string {
		_, problems, err := s.checkFlow(ctx, flow, req.FlowID, workspace)
		if err != nil {
			return []string{"(top level): not valid JSON: " + err.Error()}
		}
		return problems
	}
	progress := func(msg string) { s.builds.update(id, func(j *buildJob) { j.Progress = msg }) }
	res, err := s.Builder.Build(ctx, req, check, progress)
	now := time.Now()
	s.builds.update(id, func(j *buildJob) {
		j.Finished = &now
		if err != nil {
			j.Status, j.Error, j.Progress = "failed", err.Error(), "Failed"
			return
		}
		j.Status, j.Result, j.Progress = "done", res, "Done"
	})
	if err != nil {
		slog.Warn("flow build failed", "build", id, "error", err.Error())
	} else {
		slog.Info("flow built", "build", id, "steps", res.Steps, "attempts", res.Attempts,
			"problems", len(res.Problems), "input_tokens", res.InputTokens, "output_tokens", res.OutputTokens)
	}
}

// GET /v1/builds/{build_id} — progress, then the flow (only for whoever started it).
func (s *Server) getBuild(w http.ResponseWriter, r *http.Request) {
	job, ok := s.builds.snapshot(r.PathValue("build_id"), principal(r).Actor())
	if !ok {
		writeError(w, http.StatusNotFound, "no such build (builds are kept for an hour)")
		return
	}
	writeJSON(w, http.StatusOK, job)
}
