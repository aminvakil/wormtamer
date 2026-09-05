package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aminvakil/wormtamer/internal/failure"
)

var (
	errBashOutputLimit = errors.New("bash output limit exceeded")
	errBashSpool       = errors.New("bash output spool failed")
)

func (w *localWorkspace) callBash(ctx context.Context, arguments map[string]any) (ToolResult, error) {
	if !onlyArguments(arguments, "command", "timeout") {
		return correctableError("Invalid bash arguments"), nil
	}
	commandSource, ok := arguments["command"].(string)
	if !ok {
		return correctableError("Invalid command: must be a string"), nil
	}
	var timeout time.Duration
	var timeoutLabel string
	if value, present := arguments["timeout"]; present {
		seconds, valid := positiveSeconds(value)
		if !valid {
			return correctableError("Invalid timeout: must be a finite positive number of seconds"), nil
		}
		timeout = time.Duration(seconds * float64(time.Second))
		if timeout <= 0 {
			return correctableError("Invalid timeout: must be a finite positive number of seconds"), nil
		}
		timeoutLabel = strconv.FormatFloat(seconds, 'f', -1, 64)
	}

	command := exec.Command("/bin/bash", "-c", commandSource)
	command.Dir = w.cwd
	command.Env = w.toolEnvironment()
	command.SysProcAttr = toolSysProcAttr(w.toolUID, w.toolGID)
	reader, writer, err := os.Pipe()
	if err != nil {
		return ToolResult{}, fmt.Errorf("create bash output pipe: %w", failure.Retry("bash_start_failed", 0))
	}
	command.Stdout = writer
	command.Stderr = writer
	if err := command.Start(); err != nil {
		reader.Close()
		writer.Close()
		return ToolResult{}, fmt.Errorf("start bash: %w", failure.Retry("bash_start_failed", 0))
	}
	_ = writer.Close()
	pid := command.Process.Pid
	capture := &bashCapture{
		workspace: w, limitReached: make(chan struct{}), infrastructureFailure: make(chan struct{}),
	}
	readDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(capture, reader)
		readDone <- copyErr
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()

	var timeoutTimer *time.Timer
	var timeoutChannel <-chan time.Time
	if timeout > 0 {
		timeoutTimer = time.NewTimer(timeout)
		timeoutChannel = timeoutTimer.C
		defer timeoutTimer.Stop()
	}

	var waitErr error
	var outcome string
	select {
	case waitErr = <-waitDone:
		outcome = "exit"
	case <-capture.limitReached:
		outcome = "output_limit"
		terminateProcessGroup(pid)
		waitErr = <-waitDone
	case <-capture.infrastructureFailure:
		outcome = "capture_failure"
		terminateProcessGroup(pid)
		waitErr = <-waitDone
	case <-timeoutChannel:
		outcome = "timeout"
		terminateProcessGroup(pid)
		waitErr = <-waitDone
	case <-ctx.Done():
		outcome = "canceled"
		terminateProcessGroup(pid)
		waitErr = <-waitDone
	}

	var readErr error
	readerClosed := false
	select {
	case readErr = <-readDone:
	case <-time.After(100 * time.Millisecond):
		readerClosed = true
		_ = reader.Close()
		readErr = <-readDone
	}
	_ = reader.Close()
	if closeErr := capture.close(); closeErr != nil {
		outcome = "capture_failure"
		readErr = closeErr
	}

	if outcome == "canceled" {
		return ToolResult{}, ctx.Err()
	}
	expectedClosedReader := readerClosed && errors.Is(readErr, os.ErrClosed)
	if outcome == "capture_failure" || (readErr != nil && !expectedClosedReader && !errors.Is(readErr, errBashOutputLimit) && !errors.Is(readErr, errBashSpool)) {
		return ToolResult{}, fmt.Errorf("capture bash output: %w", failure.Retry("bash_output_spool_failed", 0))
	}
	text := capture.formatted(outcome != "output_limit")
	if outcome == "output_limit" || errors.Is(readErr, errBashOutputLimit) {
		capture.removeSpool()
		return correctableCategorizedError("bash_output_limit_exceeded", text), nil
	}
	if outcome == "timeout" {
		message := text
		if message != "" {
			message += "\n\n"
		}
		message += "Command timed out after " + timeoutLabel + " seconds"
		return correctableError(message), nil
	}
	if waitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(waitErr, &exitError) {
			return ToolResult{}, fmt.Errorf("wait for bash: %w", failure.Retry("bash_execution_failed", 0))
		}
		message := text
		if message != "" {
			message += "\n\n"
		}
		message += "Command exited with code " + strconv.Itoa(exitError.ExitCode())
		return correctableError(message), nil
	}
	if text == "" {
		text = "(no output)"
	}
	return ToolResult{Response: map[string]any{"output": text}}, nil
}

func positiveSeconds(value any) (float64, bool) {
	var seconds float64
	switch number := value.(type) {
	case int:
		seconds = float64(number)
	case int32:
		seconds = float64(number)
	case int64:
		seconds = float64(number)
	case float64:
		seconds = number
	default:
		return 0, false
	}
	return seconds, seconds > 0 && seconds <= float64((1<<63-1)/int64(time.Second))
}

type bashCapture struct {
	workspace *localWorkspace

	mu                    sync.Mutex
	totalBytes            int64
	newlines              int64
	lastByteNewline       bool
	lastLineBytes         int64
	prefix                bytes.Buffer
	tail                  []byte
	truncated             bool
	spool                 *os.File
	spoolPath             string
	spoolDirectory        string
	spoolName             string
	spoolErr              error
	limitErr              bool
	limitReached          chan struct{}
	infrastructureFailure chan struct{}
	limitOnce             sync.Once
	failureOnce           sync.Once
}

func (c *bashCapture) Write(contents []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limitErr {
		return 0, errBashOutputLimit
	}
	if c.spoolErr != nil {
		return 0, errBashSpool
	}
	remaining := int64(MaxCommandOutputBytes) - c.totalBytes
	accepted := contents
	limitAfter := false
	if int64(len(accepted)) > remaining {
		accepted = accepted[:max(0, int(remaining))]
		limitAfter = true
	}
	if len(accepted) > 0 {
		c.addOutput(accepted)
		if c.spool != nil {
			if err := c.writeSpool(accepted); err != nil {
				c.failSpool(err)
				if errors.Is(err, errBashOutputLimit) {
					return 0, errBashOutputLimit
				}
				return 0, errBashSpool
			}
		} else {
			_, _ = c.prefix.Write(accepted)
			if c.totalBytes > MaxToolBytes || c.totalLines() > MaxToolLines {
				c.truncated = true
				if err := c.startSpool(); err != nil {
					if errors.Is(err, errBashOutputLimit) {
						c.reachLimit()
						return 0, errBashOutputLimit
					}
					c.failSpool(err)
					return 0, errBashSpool
				}
			}
		}
	}
	if limitAfter {
		c.reachLimit()
		return len(accepted), errBashOutputLimit
	}
	return len(contents), nil
}

func (c *bashCapture) addOutput(contents []byte) {
	c.totalBytes += int64(len(contents))
	for _, character := range contents {
		if character == '\n' {
			c.newlines++
			c.lastByteNewline = true
			c.lastLineBytes = 0
		} else {
			c.lastByteNewline = false
			c.lastLineBytes++
		}
	}
	c.tail = append(c.tail, contents...)
	const retained = MaxToolBytes * 2
	if len(c.tail) > retained {
		c.tail = append([]byte(nil), c.tail[len(c.tail)-retained:]...)
	}
}

func (c *bashCapture) startSpool() error {
	identifier, err := randomIdentifier()
	if err != nil {
		return err
	}
	name := "bash-" + identifier + ".log"
	directory := filepath.Join(c.workspace.root, ".wormtamer-output")
	path := filepath.Join(directory, name)
	file, err := secureCreateSpool(directory, name)
	if err != nil {
		return err
	}
	c.spool = file
	c.spoolPath = path
	c.spoolDirectory = directory
	c.spoolName = name
	contents := c.prefix.Bytes()
	if err := c.writeSpool(contents); err != nil {
		file.Close()
		_ = secureRemoveSpool(directory, name)
		c.spool = nil
		c.spoolPath = ""
		c.spoolDirectory = ""
		c.spoolName = ""
		return err
	}
	c.prefix.Reset()
	return nil
}

func (c *bashCapture) writeSpool(contents []byte) error {
	if len(contents) == 0 {
		return nil
	}
	c.workspace.spoolMu.Lock()
	if c.workspace.spoolUsed+int64(len(contents)) > MaxReviewSpoolBytes {
		c.workspace.spoolMu.Unlock()
		return errBashOutputLimit
	}
	c.workspace.spoolUsed += int64(len(contents))
	c.workspace.spoolMu.Unlock()
	written, err := c.spool.Write(contents)
	if err != nil || written != len(contents) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	return nil
}

func (c *bashCapture) reachLimit() {
	c.limitErr = true
	c.limitOnce.Do(func() { close(c.limitReached) })
}

func (c *bashCapture) failSpool(err error) {
	if errors.Is(err, errBashOutputLimit) {
		c.reachLimit()
		return
	}
	c.spoolErr = err
	c.failureOnce.Do(func() { close(c.infrastructureFailure) })
}

func (c *bashCapture) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spool == nil {
		return c.spoolErr
	}
	if c.spoolErr == nil {
		c.spoolErr = c.spool.Sync()
	}
	if c.spoolErr == nil && !c.limitErr {
		c.spoolErr = c.spool.Chown(int(c.workspace.toolUID), int(c.workspace.toolGID))
	}
	if err := c.spool.Close(); c.spoolErr == nil {
		c.spoolErr = err
	}
	c.spool = nil
	return c.spoolErr
}

func (c *bashCapture) removeSpool() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spoolPath != "" {
		_ = secureRemoveSpool(c.spoolDirectory, c.spoolName)
		c.spoolPath = ""
		c.spoolDirectory = ""
		c.spoolName = ""
	}
}

func (c *bashCapture) formatted(includeSpool bool) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	content, outputLines, lastLinePartial := tailContent(c.tail, c.totalBytes, c.totalLines())
	text := strings.ToValidUTF8(string(content), "�")
	if !c.truncated && c.totalBytes <= MaxToolBytes && c.totalLines() <= MaxToolLines {
		return text
	}
	if !includeSpool || c.spoolPath == "" {
		return text
	}
	endLine := c.totalLines()
	if lastLinePartial {
		text += fmt.Sprintf("\n\n[Showing last %s of line %d (line is %s). Full output: %s]",
			formatSize(int64(len(content))), endLine, formatSize(c.lastLineBytes), c.spoolPath)
	} else if outputLines == MaxToolLines {
		text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Full output: %s]", endLine-int64(outputLines)+1, endLine, endLine, c.spoolPath)
	} else {
		text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Full output: %s]",
			endLine-int64(outputLines)+1, endLine, endLine, formatSize(MaxToolBytes), c.spoolPath)
	}
	return text
}

func (c *bashCapture) totalLines() int64 {
	lines := c.newlines
	if c.totalBytes > 0 && !c.lastByteNewline {
		lines++
	}
	return lines
}

func tailContent(retained []byte, totalBytes, totalLines int64) ([]byte, int, bool) {
	if totalBytes <= MaxToolBytes && totalLines <= MaxToolLines {
		return append([]byte(nil), retained...), int(totalLines), false
	}
	content := retained
	if len(content) > 0 && content[len(content)-1] == '\n' {
		content = content[:len(content)-1]
	}
	end := len(content)
	position := end
	selectedStart := end
	outputBytes := 0
	outputLines := 0
	lastLinePartial := false
	for position >= 0 && outputLines < MaxToolLines {
		lineStart := bytes.LastIndexByte(content[:position], '\n') + 1
		lineBytes := position - lineStart
		if outputLines > 0 {
			lineBytes++
		}
		if outputBytes+lineBytes > MaxToolBytes {
			if outputLines == 0 {
				selectedStart = max(lineStart, position-MaxToolBytes)
				for selectedStart < position && !utf8.Valid(content[selectedStart:position]) {
					selectedStart++
				}
				outputLines = 1
				lastLinePartial = selectedStart > lineStart
			}
			break
		}
		selectedStart = lineStart
		outputBytes += lineBytes
		outputLines++
		if lineStart == 0 {
			break
		}
		position = lineStart - 1
	}
	return append([]byte(nil), content[selectedStart:end]...), outputLines, lastLinePartial
}
