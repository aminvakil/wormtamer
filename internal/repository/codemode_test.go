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

func TestParseCodemodePreservesFirstLineOptions(t *testing.T) {
	for _, test := range []struct {
		name    string
		code    string
		want    string
		options codemodeOptions
	}{
		{"defaults", "\nreturn 1;", "\nreturn 1;", codemodeOptions{MaxOutputTokens: 10000}},
		{"first-line", " \t// @options: {\"max_output_tokens\": 5, \"timeout_ms\": 100}\r\nreturn 1;", "\nreturn 1;", codemodeOptions{MaxOutputTokens: 5, TimeoutMS: 100}},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, options, err := parseCodemode(map[string]any{"code": test.code})
			if err != nil || code != test.want || options != test.options {
				t.Fatalf("parsed codemode = %q, %+v, %v", code, options, err)
			}
		})
	}
}

func TestCodemodeRejectsMisplacedOptionsBeforeStartingHelper(t *testing.T) {
	workspace := testToolWorkspace(t)
	workspace.executable = filepath.Join(t.TempDir(), "must-not-start")
	for _, test := range []struct {
		name string
		code string
		line string
	}{
		{"blank-first-line", "\n// @options: {\"timeout_ms\": 1}\nreturn 1;", "2"},
		{"after-code", "await tools.bash({command: 'touch unexpected'});\n\n \t// @options: invalid\r\nreturn 1;", "3"},
		{"duplicate", "// @options: {\"timeout_ms\": 1}\n// @options: {\"timeout_ms\": 2}\nreturn 1;", "2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := workspace.RunCode(context.Background(), map[string]any{"code": test.code}, codeTools(workspace))
			message, _ := result.Response["error"].(string)
			if err != nil || !strings.Contains(message, "first input line") || !strings.Contains(message, "found on line "+test.line) {
				t.Fatalf("misplaced options = %+v, %v", result, err)
			}
		})
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

func TestCodemodeOutputBoundaries(t *testing.T) {
	workspace := testToolWorkspace(t)
	for _, test := range []struct {
		name string
		code string
		want string
	}{
		{"none", `return;`, ""},
		{"single", `return "only";`, "only\n"},
		{"console-only", `console.log("log"); console.info({ready: true}); console.warn(""); console.error("error"); console.debug("debug");`,
			"<console_output>\nlog\n{\"ready\":true}\n\nerror\ndebug\n</console_output>\n"},
		{"mixed", `console.log("before"); text("first\nline"); console.warn("between"); text(""); return "last";`,
			"==> text 1/3 <==\nfirst\nline\n==> text 2/3 <==\n\n==> text 3/3 <==\nlast\n<console_output>\nbefore\nbetween\n</console_output>\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := workspace.RunCode(ctx, map[string]any{"code": test.code}, codeTools(workspace))
			output, _ := result.Response["output"].(string)
			_, body, found := strings.Cut(output, "Output:\n")
			if err != nil || !found || body != test.want {
				t.Fatalf("script output = %+v, %v; want body %q", result, err, test.want)
			}
		})
	}
}

func TestCodemodePreservesNestedErrorsInOutput(t *testing.T) {
	workspace := testToolWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code := `
const error = new TypeError("invalid evidence");
error.code = "INVALID_EVIDENCE";
const results = await Promise.allSettled([Promise.resolve("ready"), Promise.reject(error)]);
text(results);
console.log({results});
return results;`
	result, err := workspace.RunCode(ctx, map[string]any{"code": code}, codeTools(workspace))
	if err != nil {
		t.Fatal(err)
	}
	output, _ := result.Response["output"].(string)
	for _, field := range []string{`"value":"ready"`, `"name":"TypeError"`, `"message":"invalid evidence"`, `"code":"INVALID_EVIDENCE"`} {
		if strings.Count(output, field) != 3 {
			t.Fatalf("missing %s in text, console, or return output: %+v", field, result)
		}
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
		`throw new Error("deliberate");`,
		`throw "";`,
		`await Promise.reject("");`,
		`await new Promise(() => {});`,
		`const values = []; for (;;) values.push(new ArrayBuffer(16 * 1024 * 1024));`,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		code = `text("partial"); console.log("diagnostic"); text("more");` + code
		result, err := workspace.RunCode(ctx, map[string]any{"code": code}, codeTools(workspace))
		cancel()
		output, _ := result.Response["error"].(string)
		want := "Output:\n==> text 1/2 <==\npartial\n==> text 2/2 <==\nmore\n<console_output>\ndiagnostic\n</console_output>\nScript error:"
		if err != nil || !strings.Contains(output, "Script failed") || !strings.Contains(output, want) {
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
	result, err := workspace.RunCode(ctx, map[string]any{"code": "// @options: {\"timeout_ms\": 1000}\nconsole.log('diagnostic'); text('partial'); while(true) {}"}, codeTools(workspace))
	output, _ := result.Response["error"].(string)
	if err != nil || !strings.Contains(output, "Output:\npartial\n<console_output>\ndiagnostic\n</console_output>\nScript error:\nScript exceeded timeout_ms") {
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
	for _, test := range []struct {
		name string
		code string
		full string
	}{
		{"single", `text('start-' + 'x'.repeat(200) + '-end');`, "start-" + strings.Repeat("x", 200) + "-end\n"},
		{"chunked-items", `console.log('log-' + '界'.repeat(32769)); text('start-' + 'é'.repeat(32769) + '-end'); return 'last';`,
			"==> text 1/2 <==\nstart-" + strings.Repeat("é", 32769) + "-end\n==> text 2/2 <==\nlast\n<console_output>\nlog-" + strings.Repeat("界", 32769) + "\n</console_output>\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code := "// @options: {\"max_output_tokens\": 5}\n" + test.code
			result, err := workspace.RunCode(context.Background(), map[string]any{"code": code}, codeTools(workspace))
			output, _ := result.Response["output"].(string)
			_, path, found := strings.Cut(output, "Full output: ")
			if err != nil || !found || !strings.Contains(output, test.full[:10]) || !strings.Contains(output, test.full[len(test.full)-10:]) || !strings.Contains(output, "[Output truncated]") {
				t.Fatalf("truncated script = %+v, %v", result, err)
			}
			full, err := os.ReadFile(path)
			if err != nil || string(full) != test.full {
				t.Fatalf("full script output differs: got %d bytes, want %d; error %v", len(full), len(test.full), err)
			}
		})
	}
}

func TestCodemodeBoundsOutputAndReportsSpoolExhaustion(t *testing.T) {
	workspace := testToolWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code := fmt.Sprintf(`text("partial"); console.log("x".repeat(%d));`, maxCodemodeOutput+1)
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

func TestCodemodeTextAndConsoleShareItemLimit(t *testing.T) {
	workspace := testToolWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code := fmt.Sprintf(`text("partial"); for (let i = 1; i < %d; i++) console.log(""); text("unreachable");`, maxCodemodeItems)
	result, err := workspace.RunCode(ctx, map[string]any{"code": code}, codeTools(workspace))
	output, _ := result.Response["error"].(string)
	if err != nil || !strings.Contains(output, "partial") || !strings.Contains(output, "Script output exceeded") || strings.Contains(output, "unreachable") {
		t.Fatalf("output item limit = %+v, %v", result, err)
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

func TestCodemodePreservesHardFailureDuringParentCancellation(t *testing.T) {
	workspace := testToolWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := workspace.RunCode(ctx, map[string]any{"code": `await tools.read({path:"a"});`},
		func(context.Context, int64, string, map[string]any) (map[string]any, error) {
			cancel()
			return nil, failure.Failed("sensitive_tool_content")
		})
	var hardFailure *failure.Error
	if !errors.Is(err, context.Canceled) || !errors.As(err, &hardFailure) || hardFailure.Category != "sensitive_tool_content" || result.Response != nil {
		t.Fatalf("concurrent cancellation and failure = %+v, %v", result, err)
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
