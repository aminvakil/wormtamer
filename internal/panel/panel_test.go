package panel

import (
	"context"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/diagnostics"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
	"github.com/aminvakil/wormtamer/internal/usage"
)

func TestTimeFormattersRejectUnsupportedTemplateValues(t *testing.T) {
	if formatTime(time.Time{}) != "—" || formatOptionalTime(nil) != "—" ||
		formatCompactTime(time.Time{}) != "—" || formatCompactOptionalTime(nil) != "—" ||
		timeAttribute(time.Time{}) != "" || optionalTimeAttribute(nil) != "" {
		t.Fatal("zero or absent time did not render as missing")
	}
	now := time.Date(2026, 8, 16, 12, 0, 0, 123, time.FixedZone("test", 90*60))
	if formatCompactTime(now) != "2026 Aug 16 · 10:30 UTC" ||
		timeAttribute(now) != "2026-08-16T10:30:00.000000123Z" {
		t.Fatalf("compact time = %q attribute = %q", formatCompactTime(now), timeAttribute(now))
	}
	for name, source := range map[string]string{
		"required": `{{formatTime .}}`,
		"optional": `{{formatOptionalTime .}}`,
	} {
		t.Run(name, func(t *testing.T) {
			view, err := template.New("time").Funcs(template.FuncMap{
				"formatTime":         formatTime,
				"formatOptionalTime": formatOptionalTime,
			}).Parse(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := view.Execute(io.Discard, "not-a-time"); err == nil {
				t.Fatal("template accepted an unsupported timestamp type")
			}
		})
	}
}

func TestOverviewRendersStateAndOnlyExplicitConfiguration(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	storage := &fakeStore{dashboard: store.Dashboard{
		ReviewCounts:       []store.StateCount{{State: store.JobQueued, Count: 2}},
		FeedbackCounts:     []store.StateCount{{State: store.FeedbackFailed, Count: 1}},
		OldestQueuedReview: &now, MemoryCount: 3,
		RecentReviews: []store.ReviewRecord{{
			ID: 9, ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
			HeadSHA: strings.Repeat("a", 40), State: store.JobQueued, Source: "webhook", CreatedAt: now,
		}},
	}}
	handler := newTestHandler(t, storage)
	response := request(t, handler, http.MethodGet, "/")
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "<title>Overview · Wormtamer</title>") ||
		!strings.Contains(body, "Skip to content") || !strings.Contains(body, `aria-current="page"`) ||
		!strings.Contains(body, "Review activity") || !strings.Contains(body, `data-tone="waiting" data-empty="false"><span>Waiting</span><strong>2</strong>`) ||
		!strings.Contains(body, "No queued synthesis") || !strings.Contains(body, "group/project") ||
		!strings.Contains(body, "gemini-test") || !strings.Contains(body, "docs.example.com") ||
		!strings.Contains(body, "group/shared") {
		t.Fatalf("overview status=%d body=%s", response.Code, body)
	}
	for _, excluded := range []string{"gitlab-token", "gemini-key", "webhook-secret", "/private/wormtamer.db"} {
		if strings.Contains(body, excluded) {
			t.Fatalf("overview exposed %q: %s", excluded, body)
		}
	}
	assertPanelHeaders(t, response)
	if storage.dashboardLimit != dashboardRecent {
		t.Fatalf("dashboard limit = %d", storage.dashboardLimit)
	}
}

func TestReviewListValidatesFiltersAndPaginates(t *testing.T) {
	storage := &fakeStore{reviewPage: store.ReviewRecordsPage{
		Records: []store.ReviewRecord{{
			ID: 8, ProjectID: 55, MergeRequestIID: 9, HeadSHA: strings.Repeat("b", 40),
			State: store.JobFailed, Source: "reconciled", AttemptCount: 5, FindingCount: 2,
		}},
		NextBefore: 8,
	}}
	handler := newTestHandler(t, storage)
	response := request(t, handler, http.MethodGet, "/reviews?state=failed&before=10")
	body := response.Body.String()
	if response.Code != http.StatusOK || storage.reviewState != store.JobFailed || storage.reviewBefore != 10 ||
		storage.reviewLimit != pageSize || !strings.Contains(body, "<title>Reviews · Wormtamer</title>") ||
		!strings.Contains(body, `class="filter current" aria-current="page"`) ||
		!strings.Contains(body, "Review #8") || !strings.Contains(body, "Project #55") ||
		!strings.Contains(body, "Reconciler") || !strings.Contains(body, `data-retried="true"`) ||
		!strings.Contains(body, "Older reviews") || !strings.Contains(body, "before=8") ||
		!strings.Contains(body, "state=failed") {
		t.Fatalf("reviews status=%d calls=%+v body=%s", response.Code, storage, body)
	}
	for _, path := range []string{
		"/reviews?state=unknown", "/reviews?before=0", "/reviews?before=text",
		"/reviews?state=failed&state=queued", "/reviews?unknown=value",
	} {
		response := request(t, handler, http.MethodGet, path)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	response = request(t, handler, http.MethodPost, "/reviews")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /reviews status=%d body=%s", response.Code, response.Body.String())
	}
	assertPanelHeaders(t, response)
}

func TestReviewDetailEscapesUntrustedContentAndDistinguishesPublication(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	result := review.Result{
		Summary: "summary <script>alert(1)</script> **markdown**",
		Findings: []review.Finding{{
			ID: "WT-F-" + strings.Repeat("A", 26), Priority: "P1", Title: "bad <img src=x>",
			Explanation: "line one\n<iframe>", Recommendation: "use `safe`", Path: "file<script>.go",
		}},
	}
	storage := &fakeStore{detail: store.ReviewRecordDetail{
		ReviewRecord: store.ReviewRecord{
			ID: 9, ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
			HeadSHA: strings.Repeat("a", 40), Source: "webhook", State: store.JobCompleted,
			CreatedAt: now, UpdatedAt: &now, LastErrorCategory: "safe_failure_category",
			PatchIDStatus: store.PatchIDAvailable, PatchIDSHA: strings.Repeat("d", 64),
			HasResult: true, Published: true,
		},
		ReviewID: "WT-R-" + strings.Repeat("B", 26), Result: &result,
		GitLabNoteID: 81,
		Retrievals: []store.ReviewMemoryRetrievalRecord{{
			MemoryID: "WT-M-" + strings.Repeat("C", 26), MemoryUpdatedAt: now, RetrievedAt: now,
		}},
	}}
	handler := newTestHandler(t, storage)
	response := request(t, handler, http.MethodGet, "/reviews/9")
	body := response.Body.String()
	if response.Code != http.StatusOK || storage.detailID != 9 ||
		!strings.Contains(body, "summary &lt;script&gt;alert(1)&lt;/script&gt; **markdown**") ||
		!strings.Contains(body, "file&lt;script&gt;.go") || strings.Contains(body, "<script>") ||
		strings.Contains(body, "<img src=x>") || strings.Contains(body, "<iframe>") ||
		!strings.Contains(body, "#note_81") || !strings.Contains(body, strings.Repeat("d", 64)) ||
		!strings.Contains(body, "safe_failure_category") {
		t.Fatalf("review detail status=%d body=%s", response.Code, body)
	}
	storage.detail = store.ReviewRecordDetail{
		ReviewRecord: store.ReviewRecord{
			ID: 10, ProjectID: 55, MergeRequestIID: 9, HeadSHA: strings.Repeat("b", 40),
			Source: "reconciled", State: store.JobCompleted, CreatedAt: now,
			Published: true, ExternalOnly: true,
		},
		ReviewID: "WT-R-" + strings.Repeat("E", 26), GitLabNoteID: 82,
	}
	response = request(t, handler, http.MethodGet, "/reviews/10")
	if response.Code != http.StatusOK || !strings.Contains(strings.ToLower(response.Body.String()), "external-only") ||
		!strings.Contains(response.Body.String(), "No local structured result is available") ||
		strings.Contains(response.Body.String(), "<h3>Failure</h3>") {
		t.Fatalf("external-only detail status=%d body=%s", response.Code, response.Body.String())
	}
	storage.detail = store.ReviewRecordDetail{
		ReviewRecord: store.ReviewRecord{
			ID: 11, ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
			HeadSHA: strings.Repeat("c", 40), Source: "webhook", State: store.JobCompleted,
			CreatedAt: now, PatchIDStatus: store.PatchIDAvailable, PatchIDSHA: strings.Repeat("d", 64),
			Equivalent: true, EquivalentToJobID: 9,
		},
		ReviewID: "WT-R-" + strings.Repeat("F", 26),
	}
	response = request(t, handler, http.MethodGet, "/reviews/11")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Equivalent to canonical") ||
		!strings.Contains(response.Body.String(), "/reviews/9") ||
		strings.Contains(response.Body.String(), "No locally validated result has been checkpointed") {
		t.Fatalf("equivalent detail status=%d body=%s", response.Code, response.Body.String())
	}
	response = request(t, handler, http.MethodGet, "/reviews/not-a-number")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid detail status=%d", response.Code)
	}
	storage.detailErr = store.ErrReviewRecordNotFound
	response = request(t, handler, http.MethodGet, "/reviews/100")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing detail status=%d", response.Code)
	}
}

func TestFeedbackAndMemoryViewsUseBoundedReadQueries(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	storage := &fakeStore{
		feedbackPage: store.FeedbackRecordsPage{Records: []store.FeedbackRecord{{
			ID: 3, ReviewJobID: 9, ProjectID: 42, ProjectPath: "group/project",
			MergeRequestIID: 7, HeadSHA: strings.Repeat("a", 40), TerminalState: "merged",
			State: store.FeedbackCompleted, ReceivedAt: now, UpdatedAt: now, MemoryCreated: true,
		}}, NextBefore: 3},
		memoryPage: store.MemoryRecordsPage{Records: []store.MemoryRecord{{
			RowID: 4, MemoryID: "WT-M-" + strings.Repeat("A", 26), ProjectID: 42,
			ProjectPath: "group/project", MergeRequestIID: 7, Lesson: "lesson <b>not html</b>",
			SourceURL: "http://gitlab.internal/group/project/-/merge_requests/7", UpdatedAt: now,
		}, {
			RowID: 2, MemoryID: "WT-M-" + strings.Repeat("C", 26), ProjectID: 42,
			ProjectPath: "group/project", MergeRequestIID: 7, Lesson: "another lesson",
			SourceURL: "javascript:alert(1)", UpdatedAt: now,
		}}, NextBefore: 2},
	}
	handler := newTestHandler(t, storage)
	feedback := request(t, handler, http.MethodGet, "/feedback?state=completed")
	if feedback.Code != http.StatusOK || storage.feedbackState != store.FeedbackCompleted ||
		storage.feedbackLimit != pageSize || !strings.Contains(feedback.Body.String(), "Review #9") ||
		!strings.Contains(feedback.Body.String(), `class="filter current" aria-current="page"`) ||
		!strings.Contains(feedback.Body.String(), "Older feedback") {
		t.Fatalf("feedback status=%d body=%s", feedback.Code, feedback.Body.String())
	}
	memory := request(t, handler, http.MethodGet, "/memory")
	body := memory.Body.String()
	if memory.Code != http.StatusOK || storage.memoryLimit != pageSize ||
		!strings.Contains(body, "lesson &lt;b&gt;not html&lt;/b&gt;") ||
		!strings.Contains(body, "http://gitlab.internal/group/project") ||
		strings.Contains(body, `href="javascript:alert(1)"`) || !strings.Contains(body, "Older memory") {
		t.Fatalf("memory status=%d calls=%+v body=%s", memory.Code, storage, body)
	}
	for _, path := range []string{"/memory?active=true", "/memory?before=-1", "/memory?before=1&before=2"} {
		response := request(t, handler, http.MethodGet, path)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status=%d", path, response.Code)
		}
	}
}

func TestUsageAndGenerationViewsAreBoundedReadOnlyReports(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	turn := 0
	latency := int64(321)
	completed := now.Add(time.Second)
	generation := store.GenerationRecord{
		ID: 8, RequestKind: "review", ReviewJobID: 9, WorkflowAttempt: 2, ReviewTurn: &turn,
		ConfiguredModel: "configured-<model>", ResolvedModel: "resolved-model",
		RequestStartedAt: now, CompletedAt: &completed, CompletionState: "response", LatencyMS: &latency,
		FinishReason: "STOP", StructuredValidation: "valid", ToolCallsAvailable: true,
		ToolNames: []string{"read_repository_file"}, FinalOnly: true,
		UsageMetadataAvailable: true, UsageMetadataValid: true, TokenCountsAvailable: true,
		PromptTokens: 100, CachedTokens: 20, ToolUsePromptTokens: 10,
		CandidateTokens: 30, ThoughtTokens: 5, TotalTokens: 145,
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
	}
	storage := &fakeStore{usageReport: store.UsageReport{
		GenerationCount: 1, ResponseCount: 1, UsageAvailableCount: 1, PricedCount: 1,
		Tokens: store.UsageTokenTotals{Input: 110, Output: 35, Prompt: 100, UncachedInput: 80, CachedInput: 20, ToolUseInput: 10, Candidate: 30, Thought: 5, Total: 145},
		Costs:  []store.UsageCostTotal{{EstimatedCostPicos: 123_400_000, GenerationCount: 1}},
		Models: []store.UsageModelBreakdown{
			{ConfiguredModel: "configured-<model>", ResolvedModel: "resolved-model", GenerationCount: 1, TotalTokens: 145},
			{ConfiguredModel: "configured-unresolved", GenerationCount: 1},
		},
		Projects:    []store.UsageProjectBreakdown{{ProjectID: 42, ProjectPath: "group/project", GenerationCount: 1, TotalTokens: 145}},
		Kinds:       []store.UsageKindBreakdown{{RequestKind: "review", GenerationCount: 1, TotalTokens: 145}},
		Generations: store.GenerationRecordsPage{Records: []store.GenerationRecord{generation}, NextBefore: 8},
	}, generation: generation}
	handler := newTestHandler(t, storage)
	response := request(t, handler, http.MethodGet, "/usage?window=week&kind=review&configured_model=configured-%3Cmodel%3E&resolved_model=resolved-model&project_id=42&before=99")
	body := response.Body.String()
	if response.Code != http.StatusOK || storage.usageQuery.RequestKind != "review" ||
		storage.usageQuery.ProjectID != 42 || storage.usageQuery.BeforeID != 99 ||
		storage.usageQuery.Until.Sub(storage.usageQuery.Since) != 7*24*time.Hour || storage.usageQuery.Limit != pageSize ||
		!strings.Contains(body, "Model usage") || !strings.Contains(body, "Uncached input") ||
		!strings.Contains(body, "0.0001234") || !strings.Contains(body, "configured-&lt;model&gt;") ||
		!strings.Contains(body, "Generation #8") || !strings.Contains(body, "Older generations") ||
		!strings.Contains(body, "resolved_model_unavailable=true") {
		t.Fatalf("usage status=%d query=%+v body=%s", response.Code, storage.usageQuery, body)
	}
	unavailable := request(t, handler, http.MethodGet, "/usage?configured_model=configured-unresolved&resolved_model_unavailable=true")
	if unavailable.Code != http.StatusOK || !storage.usageQuery.ResolvedModelUnavailable || storage.usageQuery.ResolvedModel != "" ||
		!strings.Contains(unavailable.Body.String(), "Resolved model unavailable") {
		t.Fatalf("unavailable model status=%d query=%+v body=%s", unavailable.Code, storage.usageQuery, unavailable.Body.String())
	}
	for _, path := range []string{
		"/usage?window=year", "/usage?kind=other", "/usage?project_id=0", "/usage?before=-1",
		"/usage?kind=review&kind=feedback", "/usage?resolved_model_unavailable=false",
		"/usage?resolved_model=resolved&resolved_model_unavailable=true",
	} {
		invalid := request(t, handler, http.MethodGet, path)
		if invalid.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status=%d", path, invalid.Code)
		}
	}
	detail := request(t, handler, http.MethodGet, "/usage/8")
	detailBody := detail.Body.String()
	if detail.Code != http.StatusOK || storage.generationID != 8 ||
		!strings.Contains(detailBody, "Request metadata") || !strings.Contains(detailBody, "read_repository_file") ||
		!strings.Contains(detailBody, "Cached input") || !strings.Contains(detailBody, "145") ||
		strings.Contains(detailBody, "Estimated cost") || strings.Contains(detailBody, "USD") {
		t.Fatalf("generation detail status=%d body=%s", detail.Code, detailBody)
	}
	if post := request(t, handler, http.MethodPost, "/usage"); post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /usage status=%d", post.Code)
	}
}

func TestDiagnosticConversationViewsAreBoundedEscapedAndCorrelated(t *testing.T) {
	now := time.Now().UTC()
	turn := 0
	completed := now.Add(time.Second)
	latency := int64(1000)
	generation := store.GenerationRecord{
		ID: 1, RequestKind: "review", ReviewJobID: 9, WorkflowAttempt: 2, ReviewTurn: &turn,
		ConfiguredModel: "gemini-test", ResolvedModel: "resolved-test", RequestStartedAt: now,
		CompletedAt: &completed, CompletionState: "response", LatencyMS: &latency,
		FinishReason: "STOP", StructuredValidation: "valid", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7,
	}
	storage := &fakeStore{generation: generation, reviewGenerations: []store.GenerationRecord{generation}}
	recorder := diagnostics.New(true, []string{"configured-secret"})
	observed := diagnostics.ObserveGenerations(&panelUsageRecorder{}, recorder)
	ctx := usage.WithScope(context.Background(), usage.Scope{RequestKind: usage.RequestReview, ReviewJobID: 9, Attempt: 2})
	generationID, err := observed.Start(ctx, usage.GenerationStart{
		Scope: usage.Scope{RequestKind: usage.RequestReview, ReviewJobID: 9, Attempt: 2},
		Turn:  &turn, ConfiguredModel: "gemini-test", StartedAt: now,
	})
	if err != nil || generationID != 1 {
		t.Fatalf("generation start = %d, %v", generationID, err)
	}
	recorder.BeginConversation(ctx, diagnostics.ConversationStart{
		GenerationID: generationID, ProjectID: 42, ProjectPath: "group/project", MergeRequestID: 7,
		SystemInstruction: "system <b>plain</b>", Prompt: "configured-secret",
	})
	recorder.RecordModelTurn(ctx, diagnostics.ModelTurn{
		GenerationID: generationID, ReviewTurn: &turn, Text: "response <script>alert(1)</script> **markdown**",
	})
	if err := observed.Complete(ctx, generationID, usage.GenerationCompletion{
		State: usage.CompletionResponse, CompletedAt: completed, Latency: time.Second,
		ResolvedModel: "resolved-test", FinishReason: "STOP", StructuredValidation: "valid",
	}); err != nil {
		t.Fatal(err)
	}
	handler := newTestHandlerWithDiagnostics(t, storage, recorder)

	list := request(t, handler, http.MethodGet, "/diagnostics/conversations?kind=review&project_id=42&merge_request_iid=7&generation_id=1")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "review conversation · attempt 2") ||
		!strings.Contains(list.Body.String(), "Content captured") || !strings.Contains(list.Body.String(), "/reviews/9") {
		t.Fatalf("conversation list status=%d body=%s", list.Code, list.Body.String())
	}
	detail := request(t, handler, http.MethodGet, "/diagnostics/conversations/1")
	body := detail.Body.String()
	if detail.Code != http.StatusOK || !strings.Contains(body, "system &lt;b&gt;plain&lt;/b&gt;") ||
		!strings.Contains(body, "response &lt;script&gt;alert(1)&lt;/script&gt; **markdown**") ||
		!strings.Contains(body, "[redacted sensitive content]") || strings.Contains(body, "<script>") ||
		!strings.Contains(body, "Generation #1") || !strings.Contains(body, "/usage/1") {
		t.Fatalf("conversation detail status=%d body=%s", detail.Code, body)
	}
	for _, target := range []string{
		"/diagnostics/conversations?kind=other", "/diagnostics/conversations?project_id=0",
		"/diagnostics/conversations?generation_id=1&generation_id=2",
	} {
		if response := request(t, handler, http.MethodGet, target); response.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status=%d", target, response.Code)
		}
	}
	if response := request(t, handler, http.MethodPost, "/diagnostics/conversations"); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST conversations status=%d", response.Code)
	}
	assertPanelHeaders(t, detail)
}

func TestConversationCorrelationSplitsResetWorkflowAttemptNumbers(t *testing.T) {
	turn0, turn1 := 0, 1
	records := []store.GenerationRecord{
		{ID: 4, RequestKind: "review", ReviewJobID: 9, WorkflowAttempt: 1, ReviewTurn: &turn1},
		{ID: 3, RequestKind: "review", ReviewJobID: 9, WorkflowAttempt: 1, ReviewTurn: &turn0},
		{ID: 2, RequestKind: "review", ReviewJobID: 9, WorkflowAttempt: 1, ReviewTurn: &turn1},
		{ID: 1, RequestKind: "review", ReviewJobID: 9, WorkflowAttempt: 1, ReviewTurn: &turn0},
	}
	handler := &Handler{store: &fakeStore{reviewGenerations: records}}
	for _, test := range []struct {
		target int
		want   []int64
	}{
		{target: 2, want: []int64{1, 2}},
		{target: 0, want: []int64{3, 4}},
	} {
		got, err := handler.workflowAttemptGenerations(context.Background(), records[test.target], diagnostics.Conversation{}, false)
		if err != nil || len(got) != len(test.want) {
			t.Fatalf("target=%d generations=%+v error=%v", records[test.target].ID, got, err)
		}
		for index := range got {
			if got[index].ID != test.want[index] {
				t.Fatalf("target=%d generations=%+v want=%v", records[test.target].ID, got, test.want)
			}
		}
	}
}

func TestDiagnosticLogsKeepContentInertAndSupportStructuredFilters(t *testing.T) {
	storage := &fakeStore{}
	recorder := diagnostics.New(true, nil)
	logger := slog.New(diagnostics.NewTeeHandler(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}), recorder)).With(
		"component", "review", "job_kind", "review", "project_id", int64(42),
		"merge_request_iid", int64(7), "generation_id", int64(3),
	)
	logger.Info("hostile <img src=x> **markdown**", "source", "https://model.example/private")
	logger.Debug("Gemini review prompt", "prompt", "private prompt content")
	handler := newTestHandlerWithDiagnostics(t, storage, recorder)

	response := request(t, handler, http.MethodGet, "/diagnostics/logs?level=info&component=review&kind=review&project_id=42&merge_request_iid=7&generation_id=3")
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "hostile &lt;img src=x&gt; **markdown**") ||
		!strings.Contains(body, "https://model.example/private") || strings.Contains(body, `href="https://model.example/private"`) ||
		!strings.Contains(body, "Generation #3") || !strings.Contains(body, "Buffer started") {
		t.Fatalf("logs status=%d body=%s", response.Code, body)
	}
	all := request(t, handler, http.MethodGet, "/diagnostics/logs")
	if strings.Contains(all.Body.String(), "private prompt content") || !strings.Contains(all.Body.String(), "see correlated conversation") {
		t.Fatalf("content-bearing debug log was duplicated: %s", all.Body.String())
	}
	for _, target := range []string{
		"/diagnostics/logs?level=trace", "/diagnostics/logs?before=0", "/diagnostics/logs?component=a&component=b",
	} {
		if invalid := request(t, handler, http.MethodGet, target); invalid.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status=%d", target, invalid.Code)
		}
	}
	if post := request(t, handler, http.MethodPost, "/diagnostics/logs"); post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST logs status=%d", post.Code)
	}
}

func TestFeedbackDetailShowsGenerationHistory(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	storage := &fakeStore{feedbackDetail: store.FeedbackRecordDetail{
		FeedbackRecord: store.FeedbackRecord{
			ID: 3, ReviewJobID: 9, ReviewID: "WT-R-" + strings.Repeat("A", 26),
			ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
			HeadSHA: strings.Repeat("b", 40), TerminalState: "closed",
			State: store.FeedbackCompleted, AttemptCount: 2, ReceivedAt: now, UpdatedAt: now,
		},
		Generations: []store.GenerationRecord{{
			ID: 12, RequestKind: "feedback", FeedbackJobID: 3, WorkflowAttempt: 2,
			ConfiguredModel: "gemini-test", RequestStartedAt: now, CompletionState: "failed",
			StructuredValidation: "request_failed", ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		}},
	}}
	handler := newTestHandler(t, storage)
	response := request(t, handler, http.MethodGet, "/feedback/3")
	body := response.Body.String()
	if response.Code != http.StatusOK || storage.feedbackDetailID != 3 ||
		!strings.Contains(body, "Memory synthesis record") || !strings.Contains(body, "Generation #12") ||
		!strings.Contains(body, "request_failed") || !strings.Contains(body, "/reviews/9") {
		t.Fatalf("feedback detail status=%d body=%s", response.Code, body)
	}
	storage.feedbackDetailErr = store.ErrFeedbackRecordNotFound
	if missing := request(t, handler, http.MethodGet, "/feedback/404"); missing.Code != http.StatusNotFound {
		t.Fatalf("missing feedback status=%d", missing.Code)
	}
}

func TestPanelErrorsAreGenericAndStylesheetIsEmbedded(t *testing.T) {
	storage := &fakeStore{dashboardErr: errors.New("private database error with gitlab-token")}
	handler := newTestHandler(t, storage)
	response := request(t, handler, http.MethodGet, "/")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private") ||
		strings.Contains(response.Body.String(), "gitlab-token") {
		t.Fatalf("internal error status=%d body=%s", response.Code, response.Body.String())
	}
	css := request(t, handler, http.MethodGet, "/assets/panel.css")
	if css.Code != http.StatusOK || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") ||
		!strings.Contains(css.Body.String(), "--background") ||
		!strings.Contains(css.Body.String(), ":focus-visible") ||
		!strings.Contains(css.Body.String(), "prefers-reduced-motion") {
		t.Fatalf("stylesheet status=%d body=%s", css.Code, css.Body.String())
	}
	if css.Header().Get("Cache-Control") != "public, max-age=3600" {
		t.Fatalf("stylesheet cache control = %q", css.Header().Get("Cache-Control"))
	}
}

func newTestHandler(t *testing.T, storage Store) http.Handler {
	t.Helper()
	return newTestHandlerWithDiagnostics(t, storage, diagnostics.New(false, nil))
}

func newTestHandlerWithDiagnostics(t *testing.T, storage Store, diagnosticReader Diagnostics) http.Handler {
	t.Helper()
	handler, err := New(storage, Config{
		GitLabBaseURL:  "http://gitlab.internal",
		GeminiEndpoint: "http://gemini.internal",
		GeminiModel:    "gemini-test", GeminiThinkingLevel: "high", LogLevel: "info",
		AuthorizedRepositories:   []string{"group/project", "group/shared"},
		RepositorySharing:        map[string][]string{"group/project": {"group/shared"}},
		AllowedPublicDomains:     []string{"github.com", "docs.example.com"},
		PublicGitHubRepositories: []string{"owner/repository"},
	}, slog.New(slog.DiscardHandler), diagnosticReader)
	if err != nil {
		t.Fatal(err)
	}
	return handler.Routes()
}

func request(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, target, nil))
	return response
}

func assertPanelHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	for _, header := range []string{"Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if response.Header().Get(header) == "" {
			t.Fatalf("response lacks %s", header)
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control = %q", response.Header().Get("Cache-Control"))
	}
}

type fakeStore struct {
	dashboard      store.Dashboard
	dashboardErr   error
	dashboardLimit int

	reviewPage   store.ReviewRecordsPage
	reviewState  string
	reviewBefore int64
	reviewLimit  int

	detail    store.ReviewRecordDetail
	detailID  int64
	detailErr error

	feedbackPage      store.FeedbackRecordsPage
	feedbackState     string
	feedbackBefore    int64
	feedbackLimit     int
	feedbackDetail    store.FeedbackRecordDetail
	feedbackDetailID  int64
	feedbackDetailErr error

	usageReport         store.UsageReport
	usageQuery          store.UsageQuery
	usageErr            error
	generation          store.GenerationRecord
	generationID        int64
	generationErr       error
	reviewGenerations   []store.GenerationRecord
	feedbackGenerations []store.GenerationRecord

	memoryPage   store.MemoryRecordsPage
	memoryBefore int64
	memoryLimit  int
}

func (s *fakeStore) ReadDashboard(_ context.Context, limit int) (store.Dashboard, error) {
	s.dashboardLimit = limit
	return s.dashboard, s.dashboardErr
}

func (s *fakeStore) ListReviewRecords(_ context.Context, state string, before int64, limit int) (store.ReviewRecordsPage, error) {
	s.reviewState, s.reviewBefore, s.reviewLimit = state, before, limit
	return s.reviewPage, nil
}

func (s *fakeStore) GetReviewRecord(_ context.Context, jobID int64) (store.ReviewRecordDetail, error) {
	s.detailID = jobID
	return s.detail, s.detailErr
}

func (s *fakeStore) ListFeedbackRecords(_ context.Context, state string, before int64, limit int) (store.FeedbackRecordsPage, error) {
	s.feedbackState, s.feedbackBefore, s.feedbackLimit = state, before, limit
	return s.feedbackPage, nil
}

func (s *fakeStore) GetFeedbackRecord(_ context.Context, jobID int64) (store.FeedbackRecordDetail, error) {
	s.feedbackDetailID = jobID
	return s.feedbackDetail, s.feedbackDetailErr
}

func (s *fakeStore) ReadUsageReport(_ context.Context, query store.UsageQuery) (store.UsageReport, error) {
	s.usageQuery = query
	return s.usageReport, s.usageErr
}

func (s *fakeStore) GetGenerationRecord(_ context.Context, generationID int64) (store.GenerationRecord, error) {
	s.generationID = generationID
	return s.generation, s.generationErr
}

func (s *fakeStore) ListReviewGenerations(_ context.Context, _ int64, _ int) ([]store.GenerationRecord, bool, error) {
	return s.reviewGenerations, false, nil
}

func (s *fakeStore) ListFeedbackGenerations(_ context.Context, _ int64, _ int) ([]store.GenerationRecord, bool, error) {
	return s.feedbackGenerations, false, nil
}

func (s *fakeStore) ListMemoryRecords(_ context.Context, before int64, limit int) (store.MemoryRecordsPage, error) {
	s.memoryBefore, s.memoryLimit = before, limit
	return s.memoryPage, nil
}

type panelUsageRecorder struct {
	nextID int64
}

func (r *panelUsageRecorder) Start(_ context.Context, _ usage.GenerationStart) (int64, error) {
	r.nextID++
	return r.nextID, nil
}

func (r *panelUsageRecorder) Complete(_ context.Context, _ int64, _ usage.GenerationCompletion) error {
	return nil
}
