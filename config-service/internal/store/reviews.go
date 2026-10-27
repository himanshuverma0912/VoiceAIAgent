package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/lib/pq"
)

// ── Maker-checker reviews ─────────────────────────────────────────────────────
//
// See migrations/007_reviews.sql. A maker submits the current draft (a copy is
// kept); a checker who isn't the maker approves it (publishing exactly that
// copy) or rejects it; the maker can withdraw it while it's open.

var (
	ErrUnchangedDraft = errors.New("nothing to review: the draft is the same as the live version")
	ErrNotPending     = errors.New("this review is already closed")
	ErrSelfApproval   = errors.New("you submitted this change, so someone else has to approve it")
	ErrNotSubmitter   = errors.New("only the person who submitted it can withdraw it")
	ErrOwnRejection   = errors.New("you submitted this change; withdraw it instead")
)

type Review struct {
	ReviewID     int64           `json:"review_id"`
	FlowID       string          `json:"flow_id"`
	FlowName     string          `json:"flow_name"`
	Workspace    string          `json:"workspace"`
	Checksum     string          `json:"checksum"`
	BaseVersion  *int            `json:"base_version"`
	Note         string          `json:"note"`
	SubmittedBy  string          `json:"submitted_by"`
	SubmittedAt  time.Time       `json:"submitted_at"`
	Status       string          `json:"status"`
	DecidedBy    string          `json:"decided_by"`
	DecidedAt    *time.Time      `json:"decided_at"`
	DecisionNote string          `json:"decision_note"`
	Version      *int            `json:"version"`
	Spec         json.RawMessage `json:"spec,omitempty"`
}

const reviewSQL = `
	SELECT r.review_id, r.flow_id, f.name, COALESCE(f.workspace, ''), r.checksum, r.base_version, r.note,
	       r.submitted_by, r.submitted_at, r.status, COALESCE(r.decided_by, ''), r.decided_at,
	       r.decision_note, r.version`

func scanReview(row interface{ Scan(...any) error }, withSpec bool) (Review, error) {
	var r Review
	var base, version sql.NullInt64
	var decided sql.NullTime
	var spec string
	dest := []any{&r.ReviewID, &r.FlowID, &r.FlowName, &r.Workspace, &r.Checksum, &base, &r.Note,
		&r.SubmittedBy, &r.SubmittedAt, &r.Status, &r.DecidedBy, &decided, &r.DecisionNote, &version}
	if withSpec {
		dest = append(dest, &spec)
	}
	if err := row.Scan(dest...); err != nil {
		return r, err
	}
	if base.Valid {
		b := int(base.Int64)
		r.BaseVersion = &b
	}
	if version.Valid {
		v := int(version.Int64)
		r.Version = &v
	}
	if decided.Valid {
		r.DecidedAt = &decided.Time
	}
	if withSpec {
		r.Spec = json.RawMessage(spec)
	}
	return r, nil
}

// SubmitReview copies the flow's current draft into a new open review. An
// older open review of the same flow is closed as superseded. validate
// re-checks the draft; one with problems can't be submitted.
func (s *Store) SubmitReview(ctx context.Context, flowID, note, actor string, validate func([]byte) []string) (*Review, error) {
	var id int64
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		spec, err := lockDraft(ctx, tx, flowID)
		if err != nil {
			return err
		}
		if problems := validate([]byte(spec)); len(problems) > 0 {
			return &InvalidError{Problems: problems}
		}
		checksum := Checksum([]byte(spec))
		latest, err := latestVersion(ctx, tx, flowID)
		if err != nil {
			return err
		}
		var base sql.NullInt64
		if latest != nil {
			if latest.Checksum == checksum {
				return ErrUnchangedDraft
			}
			base = sql.NullInt64{Int64: int64(latest.Version), Valid: true}
		}
		var superseded []int64
		rows, err := tx.QueryContext(ctx, `
			UPDATE flow_reviews SET status = 'superseded', decided_at = now(), decided_by = $2
			WHERE flow_id = $1 AND status = 'pending' RETURNING review_id`, flowID, actor)
		if err != nil {
			return err
		}
		for rows.Next() {
			var old int64
			if err := rows.Scan(&old); err != nil {
				rows.Close()
				return err
			}
			superseded = append(superseded, old)
		}
		rows.Close()
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO flow_reviews (flow_id, spec, checksum, base_version, note, submitted_by)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING review_id`,
			flowID, spec, checksum, base, note, actor).Scan(&id); err != nil {
			return err
		}
		detail := map[string]any{"review_id": id, "checksum": checksum}
		if len(superseded) > 0 {
			detail["superseded"] = superseded
		}
		return audit(ctx, tx, actor, "review.submit", flowID, detail)
	})
	if err != nil {
		return nil, err
	}
	return s.GetReview(ctx, flowID, id)
}

// GetReview returns one review of a flow, with the submitted flow.
func (s *Store) GetReview(ctx context.Context, flowID string, id int64) (*Review, error) {
	r, err := scanReview(s.db.QueryRowContext(ctx, reviewSQL+`, r.spec
		FROM flow_reviews r JOIN flows f USING (flow_id) WHERE r.flow_id = $1 AND r.review_id = $2`, flowID, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) listReviews(ctx context.Context, where string, args ...any) ([]Review, error) {
	rows, err := s.db.QueryContext(ctx, reviewSQL+` FROM flow_reviews r JOIN flows f USING (flow_id) `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Review{}
	for rows.Next() {
		r, err := scanReview(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListReviews returns a flow's reviews, newest first (without the flows).
func (s *Store) ListReviews(ctx context.Context, flowID string, limit int) ([]Review, error) {
	return s.listReviews(ctx, `WHERE r.flow_id = $1 ORDER BY r.review_id DESC LIMIT $2`, flowID, limit)
}

// PendingReviews returns the open reviews in the given workspaces (all if nil),
// oldest first.
func (s *Store) PendingReviews(ctx context.Context, workspaces []string) ([]Review, error) {
	if workspaces == nil {
		return s.listReviews(ctx, `WHERE r.status = 'pending' ORDER BY r.submitted_at`)
	}
	return s.listReviews(ctx, `WHERE r.status = 'pending' AND f.workspace = ANY($1) ORDER BY r.submitted_at`, pq.StringArray(workspaces))
}

// lockPending locks an open review for a decision.
func lockPending(ctx context.Context, tx *sql.Tx, flowID string, id int64) (spec, submittedBy, status string, err error) {
	// Lock the flow first (same order as publishing), then the review.
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM flows WHERE flow_id = $1 FOR UPDATE`, flowID); err != nil {
		return
	}
	err = tx.QueryRowContext(ctx, `
		SELECT spec, submitted_by, status FROM flow_reviews WHERE flow_id = $1 AND review_id = $2 FOR UPDATE`,
		flowID, id).Scan(&spec, &submittedBy, &status)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err == nil && status != "pending" {
		err = ErrNotPending
	}
	return
}

// ApproveReview publishes exactly the submitted flow and closes the review.
// The checker can't be the person who submitted it. validate re-checks the
// submitted flow (rules or the model catalog may have changed since).
func (s *Store) ApproveReview(ctx context.Context, flowID string, id int64, actor, note string, validate func([]byte) []string) (v *Version, err error) {
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		spec, submittedBy, _, err := lockPending(ctx, tx, flowID, id)
		if err != nil {
			return err
		}
		if submittedBy == actor {
			return ErrSelfApproval
		}
		if problems := validate([]byte(spec)); len(problems) > 0 {
			return &InvalidError{Problems: problems}
		}
		if v, _, err = publishSpec(ctx, tx, flowID, spec, actor, submittedBy, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE flow_reviews SET status = 'approved', decided_by = $3, decided_at = now(), decision_note = $4, version = $5
			WHERE flow_id = $1 AND review_id = $2`, flowID, id, actor, note, v.Version); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "review.approve", flowID, map[string]any{"review_id": id, "version": v.Version, "submitted_by": submittedBy})
	})
	return v, err
}

// RejectReview closes the review with the checker's reason. Nothing is published.
func (s *Store) RejectReview(ctx context.Context, flowID string, id int64, actor, note string) error {
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		_, submittedBy, _, err := lockPending(ctx, tx, flowID, id)
		if err != nil {
			return err
		}
		if submittedBy == actor {
			return ErrOwnRejection
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE flow_reviews SET status = 'rejected', decided_by = $3, decided_at = now(), decision_note = $4
			WHERE flow_id = $1 AND review_id = $2`, flowID, id, actor, note); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "review.reject", flowID, map[string]any{"review_id": id, "submitted_by": submittedBy})
	})
}

// WithdrawReview lets the maker take back their open submission (admins may too).
func (s *Store) WithdrawReview(ctx context.Context, flowID string, id int64, actor string, admin bool) error {
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		_, submittedBy, _, err := lockPending(ctx, tx, flowID, id)
		if err != nil {
			return err
		}
		if submittedBy != actor && !admin {
			return ErrNotSubmitter
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE flow_reviews SET status = 'withdrawn', decided_by = $3, decided_at = now()
			WHERE flow_id = $1 AND review_id = $2`, flowID, id, actor); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "review.withdraw", flowID, map[string]any{"review_id": id})
	})
}
