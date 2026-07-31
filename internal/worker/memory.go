package worker

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
)

const (
	memoryQueryBytes       = 256
	memoryResultLimit      = 5
	memoryRetrievalTimeout = time.Second
)

type reviewTools struct {
	repositories *reviewRepository
	store        JobStore
	snapshot     gitlab.Snapshot
	now          func() time.Time
	retrieved    map[string]store.ReviewMemoryRetrieval
}

func newReviewTools(snapshot gitlab.Snapshot, gitLab GitLabBroker, workspaces RepositoryWorkspaces, storage JobStore, now func() time.Time) *reviewTools {
	return &reviewTools{
		repositories: newReviewRepository(snapshot, gitLab, workspaces),
		store:        storage, snapshot: snapshot, now: now,
		retrieved: make(map[string]store.ReviewMemoryRetrieval),
	}
}

func (t *reviewTools) Call(ctx context.Context, name string, arguments map[string]any) (map[string]any, error) {
	if name != review.ToolSearchMemory {
		return t.repositories.Call(ctx, name, arguments)
	}
	if len(arguments) != 1 {
		return nil, failure.Failed("memory_tool_arguments_invalid")
	}
	query, ok := arguments["query"].(string)
	query = strings.TrimSpace(query)
	if !ok || query == "" || len(query) > memoryQueryBytes || !utf8.ValidString(query) || hasMemoryControl(query) {
		return nil, failure.Failed("memory_tool_arguments_invalid")
	}
	terms := memoryQueryTerms(query)
	if len(terms) == 0 {
		return nil, failure.Failed("memory_tool_arguments_invalid")
	}

	requestCtx, cancel := context.WithTimeout(ctx, memoryRetrievalTimeout)
	defer cancel()
	candidates, err := t.store.ListActiveReviewMemories(requestCtx, t.snapshot.Identity.GitLabInstance, t.snapshot.Identity.ProjectID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure.Retry("memory_retrieval_failed", 0)
	}
	matches := rankReviewMemories(candidates, query, terms)
	results := make([]map[string]any, 0, len(matches))
	for _, memory := range matches {
		results = append(results, map[string]any{
			"memory_id": memory.MemoryID,
			"scope": map[string]any{
				"type": "repository", "project_id": t.snapshot.Identity.ProjectID, "project_path": t.snapshot.ProjectPath,
			},
			"finding_id":         memory.FindingID,
			"outcome":            memory.Outcome,
			"confidence":         memory.Confidence,
			"lesson":             memory.Lesson,
			"source_role":        memory.SourceRole,
			"evidence_reference": memory.SourceURL,
			"updated_at":         memory.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
		key := memory.MemoryID + "\x00" + memory.UpdatedAt.UTC().Format(time.RFC3339Nano)
		if _, exists := t.retrieved[key]; !exists {
			t.retrieved[key] = store.ReviewMemoryRetrieval{
				MemoryID: memory.MemoryID, MemoryUpdatedAt: memory.UpdatedAt, RetrievedAt: t.now().UTC(),
			}
		}
	}
	return map[string]any{
		"authority": "untrusted_advisory",
		"memories":  results,
	}, nil
}

func (t *reviewTools) Close() error {
	return t.repositories.Close()
}

func (t *reviewTools) Retrievals() []store.ReviewMemoryRetrieval {
	result := make([]store.ReviewMemoryRetrieval, 0, len(t.retrieved))
	for _, retrieval := range t.retrieved {
		result = append(result, retrieval)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].MemoryID != result[j].MemoryID {
			return result[i].MemoryID < result[j].MemoryID
		}
		return result[i].MemoryUpdatedAt.Before(result[j].MemoryUpdatedAt)
	})
	return result
}

type rankedMemory struct {
	memory store.ReviewMemory
	score  int
}

func rankReviewMemories(candidates []store.ReviewMemory, query string, terms []string) []store.ReviewMemory {
	ranked := make([]rankedMemory, 0, len(candidates))
	phrase := strings.ToLower(strings.TrimSpace(query))
	for _, memory := range candidates {
		lesson := strings.ToLower(memory.Lesson)
		score := 0
		for _, term := range terms {
			if strings.Contains(lesson, term) {
				score++
			}
		}
		if score == 0 {
			continue
		}
		if strings.Contains(lesson, phrase) {
			score += len(terms)
		}
		ranked = append(ranked, rankedMemory{memory: memory, score: score})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if !ranked[i].memory.UpdatedAt.Equal(ranked[j].memory.UpdatedAt) {
			return ranked[i].memory.UpdatedAt.After(ranked[j].memory.UpdatedAt)
		}
		return ranked[i].memory.MemoryID < ranked[j].memory.MemoryID
	})
	if len(ranked) > memoryResultLimit {
		ranked = ranked[:memoryResultLimit]
	}
	result := make([]store.ReviewMemory, len(ranked))
	for index := range ranked {
		result[index] = ranked[index].memory
	}
	return result
}

func memoryQueryTerms(query string) []string {
	seen := make(map[string]struct{})
	terms := make([]string, 0)
	for _, term := range strings.FieldsFunc(strings.ToLower(query), func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character)
	}) {
		if _, exists := seen[term]; exists {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}
	return terms
}

func hasMemoryControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}
