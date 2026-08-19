package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const reviewMemoryCandidateLimit = 100

func (s *Store) ListReviewMemories(ctx context.Context, gitLabInstance string, projectID int64) ([]ReviewMemory, error) {
	if gitLabInstance == "" || projectID <= 0 {
		return nil, errors.New("invalid review memory scope")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT m.memory_id, m.lesson, m.source_url, m.created_at
FROM review_memories m
JOIN feedback_jobs j ON j.id = m.feedback_job_id
WHERE j.gitlab_instance = ? AND j.project_id = ?
ORDER BY m.created_at DESC, m.memory_id
LIMIT ?`, gitLabInstance, projectID, reviewMemoryCandidateLimit)
	if err != nil {
		return nil, fmt.Errorf("query review memories: %w", err)
	}
	defer rows.Close()

	memories := make([]ReviewMemory, 0)
	for rows.Next() {
		var memory ReviewMemory
		var createdAt string
		if err := rows.Scan(&memory.MemoryID, &memory.Lesson, &memory.SourceURL, &createdAt); err != nil {
			return nil, fmt.Errorf("scan active review memory: %w", err)
		}
		parsed, err := time.Parse(timestampLayout, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse active review memory timestamp: %w", err)
		}
		memory.UpdatedAt = parsed
		memories = append(memories, memory)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review memories: %w", err)
	}
	return memories, nil
}
