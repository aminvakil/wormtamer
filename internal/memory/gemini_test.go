package memory

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/review"
	"google.golang.org/genai"
)

func TestEvaluatorProducesBoundedActiveLesson(t *testing.T) {
	findingID := "WT-F-" + strings.Repeat("A", 26)
	generator := &fakeGenerator{output: `{"decisions":[{"finding_id":"` + findingID + `","outcome":"rejects_finding","confidence":"high","create_memory":true,"lesson":"Generated files are maintained by the generator."}]}`}
	evaluator := NewEvaluatorWithGenerator(generator, "gemini-test", []string{"configured-secret"})
	result, err := evaluator.Evaluate(context.Background(), testInput(findingID))
	if err != nil || len(result.Decisions) != 1 || !result.Decisions[0].CreateMemory {
		t.Fatalf("Evaluate() = %+v, %v", result, err)
	}
	if !strings.Contains(generator.prompt, `"actor_role":"maintainer"`) || strings.Contains(generator.prompt, "configured-secret") {
		t.Fatalf("prompt role or secret handling is wrong: %s", generator.prompt)
	}
	if generator.config == nil || generator.config.SystemInstruction == nil || !strings.Contains(generator.config.SystemInstruction.Parts[0].Text, "can still be mistaken") {
		t.Fatal("role-aware non-authoritative system instruction missing")
	}
	if got := ID("http://gitlab.internal", 42, 91, findingID); len(got) != 31 || !strings.HasPrefix(got, "WT-M-") || got != ID("http://gitlab.internal", 42, 91, findingID) {
		t.Fatalf("memory ID = %q", got)
	}
}

func TestEvaluatorDebugLogsModelTranscript(t *testing.T) {
	findingID := "WT-F-" + strings.Repeat("A", 26)
	generator := &fakeGenerator{output: `{"decisions":[]}`}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	evaluator := newEvaluator(generator, "gemini-test", nil, logger)

	if _, err := evaluator.Evaluate(context.Background(), testInput(findingID)); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	output := logs.String()
	for _, expected := range []string{"Gemini feedback prompt", "system_instruction", "feedback_json", "Gemini feedback response", "decisions"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("debug logs do not contain %q: %s", expected, output)
		}
	}
}

func TestEvaluatorDoesNotLogUnicodeEscapedCredential(t *testing.T) {
	const (
		secret  = `configured-credential`
		escaped = `configured-\u0063redential`
	)
	findingID := "WT-F-" + strings.Repeat("A", 26)
	generator := &fakeGenerator{output: `{"decisions":[{"finding_id":"` + findingID + `","outcome":"rejects_finding","confidence":"high","create_memory":true,"lesson":"` + escaped + `"}]}`}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	evaluator := newEvaluator(generator, "gemini-test", []string{secret}, logger)

	if _, err := evaluator.Evaluate(context.Background(), testInput(findingID)); failureCategory(err) != "invalid_feedback_model_output" {
		t.Fatalf("Evaluate() error = %v", err)
	}
	output := logs.String()
	if strings.Contains(output, secret) || strings.Contains(output, escaped) || strings.Contains(output, "Gemini feedback response") {
		t.Fatalf("debug logs exposed rejected model output: %s", output)
	}
}

func TestEvaluatorRejectsUntrustedOutputAndSecrets(t *testing.T) {
	findingID := "WT-F-" + strings.Repeat("A", 26)
	tests := []struct {
		name   string
		output string
	}{
		{name: "unknown finding", output: `{"decisions":[{"finding_id":"WT-F-BBBBBBBBBBBBBBBBBBBBBBBBBB","outcome":"supports_finding","confidence":"high","create_memory":false,"lesson":""}]}`},
		{name: "lesson without activation", output: `{"decisions":[{"finding_id":"` + findingID + `","outcome":"supports_finding","confidence":"high","create_memory":false,"lesson":"lesson"}]}`},
		{name: "secret lesson", output: `{"decisions":[{"finding_id":"` + findingID + `","outcome":"supports_finding","confidence":"high","create_memory":true,"lesson":"configured-secret"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evaluator := NewEvaluatorWithGenerator(&fakeGenerator{output: test.output}, "gemini-test", []string{"configured-secret"})
			if _, err := evaluator.Evaluate(context.Background(), testInput(findingID)); failureCategory(err) != "invalid_feedback_model_output" {
				t.Fatalf("Evaluate() error = %v", err)
			}
		})
	}

	input := testInput(findingID)
	input.Comment = "please inspect configured-secret"
	evaluator := NewEvaluatorWithGenerator(&fakeGenerator{output: `{"decisions":[]}`}, "gemini-test", []string{"configured-secret"})
	if _, err := evaluator.Evaluate(context.Background(), input); failureCategory(err) != "sensitive_feedback_input" {
		t.Fatalf("sensitive input error = %v", err)
	}
}

func testInput(findingID string) Input {
	return Input{
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		HeadSHA: strings.Repeat("a", 40), ActorID: 12, ActorAccess: 40, ActorRole: "maintainer",
		Comment: "This warning does not apply because the file is generated.",
		Findings: []review.Finding{{
			ID: findingID, Severity: "medium", Title: "Generated file edited",
			Explanation: "The generated output changed.", Recommendation: "Edit the source generator.", Path: "generated.go",
		}},
	}
}

type fakeGenerator struct {
	output string
	prompt string
	config *genai.GenerateContentConfig
}

func (g *fakeGenerator) Generate(_ context.Context, _ string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.Content, error) {
	g.config = config
	if len(contents) == 1 && len(contents[0].Parts) == 1 {
		g.prompt = contents[0].Parts[0].Text
	}
	return genai.NewContentFromText(g.output, genai.RoleModel), nil
}

func failureCategory(err error) string {
	if err == nil {
		return ""
	}
	if typed, ok := err.(*failure.Error); ok {
		return typed.Category
	}
	return err.Error()
}
