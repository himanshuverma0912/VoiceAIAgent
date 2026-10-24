// Package store keeps flows, their published versions and the phone-number
// mapping in Postgres.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/lib/pq"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrNoDraft      = errors.New("this flow has no draft")
	ErrNotPublished = errors.New("this flow has no published version yet")
)

// InvalidError is returned by Publish when the draft has problems.
type InvalidError struct{ Problems []string }

func (e *InvalidError) Error() string { return "the draft has problems" }

type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Checksum is the sha256 (hex) of a flow's JSON text.
func Checksum(spec []byte) string {
	sum := sha256.Sum256(spec)
	return hex.EncodeToString(sum[:])
}

func audit(ctx context.Context, tx *sql.Tx, actor, action, target string, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO audit_log (actor, action, target, detail) VALUES ($1, $2, $3, $4)`,
		actor, action, target, string(b))
	return err
}

func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	return tx.Commit()
}

// ── Flows and drafts ──────────────────────────────────────────────────────────

type FlowSummary struct {
	FlowID         string     `json:"flow_id"`
	Name           string     `json:"name"`
	LatestVersion  *int       `json:"latest_version"`
	DraftUpdatedAt *time.Time `json:"draft_updated_at"`
	DraftValid     *bool      `json:"draft_valid"`
	Numbers        []string   `json:"numbers"`
}

type Draft struct {
	Spec      json.RawMessage `json:"spec"`
	Problems  []string        `json:"problems"`
	UpdatedAt time.Time       `json:"updated_at"`
	UpdatedBy string          `json:"updated_by"`
}

// SaveDraft stores a draft (valid or not), creating the flow on first save.
func (s *Store) SaveDraft(ctx context.Context, flowID, name string, spec []byte, problems []string, actor string) error {
	if problems == nil {
		problems = []string{}
	}
	p, _ := json.Marshal(problems)
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO flows (flow_id, name, created_by) VALUES ($1, $2, $3) ON CONFLICT (flow_id) DO NOTHING`,
			flowID, name, actor)
		if err != nil {
			return err
		}
		if created, _ := res.RowsAffected(); created == 1 {
			if err := audit(ctx, tx, actor, "flow.create", flowID, map[string]any{"name": name}); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE flows SET name = $2 WHERE flow_id = $1`, flowID, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO flow_drafts (flow_id, spec, problems, updated_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (flow_id) DO UPDATE
			SET spec = EXCLUDED.spec, problems = EXCLUDED.problems, updated_at = now(), updated_by = EXCLUDED.updated_by`,
			flowID, string(spec), string(p), actor); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "draft.save", flowID, map[string]any{
			"valid": len(problems) == 0, "problems": len(problems), "checksum": Checksum(spec),
		})
	})
}

func (s *Store) GetDraft(ctx context.Context, flowID string) (*Draft, error) {
	var d Draft
	var spec, problems string
	err := s.db.QueryRowContext(ctx,
		`SELECT spec, problems, updated_at, updated_by FROM flow_drafts WHERE flow_id = $1`, flowID,
	).Scan(&spec, &problems, &d.UpdatedAt, &d.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Spec = json.RawMessage(spec)
	if err := json.Unmarshal([]byte(problems), &d.Problems); err != nil {
		return nil, err
	}
	return &d, nil
}

const flowSummarySQL = `
	SELECT f.flow_id, f.name,
	       (SELECT max(version) FROM flow_versions v WHERE v.flow_id = f.flow_id),
	       d.updated_at,
	       CASE WHEN d.flow_id IS NULL THEN NULL ELSE json_array_length(d.problems) = 0 END,
	       COALESCE((SELECT array_agg(number ORDER BY number) FROM phone_numbers p WHERE p.flow_id = f.flow_id), '{}')
	FROM flows f LEFT JOIN flow_drafts d ON d.flow_id = f.flow_id`

func scanSummary(row interface{ Scan(...any) error }) (FlowSummary, error) {
	var f FlowSummary
	var latest sql.NullInt64
	var updated sql.NullTime
	var valid sql.NullBool
	var numbers pq.StringArray
	if err := row.Scan(&f.FlowID, &f.Name, &latest, &updated, &valid, &numbers); err != nil {
		return f, err
	}
	if latest.Valid {
		v := int(latest.Int64)
		f.LatestVersion = &v
	}
	if updated.Valid {
		f.DraftUpdatedAt = &updated.Time
	}
	if valid.Valid {
		f.DraftValid = &valid.Bool
	}
	f.Numbers = []string(numbers)
	return f, nil
}

func (s *Store) ListFlows(ctx context.Context) ([]FlowSummary, error) {
	rows, err := s.db.QueryContext(ctx, flowSummarySQL+` ORDER BY f.flow_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FlowSummary{}
	for rows.Next() {
		f, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) GetFlow(ctx context.Context, flowID string) (*FlowSummary, error) {
	f, err := scanSummary(s.db.QueryRowContext(ctx, flowSummarySQL+` WHERE f.flow_id = $1`, flowID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// ── Publishing ────────────────────────────────────────────────────────────────

type Version struct {
	FlowID      string          `json:"flow_id"`
	Version     int             `json:"version"`
	Checksum    string          `json:"checksum"`
	PublishedAt time.Time       `json:"published_at"`
	PublishedBy string          `json:"published_by"`
	Spec        json.RawMessage `json:"spec,omitempty"`
}

// Publish turns the current draft into a new version. validate re-checks the
// draft (the rules may be newer than when it was saved); an invalid draft
// returns *InvalidError. If the draft equals the latest version, that version
// is returned with unchanged = true and nothing is written.
func (s *Store) Publish(ctx context.Context, flowID, actor string, validate func([]byte) []string) (v *Version, unchanged bool, err error) {
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		var spec string
		// FOR UPDATE: two publishes of the same flow at once get different numbers.
		err := tx.QueryRowContext(ctx,
			`SELECT d.spec FROM flows f LEFT JOIN flow_drafts d USING (flow_id) WHERE f.flow_id = $1 FOR UPDATE OF f`,
			flowID).Scan(&sqlNullString{&spec})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if spec == "" {
			return ErrNoDraft
		}
		if problems := validate([]byte(spec)); len(problems) > 0 {
			return &InvalidError{Problems: problems}
		}
		checksum := Checksum([]byte(spec))
		latest := &Version{FlowID: flowID}
		err = tx.QueryRowContext(ctx, `
			SELECT version, checksum, published_at, published_by FROM flow_versions
			WHERE flow_id = $1 ORDER BY version DESC LIMIT 1`, flowID,
		).Scan(&latest.Version, &latest.Checksum, &latest.PublishedAt, &latest.PublishedBy)
		switch {
		case err == nil && latest.Checksum == checksum:
			v, unchanged = latest, true
			return nil
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		}
		v = &Version{FlowID: flowID, Version: latest.Version + 1, Checksum: checksum, PublishedBy: actor}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO flow_versions (flow_id, version, spec, checksum, published_by)
			VALUES ($1, $2, $3, $4, $5) RETURNING published_at`,
			flowID, v.Version, spec, checksum, actor).Scan(&v.PublishedAt); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "flow.publish", flowID, map[string]any{"version": v.Version, "checksum": checksum})
	})
	return v, unchanged, err
}

// sqlNullString scans a nullable text column into a string ("" for NULL).
type sqlNullString struct{ s *string }

func (n *sqlNullString) Scan(src any) error {
	switch t := src.(type) {
	case nil:
		*n.s = ""
	case []byte:
		*n.s = string(t)
	case string:
		*n.s = t
	default:
		return errors.New("unexpected type for text column")
	}
	return nil
}

func (s *Store) ListVersions(ctx context.Context, flowID string) ([]Version, error) {
	if _, err := s.GetFlow(ctx, flowID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT version, checksum, published_at, published_by FROM flow_versions
		WHERE flow_id = $1 ORDER BY version DESC`, flowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		v := Version{FlowID: flowID}
		if err := rows.Scan(&v.Version, &v.Checksum, &v.PublishedAt, &v.PublishedBy); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) GetVersion(ctx context.Context, flowID string, version int) (*Version, error) {
	v := Version{FlowID: flowID, Version: version}
	var spec string
	err := s.db.QueryRowContext(ctx, `
		SELECT checksum, published_at, published_by, spec FROM flow_versions
		WHERE flow_id = $1 AND version = $2`, flowID, version,
	).Scan(&v.Checksum, &v.PublishedAt, &v.PublishedBy, &spec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.Spec = json.RawMessage(spec)
	return &v, nil
}

// ── Phone numbers ─────────────────────────────────────────────────────────────

type Number struct {
	Number     string    `json:"number"`
	FlowID     string    `json:"flow_id"`
	VersionPin *int      `json:"version_pin"`
	UpdatedAt  time.Time `json:"updated_at"`
	UpdatedBy  string    `json:"updated_by"`
}

// PutNumber points a phone number at a flow. pin = nil follows the latest
// published version; otherwise calls get exactly that version (e.g. a rollback).
func (s *Store) PutNumber(ctx context.Context, number, flowID string, pin *int, actor string) (*Number, error) {
	n := &Number{Number: number, FlowID: flowID, VersionPin: pin, UpdatedBy: actor}
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		var latest sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT (SELECT max(version) FROM flow_versions WHERE flow_id = $1) FROM flows WHERE flow_id = $1`,
			flowID).Scan(&latest)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !latest.Valid {
			return ErrNotPublished
		}
		if pin != nil && (*pin < 1 || int64(*pin) > latest.Int64) {
			return ErrNotFound
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO phone_numbers (number, flow_id, version_pin, updated_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (number) DO UPDATE
			SET flow_id = EXCLUDED.flow_id, version_pin = EXCLUDED.version_pin,
			    updated_at = now(), updated_by = EXCLUDED.updated_by
			RETURNING updated_at`, number, flowID, pin, actor).Scan(&n.UpdatedAt); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "number.map", number, map[string]any{"flow_id": flowID, "version_pin": pin})
	})
	if err != nil {
		return nil, err
	}
	return n, nil
}

func (s *Store) DeleteNumber(ctx context.Context, number, actor string) error {
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM phone_numbers WHERE number = $1`, number)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, actor, "number.unmap", number, map[string]any{})
	})
}

func (s *Store) ListNumbers(ctx context.Context) ([]Number, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT number, flow_id, version_pin, updated_at, updated_by FROM phone_numbers ORDER BY number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Number{}
	for rows.Next() {
		var n Number
		var pin sql.NullInt64
		if err := rows.Scan(&n.Number, &n.FlowID, &pin, &n.UpdatedAt, &n.UpdatedBy); err != nil {
			return nil, err
		}
		if pin.Valid {
			p := int(pin.Int64)
			n.VersionPin = &p
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Resolved is what a call needs: the flow version a number runs.
type Resolved struct {
	Number   string          `json:"number"`
	FlowID   string          `json:"flow_id"`
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	Spec     json.RawMessage `json:"spec"`
}

// Resolve returns the flow version a phone number runs right now.
func (s *Store) Resolve(ctx context.Context, number string) (*Resolved, error) {
	r := Resolved{Number: number}
	var spec string
	err := s.db.QueryRowContext(ctx, `
		SELECT p.flow_id, v.version, v.checksum, v.spec
		FROM phone_numbers p
		JOIN flow_versions v ON v.flow_id = p.flow_id
		 AND v.version = COALESCE(p.version_pin,
		                          (SELECT max(version) FROM flow_versions WHERE flow_id = p.flow_id))
		WHERE p.number = $1`, number,
	).Scan(&r.FlowID, &r.Version, &r.Checksum, &spec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Spec = json.RawMessage(spec)
	return &r, nil
}

// ── Audit log ─────────────────────────────────────────────────────────────────

type AuditEntry struct {
	ID     int64           `json:"id"`
	At     time.Time       `json:"at"`
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail"`
}

func (s *Store) Audit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, actor, action, target, detail FROM audit_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var detail string
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Target, &detail); err != nil {
			return nil, err
		}
		e.Detail = json.RawMessage(detail)
		out = append(out, e)
	}
	return out, rows.Err()
}
