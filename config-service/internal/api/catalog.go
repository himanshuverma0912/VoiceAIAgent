package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/flowspec"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

// Model catalog: the LLMs (LiteLLM aliases), Deepgram models and Cartesia
// voices flows may use. Flow Studio shows them as dropdowns; a draft that names
// anything else, or something switched off or not offered to its workspace,
// gets a problem, so it can't be test-called or published. Leaving a setting empty
// means "the agent's default" and is always allowed.

var (
	modelIDRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,99}$`)
	langCodeRE = regexp.MustCompile(`^[a-z]{2}(-[A-Za-z]{2})?$`)

	kindLabel = map[string]string{"llm": "LLM", "stt": "speech-to-text model", "tts_voice": "voice"}
)

func catalogKinds() []string { return []string{"llm", "stt", "tts_voice"} }

// catalogProblems checks a flow's model settings against the catalog.
func catalogProblems(entries []store.CatalogEntry, decoded any, workspace string) []string {
	root, ok := decoded.(*flowspec.Object)
	if !ok {
		return nil
	}
	settings, ok := root.Values["settings"].(*flowspec.Object)
	if !ok {
		return nil
	}
	language, _ := settings.Values["language"].(string)
	if language == "" {
		language = "hi"
	}
	var problems []string
	for _, kind := range catalogKinds() {
		key := store.CatalogKinds[kind]
		value, _ := settings.Values[key].(string)
		if value == "" {
			continue // the agent's default
		}
		loc := "settings." + key
		var found *store.CatalogEntry
		for i := range entries {
			if entries[i].Kind == kind && entries[i].ModelID == value {
				found = &entries[i]
				break
			}
		}
		switch {
		case found == nil:
			problems = append(problems, fmt.Sprintf("%s: '%s' isn't in the model catalog; pick a %s from the list (a platform admin can add more)", loc, value, kindLabel[kind]))
		case !found.Enabled:
			problems = append(problems, fmt.Sprintf("%s: '%s' has been switched off in the model catalog; pick another %s", loc, value, kindLabel[kind]))
		case !found.AllowedFor(workspace):
			problems = append(problems, fmt.Sprintf("%s: '%s' isn't offered to workspace %s; pick another %s", loc, value, orUnassigned(workspace), kindLabel[kind]))
		case len(found.Languages) > 0 && language != "multi" && !hasLanguage(found.Languages, language):
			problems = append(problems, fmt.Sprintf("%s: '%s' supports %s, not this flow's language (%s)", loc, value, strings.Join(found.Languages, ", "), language))
		}
	}
	return problems
}

func orUnassigned(workspace string) string {
	if workspace == "" {
		return "(unassigned)"
	}
	return workspace
}

func hasLanguage(langs []string, language string) bool {
	base, _, _ := strings.Cut(language, "-")
	for _, l := range langs {
		if l == language || l == base {
			return true
		}
	}
	return false
}

// checkFlow is checkSpec plus the model catalog, for a flow of the given workspace.
func (s *Server) checkFlow(ctx context.Context, body []byte, flowID, workspace string) (any, []string, error) {
	decoded, problems, err := checkSpec(body, flowID)
	if err != nil {
		return nil, nil, err
	}
	entries, err := s.Store.ListCatalog(ctx)
	if err != nil {
		return nil, nil, err
	}
	return decoded, append(problems, catalogProblems(entries, decoded, workspace)...), nil
}

// GET /v1/catalog — what Flow Studio offers. People see the entries offered to
// their workspaces (switched-off ones too, so Studio can say why a flow's model is
// no longer allowed); platform admins see everything.
func (s *Server) listCatalog(w http.ResponseWriter, r *http.Request) {
	entries, err := s.Store.ListCatalog(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	p := principal(r)
	if !p.IsAdmin() {
		visible := []store.CatalogEntry{}
		for _, e := range entries {
			ok := len(e.Workspaces) == 0
			for _, ws := range e.Workspaces {
				ok = ok || p.Sees(ws)
			}
			if ok {
				visible = append(visible, e)
			}
		}
		entries = visible
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

type catalogInput struct {
	Label      string   `json:"label"`
	Notes      string   `json:"notes"`
	Languages  []string `json:"languages"`
	Workspaces []string `json:"workspaces"`
	Enabled    *bool    `json:"enabled"`
}

func catalogPath(w http.ResponseWriter, r *http.Request) (kind, id string, ok bool) {
	kind, id = r.PathValue("kind"), r.PathValue("model_id")
	if _, known := store.CatalogKinds[kind]; !known {
		writeError(w, http.StatusBadRequest, "kind must be llm, stt or tts_voice")
		return "", "", false
	}
	if !modelIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "model id: letters, digits and . _ : - (max 100), e.g. voice-fast, nova-3 or a Cartesia voice id")
		return "", "", false
	}
	return kind, id, true
}

// PUT /v1/catalog/{kind}/{model_id} — add or change an entry (platform admins).
func (s *Server) putCatalogEntry(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "change the model catalog") {
		return
	}
	kind, id, ok := catalogPath(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var in catalogInput
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "expected {\"label\", \"notes\", \"languages\", \"workspaces\", \"enabled\"}: "+err.Error())
		return
	}
	in.Label, in.Notes = strings.TrimSpace(in.Label), strings.TrimSpace(in.Notes)
	var problems []string
	if in.Label == "" || len(in.Label) > 80 {
		problems = append(problems, "label: 1 to 80 characters")
	}
	if len(in.Notes) > 300 {
		problems = append(problems, "notes: at most 300 characters")
	}
	for _, l := range in.Languages {
		if !langCodeRE.MatchString(l) {
			problems = append(problems, fmt.Sprintf("languages: '%s' isn't a language code like hi or en", l))
		}
	}
	for _, ws := range in.Workspaces {
		if !workspaceRE.MatchString(ws) {
			problems = append(problems, fmt.Sprintf("workspaces: '%s' isn't a workspace id", ws))
		}
	}
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the entry has problems", "problems": problems})
		return
	}
	e := store.CatalogEntry{
		Kind: kind, ModelID: id, Label: in.Label, Notes: in.Notes,
		Languages: nonNil(in.Languages), Workspaces: nonNil(in.Workspaces), Enabled: in.Enabled == nil || *in.Enabled,
	}
	created, err := s.Store.PutCatalogEntry(r.Context(), e, actor(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	used, err := s.Store.CatalogUsage(r.Context(), kind, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, status, map[string]any{"entry": e, "created": created, "used_by": used})
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// DELETE /v1/catalog/{kind}/{model_id} — remove an entry nothing uses (platform admins).
func (s *Server) deleteCatalogEntry(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "change the model catalog") {
		return
	}
	kind, id, ok := catalogPath(w, r)
	if !ok {
		return
	}
	used, err := s.Store.CatalogUsage(r.Context(), kind, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(used) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   fmt.Sprintf("still used by %s; switch those flows to another %s first, or switch this one off instead", strings.Join(used, ", "), kindLabel[kind]),
			"used_by": used,
		})
		return
	}
	if err := s.Store.DeleteCatalogEntry(r.Context(), kind, id, actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
