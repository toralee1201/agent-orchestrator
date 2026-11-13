package store

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// RecordReviewTestPass remembers that unit tests passed for one worker PR head.
// Recording the same head again refreshes the timestamp.
func (s *Store) RecordReviewTestPass(ctx context.Context, sessionID domain.SessionID, sha string, at time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.writeDB.ExecContext(ctx, `
INSERT INTO review_test_passes (session_id, target_sha, passed_at)
VALUES (?, ?, ?)
ON CONFLICT(session_id, target_sha) DO UPDATE SET passed_at = excluded.passed_at`,
		string(sessionID), sha, at.UTC())
	return err
}

// HasReviewTestPass reports whether this worker recorded a passing unit-test
// run for the PR head.
func (s *Store) HasReviewTestPass(ctx context.Context, sessionID domain.SessionID, sha string) (bool, error) {
	var n int
	err := s.readDB.QueryRowContext(ctx, `
SELECT COUNT(*) FROM review_test_passes WHERE session_id = ? AND target_sha = ?`,
		string(sessionID), sha).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
