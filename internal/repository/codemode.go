package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aminvakil/wormtamer/internal/failure"
)

const ToolCodemode = "codemode"

// ErrToolEvidenceLimit requests final-only generation, but must never mask a
// concurrent security, credential, or infrastructure failure.
var ErrToolEvidenceLimit = errors.New("tool_result_limit_exceeded")

// NestedToolCall returns the checked output/error envelope to expose to a script.
// An error aborts the script and attempt; it is never catchable by JavaScript.
type NestedToolCall func(context.Context, int64, string, map[string]any) (map[string]any, error)

type CodemodeBroker interface {
	RunCode(context.Context, map[string]any, NestedToolCall) (ToolResult, error)
}

type codemodeOptions struct {
	MaxOutputTokens int64 `json:"max_output_tokens"`
	TimeoutMS       int64 `json:"timeout_ms"`
}

func parseCodemode(arguments map[string]any) (string, codemodeOptions, error) {
	options := codemodeOptions{MaxOutputTokens: 10000}
	code, ok := arguments["code"].(string)
	if !onlyArguments(arguments, "code") || !ok || strings.TrimSpace(code) == "" || len(code) > maxCodemodeCode || !utf8.ValidString(code) {
		return "", options, errors.New("codemode requires code: a non-empty JavaScript string of at most 256 KiB")
	}
	line, rest, found := strings.Cut(code, "\n")
	if strings.HasPrefix(strings.TrimSpace(line), "// @options:") {
		encoded := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "// @options:"))
		decoder := json.NewDecoder(strings.NewReader(encoded))
		var settings map[string]int64
		if err := decoder.Decode(&settings); err != nil || settings == nil {
			return "", options, errors.New("invalid codemode options: use max_output_tokens and timeout_ms as positive integers")
		}
		for key, value := range settings {
			if value <= 0 {
				return "", options, errors.New("codemode options must be positive integers")
			}
			switch key {
			case "max_output_tokens":
				options.MaxOutputTokens = value
			case "timeout_ms":
				options.TimeoutMS = value
			default:
				return "", options, errors.New("unknown codemode option: " + key)
			}
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF || !found || strings.TrimSpace(rest) == "" ||
			options.MaxOutputTokens > maxCodemodeOutput/4 ||
			options.TimeoutMS > int64(time.Duration(1<<63-1)/time.Millisecond) {
			return "", options, errors.New("invalid codemode options or missing script")
		}
		code = "\n" + rest
	}
	return code, options, nil
}

func (w *localWorkspace) RunCode(ctx context.Context, arguments map[string]any, call NestedToolCall) (result ToolResult, resultErr error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	code, options, err := parseCodemode(arguments)
	if err != nil {
		return correctableError(err.Error()), nil
	}
	started := time.Now()
	runCtx, cancel := context.WithCancel(ctx)
	if options.TimeoutMS > 0 {
		cancel()
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(options.TimeoutMS)*time.Millisecond)
	}
	defer cancel()
	input, send, err := os.Pipe()
	if err != nil {
		return ToolResult{}, failure.Retry("codemode_start_failed", 0)
	}
	defer input.Close()
	defer send.Close()
	receive, output, err := os.Pipe()
	if err != nil {
		return ToolResult{}, failure.Retry("codemode_start_failed", 0)
	}
	defer receive.Close()
	defer output.Close()
	command := exec.Command(w.executable, codemodeHelperArgument)
	command.Dir, command.Env = w.cwd, w.toolEnvironment()
	command.SysProcAttr = toolSysProcAttr(w.toolUID, w.toolGID)
	command.Stdin, command.Stdout = input, output
	var stderr boundedOutput
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return ToolResult{}, failure.Retry("codemode_start_failed", 0)
	}
	input.Close()
	output.Close()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()

	var operations sync.WaitGroup
	var fatalMu sync.Mutex
	var fatalErr error
	fail := func(err error) {
		fatalMu.Lock()
		if fatalErr == nil || errors.Is(fatalErr, ErrToolEvidenceLimit) {
			fatalErr = err
		} else if !errors.Is(err, ErrToolEvidenceLimit) {
			fatalErr = errors.Join(fatalErr, err)
		}
		fatalMu.Unlock()
		cancel()
	}
	defer func() {
		if resultErr != nil {
			fail(resultErr)
		}
		cancel()
		send.Close()
		receive.Close()
		_ = command.Process.Kill()
		<-wait
		operations.Wait()
		// A concurrent infrastructure/credential failure cannot be hidden by a
		// script returning without awaiting that call.
		if fatalErr != nil {
			result, resultErr = ToolResult{}, fatalErr
		}
		if err := ctx.Err(); err != nil {
			result, resultErr = ToolResult{}, err
		}
	}()

	var writeMu sync.Mutex
	write := func(message codemodeMessage) {
		frame, err := encodeCodemodeMessage(message)
		if err != nil {
			fail(failure.Retry("codemode_helper_failed", 0))
			return
		}
		writeMu.Lock()
		_, _ = send.Write(frame)
		writeMu.Unlock()
		// A script may finish before an outstanding response is written. Its
		// done frame decides success; EOF without done fails in the read loop.
	}
	operations.Go(func() { write(codemodeMessage{Kind: "run", Code: code}) })
	type received struct {
		message codemodeMessage
		err     error
	}
	messages := make(chan received)
	operations.Go(func() {
		for {
			message, err := readCodemodeMessage(receive)
			select {
			case messages <- received{message, err}:
			case <-runCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	})
	slots := make(chan struct{}, maxCodemodeConcurrent)
	var lastID int64
	var emitted strings.Builder
	outputFrames := 0
	var scriptError string
	scriptFailed := false
loop:
	for {
		select {
		case <-runCtx.Done():
			if ctx.Err() != nil {
				return ToolResult{}, ctx.Err()
			}
			fatalMu.Lock()
			err := fatalErr
			fatalMu.Unlock()
			if err != nil {
				return ToolResult{}, err
			}
			scriptFailed = true
			scriptError = "Script exceeded timeout_ms"
			break loop
		case received := <-messages:
			if received.err != nil {
				return ToolResult{}, failure.Retry("codemode_helper_response_invalid", 0)
			}
			message := received.message
			switch message.Kind {
			case "call":
				if message.ID <= lastID || message.ID > 1<<53 || (message.Name != ToolRead && message.Name != ToolBash) || len(message.Arguments) > maxCodemodeCode {
					return ToolResult{}, failure.Retry("codemode_helper_response_invalid", 0)
				}
				lastID = message.ID
				select {
				case slots <- struct{}{}:
				default:
					return ToolResult{}, failure.Retry("codemode_helper_response_invalid", 0)
				}
				operations.Go(func() {
					var args map[string]any
					var response map[string]any
					var callErr error
					if err := json.Unmarshal(message.Arguments, &args); err != nil || args == nil {
						response = map[string]any{"error": "Tool arguments must be an object"}
					} else {
						response, callErr = call(runCtx, message.ID, message.Name, args)
					}
					<-slots
					if callErr != nil {
						if runCtx.Err() == nil || (!errors.Is(callErr, context.Canceled) && !errors.Is(callErr, context.DeadlineExceeded)) {
							fail(callErr)
						}
						return
					}
					encoded, err := json.Marshal(response)
					if err != nil {
						fail(failure.Retry("tool_result_encoding_failed", 0))
						return
					}
					write(codemodeMessage{Kind: "response", ID: message.ID, Response: encoded})
				})
			case "output":
				outputFrames++
				if emitted.Len()+len(message.Text) > maxCodemodeOutput || outputFrames > 2*maxCodemodeItems {
					scriptFailed = true
					scriptError = "Script output exceeded the limit"
					break loop
				}
				emitted.WriteString(message.Text)
			case "done":
				scriptFailed = !message.OK
				scriptError = message.Error
				break loop
			default:
				return ToolResult{}, failure.Retry("codemode_helper_response_invalid", 0)
			}
		}
	}
	cancel()
	text := emitted.String()
	if scriptFailed {
		text += "Script error:\n" + scriptError
	}
	text, err = w.codemodeOutput(text, int(options.MaxOutputTokens)*4)
	if errors.Is(err, errBashOutputLimit) {
		scriptFailed = true
		text += "\nScript error: review output spool allowance exhausted"
	} else if err != nil {
		return ToolResult{}, err
	}
	status := "completed"
	if scriptFailed {
		status = "failed"
	}
	text = fmt.Sprintf("Script %s\nWall time %.1f seconds\nOutput:\n%s", status, time.Since(started).Seconds(), text)
	if scriptFailed {
		return correctableError(text), nil
	}
	return ToolResult{Response: map[string]any{"output": text}}, nil
}

func (w *localWorkspace) codemodeOutput(text string, limit int) (string, error) {
	if len(text) <= limit {
		return text, nil
	}
	id, err := randomIdentifier()
	if err != nil {
		return "", failure.Retry("codemode_output_spool_failed", 0)
	}
	name := "codemode-" + id + ".log"
	directory := filepath.Join(w.root, ".wormtamer-output")
	file, err := secureCreateSpool(directory, name)
	if err != nil {
		return "", failure.Retry("codemode_output_spool_failed", 0)
	}
	capture := &bashCapture{workspace: w, spool: file, spoolPath: filepath.Join(directory, name), spoolDirectory: directory, spoolName: name}
	writeErr := capture.writeSpool([]byte(text))
	closeErr := capture.close()
	start := utf8PrefixLength(text, limit/2)
	end := len(text) - limit/2
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	if writeErr != nil || closeErr != nil {
		capture.removeSpool()
		if errors.Is(writeErr, errBashOutputLimit) {
			return text[:start] + "\n[Output truncated]\n" + text[end:], errBashOutputLimit
		}
		return "", failure.Retry("codemode_output_spool_failed", 0)
	}
	return text[:start] + "\n[Output truncated]\n" + text[end:] + "\n\nFull output: " + capture.spoolPath, nil
}

func utf8PrefixLength(text string, limit int) int {
	end := min(len(text), limit)
	for end > 0 && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	return end
}

// ScriptResponse preserves the ordinary error envelope unless the tool has a
// structured result (notably Bash's non-zero exit status).
func (r ToolResult) ScriptResponse() map[string]any {
	if r.ScriptValue != nil {
		return map[string]any{"output": r.ScriptValue}
	}
	return r.Response
}
