package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

// Workspaces: groups of flows and the people who work on them. Platform admins
// create workspaces; a workspace's owners manage its members.

var emailRE = regexp.MustCompile(`^[^@\s/]+@[^@\s/]+\.[^@\s/]+$`)

func workspacePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	ws := r.PathValue("workspace")
	if !workspaceRE.MatchString(ws) {
		writeError(w, http.StatusBadRequest, "workspace ids are lower-case letters, digits, - and _ , starting with a letter (max 32)")
		return "", false
	}
	return ws, true
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any, shape string) bool {
	body, ok := readBody(w, r)
	if !ok {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "body must be "+shape+": "+err.Error())
		return false
	}
	return true
}

type workspaceView struct {
	store.Workspace
	MyRoles *struct {
		Maker   bool `json:"maker"`
		Checker bool `json:"checker"`
		Owner   bool `json:"owner"`
	} `json:"my_roles"` // null for admins viewing a workspace they aren't in
}

// GET /v1/workspaces — the caller's workspaces (platform admins: all).
func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var ids []string
	if !p.IsAdmin() {
		ids = p.WorkspaceIDs()
	}
	list, err := s.Store.ListWorkspaces(r.Context(), ids)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []workspaceView{}
	for _, ws := range list {
		v := workspaceView{Workspace: ws}
		if m, ok := p.Member(ws.WorkspaceID); ok {
			v.MyRoles = &struct {
				Maker   bool `json:"maker"`
				Checker bool `json:"checker"`
				Owner   bool `json:"owner"`
			}{m.Maker, m.Checker, m.Owner}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": out})
}

// PUT /v1/workspaces/{workspace} {"name": "EMI collections"} — create one
// (platform admins) or rename it (admins and its owners).
func (s *Server) putWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := workspacePath(w, r)
	if !ok {
		return
	}
	p := principal(r)
	_, err := s.Store.GetWorkspace(r.Context(), ws)
	exists := err == nil
	switch {
	case err != nil && err != store.ErrNoWorkspace:
		s.fail(w, err)
		return
	case !exists && !p.IsAdmin():
		writeError(w, http.StatusForbidden, "only platform admins can create workspaces")
		return
	case exists && !p.Owns(ws):
		if p.Sees(ws) {
			writeError(w, http.StatusForbidden, "only the workspace's owners can rename it")
		} else {
			writeError(w, http.StatusNotFound, "there's no such workspace")
		}
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeStrict(w, r, &req, `{"name": "<display name>"}`) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 80 {
		writeError(w, http.StatusUnprocessableEntity, "name: 1 to 80 characters")
		return
	}
	created, err := s.Store.PutWorkspace(r.Context(), ws, req.Name, actor(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	saved, err := s.Store.GetWorkspace(r.Context(), ws)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, saved)
}

// DELETE /v1/workspaces/{workspace} — remove an empty workspace (platform admins).
func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "remove workspaces") {
		return
	}
	ws, ok := workspacePath(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteWorkspace(r.Context(), ws, actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /v1/workspaces/{workspace}/members — who's in it (any member).
func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	ws, ok := workspacePath(w, r)
	if !ok {
		return
	}
	if !principal(r).Sees(ws) {
		writeError(w, http.StatusNotFound, "there's no such workspace")
		return
	}
	info, err := s.Store.GetWorkspace(r.Context(), ws)
	if err != nil {
		s.fail(w, err)
		return
	}
	members, err := s.Store.ListMembers(r.Context(), ws)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": info, "members": members})
}

func memberPath(w http.ResponseWriter, r *http.Request) (ws, email string, ok bool) {
	if ws, ok = workspacePath(w, r); !ok {
		return
	}
	email = strings.ToLower(strings.TrimSpace(r.PathValue("email")))
	if !emailRE.MatchString(email) || len(email) > 254 {
		writeError(w, http.StatusBadRequest, "not an email address")
		return "", "", false
	}
	if !principal(r).Owns(ws) {
		if principal(r).Sees(ws) {
			writeError(w, http.StatusForbidden, "only the workspace's owners can change its members")
		} else {
			writeError(w, http.StatusNotFound, "there's no such workspace")
		}
		return "", "", false
	}
	return ws, email, true
}

// PUT /v1/workspaces/{workspace}/members/{email} {"maker": true, "checker": false, "owner": false}
// — add someone or change their roles (owners and platform admins). The person
// signs in with that email; they don't need to have signed in before.
func (s *Server) putMember(w http.ResponseWriter, r *http.Request) {
	ws, email, ok := memberPath(w, r)
	if !ok {
		return
	}
	if _, err := s.Store.GetWorkspace(r.Context(), ws); err != nil {
		s.fail(w, err)
		return
	}
	var req struct {
		Maker   bool `json:"maker"`
		Checker bool `json:"checker"`
		Owner   bool `json:"owner"`
	}
	if !decodeStrict(w, r, &req, `{"maker": bool, "checker": bool, "owner": bool}`) {
		return
	}
	created, err := s.Store.PutMember(r.Context(), ws, store.Member{Email: email, Maker: req.Maker, Checker: req.Checker, Owner: req.Owner}, actor(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"email": email, "maker": req.Maker, "checker": req.Checker, "owner": req.Owner, "created": created})
}

// DELETE /v1/workspaces/{workspace}/members/{email}
func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	ws, email, ok := memberPath(w, r)
	if !ok {
		return
	}
	if err := s.Store.RemoveMember(r.Context(), ws, email, actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
