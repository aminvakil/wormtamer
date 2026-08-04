package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const reviewMemoryCandidateLimit = 100

func (s *Store) ListActiveReviewMemories(ctx context.Context, gitLabInstance string, projectID int64) ([]ReviewMemory, error) {
	if gitLabInstance == "" || projectID <= 0 {
		return nil, errors.New("invalid review memory scope")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT m.memory_id, m.target_type, m.target_id, m.outcome, m.confidence, m.lesson,
       e.actor_role, m.source_url, m.updated_at
FROM review_memories m
JOIN feedback_jobs j ON j.id = m.feedback_job_id
JOIN feedback_evaluations e ON e.job_id = j.id AND e.source_event_id = j.source_event_id
WHERE j.gitlab_instance = ? AND j.project_id = ?
  AND m.active = 1 AND m.lesson IS NOT NULL
ORDER BY m.updated_at DESC, m.memory_id
LIMIT ?`, gitLabInstance, projectID, reviewMemoryCandidateLimit)
	if err != nil {
		return nil, fmt.Errorf("query active review memories: %w", err)
	}
	defer rows.Close()

	memories := make([]ReviewMemory, 0)
	for rows.Next() {
		var memory ReviewMemory
		var updatedAt string
		if err := rows.Scan(&memory.MemoryID, &memory.TargetType, &memory.TargetID, &memory.Outcome,
			&memory.Confidence, &memory.Lesson, &memory.SourceRole, &memory.SourceURL, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan active review memory: %w", err)
		}
		parsed, err := time.Parse(timestampLayout, updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse active review memory timestamp: %w", err)
		}
		memory.UpdatedAt = parsed
		memories = append(memories, memory)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active review memories: %w", err)
	}
	return memories, nil
}
