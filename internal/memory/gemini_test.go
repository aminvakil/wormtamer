package memory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aminvakil/wormtamer/internal/diagnostics"
	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/usage"
	"google.golang.org/genai"
)

func TestEvaluatorPinsDeveloperAPIBaseURL(t *testing.T) {
	t.Setenv("GOOGLE_GEMINI_BASE_URL", "https://ambient-endpoint.invalid")
	if got := resolvedGeminiBaseURL(""); got != geminiDeveloperAPIBaseURL {
		t.Fatalf("resolvedGeminiBaseURL(\"\") = %q, want %q", got, geminiDeveloperAPIBaseURL)
	}
}

func TestEvaluatorUsesConfiguredBaseURL(t *testing.T) {
	var gotPath, gotAPIKey string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		gotAPIKey = request.Header.Get("x-goog-api-key")
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("X-Litellm-Response-Cost", "0.000001")
		_, _ = response.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"create_memory\":false,\"lesson\":\"\"}"}],"role":"model"},"finishReason":"STOP"}]}`))
	}))
	defer server.Close()

	recorder := &fakeUsageRecorder{}
	evaluator, err := NewEvaluator(context.Background(), "gateway-key", server.URL, "gemini-proxy", nil, nil, recorder, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := usage.WithScope(context.Background(), usage.Scope{RequestKind: usage.RequestFeedback, FeedbackJobID: 9, Attempt: 1})
	result, err := evaluator.Evaluate(ctx, testInput())
	if err != nil || result.CreateMemory || gotPath != "/v1beta/models/gemini-proxy:generateContent" || gotAPIKey != "gateway-key" ||
		len(recorder.completions) != 1 || recorder.completions[0].EndpointCostPicos == nil || *recorder.completions[0].EndpointCostPicos != 1_000_000 {
		t.Fatalf("result=%+v error=%v path=%q key=%q completions=%+v", result, err, gotPath, gotAPIKey, recorder.completions)
	}
}

func TestEvaluatorSynthesizesOneBoundedLesson(t *testing.T) {
	generator := &fakeGenerator{output: `{"create_memory":true,"lesson":"Generated files are changed through their source generator."}`}
	evaluator := NewEvaluatorWithGenerator(generator, "gemini-test", []string{"configured-secret"})
	result, err := evaluator.Evaluate(context.Background(), testInput())
	if err != nil || !result.CreateMemory || result.Lesson == "" {
		t.Fatalf("Evaluate() = %+v, %v", result, err)
	}
	for _, expected := range []string{"review_summary", "review_findings", "diff", "comments", "Generated output must be changed"} {
		if !strings.Contains(generator.prompt, expected) {
			t.Fatalf("prompt missing %q: %s", expected, generator.prompt)
		}
	}
	instruction := generator.config.SystemInstruction.Parts[0].Text
	for _, expected := range []string{"closed or merged", "untrusted evidence", "not evidence", "at most one lesson"} {
		if !strings.Contains(instruction, expected) {
			t.Fatalf("instruction missing %q: %s", expected, instruction)
		}
	}
	if got := ID("http://gitlab.internal", 42, 7); len(got) != 31 || !strings.HasPrefix(got, "WT-M-") || got != ID("http://gitlab.internal", 42, 7) {
		t.Fatalf("memory ID = %q", got)
	}
}

func TestEvaluatorModelContract(t *testing.T) {
	generator := &fakeGenerator{output: `{"create_memory":false,"lesson":""}`}
	evaluator := NewEvaluatorWithGenerator(generator, "gemini-test", nil)
	if _, err := evaluator.Evaluate(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	if len(generator.config.Tools) != 0 || generator.config.ToolConfig != nil || generator.config.ResponseMIMEType != "application/json" {
		t.Fatalf("tools=%+v tool config=%+v MIME=%q", generator.config.Tools, generator.config.ToolConfig, generator.config.ResponseMIMEType)
	}
	schema := generator.config.ResponseJsonSchema.(map[string]any)
	if schema["additionalProperties"] != false || !slices.Equal(schema["required"].([]string), []string{"create_memory", "lesson"}) {
		t.Fatalf("schema = %+v", schema)
	}
}

func TestFeedbackDiagnosticValueUsesSharedRedaction(t *testing.T) {
	secret := "configured\nsecret"
	for _, value := range []string{"prefix " + secret, `{"value":"configured\nsecret"}`, "safe"} {
		if got, want := diagnosticValue(value, []string{secret}), diagnostics.Redact(value, []string{secret}); got != want {
			t.Fatalf("diagnosticValue(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestEvaluatorRejectsInvalidOrSensitiveEvidenceAndOutput(t *testing.T) {
	for _, output := range []string{
		`{"create_memory":true,"lesson":""}`,
		`{"create_memory":false,"lesson":"unexpected"}`,
		`{"create_memory":true,"lesson":"configured-secret"}`,
		`{"create_memory":false,"lesson":"","extra":true}`,
	} {
		evaluator := NewEvaluatorWithGenerator(&fakeGenerator{output: output}, "gemini-test", []string{"configured-secret"})
		if _, err := evaluator.Evaluate(context.Background(), testInput()); failureCategory(err) != "invalid_feedback_model_output" {
			t.Fatalf("output %s error = %v", output, err)
		}
	}
	input := testInput()
	input.Comments[0].Body = "configured-secret"
	evaluator := NewEvaluatorWithGenerator(&fakeGenerator{output: `{"create_memory":false,"lesson":""}`}, "gemini-test", []string{"configured-secret"})
	if _, err := evaluator.Evaluate(context.Background(), input); failureCategory(err) != "sensitive_feedback_input" {
		t.Fatalf("sensitive input error = %v", err)
	}
}

func TestEvaluatorRecordsFeedbackGenerationMetadata(t *testing.T) {
	generator := &fakeGenerator{
		output: `{"create_memory":false,"lesson":""}`,
		generation: review.Generation{
			ModelVersion: "resolved-test", FinishReason: genai.FinishReasonStop,
			PromptTokenCount: 80, CachedContentTokenCount: 10, CandidatesTokenCount: 20,
			ThoughtsTokenCount: 5, TotalTokenCount: 105, UsageMetadataAvailable: true,
		},
	}
	recorder := &fakeUsageRecorder{}
	evaluator := newEvaluator(generator, "gemini-test", nil, nil)
	evaluator.recorder = recorder
	ctx := usage.WithScope(context.Background(), usage.Scope{RequestKind: usage.RequestFeedback, FeedbackJobID: 91, Attempt: 4})
	if _, err := evaluator.Evaluate(ctx, testInput()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.starts) != 1 || recorder.starts[0].FeedbackJobID != 91 || len(recorder.completions) != 1 ||
		recorder.completions[0].StructuredValidation != "valid" || recorder.completions[0].ResolvedModel != "resolved-test" {
		t.Fatalf("starts=%+v completions=%+v", recorder.starts, recorder.completions)
	}
}

func TestEvaluatorRecordsReturnedContentAndValidatedDecision(t *testing.T) {
	generator := &fakeGenerator{output: `{"create_memory":true,"lesson":"Use the schema source."}`}
	usageRecorder := &fakeUsageRecorder{}
	conversationRecorder := diagnostics.New(true, nil)
	evaluator := newEvaluator(generator, "gemini-test", nil, nil)
	evaluator.recorder = diagnostics.ObserveGenerations(usageRecorder, conversationRecorder)
	evaluator.conversations = conversationRecorder
	ctx := usage.WithScope(context.Background(), usage.Scope{
		RequestKind: usage.RequestFeedback, FeedbackJobID: 91, Attempt: 4,
	})
	if _, err := evaluator.Evaluate(ctx, testInput()); err != nil {
		t.Fatal(err)
	}
	conversation, ok := conversationRecorder.ConversationByGeneration(1)
	if !ok || conversation.FeedbackJobID != 91 || len(conversation.Events) != 2 ||
		conversation.Events[0].Kind != "model" || !strings.Contains(conversation.Events[0].Text, "Use the schema source") ||
		conversation.Events[1].Kind != "decision" || conversation.Events[1].Text != `{"create_memory":true,"lesson":"Use the schema source."}` {
		t.Fatalf("feedback conversation = %+v, %v", conversation, ok)
	}
}

func TestEvaluatorCheckpointsAfterWorkflowCancellation(t *testing.T) {
	scoped := usage.WithScope(context.Background(), usage.Scope{RequestKind: usage.RequestFeedback, FeedbackJobID: 91, Attempt: 2})
	ctx, cancel := context.WithCancel(scoped)
	generator := &fakeGenerator{err: context.Canceled, onGenerate: cancel}
	recorder := &fakeUsageRecorder{}
	evaluator := newEvaluator(generator, "gemini-test", nil, nil)
	evaluator.recorder = recorder
	_, err := evaluator.Evaluate(ctx, testInput())
	if !errors.Is(err, context.Canceled) || len(recorder.completions) != 1 || recorder.completionContextErrs[0] != nil ||
		!recorder.completionHasDeadlines[0] || recorder.completions[0].State != usage.CompletionFailed {
		t.Fatalf("error=%v completions=%+v", err, recorder.completions)
	}
}

func testInput() Input {
	findingID := "WT-F-" + strings.Repeat("A", 26)
	return Input{
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		HeadSHA: strings.Repeat("b", 40), ReviewHeadSHA: strings.Repeat("a", 40),
		Summary: "The review found a generated-file issue.",
		Findings: []Finding{{TargetID: findingID, Finding: review.Finding{
			Priority: "P2", Title: "Generated file edited", Explanation: "The generated output changed.",
			Recommendation: "Edit the source generator.", Path: "generated.go",
		}}},
		Files:    []gitlab.ChangedFile{{OldPath: "generated.go", NewPath: "generated.go", Diff: "@@ -1 +1 @@\n-old\n+new"}},
		Comments: []gitlab.FeedbackComment{{AuthorID: 12, Body: "Generated output must be changed through the schema source."}},
	}
}

type fakeUsageRecorder struct {
	starts                 []usage.GenerationStart
	completions            []usage.GenerationCompletion
	completionContextErrs  []error
	completionHasDeadlines []bool
}

func (r *fakeUsageRecorder) Start(_ context.Context, start usage.GenerationStart) (int64, error) {
	r.starts = append(r.starts, start)
	return int64(len(r.starts)), nil
}

func (r *fakeUsageRecorder) Complete(ctx context.Context, _ int64, completion usage.GenerationCompletion) error {
	r.completions = append(r.completions, completion)
	r.completionContextErrs = append(r.completionContextErrs, ctx.Err())
	_, deadline := ctx.Deadline()
	r.completionHasDeadlines = append(r.completionHasDeadlines, deadline)
	return nil
}

type fakeGenerator struct {
	output     string
	prompt     string
	config     *genai.GenerateContentConfig
	generation review.Generation
	err        error
	onGenerate func()
}

func (g *fakeGenerator) Generate(_ context.Context, _ string, contents []*genai.Content, config *genai.GenerateContentConfig) (review.Generation, error) {
	g.config = config
	if g.onGenerate != nil {
		g.onGenerate()
	}
	if g.err != nil {
		return review.Generation{}, g.err
	}
	if len(contents) == 1 && len(contents[0].Parts) == 1 {
		g.prompt = contents[0].Parts[0].Text
	}
	generation := g.generation
	generation.Content = genai.NewContentFromText(g.output, genai.RoleModel)
	if generation.FinishReason == "" {
		generation.FinishReason = genai.FinishReasonStop
	}
	return generation, nil
}

func failureCategory(err error) string {
	if err == nil {
		return ""
	}
	var typed *failure.Error
	if errors.As(err, &typed) {
		return typed.Category
	}
	return err.Error()
}
