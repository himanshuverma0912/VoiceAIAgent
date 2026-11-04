package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/lib/pq"
)

// ── Integrations ──────────────────────────────────────────────────────────────
//
// The systems flows may call (see migrations/008_integrations.sql and
// 010_workspace_integrations.sql). Platform admins add integrations that apply
// at once; makers add integrations to their own workspace as proposals that a
// checker of that workspace approves before live calls use them. The API
// checks every field before it gets here.

// ErrNothingPending: approve, reject or withdraw with no change waiting.
var ErrNothingPending = errors.New("there are no changes to this integration waiting for review")

// IntegrationAuth is how the agent signs in to an integration. Sealed holds
// the token (kind "header") or client secret (kind "oauth"), encrypted for the
// voice agent: this service can't read it and never shows it.
type IntegrationAuth struct {
	Kind      string `json:"kind,omitempty"`   // "" (none), "header" or "oauth"
	Header    string `json:"header,omitempty"` // header: e.g. Authorization
	TokenURL  string `json:"token_url,omitempty"`
	ClientID  string `json:"client_id,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Sealed    string `json:"sealed,omitempty"`
	HasSecret bool   `json:"has_secret,omitempty"` // shown instead of Sealed
}

// IntegrationDef is what an integration is: where it is, how to sign in and
// its tools. An integration has a live one and, while a change waits for
// review, a pending one.
type IntegrationDef struct {
	Kind       string            `json:"kind"` // mcp | http
	Label      string            `json:"label"`
	Notes      string            `json:"notes"`
	URL        string            `json:"url,omitempty"`
	TestURL    string            `json:"test_url,omitempty"`
	AuthHeader string            `json:"auth_header,omitempty"` // platform admins: header filled from
	AuthEnv    string            `json:"auth_env,omitempty"`    // an INTEGRATION_... variable of the agent
	Auth       IntegrationAuth   `json:"auth"`
	TimeoutMs  int               `json:"timeout_ms,omitempty"`
	Workspaces []string          `json:"workspaces"` // empty: every workspace
	Enabled    bool              `json:"enabled"`
	Tools      []IntegrationTool `json:"tools"`
}

type Integration struct {
	IntegrationID string `json:"integration_id"`
	IntegrationDef
	OwnerWorkspace string          `json:"owner_workspace,omitempty"` // "": a platform admin's
	IsLive         bool            `json:"live"`                      // approved at least once
	Pending        *IntegrationDef `json:"pending,omitempty"`
	PendingBy      string          `json:"pending_by,omitempty"`
	PendingAt      *time.Time      `json:"pending_at,omitempty"`
	PendingNote    string          `json:"pending_note,omitempty"`
	ApprovedBy     string          `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time      `json:"approved_at,omitempty"`
	RejectedBy     string          `json:"rejected_by,omitempty"`
	RejectedAt     *time.Time      `json:"rejected_at,omitempty"`
	RejectedNote   string          `json:"rejected_note,omitempty"`
	UpdatedAt      time.Time       `json:"updated_at"`
	UpdatedBy      string          `json:"updated_by"`
}

// IntegrationTool is one tool of an integration, as the platform offers it.
type IntegrationTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Inputs      []ToolInput `json:"inputs"`
	Outputs     []string    `json:"outputs"`          // what an API step may save
	Sensitive   bool        `json:"sensitive"`        // needs a verified caller
	Locked      []string    `json:"locked"`           // inputs the flow must set
	MaxCalls    int         `json:"max_calls"`        // per call
	Method      string      `json:"method,omitempty"` // http: GET or POST
	Path        string      `json:"path,omitempty"`   // http: e.g. /api/get_card_balance or /cards/{card_last4}
}

type ToolInput struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"` // string | number | integer | boolean
	Description string   `json:"description"`
	Required    bool     `json:"required"`
	Enum        []string `json:"enum,omitempty"`
}

// LiveDef is what live calls use (nil until a checker first approves it).
func (i *Integration) LiveDef() *IntegrationDef {
	if !i.IsLive {
		return nil
	}
	return &i.IntegrationDef
}

// DraftDef is what drafts and test calls use: the pending change if there is
// one, else the live definition.
func (i *Integration) DraftDef() *IntegrationDef {
	if i.Pending != nil {
		return i.Pending
	}
	return i.LiveDef()
}

// Tool returns the tool called name, or nil.
func (d *IntegrationDef) Tool(name string) *IntegrationTool {
	for k := range d.Tools {
		if d.Tools[k].Name == name {
			return &d.Tools[k]
		}
	}
	return nil
}

// AllowedFor reports whether a flow of workspace may use this definition.
func (d *IntegrationDef) AllowedFor(workspace string) bool {
	if !d.Enabled {
		return false
	}
	if len(d.Workspaces) == 0 {
		return true
	}
	for _, w := range d.Workspaces {
		if w == workspace {
			return true
		}
	}
	return false
}

// Input returns the input called name, or nil.
func (t *IntegrationTool) Input(name string) *ToolInput {
	for k := range t.Inputs {
		if t.Inputs[k].Name == name {
			return &t.Inputs[k]
		}
	}
	return nil
}

const integrationColumns = `integration_id, kind, label, notes, url, test_url, auth_header, auth_env, timeout_ms,
	workspaces, enabled, tools, updated_at, updated_by, owner_workspace, live, auth, pending, pending_by,
	pending_at, pending_note, approved_by, approved_at, rejected_by, rejected_at, rejected_note`

type rowScanner interface{ Scan(dest ...any) error }

func scanIntegration(row rowScanner) (Integration, error) {
	var i Integration
	var wss pq.StringArray
	var tools, auth, pending []byte
	var owner sql.NullString
	var pendingAt, approvedAt, rejectedAt sql.NullTime
	if err := row.Scan(&i.IntegrationID, &i.Kind, &i.Label, &i.Notes, &i.URL, &i.TestURL, &i.AuthHeader,
		&i.AuthEnv, &i.TimeoutMs, &wss, &i.Enabled, &tools, &i.UpdatedAt, &i.UpdatedBy, &owner, &i.IsLive, &auth,
		&pending, &i.PendingBy, &pendingAt, &i.PendingNote, &i.ApprovedBy, &approvedAt, &i.RejectedBy, &rejectedAt,
		&i.RejectedNote); err != nil {
		return i, err
	}
	i.Workspaces = append([]string{}, wss...)
	i.OwnerWorkspace = owner.String
	if err := json.Unmarshal(tools, &i.Tools); err != nil {
		return i, err
	}
	if err := json.Unmarshal(auth, &i.Auth); err != nil {
		return i, err
	}
	if len(pending) > 0 {
		i.Pending = &IntegrationDef{}
		if err := json.Unmarshal(pending, i.Pending); err != nil {
			return i, err
		}
	}
	for _, t := range []struct {
		from sql.NullTime
		to   **time.Time
	}{{pendingAt, &i.PendingAt}, {approvedAt, &i.ApprovedAt}, {rejectedAt, &i.RejectedAt}} {
		if t.from.Valid {
			v := t.from.Time.UTC()
			*t.to = &v
		}
	}
	return i, nil
}

func (s *Store) ListIntegrations(ctx context.Context) ([]Integration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+integrationColumns+` FROM integrations ORDER BY label, integration_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Integration{}
	for rows.Next() {
		i, err := scanIntegration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) GetIntegration(ctx context.Context, id string) (*Integration, error) {
	i, err := scanIntegration(s.db.QueryRowContext(ctx, `SELECT `+integrationColumns+` FROM integrations WHERE integration_id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// writeLive replaces the live definition (inside a transaction).
func writeLive(ctx context.Context, tx *sql.Tx, id, owner string, d IntegrationDef, actor string) (created bool, err error) {
	tools, err := json.Marshal(d.Tools)
	if err != nil {
		return false, err
	}
	auth, err := json.Marshal(d.Auth)
	if err != nil {
		return false, err
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO integrations (integration_id, kind, label, notes, url, test_url, auth_header, auth_env,
			timeout_ms, workspaces, enabled, tools, updated_by, owner_workspace, auth, live, approved_by, approved_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, true, $13, now())
		ON CONFLICT (integration_id) DO UPDATE SET
			kind = EXCLUDED.kind, label = EXCLUDED.label, notes = EXCLUDED.notes, url = EXCLUDED.url,
			test_url = EXCLUDED.test_url, auth_header = EXCLUDED.auth_header, auth_env = EXCLUDED.auth_env,
			timeout_ms = EXCLUDED.timeout_ms, workspaces = EXCLUDED.workspaces, enabled = EXCLUDED.enabled,
			tools = EXCLUDED.tools, updated_at = now(), updated_by = EXCLUDED.updated_by,
			owner_workspace = EXCLUDED.owner_workspace, auth = EXCLUDED.auth, live = true,
			approved_by = EXCLUDED.approved_by, approved_at = now()
		RETURNING (xmax = 0)`,
		id, d.Kind, d.Label, d.Notes, d.URL, d.TestURL, d.AuthHeader, d.AuthEnv, d.TimeoutMs,
		pq.StringArray(d.Workspaces), d.Enabled, string(tools), actor, nullable(owner), string(auth),
	).Scan(&created)
	return created, err
}

// auditDetail: what an audit entry records about a definition (the URLs, who
// pointed calls where; never a secret).
func auditDetail(d IntegrationDef, owner string) map[string]any {
	names := make([]string, len(d.Tools))
	for k, t := range d.Tools {
		names[k] = t.Name
	}
	return map[string]any{
		"kind": d.Kind, "url": d.URL, "test_url": d.TestURL, "enabled": d.Enabled, "workspaces": d.Workspaces,
		"owner_workspace": owner, "auth": d.Auth.Kind, "token_url": d.Auth.TokenURL, "tools": names,
	}
}

func clearPending(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE integrations SET pending = NULL, pending_by = '', pending_at = NULL, pending_note = ''
		WHERE integration_id = $1`, id)
	return err
}

// PutIntegration adds or changes an integration directly (platform admins):
// it applies at once, and replaces any change waiting for review.
func (s *Store) PutIntegration(ctx context.Context, id, owner string, d IntegrationDef, actor string) (created bool, err error) {
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		created, err = writeLive(ctx, tx, id, owner, d, actor)
		if err != nil {
			return err
		}
		if err := clearPending(ctx, tx, id); err != nil {
			return err
		}
		action := "integration.update"
		if created {
			action = "integration.add"
		}
		return audit(ctx, tx, actor, action, id, auditDetail(d, owner))
	})
	return created, err
}

// ProposeIntegration records a maker's new integration or change for review.
// A new one isn't live until approved; drafts and test calls use it already.
func (s *Store) ProposeIntegration(ctx context.Context, id, owner string, d IntegrationDef, note, actor string) (created bool, err error) {
	pending, err := json.Marshal(d)
	if err != nil {
		return false, err
	}
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integrations WHERE integration_id = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			// A new integration: a row that isn't live yet. Its live columns hold
			// the proposal only because they can't be empty; nothing uses them.
			if _, err := writeLive(ctx, tx, id, owner, d, actor); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE integrations SET live = false, approved_by = '', approved_at = NULL WHERE integration_id = $1`, id); err != nil {
				return err
			}
			created = true
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE integrations SET pending = $2, pending_by = $3, pending_at = now(), pending_note = $4,
				rejected_by = '', rejected_at = NULL, rejected_note = '', updated_at = now(), updated_by = $3
			WHERE integration_id = $1`, id, string(pending), actor, note); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "integration.propose", id, auditDetail(d, owner))
	})
	return created, err
}

// lockPendingIntegration reads an integration for an approve, reject or withdraw.
func lockPendingIntegration(ctx context.Context, tx *sql.Tx, id string) (*Integration, error) {
	i, err := scanIntegration(tx.QueryRowContext(ctx, `SELECT `+integrationColumns+` FROM integrations WHERE integration_id = $1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if i.Pending == nil {
		return nil, ErrNothingPending
	}
	return &i, nil
}

// ApproveIntegration makes the pending change live. The person who proposed
// it can't approve it.
func (s *Store) ApproveIntegration(ctx context.Context, id, actor, note string) error {
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		i, err := lockPendingIntegration(ctx, tx, id)
		if err != nil {
			return err
		}
		if i.PendingBy == actor {
			return ErrSelfApproval
		}
		if _, err := writeLive(ctx, tx, id, i.OwnerWorkspace, *i.Pending, actor); err != nil {
			return err
		}
		if err := clearPending(ctx, tx, id); err != nil {
			return err
		}
		detail := auditDetail(*i.Pending, i.OwnerWorkspace)
		detail["proposed_by"], detail["note"] = i.PendingBy, note
		return audit(ctx, tx, actor, "integration.approve", id, detail)
	})
}

// RejectIntegration drops the pending change, with a reason for the maker.
func (s *Store) RejectIntegration(ctx context.Context, id, actor, note string) error {
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		i, err := lockPendingIntegration(ctx, tx, id)
		if err != nil {
			return err
		}
		if i.PendingBy == actor {
			return ErrOwnRejection
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE integrations SET pending = NULL, pending_by = '', pending_at = NULL, pending_note = '',
				rejected_by = $2, rejected_at = now(), rejected_note = $3
			WHERE integration_id = $1`, id, actor, note); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "integration.reject", id, map[string]any{"proposed_by": i.PendingBy, "note": note})
	})
}

// WithdrawIntegration drops the pending change (the person who proposed it).
// A new integration that was never approved is removed.
func (s *Store) WithdrawIntegration(ctx context.Context, id, actor string) error {
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		i, err := lockPendingIntegration(ctx, tx, id)
		if err != nil {
			return err
		}
		if i.PendingBy != actor {
			return ErrNotSubmitter
		}
		if !i.IsLive {
			if _, err := tx.ExecContext(ctx, `DELETE FROM integrations WHERE integration_id = $1`, id); err != nil {
				return err
			}
		} else if err := clearPending(ctx, tx, id); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "integration.withdraw", id, map[string]any{})
	})
}

// SetIntegrationEnabled switches an integration on or off at once, live and
// pending alike: switching off is how to stop a misbehaving server now.
func (s *Store) SetIntegrationEnabled(ctx context.Context, id string, on bool, actor string) error {
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE integrations SET enabled = $2,
				pending = CASE WHEN pending IS NULL THEN NULL ELSE jsonb_set(pending, '{enabled}', to_jsonb($2::boolean)) END,
				updated_at = now(), updated_by = $3
			WHERE integration_id = $1`, id, on, actor)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, actor, "integration.enabled", id, map[string]any{"enabled": on})
	})
}

func (s *Store) DeleteIntegration(ctx context.Context, id, actor string) error {
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM integrations WHERE integration_id = $1`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, actor, "integration.delete", id, map[string]any{})
	})
}

// IntegrationUsage lists the flows whose draft, latest version or a pinned
// version uses a tool of this integration.
func (s *Store) IntegrationUsage(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT flow_id FROM (
			SELECT flow_id, spec FROM flow_drafts
			UNION ALL
			SELECT v.flow_id, v.spec FROM flow_versions v
			WHERE v.version = (SELECT max(version) FROM flow_versions w WHERE w.flow_id = v.flow_id)
			   OR (v.flow_id, v.version) IN (SELECT flow_id, version_pin FROM phone_numbers WHERE version_pin IS NOT NULL)
		) specs
		WHERE spec::text LIKE '%"' || $1 || '.%'
		ORDER BY flow_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var flowID string
		if err := rows.Scan(&flowID); err != nil {
			return nil, err
		}
		out = append(out, flowID)
	}
	return out, rows.Err()
}
