package webhook

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/store"
	_ "github.com/mattn/go-sqlite3"
)

const (
	testSecret = "webhook-secret"
	headA      = "0123456789abcdef0123456789abcdef01234567"
	headB      = "1123456789abcdef0123456789abcdef01234567"
	headC      = "2123456789abcdef0123456789abcdef01234567"
	headD      = "3123456789abcdef0123456789abcdef01234567"
)

func TestWebhookIngress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	handler := newTestHandler(storage, &logs).Routes()

	ready := payload("group/project", "open", false, false, headA)
	assertWebhookStatus(t, handler, ready, testSecret, "event-1", http.StatusOK)
	assertWebhookStatus(t, handler, ready, testSecret, "event-1", http.StatusOK)
	assertWebhookStatus(t, handler, ready, testSecret, "event-2", http.StatusOK)
	assertWebhookStatus(t, handler, payload("group/project", "open", true, false, headB), testSecret, "event-3", http.StatusOK)
	assertWebhookStatus(t, handler, payload("group/project", "open", false, true, headC), testSecret, "event-4", http.StatusOK)
	assertWebhookStatus(t, handler, payload("group/project", "update", false, false, headD), testSecret, "event-5", http.StatusOK)
	assertWebhookStatus(t, handler, ready, testSecret, "", http.StatusOK)
	assertWebhookStatus(t, handler, ready, testSecret, "", http.StatusOK)

	assertWebhookStatus(t, handler, ready, "wrong-secret", "rejected-1", http.StatusUnauthorized)
	assertWebhookStatus(t, handler, payload("other/project", "open", false, false, headA), testSecret, "rejected-2", http.StatusForbidden)
	assertWebhookStatus(t, handler, []byte(`{"object_kind":`), testSecret, "rejected-3", http.StatusBadRequest)

	request := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", strings.NewReader(strings.Repeat("x", maxBodyBytes+1)))
	request.Header.Set("X-Gitlab-Token", testSecret)
	request.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized webhook status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}

	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertDatabaseCount(t, db, "webhook_events", 6)
	assertDatabaseCount(t, db, "review_jobs", 1)
	assertOutcomeCount(t, db, store.OutcomeQueued, 1)
	assertOutcomeCount(t, db, store.OutcomeDuplicateReview, 2)
	assertOutcomeCount(t, db, store.OutcomeIgnoredDraft, 2)
	assertOutcomeCount(t, db, store.OutcomeIgnoredAction, 1)

	logOutput := logs.String()
	if strings.Contains(logOutput, testSecret) || strings.Contains(logOutput, string(ready)) {
		t.Fatalf("logs contain a secret or payload: %s", logOutput)
	}
	for _, field := range []string{"delivery_id", "project_id", "project_path", "merge_request_iid", "head_sha", "job_id", "outcome"} {
		if !strings.Contains(logOutput, `"`+field+`"`) {
			t.Errorf("accepted log does not contain %q: %s", field, logOutput)
		}
	}
}

func TestNoteHookDoesNotScheduleMemory(t *testing.T) {
	storage := &recordingStore{}
	var logs bytes.Buffer
	handler := newTestHandler(storage, &logs).Routes()
	body := notePayloadJSON("group/project", "create", false, false, "sensitive comment text")
	request := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
	request.Header.Set("X-Gitlab-Token", testSecret)
	request.Header.Set("X-Gitlab-Event", "Note Hook")
	request.Header.Set("X-Gitlab-Event-UUID", "note-event")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || storage.calls != 0 {
		t.Fatalf("note hook status=%d store calls=%d", response.Code, storage.calls)
	}
	if strings.Contains(logs.String(), "sensitive comment text") {
		t.Fatalf("note body was logged: %s", logs.String())
	}
}

func TestClosedAndMergedHooksScheduleMemoryEvaluation(t *testing.T) {
	for _, test := range []struct {
		action string
		state  string
	}{{"close", "closed"}, {"merge", "merged"}} {
		t.Run(test.action, func(t *testing.T) {
			storage := &recordingStore{}
			handler := newTestHandler(storage, io.Discard).Routes()
			assertWebhookStatus(t, handler, payload("group/project", test.action, false, false, headA), testSecret, test.action, http.StatusOK)
			if storage.calls != 1 || !storage.event.QueueFeedback || storage.event.QueueReview || storage.event.TerminalState != test.state {
				t.Fatalf("stored event = %+v", storage.event)
			}
		})
	}
}

func TestAuthenticationHappensBeforeReadingBody(t *testing.T) {
	storage := &recordingStore{}
	handler := newTestHandler(storage, io.Discard).Routes()
	body := &trackingBody{}
	request := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", nil)
	request.Body = body
	request.Header.Set("X-Gitlab-Token", "wrong-secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if body.read {
		t.Fatal("request body was read before authentication")
	}
	if storage.calls != 0 {
		t.Fatalf("store calls = %d, want 0", storage.calls)
	}
}

func TestWebhookAdmissionRejectsExcessBeforeReadingBody(t *testing.T) {
	storage := &blockingStore{
		started: make(chan struct{}, maxConcurrentWebhooks),
		release: make(chan struct{}),
	}
	handler := newTestHandler(storage, io.Discard).Routes()
	responses := make(chan int, maxConcurrentWebhooks)
	for index := 0; index < maxConcurrentWebhooks; index++ {
		go func(index int) {
			request := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(payload("group/project", "open", false, false, headA)))
			request.Header.Set("X-Gitlab-Token", testSecret)
			request.Header.Set("X-Gitlab-Event", "Merge Request Hook")
			request.Header.Set("X-Gitlab-Event-UUID", fmt.Sprintf("admitted-%d", index))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			responses <- response.Code
		}(index)
	}
	for index := 0; index < maxConcurrentWebhooks; index++ {
		select {
		case <-storage.started:
		case <-time.After(5 * time.Second):
			t.Fatal("admitted request did not reach persistence")
		}
	}

	body := &trackingBody{}
	request := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", nil)
	request.Body = body
	request.Header.Set("X-Gitlab-Token", testSecret)
	request.Header.Set("X-Gitlab-Event-UUID", "overloaded-delivery")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("overloaded status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if response.Header().Get("Retry-After") != overloadRetryAfterSecond {
		t.Fatalf("Retry-After = %q", response.Header().Get("Retry-After"))
	}
	if body.read {
		t.Fatal("overloaded request body was read")
	}

	close(storage.release)
	for index := 0; index < maxConcurrentWebhooks; index++ {
		select {
		case status := <-responses:
			if status != http.StatusOK {
				t.Fatalf("admitted status = %d, want %d", status, http.StatusOK)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("admitted request did not finish")
		}
	}
}

func TestAuthenticatedRejectionLogsDeliveryID(t *testing.T) {
	storage := &recordingStore{}
	var logs bytes.Buffer
	handler := newTestHandler(storage, &logs).Routes()

	assertWebhookStatus(t, handler, []byte(`{"object_kind":`), testSecret, "malformed-delivery", http.StatusBadRequest)
	unsupportedBody := []byte(`{"object_kind":"push"}`)
	request := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(unsupportedBody))
	request.Header.Set("X-Gitlab-Token", testSecret)
	request.Header.Set("X-Gitlab-Event", "Push Hook")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unsupported status = %d, want %d", response.Code, http.StatusBadRequest)
	}

	logOutput := logs.String()
	if !strings.Contains(logOutput, `"delivery_id":"uuid:malformed-delivery"`) {
		t.Fatalf("malformed JSON log lacks UUID delivery ID: %s", logOutput)
	}
	fallback := deliveryID("http://gitlab.internal", "", unsupportedBody)
	if !strings.Contains(logOutput, `"delivery_id":"`+fallback+`"`) {
		t.Fatalf("unsupported event log lacks fallback delivery ID: %s", logOutput)
	}
	if strings.Contains(logOutput, string(unsupportedBody)) {
		t.Fatalf("rejection log contains payload: %s", logOutput)
	}
}

func TestMalformedAndUnsupportedInputs(t *testing.T) {
	storage := &recordingStore{}
	handler := newTestHandler(storage, io.Discard).Routes()
	tests := []struct {
		name        string
		eventHeader string
		body        []byte
	}{
		{name: "missing event header", body: payload("group/project", "open", false, false, headA)},
		{name: "wrong object kind", eventHeader: "Merge Request Hook", body: []byte(`{"object_kind":"push"}`)},
		{name: "missing project ID", eventHeader: "Merge Request Hook", body: []byte(fmt.Sprintf(`{"object_kind":"merge_request","project":{"path_with_namespace":"group/project"},"object_attributes":{"iid":7,"action":"open","last_commit":{"id":%q}}}`, headA))},
		{name: "invalid head SHA", eventHeader: "Merge Request Hook", body: payload("group/project", "open", false, false, "not-a-sha")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(test.body))
			request.Header.Set("X-Gitlab-Token", testSecret)
			if test.eventHeader != "" {
				request.Header.Set("X-Gitlab-Event", test.eventHeader)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
			}
		})
	}
	if storage.calls != 0 {
		t.Fatalf("store calls = %d, want 0", storage.calls)
	}
}

func TestSQLiteCommitFailureReturnsServerError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	admin, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Exec(`
CREATE TABLE commit_failure (
    id INTEGER PRIMARY KEY,
    missing_event_id INTEGER NOT NULL,
    FOREIGN KEY (missing_event_id) REFERENCES webhook_events(id) DEFERRABLE INITIALLY DEFERRED
);
CREATE TRIGGER force_commit_failure AFTER INSERT ON webhook_events
BEGIN
    INSERT INTO commit_failure (missing_event_id) VALUES (-1);
END;`)
	admin.Close()
	if err != nil {
		t.Fatal(err)
	}

	marker := "sensitive-payload-marker"
	var logs bytes.Buffer
	handler := newTestHandler(storage, &logs).Routes()
	assertWebhookStatus(t, handler, payloadWithExtraField(marker), testSecret, "event-failure", http.StatusInternalServerError)

	if strings.Contains(logs.String(), marker) || strings.Contains(logs.String(), testSecret) {
		t.Fatalf("failure log contains untrusted or sensitive content: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "persistence_failed") {
		t.Fatalf("failure log lacks sanitized reason: %s", logs.String())
	}

	check, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	assertDatabaseCount(t, check, "webhook_events", 0)
	assertDatabaseCount(t, check, "review_jobs", 0)
}

func TestHealthcheck(t *testing.T) {
	handler := newTestHandler(&recordingStore{}, io.Discard).Routes()
	request := httptest.NewRequest(http.MethodGet, "/healthcheck", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
		t.Fatalf("healthcheck = status %d body %q", response.Code, response.Body.String())
	}
}

func TestDeliveryIDFallbackIsDeterministicAndInstanceScoped(t *testing.T) {
	body := []byte(`{"object_kind":"merge_request"}`)
	first := deliveryID("http://gitlab.one", "", body)
	if first != deliveryID("http://gitlab.one", "", body) {
		t.Fatal("fallback delivery ID is not deterministic")
	}
	if first == deliveryID("http://gitlab.two", "", body) {
		t.Fatal("fallback delivery ID is not scoped to the GitLab instance")
	}
	if got := deliveryID("http://gitlab.one", "uuid-value", body); got != "uuid:uuid-value" {
		t.Fatalf("UUID delivery ID = %q", got)
	}
}

func newTestHandler(storage EventStore, output io.Writer) *Handler {
	logger := slog.New(slog.NewJSONHandler(output, nil))
	return New(Config{
		GitLabInstance:         "http://gitlab.internal",
		WebhookSecret:          testSecret,
		AuthorizedRepositories: []string{"group/project"},
	}, storage, logger)
}

func assertWebhookStatus(t *testing.T, handler http.Handler, body []byte, secret, eventUUID string, want int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
	request.Header.Set("X-Gitlab-Token", secret)
	request.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	if eventUUID != "" {
		request.Header.Set("X-Gitlab-Event-UUID", eventUUID)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("webhook status = %d, want %d; body = %q", response.Code, want, response.Body.String())
	}
}

func payload(projectPath, action string, draft, workInProgress bool, headSHA string) []byte {
	return []byte(fmt.Sprintf(`{
  "object_kind":"merge_request",
  "project":{"id":42,"path_with_namespace":%q},
  "object_attributes":{
    "iid":7,
    "action":%q,
    "draft":%t,
    "work_in_progress":%t,
    "last_commit":{"id":%q}
  }
}`, projectPath, action, draft, workInProgress, headSHA))
}

func notePayloadJSON(projectPath, action string, system, internal bool, comment string) []byte {
	return []byte(fmt.Sprintf(`{
  "object_kind":"note",
  "event_type":"note",
  "user":{"id":12},
  "project":{"id":42,"path_with_namespace":%q},
  "object_attributes":{
    "id":91,
    "internal":%t,
    "note":%q,
    "noteable_type":"MergeRequest",
    "author_id":12,
    "updated_at":"2026-07-31T12:00:00Z",
    "project_id":42,
    "noteable_id":70,
    "system":%t,
    "action":%q
  },
  "merge_request":{"id":70,"iid":7,"target_project_id":42}
}`, projectPath, internal, comment, system, action))
}

func payloadWithExtraField(marker string) []byte {
	base := string(payload("group/project", "open", false, false, headA))
	return []byte(strings.Replace(base, `"object_kind":"merge_request"`, `"marker":`+fmt.Sprintf("%q", marker)+`,"object_kind":"merge_request"`, 1))
}

type trackingBody struct {
	read bool
}

func (b *trackingBody) Read(_ []byte) (int, error) {
	b.read = true
	return 0, io.EOF
}

func (b *trackingBody) Close() error {
	return nil
}

type blockingStore struct {
	started chan struct{}
	release chan struct{}
}

func (s *blockingStore) AcceptEvent(_ context.Context, _ store.Event) (store.AcceptResult, error) {
	s.started <- struct{}{}
	<-s.release
	return store.AcceptResult{Outcome: store.OutcomeQueued}, nil
}

type recordingStore struct {
	calls int
	event store.Event
}

func (s *recordingStore) AcceptEvent(_ context.Context, event store.Event) (store.AcceptResult, error) {
	s.calls++
	s.event = event
	return store.AcceptResult{}, nil
}

func assertDatabaseCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}

func assertOutcomeCount(t *testing.T, db *sql.DB, outcome string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT count(*) FROM webhook_events WHERE outcome = ?`, outcome).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("outcome %q count = %d, want %d", outcome, got, want)
	}
}
