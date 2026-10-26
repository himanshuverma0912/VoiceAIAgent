package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"
)

// ── Model catalog ─────────────────────────────────────────────────────────────
//
// The LLMs, speech-to-text models and voices flows may use (see
// migrations/005_model_catalog.sql). Platform admins manage it.

// CatalogKinds maps each kind to the flow setting that names it.
var CatalogKinds = map[string]string{
	"llm":       "llm_model",
	"stt":       "stt_model",
	"tts_voice": "tts_voice_id",
}

type CatalogEntry struct {
	Kind      string    `json:"kind"`
	ModelID   string    `json:"model_id"`
	Label     string    `json:"label"`
	Notes     string    `json:"notes"`
	Languages []string  `json:"languages"` // empty: any language
	Teams     []string  `json:"teams"`     // empty: every team
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

// AllowedFor reports whether a flow of team may use this entry.
func (e CatalogEntry) AllowedFor(team string) bool {
	if !e.Enabled {
		return false
	}
	if len(e.Teams) == 0 {
		return true
	}
	for _, t := range e.Teams {
		if t == team {
			return true
		}
	}
	return false
}

func (s *Store) ListCatalog(ctx context.Context) ([]CatalogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, model_id, label, notes, languages, teams, enabled, updated_at, updated_by
		FROM model_catalog ORDER BY kind, label, model_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogEntry{}
	for rows.Next() {
		var e CatalogEntry
		var langs, teams pq.StringArray
		if err := rows.Scan(&e.Kind, &e.ModelID, &e.Label, &e.Notes, &langs, &teams, &e.Enabled, &e.UpdatedAt, &e.UpdatedBy); err != nil {
			return nil, err
		}
		e.Languages, e.Teams = append([]string{}, langs...), append([]string{}, teams...)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PutCatalogEntry adds or changes an entry; created says which.
func (s *Store) PutCatalogEntry(ctx context.Context, e CatalogEntry, actor string) (created bool, err error) {
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		var inserted bool
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO model_catalog (kind, model_id, label, notes, languages, teams, enabled, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (kind, model_id) DO UPDATE SET
				label = EXCLUDED.label, notes = EXCLUDED.notes, languages = EXCLUDED.languages,
				teams = EXCLUDED.teams, enabled = EXCLUDED.enabled, updated_at = now(), updated_by = EXCLUDED.updated_by
			RETURNING (xmax = 0)`,
			e.Kind, e.ModelID, e.Label, e.Notes, pq.StringArray(e.Languages), pq.StringArray(e.Teams), e.Enabled, actor,
		).Scan(&inserted); err != nil {
			return err
		}
		created = inserted
		action := "catalog.update"
		if inserted {
			action = "catalog.add"
		}
		return audit(ctx, tx, actor, action, e.Kind+"/"+e.ModelID, map[string]any{
			"label": e.Label, "enabled": e.Enabled, "teams": e.Teams, "languages": e.Languages,
		})
	})
	return created, err
}

// CatalogUsage lists the flows whose draft, latest version or a pinned version
// names this model.
func (s *Store) CatalogUsage(ctx context.Context, kind, modelID string) ([]string, error) {
	setting, ok := CatalogKinds[kind]
	if !ok {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT flow_id FROM (
			SELECT flow_id, spec FROM flow_drafts
			UNION ALL
			SELECT v.flow_id, v.spec FROM flow_versions v
			WHERE v.version = (SELECT max(version) FROM flow_versions w WHERE w.flow_id = v.flow_id)
			   OR (v.flow_id, v.version) IN (SELECT flow_id, version_pin FROM phone_numbers WHERE version_pin IS NOT NULL)
		) specs
		WHERE spec->'settings'->>$1 = $2
		ORDER BY flow_id`, setting, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) DeleteCatalogEntry(ctx context.Context, kind, modelID, actor string) error {
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM model_catalog WHERE kind = $1 AND model_id = $2`, kind, modelID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, actor, "catalog.delete", kind+"/"+modelID, map[string]any{})
	})
}

// FlowTeam is the team a flow belongs to ("" if unassigned or not found).
func (s *Store) FlowTeam(ctx context.Context, flowID string) (string, error) {
	f, err := s.GetFlow(ctx, flowID)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return f.Team, nil
}
