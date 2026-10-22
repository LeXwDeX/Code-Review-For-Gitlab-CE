package sqlite

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// retentionBatch bounds one DELETE statement so a large cleanup never holds
// the single writer lock for the whole table.
const retentionBatch = 500

// DeleteLLMCallsBefore deletes whole audit rows older than the cutoff. Expired
// rows are removed in bounded batches until none remain.
func (s *Store) DeleteLLMCallsBefore(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("sqlite: retention cutoff is required")
	}
	cutoff := formatTime(before.UTC())
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM llm_calls
			WHERE id IN (
				SELECT id FROM llm_calls WHERE created_at < ?
				ORDER BY created_at, id LIMIT ?
			)`, cutoff, retentionBatch)
		if err != nil {
			return total, fmt.Errorf("sqlite: delete llm calls: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("sqlite: delete llm calls: %w", err)
		}
		total += n
		if n < retentionBatch {
			return total, nil
		}
	}
}

// DeleteDeliveriesBefore deletes webhook delivery records received before the
// cutoff in bounded batches.
func (s *Store) DeleteDeliveriesBefore(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("sqlite: retention cutoff is required")
	}
	cutoff := formatTime(before.UTC())
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM webhook_deliveries
			WHERE rowid IN (
				SELECT rowid FROM webhook_deliveries WHERE received_at < ?
				ORDER BY received_at, rowid LIMIT ?
			)`, cutoff, retentionBatch)
		if err != nil {
			return total, fmt.Errorf("sqlite: delete webhook deliveries: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("sqlite: delete webhook deliveries: %w", err)
		}
		total += n
		if n < retentionBatch {
			return total, nil
		}
	}
}

// DeleteTerminalJobsBefore deletes done, failed, and cancelled jobs last
// updated before the cutoff. Queued and running jobs are never deleted.
func (s *Store) DeleteTerminalJobsBefore(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("sqlite: retention cutoff is required")
	}
	cutoff := formatTime(before.UTC())
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM jobs
			WHERE id IN (
				SELECT id FROM jobs
				WHERE state IN ('done', 'failed', 'cancelled') AND updated_at < ?
				ORDER BY updated_at, id LIMIT ?
			)`, cutoff, retentionBatch)
		if err != nil {
			return total, fmt.Errorf("sqlite: delete terminal jobs: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("sqlite: delete terminal jobs: %w", err)
		}
		total += n
		if n < retentionBatch {
			return total, nil
		}
	}
}

// PruneContextDocs keeps the newest keep unlocked context versions for each
// (project_id, kind) pair and deletes older unlocked versions in bounded
// batches. Locked rows are never deleted and do not consume a retained slot.
// The project's active version (workspaces.context_sha) is never deleted,
// whatever its kind.
func (s *Store) PruneContextDocs(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		return 0, errors.New("sqlite: context versions to keep must be positive")
	}
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM context_docs
			WHERE locked = 0 AND id IN (
				SELECT id FROM (
					SELECT id, project_id, base_sha, ROW_NUMBER() OVER (
						PARTITION BY project_id, kind
						ORDER BY created_at DESC, id DESC
					) AS rn
					FROM context_docs
					WHERE locked = 0
				) AS ranked
				WHERE ranked.rn > ?
					AND NOT EXISTS (
						SELECT 1 FROM workspaces w
						WHERE w.project_id = ranked.project_id
							AND w.context_sha = ranked.base_sha
							AND w.context_sha <> ''
					)
				ORDER BY ranked.rn DESC, ranked.id
				LIMIT ?
			)`, keep, retentionBatch)
		if err != nil {
			return total, fmt.Errorf("sqlite: prune context documents: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("sqlite: prune context documents: %w", err)
		}
		total += n
		if n < retentionBatch {
			return total, nil
		}
	}
}
