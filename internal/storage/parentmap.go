package storage

import (
	"context"
	"fmt"
)

// ClaimSessionForParent maps parentKey to a session id, returning the session
// the caller should join.
//
// If no mapping exists, candidateID is claimed and returned. If one already
// exists, the previously claimed id is returned instead, so concurrent runs
// under the same parent converge on a single session. The insert is atomic, so
// the race between "check" and "claim" is resolved by SQLite rather than by
// application-level locking.
//
// A mapping pointing at a session that no longer exists (removed by gc) is
// replaced by candidateID rather than resurrecting the dead id.
func (s *Store) ClaimSessionForParent(ctx context.Context, parentKey, candidateID string) (string, error) {
	if parentKey == "" {
		return "", fmt.Errorf("empty parent key")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("claim parent session: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Drop a mapping whose session is gone so the claim below can take it.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM parent_sessions
		WHERE parent_key = ?
		  AND session_id NOT IN (SELECT id FROM sessions)`, parentKey); err != nil {
		return "", fmt.Errorf("claim parent session: prune stale mapping: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO parent_sessions(parent_key, session_id)
		VALUES(?, ?)
		ON CONFLICT(parent_key) DO NOTHING`, parentKey, candidateID); err != nil {
		return "", fmt.Errorf("claim parent session: insert: %w", err)
	}

	var claimed string
	if err := tx.QueryRowContext(ctx,
		`SELECT session_id FROM parent_sessions WHERE parent_key = ?`, parentKey).Scan(&claimed); err != nil {
		return "", fmt.Errorf("claim parent session: read back: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("claim parent session: commit: %w", err)
	}
	return claimed, nil
}
