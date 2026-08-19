package diagnostics

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/usage"
)

func TestConversationRecorderKeepsOrderedRedactedContentAndUsageMetadata(t *testing.T) {
	recorder := New(true, []string{"configured-secret"})
	durable := &fakeGenerationRecorder{nextID: 40}
	observed := ObserveGenerations(durable, recorder)
	ctx := usage.WithScope(context.Background(), usage.Scope{
		RequestKind: usage.RequestReview, ReviewJobID: 9, Attempt: 2,
	})
	turn := 0
	started := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	generationID, err := observed.Start(ctx, usage.GenerationStart{
		Scope: usage.Scope{RequestKind: usage.RequestReview, ReviewJobID: 9, Attempt: 2},
		Turn:  &turn, ConfiguredModel: "gemini-test", StartedAt: started,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder.BeginConversation(ctx, ConversationStart{
		GenerationID: generationID, ProjectID: 42, ProjectPath: "group/project",
		MergeRequestID: 7, SystemInstruction: "system", Prompt: `prompt configured-secret`,
	})
	recorder.RecordModelTurn(ctx, ModelTurn{GenerationID: generationID, ReviewTurn: &turn, Calls: []FunctionCall{{
		ID: "call-1", Name: "read_repository_file", Arguments: `{"path":"README.md"}`,
	}}})
	recorder.RecordToolResponses(ctx, generationID, &turn, []ToolResponse{{
		ID: "call-1", Name: "read_repository_file", Response: `{"content":"safe"}`,
	}})
	completed := started.Add(time.Second)
	if err := observed.Complete(ctx, generationID, usage.GenerationCompletion{
		State: usage.CompletionResponse, CompletedAt: completed, Latency: time.Second,
		ResolvedModel: "gemini-resolved", FinishReason: "STOP", StructuredValidation: "not_final",
	}); err != nil {
		t.Fatal(err)
	}

	snapshot := recorder.Conversations(ConversationFilter{GenerationID: generationID})
	if len(snapshot.Conversations) != 1 {
		t.Fatalf("conversations = %+v", snapshot.Conversations)
	}
	conversation, ok := recorder.ConversationByGeneration(generationID)
	if !ok {
		t.Fatal("generation did not resolve to its conversation")
	}
	if conversation.JobKind != usage.RequestReview || conversation.ReviewJobID != 9 ||
		conversation.WorkflowAttempt != 2 || conversation.ProjectID != 42 || conversation.MergeRequestID != 7 ||
		conversation.InitialPrompt != "[redacted sensitive content]" || !conversation.ContentCaptured ||
		len(conversation.Events) != 2 || conversation.Events[0].Kind != "model" || conversation.Events[1].Kind != "tool" {
		t.Fatalf("conversation = %+v", conversation)
	}
	if len(conversation.Generations) != 1 || conversation.Generations[0].ID != generationID ||
		conversation.Generations[0].CompletionState != usage.CompletionResponse ||
		conversation.Generations[0].ResolvedModel != "gemini-resolved" {
		t.Fatalf("generation metadata = %+v", conversation.Generations)
	}
}

func TestConversationContentIsDebugOnlyAndOversizedRecordsBecomeTombstones(t *testing.T) {
	ctx := usage.WithScope(context.Background(), usage.Scope{
		RequestKind: usage.RequestFeedback, FeedbackJobID: 3, Attempt: 1,
	})
	start := usage.GenerationStart{
		Scope:           usage.Scope{RequestKind: usage.RequestFeedback, FeedbackJobID: 3, Attempt: 1},
		ConfiguredModel: "gemini-test", StartedAt: time.Now().UTC(),
	}

	disabled := New(false, nil)
	disabled.generationStarted(11, start)
	disabled.BeginConversation(ctx, ConversationStart{
		GenerationID: 11, ProjectID: 42, MergeRequestID: 7,
		SystemInstruction: "private system", Prompt: "private prompt",
	})
	conversation, ok := disabled.ConversationByGeneration(11)
	if !ok || conversation.ContentCaptured || conversation.SystemInstruction != "" || conversation.InitialPrompt != "" {
		t.Fatalf("disabled conversation = %+v, %v", conversation, ok)
	}

	limited := New(true, nil)
	limited.generationStarted(12, start)
	limited.BeginConversation(ctx, ConversationStart{
		GenerationID: 12, ProjectID: 42, MergeRequestID: 7,
		SystemInstruction: "system", Prompt: strings.Repeat("x", maxConversationRecordBytes+1),
	})
	conversation, ok = limited.ConversationByGeneration(12)
	if !ok || conversation.ContentOmitted != ContentOmittedLimitExceeded || conversation.ContentCaptured ||
		conversation.InitialPrompt != "" || len(conversation.Events) != 0 {
		t.Fatalf("oversized conversation = %+v, %v", conversation, ok)
	}
}

func TestRepeatedWorkflowAttemptNumbersStartDistinctConversations(t *testing.T) {
	recorder := New(true, nil)
	durable := &fakeGenerationRecorder{}
	observed := ObserveGenerations(durable, recorder)
	scope := usage.Scope{RequestKind: usage.RequestReview, ReviewJobID: 9, Attempt: 1}
	ctx := usage.WithScope(context.Background(), scope)
	turn := 0
	for invocation := 0; invocation < 2; invocation++ {
		generationID, err := observed.Start(ctx, usage.GenerationStart{
			Scope: scope, Turn: &turn, ConfiguredModel: "gemini-test", StartedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		recorder.BeginConversation(ctx, ConversationStart{
			GenerationID: generationID, ProjectID: 42, MergeRequestID: 7,
			SystemInstruction: "system", Prompt: "prompt",
		})
	}
	snapshot := recorder.Conversations(ConversationFilter{})
	if len(snapshot.Conversations) != 2 || len(snapshot.Conversations[0].Generations) != 1 ||
		len(snapshot.Conversations[1].Generations) != 1 {
		t.Fatalf("repeated attempts were merged: %+v", snapshot.Conversations)
	}
}

func TestConversationBufferEvictsOldestCompletedRecord(t *testing.T) {
	recorder := New(false, nil)
	for index := 1; index <= maxConversationEntries+1; index++ {
		recorder.generationStarted(int64(index), usage.GenerationStart{
			Scope:           usage.Scope{RequestKind: usage.RequestFeedback, FeedbackJobID: int64(index), Attempt: 1},
			ConfiguredModel: "gemini-test", StartedAt: time.Unix(int64(index), 0).UTC(),
		})
		recorder.generationCompleted(int64(index), completedGeneration(time.Unix(int64(index), 0).UTC()))
	}
	if _, ok := recorder.ConversationByGeneration(1); ok {
		t.Fatal("oldest completed conversation was not evicted")
	}
	if state := recorder.State(); state.ConversationEvictions != 1 {
		t.Fatalf("conversation evictions = %d", state.ConversationEvictions)
	}
}

func TestAggregateByteCeilingsEvictCompleteRecords(t *testing.T) {
	conversationRecorder := New(true, nil)
	conversationPayload := strings.Repeat("c", 1<<20)
	for index := 1; index <= 33; index++ {
		scope := usage.Scope{RequestKind: usage.RequestReview, ReviewJobID: int64(index), Attempt: 1}
		ctx := usage.WithScope(context.Background(), scope)
		conversationRecorder.generationStarted(int64(index), usage.GenerationStart{
			Scope: scope, ConfiguredModel: "gemini-test", StartedAt: time.Unix(int64(index), 0).UTC(),
		})
		conversationRecorder.BeginConversation(ctx, ConversationStart{
			GenerationID: int64(index), ProjectID: int64(index), MergeRequestID: 1,
			SystemInstruction: "system", Prompt: conversationPayload,
		})
		conversationRecorder.generationCompleted(int64(index), completedGeneration(time.Unix(int64(index), 0).UTC()))
	}
	conversationSnapshot := conversationRecorder.Conversations(ConversationFilter{})
	if len(conversationSnapshot.Conversations) >= 33 || conversationSnapshot.State.ConversationEvictions == 0 {
		t.Fatalf("conversations=%d evictions=%d", len(conversationSnapshot.Conversations), conversationSnapshot.State.ConversationEvictions)
	}
	for _, conversation := range conversationSnapshot.Conversations {
		if conversation.ContentOmitted != "" {
			t.Fatalf("aggregate ceiling turned a complete record into a tombstone: %+v", conversation)
		}
	}

	logRecorder := New(false, nil)
	logPayload := strings.Repeat("l", 8<<10)
	for index := 0; index < 1100; index++ {
		logRecorder.addLog(LogEvent{Timestamp: time.Now().UTC(), Level: "info", Message: logPayload})
	}
	logSnapshot := logRecorder.Logs(LogFilter{}, 0, maxLogEntries)
	if len(logSnapshot.Events) >= 1100 || logSnapshot.State.LogEvictions == 0 {
		t.Fatalf("logs=%d evictions=%d", len(logSnapshot.Events), logSnapshot.State.LogEvictions)
	}
	for _, event := range logSnapshot.Events {
		if event.ContentOmitted != "" {
			t.Fatalf("aggregate ceiling turned a complete event into a tombstone: %+v", event)
		}
	}
}

func TestSlogTeePreservesPrimaryOutputAndCapturesBoundedStructuredEvents(t *testing.T) {
	var stderr bytes.Buffer
	recorder := New(true, []string{"configured-secret"})
	primary := slog.NewJSONHandler(&stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewTeeHandler(primary, recorder)).With(
		"component", "review", "job_kind", "review",
		"project_id", int64(42), "merge_request_iid", int64(7), "generation_id", int64(51),
	).WithGroup("request")
	logger.Debug("Gemini review prompt",
		"label", "configured-secret",
		"system_instruction", "private system instruction",
		"prompt", "private prompt")

	if !strings.Contains(stderr.String(), `"prompt":"private prompt"`) ||
		!strings.Contains(stderr.String(), `"request":{"label"`) {
		t.Fatalf("primary output changed: %s", stderr.String())
	}
	buffered := recorder.logs[0]
	snapshot := recorder.Logs(LogFilter{
		Level: "debug", Component: "review", JobKind: "review",
		ProjectID: 42, MergeRequestID: 7, GenerationID: 51,
	}, 0, 10)
	if len(snapshot.Events) != 1 {
		t.Fatalf("captured events = %+v", snapshot.Events)
	}
	event := snapshot.Events[0]
	if event.Message != "Gemini review prompt" || event.Component != "review" || event.GenerationID != 51 {
		t.Fatalf("event = %+v", event)
	}
	for _, attribute := range buffered.Attributes[:cap(buffered.Attributes)] {
		if strings.Contains(attribute.Value, "private prompt") ||
			strings.Contains(attribute.Value, "private system instruction") ||
			strings.Contains(attribute.Value, "configured-secret") {
			t.Fatalf("panel event backing storage retained private content: %+v", event)
		}
	}
}

func TestLogBufferLimitsAndConcurrentCapture(t *testing.T) {
	recorder := New(false, nil)
	logger := slog.New(NewTeeHandler(slog.NewJSONHandler(io.Discard, nil), recorder))
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < 300; index++ {
				logger.Info("event", "worker", worker, "index", index)
			}
		}(worker)
	}
	wait.Wait()
	snapshot := recorder.Logs(LogFilter{}, 0, maxLogEntries)
	if len(snapshot.Events) > maxLogEntries || snapshot.State.LogEvictions == 0 {
		t.Fatalf("events=%d evictions=%d", len(snapshot.Events), snapshot.State.LogEvictions)
	}

	logger.With(
		"component", "review", "job_kind", "review", "project_id", int64(42),
		"merge_request_iid", int64(7), "generation_id", int64(99),
	).Info(strings.Repeat("x", maxLogRecordBytes+1))
	snapshot = recorder.Logs(LogFilter{}, 0, 1)
	if len(snapshot.Events) != 1 || snapshot.Events[0].ContentOmitted != ContentOmittedLimitExceeded ||
		snapshot.Events[0].Message != "" || snapshot.Events[0].Component != "review" ||
		snapshot.Events[0].ProjectID != 42 || snapshot.Events[0].MergeRequestID != 7 || snapshot.Events[0].GenerationID != 99 {
		t.Fatalf("oversized event = %+v", snapshot.Events)
	}
}

func TestActiveConversationSurvivesCompletedRecordPressure(t *testing.T) {
	recorder := New(false, nil)
	recorder.generationStarted(1, usage.GenerationStart{
		Scope:           usage.Scope{RequestKind: usage.RequestFeedback, FeedbackJobID: 1, Attempt: 1},
		ConfiguredModel: "gemini-test", StartedAt: time.Unix(1, 0).UTC(),
	})
	for index := 2; index <= maxConversationEntries+1; index++ {
		recorder.generationStarted(int64(index), usage.GenerationStart{
			Scope:           usage.Scope{RequestKind: usage.RequestFeedback, FeedbackJobID: int64(index), Attempt: 1},
			ConfiguredModel: "gemini-test", StartedAt: time.Unix(int64(index), 0).UTC(),
		})
		recorder.generationCompleted(int64(index), completedGeneration(time.Unix(int64(index), 0).UTC()))
	}
	conversation, ok := recorder.ConversationByGeneration(1)
	if !ok || !conversationActive(conversation) {
		t.Fatalf("active conversation was evicted: %+v, %v", conversation, ok)
	}
	if _, ok := recorder.ConversationByGeneration(2); ok {
		t.Fatal("oldest completed conversation was retained instead of the active conversation")
	}
}

func TestAllActivePressureDropsNewestConversation(t *testing.T) {
	recorder := New(false, nil)
	for index := 1; index <= maxConversationEntries+1; index++ {
		recorder.generationStarted(int64(index), usage.GenerationStart{
			Scope:           usage.Scope{RequestKind: usage.RequestFeedback, FeedbackJobID: int64(index), Attempt: 1},
			ConfiguredModel: "gemini-test", StartedAt: time.Unix(int64(index), 0).UTC(),
		})
	}
	if _, ok := recorder.ConversationByGeneration(1); !ok {
		t.Fatal("oldest active conversation was discarded")
	}
	if _, ok := recorder.ConversationByGeneration(maxConversationEntries + 1); ok {
		t.Fatal("newest active conversation survived impossible all-active pressure")
	}
}

func TestEvictionClearsBufferBackingStorage(t *testing.T) {
	conversationRecorder := New(false, nil)
	conversationRecorder.conversations = make([]*Conversation, 0, maxConversationEntries+1)
	for index := 1; index <= maxConversationEntries; index++ {
		conversationRecorder.generationStarted(int64(index), usage.GenerationStart{
			Scope:           usage.Scope{RequestKind: usage.RequestFeedback, FeedbackJobID: int64(index), Attempt: 1},
			ConfiguredModel: "gemini-test", StartedAt: time.Unix(int64(index), 0).UTC(),
		})
		conversationRecorder.generationCompleted(int64(index), completedGeneration(time.Unix(int64(index), 0).UTC()))
	}
	evictedConversation := conversationRecorder.conversations[0]
	conversationBacking := conversationRecorder.conversations[:cap(conversationRecorder.conversations)]
	conversationRecorder.generationStarted(maxConversationEntries+1, usage.GenerationStart{
		Scope:           usage.Scope{RequestKind: usage.RequestFeedback, FeedbackJobID: maxConversationEntries + 1, Attempt: 1},
		ConfiguredModel: "gemini-test", StartedAt: time.Unix(maxConversationEntries+1, 0).UTC(),
	})
	for _, conversation := range conversationBacking {
		if conversation == evictedConversation {
			t.Fatal("conversation backing storage retained an evicted record")
		}
	}

	logRecorder := New(false, nil)
	logRecorder.logs = make([]LogEvent, 0, maxLogEntries+1)
	logRecorder.addLog(LogEvent{Timestamp: time.Now().UTC(), Level: "info", Message: "evicted private log"})
	for index := 1; index < maxLogEntries; index++ {
		logRecorder.addLog(LogEvent{Timestamp: time.Now().UTC(), Level: "info", Message: "retained log"})
	}
	logBacking := logRecorder.logs[:cap(logRecorder.logs)]
	logRecorder.addLog(LogEvent{Timestamp: time.Now().UTC(), Level: "info", Message: "new log"})
	for _, event := range logBacking {
		if event.Message == "evicted private log" {
			t.Fatal("log backing storage retained an evicted event")
		}
	}
}

func TestSharedRedactionHandlesDirectAndJSONEscapedSecrets(t *testing.T) {
	secret := "configured\nsecret"
	for _, value := range []string{"prefix " + secret + " suffix", `{"value":"configured\nsecret"}`} {
		if got := Redact(value, []string{secret}); got != RedactedSensitiveContent {
			t.Fatalf("Redact(%q) = %q", value, got)
		}
	}
	if got := Redact("safe", []string{secret}); got != "safe" {
		t.Fatalf("Redact(safe) = %q", got)
	}
}

func completedGeneration(at time.Time) usage.GenerationCompletion {
	return usage.GenerationCompletion{
		State: usage.CompletionResponse, CompletedAt: at.Add(time.Second), Latency: time.Second,
		StructuredValidation: "valid",
	}
}

type fakeGenerationRecorder struct {
	nextID int64
}

func (r *fakeGenerationRecorder) Start(_ context.Context, _ usage.GenerationStart) (int64, error) {
	r.nextID++
	return r.nextID, nil
}

func (r *fakeGenerationRecorder) Complete(_ context.Context, _ int64, _ usage.GenerationCompletion) error {
	return nil
}
