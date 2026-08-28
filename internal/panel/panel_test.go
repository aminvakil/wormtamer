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

	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
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
		!strings.Contains(body, "gemini-test") || !strings.Contains(body, "Current repository only") ||
		!strings.Contains(body, "Review tools") || !strings.Contains(body, "<code>read</code>") ||
		!strings.Contains(body, "<code>bash</code>") || !strings.Contains(body, "group/shared") {
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

func TestOverviewReportsAllAuthorizedRepositorySharing(t *testing.T) {
	handler, err := New(&fakeStore{}, Config{
		GitLabBaseURL:                  "http://gitlab.internal",
		AuthorizedRepositories:         []string{"group/project", "group/shared"},
		ShareAllAuthorizedRepositories: true,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, handler.Routes(), http.MethodGet, "/")
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "All authorized repositories") ||
		strings.Contains(body, "Current repository only") || strings.Contains(body, "Directional rules") {
		t.Fatalf("overview status=%d body=%s", response.Code, body)
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

func TestRemovedDiagnosticRoutesReturnNotFound(t *testing.T) {
	handler := newTestHandler(t, &fakeStore{})
	for _, target := range []string{
		"/usage",
		"/usage/1",
		"/diagnostics/conversations",
		"/diagnostics/conversations/1",
		"/diagnostics/logs",
	} {
		response := request(t, handler, http.MethodGet, target)
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s status=%d body=%s", target, response.Code, response.Body.String())
		}
		assertPanelHeaders(t, response)
	}
}

func TestFeedbackDetailShowsWorkflowRecord(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	storage := &fakeStore{feedbackDetail: store.FeedbackRecord{
		ID: 3, ReviewJobID: 9, ReviewID: "WT-R-" + strings.Repeat("A", 26),
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		HeadSHA: strings.Repeat("b", 40), TerminalState: "closed",
		State: store.FeedbackCompleted, AttemptCount: 2, ReceivedAt: now, UpdatedAt: now,
	}}
	handler := newTestHandler(t, storage)
	response := request(t, handler, http.MethodGet, "/feedback/3")
	body := response.Body.String()
	if response.Code != http.StatusOK || storage.feedbackDetailID != 3 ||
		!strings.Contains(body, "Memory synthesis record") || !strings.Contains(body, "/reviews/9") ||
		strings.Contains(body, "Generation history") {
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
	handler, err := New(storage, Config{
		GitLabBaseURL:  "http://gitlab.internal",
		GeminiEndpoint: "http://gemini.internal",
		GeminiModel:    "gemini-test", GeminiThinkingLevel: "high", LogLevel: "info",
		AuthorizedRepositories: []string{"group/project", "group/shared"},
	}, slog.New(slog.DiscardHandler))
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
	feedbackDetail    store.FeedbackRecord
	feedbackDetailID  int64
	feedbackDetailErr error

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

func (s *fakeStore) GetFeedbackRecord(_ context.Context, jobID int64) (store.FeedbackRecord, error) {
	s.feedbackDetailID = jobID
	return s.feedbackDetail, s.feedbackDetailErr
}

func (s *fakeStore) ListMemoryRecords(_ context.Context, before int64, limit int) (store.MemoryRecordsPage, error) {
	s.memoryBefore, s.memoryLimit = before, limit
	return s.memoryPage, nil
}
