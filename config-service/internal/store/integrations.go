package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/lib/pq"
)

// ── Integrations ──────────────────────────────────────────────────────────────
//
// The systems flows may call (see migrations/008_integrations.sql). Platform
// admins manage them; the API checks every field before it gets here.

type Integration struct {
	IntegrationID string            `json:"integration_id"`
	Kind          string            `json:"kind"` // mcp | http
	Label         string            `json:"label"`
	Notes         string            `json:"notes"`
	URL           string            `json:"url,omitempty"`
	TestURL       string            `json:"test_url,omitempty"`
	AuthHeader    string            `json:"auth_header,omitempty"`
	AuthEnv       string            `json:"auth_env,omitempty"`
	TimeoutMs     int               `json:"timeout_ms,omitempty"`
	Workspaces    []string          `json:"workspaces"` // empty: every workspace
	Enabled       bool              `json:"enabled"`
	Tools         []IntegrationTool `json:"tools"`
	UpdatedAt     time.Time         `json:"updated_at"`
	UpdatedBy     string            `json:"updated_by"`
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

// Tool returns the tool called name, or nil.
func (i *Integration) Tool(name string) *IntegrationTool {
	for k := range i.Tools {
		if i.Tools[k].Name == name {
			return &i.Tools[k]
		}
	}
	return nil
}

// AllowedFor reports whether a flow of workspace may use this integration.
func (i *Integration) AllowedFor(workspace string) bool {
	if !i.Enabled {
		return false
	}
	if len(i.Workspaces) == 0 {
		return true
	}
	for _, w := range i.Workspaces {
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

func (s *Store) ListIntegrations(ctx context.Context) ([]Integration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT integration_id, kind, label, notes, url, test_url, auth_header, auth_env, timeout_ms,
			workspaces, enabled, tools, updated_at, updated_by
		FROM integrations ORDER BY label, integration_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Integration{}
	for rows.Next() {
		var i Integration
		var wss pq.StringArray
		var tools []byte
		if err := rows.Scan(&i.IntegrationID, &i.Kind, &i.Label, &i.Notes, &i.URL, &i.TestURL, &i.AuthHeader,
			&i.AuthEnv, &i.TimeoutMs, &wss, &i.Enabled, &tools, &i.UpdatedAt, &i.UpdatedBy); err != nil {
			return nil, err
		}
		i.Workspaces = append([]string{}, wss...)
		if err := json.Unmarshal(tools, &i.Tools); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) GetIntegration(ctx context.Context, id string) (*Integration, error) {
	all, err := s.ListIntegrations(ctx)
	if err != nil {
		return nil, err
	}
	for k := range all {
		if all[k].IntegrationID == id {
			return &all[k], nil
		}
	}
	return nil, ErrNotFound
}

// PutIntegration adds or changes an integration; created says which.
func (s *Store) PutIntegration(ctx context.Context, i Integration, actor string) (created bool, err error) {
	tools, err := json.Marshal(i.Tools)
	if err != nil {
		return false, err
	}
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO integrations (integration_id, kind, label, notes, url, test_url, auth_header, auth_env,
				timeout_ms, workspaces, enabled, tools, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (integration_id) DO UPDATE SET
				kind = EXCLUDED.kind, label = EXCLUDED.label, notes = EXCLUDED.notes, url = EXCLUDED.url,
				test_url = EXCLUDED.test_url, auth_header = EXCLUDED.auth_header, auth_env = EXCLUDED.auth_env,
				timeout_ms = EXCLUDED.timeout_ms, workspaces = EXCLUDED.workspaces, enabled = EXCLUDED.enabled,
				tools = EXCLUDED.tools, updated_at = now(), updated_by = EXCLUDED.updated_by
			RETURNING (xmax = 0)`,
			i.IntegrationID, i.Kind, i.Label, i.Notes, i.URL, i.TestURL, i.AuthHeader, i.AuthEnv, i.TimeoutMs,
			pq.StringArray(i.Workspaces), i.Enabled, string(tools), actor,
		).Scan(&created); err != nil {
			return err
		}
		action := "integration.update"
		if created {
			action = "integration.add"
		}
		names := make([]string, len(i.Tools))
		for k, t := range i.Tools {
			names[k] = t.Name
		}
		// The URL is recorded (who pointed calls where); no secrets exist to record.
		return audit(ctx, tx, actor, action, i.IntegrationID, map[string]any{
			"kind": i.Kind, "url": i.URL, "test_url": i.TestURL, "enabled": i.Enabled,
			"workspaces": i.Workspaces, "tools": names,
		})
	})
	return created, err
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
