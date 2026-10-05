package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
)

func codeTools(workspace *localWorkspace) NestedToolCall {
	return func(ctx context.Context, _ int64, name string, args map[string]any) (map[string]any, error) {
		result, err := workspace.Call(ctx, name, args)
		return result.ScriptResponse(), err
	}
}

func TestCodemodeChainsBashAndReadWithStructuredResults(t *testing.T) {
	workspace := testToolWorkspace(t)
	t.Setenv("WORMTAMER_TEST_SECRET", "must-not-be-inherited")
	code := `
if (typeof process !== "undefined" || typeof fetch !== "undefined" || typeof setTimeout !== "undefined") throw new Error("Unexpected host API");
const imported = await Promise.allSettled([import("std")]);
if (imported[0].status !== "rejected") throw new Error("Unexpected module access");
const listed = await tools.bash({command: "printf evidence > found.txt; printf found.txt; exit 7"});
if (listed.exit_code !== 7 || listed.truncated || listed.wall_time_seconds < 0) throw new Error("Bad Bash result");
const results = await Promise.allSettled([
    tools.read({path: listed.output}), tools.read({path: "missing.txt"})
]);
const env = await tools.bash({command: "env"});
if (env.output.includes("WORMTAMER_TEST_SECRET")) throw new Error("Inherited credentials");
text({content: results[0].value, missing: results[1].status});
return "finished";`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := workspace.RunCode(ctx, map[string]any{"code": code}, codeTools(workspace))
	output, _ := result.Response["output"].(string)
	if err != nil || !strings.Contains(output, `{"content":"evidence","missing":"rejected"}`) || !strings.Contains(output, "finished") {
		t.Fatalf("codemode = %+v, %v", result, err)
	}
}

func TestCodemodePreservesEmbeddedNULs(t *testing.T) {
	workspace := testToolWorkspace(t)
	for _, test := range []struct {
		code  string
		field string
	}{
		{code: `const result = await tools.bash({command: "printf 'before\\000after'"}); text(result.output);`, field: "output"},
		{code: `throw "before\u0000after";`, field: "error"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := workspace.RunCode(ctx, map[string]any{"code": test.code}, codeTools(workspace))
		cancel()
		text, _ := result.Response[test.field].(string)
		if err != nil || !strings.Contains(text, "before\x00after") {
			t.Fatalf("embedded NUL in %s = %+v, %v", test.field, result, err)
		}
	}
}

func TestCodemodeRunsIndependentCallsConcurrently(t *testing.T) {
	workspace := testToolWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	started, active, peak := 0, 0, 0
	bothStarted := make(chan struct{})
	call := func(ctx context.Context, _ int64, _ string, _ map[string]any) (map[string]any, error) {
		mu.Lock()
		started++
		active++
		peak = max(peak, active)
		if started == 2 {
			close(bothStarted)
		}
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		select {
		case <-bothStarted:
			return map[string]any{"output": "ready"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	result, err := workspace.RunCode(ctx, map[string]any{"code": `return await Promise.all(Array.from({length:20}, () => tools.read({path:"a"})));`}, call)
	if err != nil || strings.Count(fmt.Sprint(result.Response["output"]), "ready") != 20 || started != 20 || peak > maxCodemodeConcurrent {
		t.Fatalf("parallel codemode = %+v, %v", result, err)
	}
}

func TestCodemodeScriptFailuresKeepPartialOutput(t *testing.T) {
	workspace := testToolWorkspace(t)
	for _, code := range []string{
		`text("partial"); throw new Error("deliberate");`,
		`text("partial"); throw "";`,
		`text("partial"); await Promise.reject("");`,
		`text("partial"); await new Promise(() => {});`,
		`text("partial"); const values = []; for (;;) values.push(new ArrayBuffer(16 * 1024 * 1024));`,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := workspace.RunCode(ctx, map[string]any{"code": code}, codeTools(workspace))
		cancel()
		output, _ := result.Response["error"].(string)
		if err != nil || !strings.Contains(output, "partial") || !strings.Contains(output, "Script failed") || !strings.Contains(output, "Script error:") {
			t.Fatalf("script failure = %+v, %v", result, err)
		}
	}
}

func TestCodemodeReportsUnhandledPromiseRejections(t *testing.T) {
	workspace := testToolWorkspace(t)
	for _, code := range []string{
		`text("partial"); Promise.reject(new Error("detached failure")); return "ok";`,
		`text("partial"); [0].forEach(async () => { await 0; await 0; throw new Error("detached failure"); }); return "ok";`,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := workspace.RunCode(ctx, map[string]any{"code": code}, codeTools(workspace))
		cancel()
		output, _ := result.Response["error"].(string)
		if err != nil || !strings.Contains(output, "Script failed") || !strings.Contains(output, "partial") || !strings.Contains(output, "detached failure") {
			t.Fatalf("unhandled rejection = %+v, %v", result, err)
		}
	}
}

func TestCodemodeAllowsHandledRejectionsAndExplicitExit(t *testing.T) {
	workspace := testToolWorkspace(t)
	for _, code := range []string{
		`const rejected = Promise.reject(new Error("handled")); await 0; rejected.catch(error => text(error.message)); return "ok";`,
		`text("ok"); exit(); throw new Error("unreachable");`,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := workspace.RunCode(ctx, map[string]any{"code": code}, codeTools(workspace))
		cancel()
		output, _ := result.Response["output"].(string)
		if err != nil || !strings.Contains(output, "Script completed") || !strings.Contains(output, "ok") {
			t.Fatalf("successful completion = %+v, %v", result, err)
		}
	}
}

func TestCodemodeExplicitTimeoutInterruptsJavaScript(t *testing.T) {
	workspace := testToolWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := workspace.RunCode(ctx, map[string]any{"code": "// @options: {\"timeout_ms\": 100}\ntext('partial'); while(true) {}"}, codeTools(workspace))
	if err != nil || !strings.Contains(fmt.Sprint(result.Response["error"]), "timeout_ms") {
		t.Fatalf("script timeout = %+v, %v", result, err)
	}
}

func TestCodemodeCancelsOutstandingCallsOnReturnAndParentCancellation(t *testing.T) {
	for _, parentCancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("parent-cancel-%t", parentCancel), func(t *testing.T) {
			workspace := testToolWorkspace(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			code := `tools.bash({command:"echo $$ > pid; exec sleep 30"}); await tools.read({path:"wait-until-started"});`
			if parentCancel {
				code += `await new Promise(() => {});`
			}
			call := func(ctx context.Context, id int64, name string, args map[string]any) (map[string]any, error) {
				if name == ToolRead {
					for {
						if _, err := os.Stat(filepath.Join(workspace.cwd, "pid")); err == nil {
							break
						}
						select {
						case <-time.After(5 * time.Millisecond):
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					if parentCancel {
						cancel()
					}
					return map[string]any{"output": "started"}, nil
				}
				return codeTools(workspace)(ctx, id, name, args)
			}
			result, err := workspace.RunCode(ctx, map[string]any{"code": code}, call)
			if parentCancel && !errors.Is(err, context.Canceled) || !parentCancel && (err != nil || result.Response["output"] == nil) {
				t.Fatalf("cleanup result = %+v, %v", result, err)
			}
			pidText, err := os.ReadFile(filepath.Join(workspace.cwd, "pid"))
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
			if err != nil || syscall.Kill(pid, 0) != syscall.ESRCH {
				t.Fatalf("nested process %d survived: %v", pid, err)
			}
		})
	}
}

func TestCodemodeTruncatesAndSpoolsSelectedOutput(t *testing.T) {
	workspace := testToolWorkspace(t)
	result, err := workspace.RunCode(context.Background(), map[string]any{"code": "// @options: {\"max_output_tokens\": 5}\ntext('start-' + 'x'.repeat(200) + '-end');"}, codeTools(workspace))
	output, _ := result.Response["output"].(string)
	_, path, found := strings.Cut(output, "Full output: ")
	if err != nil || !found || !strings.Contains(output, "start-") || !strings.Contains(output, "-end") || !strings.Contains(output, "[Output truncated]") {
		t.Fatalf("truncated script = %+v, %v", result, err)
	}
	full, err := os.ReadFile(path)
	if err != nil || string(full) != "start-"+strings.Repeat("x", 200)+"-end\n" {
		t.Fatalf("full script output = %q, %v", full, err)
	}
}

func TestCodemodeBoundsOutputAndReportsSpoolExhaustion(t *testing.T) {
	workspace := testToolWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code := fmt.Sprintf(`text("partial"); text("x".repeat(%d));`, maxCodemodeOutput+1)
	result, err := workspace.RunCode(ctx, map[string]any{"code": code}, codeTools(workspace))
	output, _ := result.Response["error"].(string)
	if err != nil || !strings.Contains(output, "partial") || !strings.Contains(output, "Script output exceeded") {
		t.Fatalf("output limit = %+v, %v", result, err)
	}
	workspace.spoolUsed = MaxReviewSpoolBytes
	result, err = workspace.RunCode(ctx, map[string]any{"code": `text("x".repeat(50000));`}, codeTools(workspace))
	if err != nil || !strings.Contains(fmt.Sprint(result.Response["error"]), "spool allowance exhausted") {
		t.Fatalf("spool limit = %+v, %v", result, err)
	}
}

func TestCodemodeRejectsMalformedHelperOutput(t *testing.T) {
	workspace := testToolWorkspace(t)
	helper := filepath.Join(t.TempDir(), "malformed-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf malformed"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace.executable = helper
	_, err := workspace.RunCode(context.Background(), map[string]any{"code": "return 1"}, codeTools(workspace))
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "codemode_helper_response_invalid" {
		t.Fatalf("malformed helper error = %v", err)
	}
}

func TestBashScriptOutputRetainsStartAndEnd(t *testing.T) {
	workspace := testToolWorkspace(t)
	result, err := workspace.Call(context.Background(), ToolBash, map[string]any{
		"command": fmt.Sprintf("printf start; head -c %d /dev/zero | tr '\\0' x; printf end", maxScriptBashBytes+1),
	})
	if err != nil {
		t.Fatal(err)
	}
	value := result.ScriptValue.(BashScriptResult)
	if !value.Truncated || !strings.HasPrefix(value.Output, "start") || !strings.HasSuffix(value.Output, "end") ||
		!strings.Contains(value.Output, "[Output truncated]") || value.FullOutputPath == "" {
		t.Fatalf("structured truncation: %+v", value)
	}
}

func TestCodemodeHardFailureOverridesEvidenceLimitDuringCleanup(t *testing.T) {
	workspace := testToolWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	budgetReturned := make(chan struct{})
	code := `tools.read({path:"budget"}); tools.read({path:"secret"}); return "done";`
	result, err := workspace.RunCode(ctx, map[string]any{"code": code},
		func(ctx context.Context, _ int64, _ string, args map[string]any) (map[string]any, error) {
			<-ctx.Done()
			if args["path"] == "budget" {
				close(budgetReturned)
				return nil, ErrToolEvidenceLimit
			}
			<-budgetReturned
			// Model a hard failure discovered after the budget cancellation,
			// while RunCode is waiting for the outstanding calls to finish.
			time.Sleep(20 * time.Millisecond)
			return nil, failure.Failed("sensitive_tool_content")
		})
	var hardFailure *failure.Error
	if !errors.As(err, &hardFailure) || hardFailure.Category != "sensitive_tool_content" || errors.Is(err, ErrToolEvidenceLimit) || result.Response != nil {
		t.Fatalf("concurrent hard failure = %+v, %v", result, err)
	}
}

func TestCodemodeCannotCatchInfrastructureFailure(t *testing.T) {
	workspace := testToolWorkspace(t)
	result, err := workspace.RunCode(context.Background(), map[string]any{"code": `try { await tools.read({path:"a"}); } catch { return "hidden"; }`},
		func(context.Context, int64, string, map[string]any) (map[string]any, error) {
			return nil, failure.Retry("read_helper_failed", 0)
		})
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "read_helper_failed" || result.Response != nil {
		t.Fatalf("infrastructure failure = %+v, %v", result, err)
	}
}
