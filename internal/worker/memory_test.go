package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
)

func TestReviewMemoryToolRejectsModelSelectedScopeAndAllowsEmptyResults(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	snapshot := gitlab.Snapshot{
		Identity:    gitlab.Identity{GitLabInstance: "http://gitlab.internal", ProjectID: 42},
		ProjectPath: "group/project",
	}
	tools := newReviewTools(snapshot, &fakeGitLab{}, nil, &fakeWorkspaces{}, storage, time.Now)
	defer tools.Close()

	_, err := tools.Call(context.Background(), review.ToolSearchMemory, map[string]any{
		"query": "generated", "repository": "group/other",
	})
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "memory_tool_arguments_invalid" || failureError.Retryable {
		t.Fatalf("scope argument error = %v", err)
	}
	result, err := tools.Call(context.Background(), review.ToolSearchMemory, map[string]any{"query": "generated"})
	if err != nil {
		t.Fatal(err)
	}
	memories, ok := result["memories"].([]map[string]any)
	if !ok || len(memories) != 0 || result["authority"] != "untrusted_advisory" {
		t.Fatalf("empty memory result = %+v", result)
	}
}

func TestRankReviewMemoriesIsDeterministicAndBounded(t *testing.T) {
	now := time.Now().UTC()
	candidates := make([]store.ReviewMemory, 0, memoryResultLimit+2)
	candidates = append(candidates, store.ReviewMemory{
		MemoryID: "WT-M-" + strings.Repeat("A", 26), Lesson: "Generated source files require generator changes.", UpdatedAt: now.Add(-time.Hour),
	})
	for index, character := range []byte{'B', 'C', 'D', 'E', 'F', 'G'} {
		candidates = append(candidates, store.ReviewMemory{
			MemoryID: "WT-M-" + strings.Repeat(string(character), 26), Lesson: "Generated files require care.", UpdatedAt: now.Add(time.Duration(index) * time.Minute),
		})
	}
	matches := rankReviewMemories(candidates, "generated source", []string{"generated", "source"})
	if len(matches) != memoryResultLimit || matches[0].MemoryID != candidates[0].MemoryID {
		t.Fatalf("ranked memories = %+v", matches)
	}
	for _, memory := range matches {
		if !strings.Contains(strings.ToLower(memory.Lesson), "generated") {
			t.Fatalf("irrelevant memory returned: %+v", memory)
		}
	}
}
