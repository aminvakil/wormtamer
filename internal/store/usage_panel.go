package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aminvakil/wormtamer/internal/usage"
)

var (
	ErrGenerationRecordNotFound = errors.New("generation record not found")
	ErrFeedbackRecordNotFound   = errors.New("feedback record not found")
)

type UsageQuery struct {
	Since                    time.Time
	Until                    time.Time
	RequestKind              string
	ConfiguredModel          string
	ResolvedModel            string
	ResolvedModelUnavailable bool
	ProjectID                int64
	BeforeID                 int64
	Limit                    int
}

type GenerationRecord struct {
	ID                     int64
	RequestKind            string
	ReviewJobID            int64
	FeedbackJobID          int64
	WorkflowAttempt        int
	ReviewTurn             *int
	ConfiguredModel        string
	ResolvedModel          string
	RequestStartedAt       time.Time
	CompletedAt            *time.Time
	CompletionState        string
	LatencyMS              *int64
	FinishReason           string
	StructuredValidation   string
	ToolCallsAvailable     bool
	ToolNames              []string
	FinalOnly              bool
	UsageMetadataAvailable bool
	UsageMetadataValid     bool
	TokenCountsAvailable   bool
	PromptTokens           int64
	CachedTokens           int64
	ToolUsePromptTokens    int64
	CandidateTokens        int64
	ThoughtTokens          int64
	TotalTokens            int64
	ProjectID              int64
	ProjectPath            string
	MergeRequestIID        int64
}

type GenerationRecordsPage struct {
	Records    []GenerationRecord
	NextBefore int64
}

type UsageTokenTotals struct {
	Input         int64
	Output        int64
	Prompt        int64
	UncachedInput int64
	CachedInput   int64
	ToolUseInput  int64
	Candidate     int64
	Thought       int64
	Total         int64
}

type UsageCostTotal struct {
	EstimatedCostPicos int64
	GenerationCount    int
}

type UsageModelBreakdown struct {
	ConfiguredModel string
	ResolvedModel   string
	GenerationCount int
	TotalTokens     int64
}

type UsageProjectBreakdown struct {
	ProjectID       int64
	ProjectPath     string
	GenerationCount int
	TotalTokens     int64
}

type UsageKindBreakdown struct {
	RequestKind     string
	GenerationCount int
	TotalTokens     int64
}

type UsageReport struct {
	GenerationCount       int
	ResponseCount         int
	FailedCount           int
	UnknownCount          int
	StartedCount          int
	UsageAvailableCount   int
	UsageUnavailableCount int
	UsageInvalidCount     int
	PricedCount           int
	CostUnavailableCount  int
	NoCostDataCount       int
	Tokens                UsageTokenTotals
	Costs                 []UsageCostTotal
	Models                []UsageModelBreakdown
	Projects              []UsageProjectBreakdown
	Kinds                 []UsageKindBreakdown
	Generations           GenerationRecordsPage
	BreakdownsTruncated   bool
}

type FeedbackRecordDetail struct {
	FeedbackRecord
	Generations          []GenerationRecord
	GenerationsTruncated bool
}

func (s *Store) ReadUsageReport(ctx context.Context, query UsageQuery) (UsageReport, error) {
	if err := validateUsageQuery(query); err != nil {
		return UsageReport{}, err
	}
	from, conditions, arguments := usageQueryParts(query, false)
	where := " WHERE " + strings.Join(conditions, " AND ")
	var report UsageReport
	if err := s.db.QueryRowContext(ctx, `
SELECT count(*),
       COALESCE(sum(g.completion_state = 'response'), 0),
       COALESCE(sum(g.completion_state = 'failed'), 0),
       COALESCE(sum(g.completion_state = 'unknown'), 0),
       COALESCE(sum(g.completion_state = 'started'), 0),
       COALESCE(sum(g.usage_metadata_available = 1), 0),
       COALESCE(sum(g.completion_state = 'response' AND g.usage_metadata_available = 0), 0),
       COALESCE(sum(g.usage_metadata_available = 1 AND g.usage_metadata_valid = 0), 0),
       COALESCE(sum(g.estimated_cost_picos IS NOT NULL), 0),
       COALESCE(sum(g.completion_state = 'response' AND g.cost_source IS NOT NULL AND g.estimated_cost_picos IS NULL), 0),
       COALESCE(sum(g.completion_state = 'response' AND g.cost_source IS NULL), 0)
`+from+where, arguments...).Scan(
		&report.GenerationCount, &report.ResponseCount, &report.FailedCount, &report.UnknownCount,
		&report.StartedCount, &report.UsageAvailableCount, &report.UsageUnavailableCount, &report.UsageInvalidCount,
		&report.PricedCount, &report.CostUnavailableCount, &report.NoCostDataCount); err != nil {
		return UsageReport{}, fmt.Errorf("read usage totals: %w", err)
	}
	if err := s.readUsageTokenTotals(ctx, from, where, arguments, &report.Tokens); err != nil {
		return UsageReport{}, err
	}
	var err error
	report.Costs, err = s.readUsageCosts(ctx, from, where, arguments)
	if err != nil {
		return UsageReport{}, err
	}
	report.Models, report.BreakdownsTruncated, err = s.readUsageModels(ctx, from, where, arguments)
	if err != nil {
		return UsageReport{}, err
	}
	var truncated bool
	report.Projects, truncated, err = s.readUsageProjects(ctx, from, where, arguments)
	if err != nil {
		return UsageReport{}, err
	}
	report.BreakdownsTruncated = report.BreakdownsTruncated || truncated
	report.Kinds, err = s.readUsageKinds(ctx, from, where, arguments)
	if err != nil {
		return UsageReport{}, err
	}
	report.Generations, err = s.listUsageGenerations(ctx, query)
	if err != nil {
		return UsageReport{}, err
	}
	return report, nil
}

func validateUsageQuery(query UsageQuery) error {
	if query.Since.IsZero() || query.Until.IsZero() || query.Since.After(query.Until) ||
		!validPanelLimit(query.Limit) || query.BeforeID < 0 || query.ProjectID < 0 ||
		(query.RequestKind != "" && query.RequestKind != usage.RequestReview && query.RequestKind != usage.RequestFeedback) ||
		(query.ResolvedModelUnavailable && query.ResolvedModel != "") ||
		!validGenerationText(query.ConfiguredModel, 256) || !validGenerationText(query.ResolvedModel, 256) {
		return errors.New("invalid usage query")
	}
	return nil
}

func usageQueryParts(query UsageQuery, includeBefore bool) (string, []string, []any) {
	from := `FROM model_generations g
LEFT JOIN review_jobs rj ON rj.id = g.review_job_id
LEFT JOIN webhook_events re ON re.id = rj.source_event_id
LEFT JOIN feedback_jobs fj ON fj.id = g.feedback_job_id`
	conditions := []string{"g.request_started_at >= ?", "g.request_started_at <= ?"}
	arguments := []any{formatTime(query.Since), formatTime(query.Until)}
	if query.RequestKind != "" {
		conditions = append(conditions, "g.request_kind = ?")
		arguments = append(arguments, query.RequestKind)
	}
	if query.ConfiguredModel != "" {
		conditions = append(conditions, "g.configured_model = ?")
		arguments = append(arguments, query.ConfiguredModel)
	}
	if query.ResolvedModelUnavailable {
		conditions = append(conditions, "g.resolved_model IS NULL")
	} else if query.ResolvedModel != "" {
		conditions = append(conditions, "g.resolved_model = ?")
		arguments = append(arguments, query.ResolvedModel)
	}
	if query.ProjectID > 0 {
		conditions = append(conditions, "COALESCE(rj.project_id, fj.project_id) = ?")
		arguments = append(arguments, query.ProjectID)
	}
	if includeBefore && query.BeforeID > 0 {
		conditions = append(conditions, "g.id < ?")
		arguments = append(arguments, query.BeforeID)
	}
	return from, conditions, arguments
}

func (s *Store) readUsageTokenTotals(ctx context.Context, from, where string, arguments []any, totals *UsageTokenTotals) error {
	values := []*int64{&totals.Input, &totals.Output, &totals.Prompt, &totals.UncachedInput, &totals.CachedInput, &totals.ToolUseInput, &totals.Candidate, &totals.Thought, &totals.Total}
	scans := make([]any, len(values))
	nulls := make([]sql.NullInt64, len(values))
	for index := range nulls {
		scans[index] = &nulls[index]
	}
	if err := s.db.QueryRowContext(ctx, `
SELECT sum(CASE WHEN g.prompt_tokens IS NOT NULL AND g.tool_use_prompt_tokens IS NOT NULL THEN g.prompt_tokens + g.tool_use_prompt_tokens END),
       sum(CASE WHEN g.candidate_tokens IS NOT NULL AND g.thought_tokens IS NOT NULL THEN g.candidate_tokens + g.thought_tokens END),
       sum(g.prompt_tokens),
       sum(CASE WHEN g.prompt_tokens IS NOT NULL AND g.cached_tokens IS NOT NULL AND g.cached_tokens <= g.prompt_tokens THEN g.prompt_tokens - g.cached_tokens END),
       sum(g.cached_tokens), sum(g.tool_use_prompt_tokens), sum(g.candidate_tokens), sum(g.thought_tokens), sum(g.total_tokens)
`+from+where, arguments...).Scan(scans...); err != nil {
		return fmt.Errorf("read usage token totals: %w", err)
	}
	for index := range nulls {
		if nulls[index].Valid {
			*values[index] = nulls[index].Int64
		}
	}
	return nil
}

func (s *Store) readUsageCosts(ctx context.Context, from, where string, arguments []any) ([]UsageCostTotal, error) {
	var cost sql.NullInt64
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT sum(g.estimated_cost_picos), count(g.estimated_cost_picos)
`+from+where, arguments...).Scan(&cost, &count); err != nil {
		return nil, fmt.Errorf("read usage cost total: %w", err)
	}
	if !cost.Valid {
		return nil, nil
	}
	return []UsageCostTotal{{EstimatedCostPicos: cost.Int64, GenerationCount: count}}, nil
}

func (s *Store) readUsageModels(ctx context.Context, from, where string, arguments []any) ([]UsageModelBreakdown, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT g.configured_model, COALESCE(g.resolved_model, ''), count(*), COALESCE(sum(g.total_tokens), 0)
`+from+where+`
GROUP BY g.configured_model, g.resolved_model
ORDER BY count(*) DESC, g.configured_model, g.resolved_model LIMIT 101`, arguments...)
	if err != nil {
		return nil, false, fmt.Errorf("read usage model breakdown: %w", err)
	}
	defer rows.Close()
	var values []UsageModelBreakdown
	for rows.Next() {
		var value UsageModelBreakdown
		if err := rows.Scan(&value.ConfiguredModel, &value.ResolvedModel, &value.GenerationCount, &value.TotalTokens); err != nil {
			return nil, false, fmt.Errorf("scan usage model breakdown: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(values) > 100
	if truncated {
		values = values[:100]
	}
	return values, truncated, nil
}

func (s *Store) readUsageProjects(ctx context.Context, from, where string, arguments []any) ([]UsageProjectBreakdown, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(rj.project_id, fj.project_id),
       MAX(COALESCE(re.project_path, fj.project_path, '')),
       count(*), COALESCE(sum(g.total_tokens), 0)
`+from+where+`
GROUP BY COALESCE(rj.project_id, fj.project_id)
ORDER BY count(*) DESC, COALESCE(rj.project_id, fj.project_id) LIMIT 101`, arguments...)
	if err != nil {
		return nil, false, fmt.Errorf("read usage project breakdown: %w", err)
	}
	defer rows.Close()
	var values []UsageProjectBreakdown
	for rows.Next() {
		var value UsageProjectBreakdown
		if err := rows.Scan(&value.ProjectID, &value.ProjectPath, &value.GenerationCount, &value.TotalTokens); err != nil {
			return nil, false, fmt.Errorf("scan usage project breakdown: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(values) > 100
	if truncated {
		values = values[:100]
	}
	return values, truncated, nil
}

func (s *Store) readUsageKinds(ctx context.Context, from, where string, arguments []any) ([]UsageKindBreakdown, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT g.request_kind, count(*), COALESCE(sum(g.total_tokens), 0)
`+from+where+`
GROUP BY g.request_kind ORDER BY g.request_kind`, arguments...)
	if err != nil {
		return nil, fmt.Errorf("read usage kind breakdown: %w", err)
	}
	defer rows.Close()
	var values []UsageKindBreakdown
	for rows.Next() {
		var value UsageKindBreakdown
		if err := rows.Scan(&value.RequestKind, &value.GenerationCount, &value.TotalTokens); err != nil {
			return nil, fmt.Errorf("scan usage kind breakdown: %w", err)
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) listUsageGenerations(ctx context.Context, query UsageQuery) (GenerationRecordsPage, error) {
	from, conditions, arguments := usageQueryParts(query, true)
	rows, err := s.db.QueryContext(ctx, generationSelect+"\n"+from+"\nWHERE "+strings.Join(conditions, " AND ")+"\nORDER BY g.id DESC LIMIT ?", append(arguments, query.Limit+1)...)
	if err != nil {
		return GenerationRecordsPage{}, fmt.Errorf("list usage generations: %w", err)
	}
	defer rows.Close()
	page := GenerationRecordsPage{Records: make([]GenerationRecord, 0, query.Limit)}
	for rows.Next() {
		record, err := scanGenerationRecord(rows)
		if err != nil {
			return GenerationRecordsPage{}, err
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return GenerationRecordsPage{}, fmt.Errorf("iterate usage generations: %w", err)
	}
	if len(page.Records) > query.Limit {
		page.Records = page.Records[:query.Limit]
		page.NextBefore = page.Records[len(page.Records)-1].ID
	}
	return page, nil
}

const generationSelect = `SELECT
    g.id, g.request_kind, COALESCE(g.review_job_id, 0), COALESCE(g.feedback_job_id, 0),
    g.workflow_attempt, g.review_turn, g.configured_model, COALESCE(g.resolved_model, ''),
    g.request_started_at, g.completed_at, g.completion_state, g.latency_ms,
    COALESCE(g.finish_reason, ''), COALESCE(g.structured_validation, ''),
    g.tool_calls_available, g.tool_names_json, g.final_only,
    g.usage_metadata_available, g.usage_metadata_valid,
    g.prompt_tokens, g.cached_tokens, g.tool_use_prompt_tokens,
    g.candidate_tokens, g.thought_tokens, g.total_tokens,
    COALESCE(rj.project_id, fj.project_id), COALESCE(re.project_path, fj.project_path, ''),
    COALESCE(rj.merge_request_iid, fj.merge_request_iid)`

func scanGenerationRecord(row rowScanner) (GenerationRecord, error) {
	var record GenerationRecord
	var reviewTurn, latency sql.NullInt64
	var completed sql.NullString
	var started string
	var toolCallsAvailable, finalOnly, usageAvailable, usageValid int
	var toolNames []byte
	var prompt, cached, toolPrompt, candidates, thoughts, total sql.NullInt64
	if err := row.Scan(
		&record.ID, &record.RequestKind, &record.ReviewJobID, &record.FeedbackJobID,
		&record.WorkflowAttempt, &reviewTurn, &record.ConfiguredModel, &record.ResolvedModel,
		&started, &completed, &record.CompletionState, &latency,
		&record.FinishReason, &record.StructuredValidation, &toolCallsAvailable, &toolNames, &finalOnly,
		&usageAvailable, &usageValid, &prompt, &cached, &toolPrompt, &candidates, &thoughts, &total,
		&record.ProjectID, &record.ProjectPath, &record.MergeRequestIID); err != nil {
		return GenerationRecord{}, err
	}
	var err error
	record.RequestStartedAt, err = parseStoredTime(started)
	if err != nil {
		return GenerationRecord{}, fmt.Errorf("parse generation request time: %w", err)
	}
	record.CompletedAt, err = parseOptionalStoredTime(completed)
	if err != nil {
		return GenerationRecord{}, fmt.Errorf("parse generation completion time: %w", err)
	}
	if reviewTurn.Valid {
		value := int(reviewTurn.Int64)
		record.ReviewTurn = &value
	}
	if latency.Valid {
		value := latency.Int64
		record.LatencyMS = &value
	}
	record.ToolCallsAvailable = toolCallsAvailable == 1
	if record.ToolCallsAvailable {
		if err := json.Unmarshal(toolNames, &record.ToolNames); err != nil {
			return GenerationRecord{}, fmt.Errorf("decode generation tool names: %w", err)
		}
	}
	record.FinalOnly = finalOnly == 1
	record.UsageMetadataAvailable = usageAvailable == 1
	record.UsageMetadataValid = usageValid == 1
	record.TokenCountsAvailable = prompt.Valid && cached.Valid && toolPrompt.Valid && candidates.Valid && thoughts.Valid && total.Valid
	if record.TokenCountsAvailable {
		record.PromptTokens, record.CachedTokens = prompt.Int64, cached.Int64
		record.ToolUsePromptTokens, record.CandidateTokens = toolPrompt.Int64, candidates.Int64
		record.ThoughtTokens, record.TotalTokens = thoughts.Int64, total.Int64
	}
	return record, nil
}

func (s *Store) GetGenerationRecord(ctx context.Context, generationID int64) (GenerationRecord, error) {
	if generationID <= 0 {
		return GenerationRecord{}, errors.New("invalid generation record ID")
	}
	row := s.db.QueryRowContext(ctx, generationSelect+`
FROM model_generations g
LEFT JOIN review_jobs rj ON rj.id = g.review_job_id
LEFT JOIN webhook_events re ON re.id = rj.source_event_id
LEFT JOIN feedback_jobs fj ON fj.id = g.feedback_job_id
WHERE g.id = ?`, generationID)
	record, err := scanGenerationRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return GenerationRecord{}, ErrGenerationRecordNotFound
	}
	if err != nil {
		return GenerationRecord{}, fmt.Errorf("read generation record: %w", err)
	}
	return record, nil
}

func (s *Store) ListReviewGenerations(ctx context.Context, jobID int64, limit int) ([]GenerationRecord, bool, error) {
	return s.listJobGenerations(ctx, "review_job_id", jobID, limit)
}

func (s *Store) ListFeedbackGenerations(ctx context.Context, jobID int64, limit int) ([]GenerationRecord, bool, error) {
	return s.listJobGenerations(ctx, "feedback_job_id", jobID, limit)
}

func (s *Store) listJobGenerations(ctx context.Context, column string, jobID int64, limit int) ([]GenerationRecord, bool, error) {
	if (column != "review_job_id" && column != "feedback_job_id") || jobID <= 0 || !validPanelLimit(limit) {
		return nil, false, errors.New("invalid job generation query")
	}
	rows, err := s.db.QueryContext(ctx, generationSelect+`
FROM model_generations g
LEFT JOIN review_jobs rj ON rj.id = g.review_job_id
LEFT JOIN webhook_events re ON re.id = rj.source_event_id
LEFT JOIN feedback_jobs fj ON fj.id = g.feedback_job_id
WHERE g.`+column+` = ? ORDER BY g.id DESC LIMIT ?`, jobID, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list job generations: %w", err)
	}
	defer rows.Close()
	var records []GenerationRecord
	for rows.Next() {
		record, err := scanGenerationRecord(rows)
		if err != nil {
			return nil, false, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(records) > limit
	if truncated {
		records = records[:limit]
	}
	return records, truncated, nil
}

func (s *Store) GetFeedbackRecord(ctx context.Context, jobID int64) (FeedbackRecordDetail, error) {
	if jobID <= 0 {
		return FeedbackRecordDetail{}, errors.New("invalid feedback record ID")
	}
	page, err := s.listFeedbackRecords(ctx, "j.id = ?", []any{jobID}, 1)
	if err != nil {
		return FeedbackRecordDetail{}, err
	}
	if len(page.Records) == 0 {
		return FeedbackRecordDetail{}, ErrFeedbackRecordNotFound
	}
	detail := FeedbackRecordDetail{FeedbackRecord: page.Records[0]}
	detail.Generations, detail.GenerationsTruncated, err = s.ListFeedbackGenerations(ctx, jobID, maxPanelPageSize)
	if err != nil {
		return FeedbackRecordDetail{}, err
	}
	return detail, nil
}
