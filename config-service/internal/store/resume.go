package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ── Drop-resume points ────────────────────────────────────────────────────────
//
// See migrations/009_resume.sql. The agent seals each point; this service only
// keeps it until it's claimed or it expires.

// PutResume keeps (or replaces) the point filed under key until expires.
func (s *Store) PutResume(ctx context.Context, key, flowID string, version *int, sealed string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO resume_points (key, flow_id, flow_version, sealed, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (key) DO UPDATE SET flow_id = $2, flow_version = $3, sealed = $4, expires_at = $5`,
		key, flowID, version, sealed, expires)
	return err
}

// ClaimResume returns the point filed under key for flowID and deletes it, in
// one step, so it can be used once. A point that has expired, or that belongs
// to another flow, isn't returned (found is false).
func (s *Store) ClaimResume(ctx context.Context, key, flowID string, now time.Time) (sealed string, version *int, found bool, err error) {
	var v sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
		DELETE FROM resume_points
		WHERE key = $1 AND flow_id = $2 AND expires_at > $3
		RETURNING sealed, flow_version`, key, flowID, now).Scan(&sealed, &v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	if v.Valid {
		n := int(v.Int64)
		version = &n
	}
	return sealed, version, true, nil
}

// PurgeResume deletes expired points and says how many.
func (s *Store) PurgeResume(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM resume_points WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
