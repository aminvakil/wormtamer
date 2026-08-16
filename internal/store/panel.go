package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aminvakil/wormtamer/internal/review"
)

const maxPanelPageSize = 100

var ErrReviewRecordNotFound = errors.New("review record not found")

type StateCount struct {
	State string
	Count int
}

type Dashboard struct {
	ReviewCounts         []StateCount
	FeedbackCounts       []StateCount
	OldestQueuedReview   *time.Time
	OldestQueuedFeedback *time.Time
	ActiveMemoryCount    int
	RecentReviews        []ReviewRecord
	RecentFeedback       []FeedbackRecord
}

type ReviewRecord struct {
	ID                int64
	GitLabInstance    string
	ProjectID         int64
	ProjectPath       string
	MergeRequestIID   int64
	HeadSHA           string
	Source            string
	State             string
	AttemptCount      int
	CreatedAt         time.Time
	StartedAt         *time.Time
	UpdatedAt         *time.Time
	LastErrorCategory string
	FindingCount      int
	HasResult         bool
	Published         bool
	ExternalOnly      bool
}

type ReviewRecordsPage struct {
	Records    []ReviewRecord
	NextBefore int64
}

type ReviewMemoryRetrievalRecord struct {
	MemoryID        string
	MemoryUpdatedAt time.Time
	RetrievedAt     time.Time
}

type ReviewRecordDetail struct {
	ReviewRecord
	ReviewID     string
	Result       *review.Result
	GitLabNoteID int64
	Retrievals   []ReviewMemoryRetrievalRecord
}

type FeedbackRecord struct {
	ID                int64
	ReviewJobID       int64
	ReviewID          string
	ProjectID         int64
	ProjectPath       string
	MergeRequestIID   int64
	NoteID            int64
	State             string
	AttemptCount      int
	ReceivedAt        time.Time
	UpdatedAt         time.Time
	LastErrorCategory string
	DecisionCount     int
	ActiveLessonCount int
}

type FeedbackRecordsPage struct {
	Records    []FeedbackRecord
	NextBefore int64
}

type MemoryRecord struct {
	RowID           int64
	MemoryID        string
	ProjectID       int64
	ProjectPath     string
	MergeRequestIID int64
	NoteID          int64
	TargetType      string
	TargetID        string
	Outcome         string
	Confidence      string
	Lesson          string
	Active          bool
	SourceRole      string
	SourceURL       string
	UpdatedAt       time.Time
}

type MemoryRecordsPage struct {
	Records    []MemoryRecord
	NextBefore int64
}

func (s *Store) ReadDashboard(ctx context.Context, recentLimit int) (Dashboard, error) {
	if recentLimit <= 0 || recentLimit > 20 {
		return Dashboard{}, errors.New("invalid dashboard recent limit")
	}
	var dashboard Dashboard
	var err error
	dashboard.ReviewCounts, err = s.readStateCounts(ctx, "review_jobs")
	if err != nil {
		return Dashboard{}, err
	}
	dashboard.FeedbackCounts, err = s.readStateCounts(ctx, "feedback_jobs")
	if err != nil {
		return Dashboard{}, err
	}
	dashboard.OldestQueuedReview, err = s.readOptionalTime(ctx,
		`SELECT MIN(created_at) FROM review_jobs WHERE state = ?`, JobQueued)
	if err != nil {
		return Dashboard{}, fmt.Errorf("read oldest queued review: %w", err)
	}
	dashboard.OldestQueuedFeedback, err = s.readOptionalTime(ctx,
		`SELECT MIN(updated_at) FROM feedback_jobs WHERE state = ?`, FeedbackQueued)
	if err != nil {
		return Dashboard{}, fmt.Errorf("read oldest queued feedback: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM review_memories WHERE active = 1 AND lesson IS NOT NULL`).Scan(&dashboard.ActiveMemoryCount); err != nil {
		return Dashboard{}, fmt.Errorf("count active review memories: %w", err)
	}
	reviews, err := s.ListReviewRecords(ctx, "", 0, recentLimit)
	if err != nil {
		return Dashboard{}, err
	}
	dashboard.RecentReviews = reviews.Records
	feedback, err := s.ListFeedbackRecords(ctx, "", 0, recentLimit)
	if err != nil {
		return Dashboard{}, err
	}
	dashboard.RecentFeedback = feedback.Records
	return dashboard, nil
}

func (s *Store) readStateCounts(ctx context.Context, table string) ([]StateCount, error) {
	if table != "review_jobs" && table != "feedback_jobs" {
		return nil, errors.New("invalid state count table")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT state, count(*) FROM `+table+` GROUP BY state ORDER BY state`)
	if err != nil {
		return nil, fmt.Errorf("count %s states: %w", table, err)
	}
	defer rows.Close()
	var counts []StateCount
	for rows.Next() {
		var count StateCount
		if err := rows.Scan(&count.State, &count.Count); err != nil {
			return nil, fmt.Errorf("scan %s state count: %w", table, err)
		}
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s state counts: %w", table, err)
	}
	return counts, nil
}

func (s *Store) readOptionalTime(ctx context.Context, query string, arguments ...any) (*time.Time, error) {
	var text sql.NullString
	if err := s.db.QueryRowContext(ctx, query, arguments...).Scan(&text); err != nil {
		return nil, err
	}
	if !text.Valid {
		return nil, nil
	}
	parsed, err := parseStoredTime(text.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func (s *Store) ListReviewRecords(ctx context.Context, state string, beforeID int64, limit int) (ReviewRecordsPage, error) {
	if !validPanelLimit(limit) || beforeID < 0 || (state != "" && !validReviewState(state)) {
		return ReviewRecordsPage{}, errors.New("invalid review record query")
	}
	query := `
SELECT j.id, j.gitlab_instance, j.project_id, COALESCE(e.project_path, ''), j.merge_request_iid,
       j.head_sha, CASE WHEN j.source_event_id IS NULL THEN 'reconciled' ELSE 'webhook' END,
       j.state, j.attempt_count, j.created_at, j.started_at, j.updated_at,
       COALESCE(j.last_error_category, ''),
       (SELECT count(*) FROM review_findings f WHERE f.job_id = j.id),
       EXISTS(SELECT 1 FROM review_results r WHERE r.job_id = j.id),
       EXISTS(SELECT 1 FROM publications p WHERE p.job_id = j.id)
FROM review_jobs j
LEFT JOIN webhook_events e ON e.id = j.source_event_id`
	conditions := make([]string, 0, 2)
	arguments := make([]any, 0, 3)
	if state != "" {
		conditions = append(conditions, "j.state = ?")
		arguments = append(arguments, state)
	}
	if beforeID > 0 {
		conditions = append(conditions, "j.id < ?")
		arguments = append(arguments, beforeID)
	}
	if len(conditions) > 0 {
		query += "\nWHERE " + strings.Join(conditions, " AND ")
	}
	query += "\nORDER BY j.id DESC\nLIMIT ?"
	arguments = append(arguments, limit+1)
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return ReviewRecordsPage{}, fmt.Errorf("list review records: %w", err)
	}
	defer rows.Close()
	page := ReviewRecordsPage{Records: make([]ReviewRecord, 0, limit)}
	for rows.Next() {
		record, err := scanReviewRecord(rows)
		if err != nil {
			return ReviewRecordsPage{}, err
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return ReviewRecordsPage{}, fmt.Errorf("iterate review records: %w", err)
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextBefore = page.Records[len(page.Records)-1].ID
	}
	return page, nil
}

func (s *Store) GetReviewRecord(ctx context.Context, jobID int64) (ReviewRecordDetail, error) {
	if jobID <= 0 {
		return ReviewRecordDetail{}, errors.New("invalid review record ID")
	}
	row := s.db.QueryRowContext(ctx, `
SELECT j.id, j.gitlab_instance, j.project_id, COALESCE(e.project_path, ''), j.merge_request_iid,
       j.head_sha, CASE WHEN j.source_event_id IS NULL THEN 'reconciled' ELSE 'webhook' END,
       j.state, j.attempt_count, j.created_at, j.started_at, j.updated_at,
       COALESCE(j.last_error_category, ''),
       (SELECT count(*) FROM review_findings f WHERE f.job_id = j.id),
       EXISTS(SELECT 1 FROM review_results r WHERE r.job_id = j.id),
       EXISTS(SELECT 1 FROM publications p WHERE p.job_id = j.id)
FROM review_jobs j
LEFT JOIN webhook_events e ON e.id = j.source_event_id
WHERE j.id = ?`, jobID)
	record, err := scanReviewRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRecordDetail{}, ErrReviewRecordNotFound
	}
	if err != nil {
		return ReviewRecordDetail{}, err
	}
	detail := ReviewRecordDetail{
		ReviewRecord: record,
		ReviewID:     review.ReviewID(record.GitLabInstance, record.ProjectID, record.MergeRequestIID, record.HeadSHA),
	}

	if record.HasResult {
		var contents []byte
		if err := s.db.QueryRowContext(ctx, `SELECT result_json FROM review_results WHERE job_id = ?`, jobID).Scan(&contents); err != nil {
			return ReviewRecordDetail{}, fmt.Errorf("read panel review result: %w", err)
		}
		decoded, err := review.DecodeStored(contents)
		if err != nil {
			return ReviewRecordDetail{}, fmt.Errorf("decode panel review result: %w", err)
		}
		rows, err := s.db.QueryContext(ctx, `
SELECT finding_index, finding_id FROM review_findings WHERE job_id = ? ORDER BY finding_index`, jobID)
		if err != nil {
			return ReviewRecordDetail{}, fmt.Errorf("read panel finding identifiers: %w", err)
		}
		for rows.Next() {
			var index int
			var id string
			if err := rows.Scan(&index, &id); err != nil {
				rows.Close()
				return ReviewRecordDetail{}, fmt.Errorf("scan panel finding identifier: %w", err)
			}
			if index < 0 || index >= len(decoded.Findings) || !review.ValidFindingID(id) {
				rows.Close()
				return ReviewRecordDetail{}, errors.New("invalid stored panel finding identifiers")
			}
			decoded.Findings[index].ID = id
		}
		if err := rows.Close(); err != nil {
			return ReviewRecordDetail{}, fmt.Errorf("close panel finding identifiers: %w", err)
		}
		for _, finding := range decoded.Findings {
			if finding.ID == "" {
				return ReviewRecordDetail{}, errors.New("missing stored panel finding identifier")
			}
		}
		detail.Result = &decoded
	}
	if record.Published {
		if err := s.db.QueryRowContext(ctx, `SELECT gitlab_note_id FROM publications WHERE job_id = ?`, jobID).Scan(&detail.GitLabNoteID); err != nil {
			return ReviewRecordDetail{}, fmt.Errorf("read panel publication: %w", err)
		}
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT memory_id, memory_updated_at, retrieved_at
FROM review_memory_retrievals
WHERE job_id = ?
ORDER BY retrieved_at, memory_id, memory_updated_at`, jobID)
	if err != nil {
		return ReviewRecordDetail{}, fmt.Errorf("read panel memory retrievals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var retrieval ReviewMemoryRetrievalRecord
		var updated, retrieved string
		if err := rows.Scan(&retrieval.MemoryID, &updated, &retrieved); err != nil {
			return ReviewRecordDetail{}, fmt.Errorf("scan panel memory retrieval: %w", err)
		}
		retrieval.MemoryUpdatedAt, err = parseStoredTime(updated)
		if err != nil {
			return ReviewRecordDetail{}, fmt.Errorf("parse panel memory update time: %w", err)
		}
		retrieval.RetrievedAt, err = parseStoredTime(retrieved)
		if err != nil {
			return ReviewRecordDetail{}, fmt.Errorf("parse panel memory retrieval time: %w", err)
		}
		detail.Retrievals = append(detail.Retrievals, retrieval)
	}
	if err := rows.Err(); err != nil {
		return ReviewRecordDetail{}, fmt.Errorf("iterate panel memory retrievals: %w", err)
	}
	return detail, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanReviewRecord(row rowScanner) (ReviewRecord, error) {
	var record ReviewRecord
	var created string
	var started, updated sql.NullString
	var hasResult, published int
	if err := row.Scan(&record.ID, &record.GitLabInstance, &record.ProjectID, &record.ProjectPath, &record.MergeRequestIID,
		&record.HeadSHA, &record.Source, &record.State, &record.AttemptCount, &created, &started,
		&updated, &record.LastErrorCategory, &record.FindingCount, &hasResult, &published); err != nil {
		return ReviewRecord{}, err
	}
	var err error
	record.CreatedAt, err = parseStoredTime(created)
	if err != nil {
		return ReviewRecord{}, fmt.Errorf("parse panel review creation time: %w", err)
	}
	record.StartedAt, err = parseOptionalStoredTime(started)
	if err != nil {
		return ReviewRecord{}, fmt.Errorf("parse panel review start time: %w", err)
	}
	record.UpdatedAt, err = parseOptionalStoredTime(updated)
	if err != nil {
		return ReviewRecord{}, fmt.Errorf("parse panel review update time: %w", err)
	}
	record.HasResult = hasResult == 1
	record.Published = published == 1
	record.ExternalOnly = record.Published && !record.HasResult
	return record, nil
}

func (s *Store) ListFeedbackRecords(ctx context.Context, state string, beforeID int64, limit int) (FeedbackRecordsPage, error) {
	if !validPanelLimit(limit) || beforeID < 0 || (state != "" && !validFeedbackState(state)) {
		return FeedbackRecordsPage{}, errors.New("invalid feedback record query")
	}
	query := `
SELECT j.id, COALESCE(j.review_job_id, 0), j.project_id, j.project_path,
       j.merge_request_iid, j.note_id, j.state, j.attempt_count,
       e.received_at, j.updated_at, COALESCE(j.last_error_category, ''),
       (SELECT count(*) FROM review_memories m WHERE m.feedback_job_id = j.id),
       (SELECT count(*) FROM review_memories m WHERE m.feedback_job_id = j.id AND m.active = 1 AND m.lesson IS NOT NULL),
       COALESCE(r.gitlab_instance, ''), COALESCE(r.head_sha, '')
FROM feedback_jobs j
JOIN feedback_events e ON e.id = j.source_event_id
LEFT JOIN review_jobs r ON r.id = j.review_job_id`
	conditions := make([]string, 0, 2)
	arguments := make([]any, 0, 3)
	if state != "" {
		conditions = append(conditions, "j.state = ?")
		arguments = append(arguments, state)
	}
	if beforeID > 0 {
		conditions = append(conditions, "j.id < ?")
		arguments = append(arguments, beforeID)
	}
	if len(conditions) > 0 {
		query += "\nWHERE " + strings.Join(conditions, " AND ")
	}
	query += "\nORDER BY j.id DESC\nLIMIT ?"
	arguments = append(arguments, limit+1)
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return FeedbackRecordsPage{}, fmt.Errorf("list feedback records: %w", err)
	}
	defer rows.Close()
	page := FeedbackRecordsPage{Records: make([]FeedbackRecord, 0, limit)}
	for rows.Next() {
		var record FeedbackRecord
		var received, updated, instance, headSHA string
		if err := rows.Scan(&record.ID, &record.ReviewJobID, &record.ProjectID, &record.ProjectPath,
			&record.MergeRequestIID, &record.NoteID, &record.State, &record.AttemptCount,
			&received, &updated, &record.LastErrorCategory, &record.DecisionCount,
			&record.ActiveLessonCount, &instance, &headSHA); err != nil {
			return FeedbackRecordsPage{}, fmt.Errorf("scan feedback record: %w", err)
		}
		record.ReceivedAt, err = parseStoredTime(received)
		if err != nil {
			return FeedbackRecordsPage{}, fmt.Errorf("parse feedback receipt time: %w", err)
		}
		record.UpdatedAt, err = parseStoredTime(updated)
		if err != nil {
			return FeedbackRecordsPage{}, fmt.Errorf("parse feedback update time: %w", err)
		}
		if record.ReviewJobID > 0 && instance != "" && headSHA != "" {
			record.ReviewID = review.ReviewID(instance, record.ProjectID, record.MergeRequestIID, headSHA)
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return FeedbackRecordsPage{}, fmt.Errorf("iterate feedback records: %w", err)
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextBefore = page.Records[len(page.Records)-1].ID
	}
	return page, nil
}

func (s *Store) ListMemoryRecords(ctx context.Context, active *bool, beforeRowID int64, limit int) (MemoryRecordsPage, error) {
	if !validPanelLimit(limit) || beforeRowID < 0 {
		return MemoryRecordsPage{}, errors.New("invalid memory record query")
	}
	query := `
SELECT m.rowid, m.memory_id, j.project_id, j.project_path, j.merge_request_iid,
       j.note_id, m.target_type, m.target_id, m.outcome, m.confidence,
       COALESCE(m.lesson, ''), m.active, e.actor_role, m.source_url, m.updated_at
FROM review_memories m
JOIN feedback_jobs j ON j.id = m.feedback_job_id
JOIN feedback_evaluations e ON e.job_id = j.id`
	conditions := make([]string, 0, 2)
	arguments := make([]any, 0, 3)
	if active != nil {
		conditions = append(conditions, "m.active = ?")
		if *active {
			arguments = append(arguments, 1)
		} else {
			arguments = append(arguments, 0)
		}
	}
	if beforeRowID > 0 {
		conditions = append(conditions, "m.rowid < ?")
		arguments = append(arguments, beforeRowID)
	}
	if len(conditions) > 0 {
		query += "\nWHERE " + strings.Join(conditions, " AND ")
	}
	query += "\nORDER BY m.rowid DESC\nLIMIT ?"
	arguments = append(arguments, limit+1)
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return MemoryRecordsPage{}, fmt.Errorf("list memory records: %w", err)
	}
	defer rows.Close()
	page := MemoryRecordsPage{Records: make([]MemoryRecord, 0, limit)}
	for rows.Next() {
		var record MemoryRecord
		var activeValue int
		var updated string
		if err := rows.Scan(&record.RowID, &record.MemoryID, &record.ProjectID, &record.ProjectPath,
			&record.MergeRequestIID, &record.NoteID, &record.TargetType, &record.TargetID,
			&record.Outcome, &record.Confidence, &record.Lesson, &activeValue,
			&record.SourceRole, &record.SourceURL, &updated); err != nil {
			return MemoryRecordsPage{}, fmt.Errorf("scan memory record: %w", err)
		}
		record.Active = activeValue == 1
		record.UpdatedAt, err = parseStoredTime(updated)
		if err != nil {
			return MemoryRecordsPage{}, fmt.Errorf("parse memory update time: %w", err)
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return MemoryRecordsPage{}, fmt.Errorf("iterate memory records: %w", err)
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextBefore = page.Records[len(page.Records)-1].RowID
	}
	return page, nil
}

func validPanelLimit(limit int) bool {
	return limit > 0 && limit <= maxPanelPageSize
}

func validReviewState(state string) bool {
	switch state {
	case JobQueued, JobRunning, JobPublishing, JobCompleted, JobFailed, JobObsolete:
		return true
	default:
		return false
	}
}

func validFeedbackState(state string) bool {
	switch state {
	case FeedbackQueued, FeedbackRunning, FeedbackCompleted, FeedbackFailed:
		return true
	default:
		return false
	}
}

func parseStoredTime(text string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, text)
}

func parseOptionalStoredTime(text sql.NullString) (*time.Time, error) {
	if !text.Valid {
		return nil, nil
	}
	parsed, err := parseStoredTime(text.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
