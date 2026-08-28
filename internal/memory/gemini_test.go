package memory

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aminvakil/wormtamer/internal/diagnostics"
	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/review"
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
		_, _ = response.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"create_memory\":false,\"lesson\":\"\"}"}],"role":"model"},"finishReason":"STOP"}]}`))
	}))
	defer server.Close()

	evaluator, err := NewEvaluator(context.Background(), "gateway-key", server.URL, "gemini-proxy", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := evaluator.Evaluate(context.Background(), testInput())
	if err != nil || result.CreateMemory || gotPath != "/v1beta/models/gemini-proxy:generateContent" || gotAPIKey != "gateway-key" {
		t.Fatalf("result=%+v error=%v path=%q key=%q", result, err, gotPath, gotAPIKey)
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

func TestEvaluatorDiagnosticsRespectLogLevel(t *testing.T) {
	secret := "configured\nsecret"
	for _, test := range []struct {
		name  string
		level slog.Level
		debug bool
	}{
		{name: "info", level: slog.LevelInfo},
		{name: "debug", level: slog.LevelDebug, debug: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: test.level}))
			evaluator := newEvaluator(&fakeGenerator{output: `{"create_memory":true,"lesson":"Use the schema source."}`}, secret, []string{secret}, logger)
			if _, err := evaluator.Evaluate(context.Background(), testInput()); err != nil {
				t.Fatal(err)
			}
			output := logs.String()
			privateValues := []string{"Generated output must be changed", "Use the schema source.", "You decide whether a closed or merged"}
			if !test.debug {
				for _, value := range privateValues {
					if strings.Contains(output, value) {
						t.Fatalf("info logs contain %q: %s", value, output)
					}
				}
				return
			}
			for _, value := range append(privateValues, "Gemini feedback prompt", "Gemini feedback response", diagnostics.RedactedSensitiveContent) {
				if !strings.Contains(output, value) {
					t.Fatalf("debug logs lack %q: %s", value, output)
				}
			}
			if strings.Contains(output, `configured\nsecret`) {
				t.Fatalf("debug logs contain JSON-escaped credential: %s", output)
			}
		})
	}
}

func TestEvaluatorPropagatesWorkflowCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	generator := &fakeGenerator{err: context.Canceled, onGenerate: cancel}
	_, err := newEvaluator(generator, "gemini-test", nil, nil).Evaluate(ctx, testInput())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
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

type fakeGenerator struct {
	output     string
	prompt     string
	config     *genai.GenerateContentConfig
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
	generation := review.Generation{Content: genai.NewContentFromText(g.output, genai.RoleModel)}
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
