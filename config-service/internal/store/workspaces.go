package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth"
)

// ── Workspaces and their members ──────────────────────────────────────────────
//
// See migrations/006_workspaces.sql. Members are identified by email, the same
// email Keycloak puts in their token.

var (
	ErrLastOwner         = errors.New("a workspace needs at least one owner; make someone else an owner first")
	ErrWorkspaceNotEmpty = errors.New("the workspace still has flows; move them to another workspace first")
)

type Workspace struct {
	WorkspaceID string    `json:"workspace_id"`
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
	Flows       int       `json:"flows"`
	Members     int       `json:"members"`
}

type Member struct {
	Email   string    `json:"email"`
	Maker   bool      `json:"maker"`
	Checker bool      `json:"checker"`
	Owner   bool      `json:"owner"`
	AddedAt time.Time `json:"added_at"`
	AddedBy string    `json:"added_by"`
}

const workspaceSQL = `
	SELECT w.workspace_id, w.name, w.created_at, w.created_by,
	       (SELECT count(*) FROM flows f WHERE f.workspace = w.workspace_id),
	       (SELECT count(*) FROM workspace_members m WHERE m.workspace_id = w.workspace_id)
	FROM workspaces w`

func scanWorkspace(row interface{ Scan(...any) error }) (Workspace, error) {
	var w Workspace
	err := row.Scan(&w.WorkspaceID, &w.Name, &w.CreatedAt, &w.CreatedBy, &w.Flows, &w.Members)
	return w, err
}

// ListWorkspaces returns the given workspaces, or all of them if ids is nil.
func (s *Store) ListWorkspaces(ctx context.Context, ids []string) ([]Workspace, error) {
	query, args := workspaceSQL+` ORDER BY w.name, w.workspace_id`, []any{}
	if ids != nil {
		query, args = workspaceSQL+` WHERE w.workspace_id = ANY($1) ORDER BY w.name, w.workspace_id`, []any{pq.StringArray(ids)}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	w, err := scanWorkspace(s.db.QueryRowContext(ctx, workspaceSQL+` WHERE w.workspace_id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoWorkspace
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// PutWorkspace creates a workspace or renames it; created says which.
func (s *Store) PutWorkspace(ctx context.Context, id, name, actor string) (created bool, err error) {
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO workspaces (workspace_id, name, created_by) VALUES ($1, $2, $3)
			ON CONFLICT (workspace_id) DO UPDATE SET name = EXCLUDED.name
			RETURNING (xmax = 0)`, id, name, actor).Scan(&created); err != nil {
			return err
		}
		action := "workspace.rename"
		if created {
			action = "workspace.create"
		}
		return audit(ctx, tx, actor, action, id, map[string]any{"name": name})
	})
	return created, err
}

// DeleteWorkspace removes an empty workspace (one with no flows).
func (s *Store) DeleteWorkspace(ctx context.Context, id, actor string) error {
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		var flows int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM flows WHERE workspace = $1`, id).Scan(&flows); err != nil {
			return err
		}
		if flows > 0 {
			return ErrWorkspaceNotEmpty
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE workspace_id = $1`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNoWorkspace
		}
		return audit(ctx, tx, actor, "workspace.delete", id, map[string]any{})
	})
}

func (s *Store) ListMembers(ctx context.Context, workspace string) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT email, maker, checker, owner, added_at, added_by FROM workspace_members
		WHERE workspace_id = $1 ORDER BY email`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Email, &m.Maker, &m.Checker, &m.Owner, &m.AddedAt, &m.AddedBy); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// owners counts a workspace's owners, inside tx.
func owners(ctx context.Context, tx *sql.Tx, workspace string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id = $1 AND owner`, workspace).Scan(&n)
	return n, err
}

// PutMember adds someone to a workspace or changes their roles. It refuses a
// change that would leave a workspace that had owners without any.
func (s *Store) PutMember(ctx context.Context, workspace string, m Member, actor string) (created bool, err error) {
	m.Email = strings.ToLower(strings.TrimSpace(m.Email))
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM workspaces WHERE workspace_id = $1 FOR UPDATE`, workspace); err != nil {
			return err
		}
		before, err := owners(ctx, tx, workspace)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO workspace_members (workspace_id, email, maker, checker, owner, added_by)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (workspace_id, email) DO UPDATE
			SET maker = EXCLUDED.maker, checker = EXCLUDED.checker, owner = EXCLUDED.owner
			RETURNING (xmax = 0)`,
			workspace, m.Email, m.Maker, m.Checker, m.Owner, actor).Scan(&created); err != nil {
			return err
		}
		after, err := owners(ctx, tx, workspace)
		if err != nil {
			return err
		}
		if before > 0 && after == 0 {
			return ErrLastOwner
		}
		action := "member.update"
		if created {
			action = "member.add"
		}
		return audit(ctx, tx, actor, action, workspace, map[string]any{
			"email": m.Email, "maker": m.Maker, "checker": m.Checker, "owner": m.Owner,
		})
	})
	return created, err
}

func (s *Store) RemoveMember(ctx context.Context, workspace, email, actor string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM workspaces WHERE workspace_id = $1 FOR UPDATE`, workspace); err != nil {
			return err
		}
		before, err := owners(ctx, tx, workspace)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM workspace_members WHERE workspace_id = $1 AND email = $2`, workspace, email)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		after, err := owners(ctx, tx, workspace)
		if err != nil {
			return err
		}
		if before > 0 && after == 0 {
			return ErrLastOwner
		}
		return audit(ctx, tx, actor, "member.remove", workspace, map[string]any{"email": email})
	})
}

// Memberships returns the workspaces a person belongs to, with their roles.
func (s *Store) Memberships(ctx context.Context, email string) ([]auth.Membership, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.workspace_id, w.name, m.maker, m.checker, m.owner
		FROM workspace_members m JOIN workspaces w USING (workspace_id)
		WHERE m.email = $1 ORDER BY w.name, m.workspace_id`, strings.ToLower(email))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []auth.Membership{}
	for rows.Next() {
		var m auth.Membership
		if err := rows.Scan(&m.WorkspaceID, &m.Name, &m.Maker, &m.Checker, &m.Owner); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ImportDirectoryMemberships gives a person their first memberships from their
// Keycloak groups (/teams/<name> → that workspace; flow-editor → maker,
// flow-publisher → checker), once. After the first import, nothing from
// Keycloak changes membership again: that's done in Flow Studio.
func (s *Store) ImportDirectoryMemberships(ctx context.Context, email string, teams []string, maker, checker bool) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || len(teams) == 0 {
		return nil
	}
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO directory_imports (email) VALUES ($1) ON CONFLICT DO NOTHING`, email)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // already imported once
		}
		for _, team := range teams {
			name := strings.Title(strings.NewReplacer("_", " ", "-", " ").Replace(team)) //nolint:staticcheck // ASCII ids
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO workspaces (workspace_id, name, created_by) VALUES ($1, $2, 'directory import')
				ON CONFLICT DO NOTHING`, team, name); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO workspace_members (workspace_id, email, maker, checker, owner, added_by)
				VALUES ($1, $2, $3, $4, false, 'directory import')
				ON CONFLICT DO NOTHING`, team, email, maker, checker); err != nil {
				return err
			}
		}
		return audit(ctx, tx, "directory import", "member.import", email, map[string]any{
			"workspaces": teams, "maker": maker, "checker": checker,
		})
	})
}
