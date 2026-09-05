package repository

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/aminvakil/wormtamer/internal/failure"
)

const (
	readHelperArgument = "__wormtamer_review_read_helper"
	maxReadHelperFrame = 512 << 10
)

type readRequest struct {
	Path   string `json:"path"`
	Offset *int64 `json:"offset,omitempty"`
	Limit  *int64 `json:"limit,omitempty"`
}

type readHelperResponse struct {
	Output *string `json:"output,omitempty"`
	Error  *string `json:"error,omitempty"`
}

func IsReadHelperInvocation(arguments []string) bool {
	return len(arguments) == 1 && arguments[0] == readHelperArgument
}

func RunReadHelper(input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(io.LimitReader(input, 64<<10))
	decoder.DisallowUnknownFields()
	var request readRequest
	if err := decoder.Decode(&request); err != nil {
		return errors.New("decode read helper request")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("decode read helper request")
	}
	response := executeRead(request)
	encoded, err := json.Marshal(response)
	if err != nil {
		return errors.New("encode read helper response")
	}
	if len(encoded) > maxReadHelperFrame {
		return errors.New("read helper response exceeds frame limit")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(encoded)))
	if _, err := output.Write(header[:]); err != nil {
		return err
	}
	_, err = output.Write(encoded)
	return err
}

func (w *localWorkspace) callRead(ctx context.Context, arguments map[string]any) (ToolResult, error) {
	if !onlyArguments(arguments, "path", "offset", "limit") {
		return correctableError("Invalid read arguments"), nil
	}
	path, ok := arguments["path"].(string)
	if !ok || path == "" || strings.ContainsRune(path, '\x00') || !utf8.ValidString(path) {
		return correctableError("Invalid read path"), nil
	}
	request := readRequest{Path: path}
	for key, destination := range map[string]**int64{"offset": &request.Offset, "limit": &request.Limit} {
		value, present := arguments[key]
		if !present {
			continue
		}
		integer, valid := integerArgument(value)
		if !valid || integer <= 0 {
			return correctableError("Invalid " + key + ": must be a positive integer"), nil
		}
		*destination = &integer
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return ToolResult{}, fmt.Errorf("encode read helper request: %w", failure.Retry("read_helper_failed", 0))
	}

	command := exec.Command(w.executable, readHelperArgument)
	command.Dir = w.cwd
	command.Env = w.toolEnvironment()
	command.Stdin = bytes.NewReader(encoded)
	command.SysProcAttr = toolSysProcAttr(w.toolUID, w.toolGID)
	var stdout limitedBuffer
	stdout.limit = maxReadHelperFrame + 4
	var stderr boundedOutput
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return ToolResult{}, fmt.Errorf("start read helper: %w", failure.Retry("read_helper_start_failed", 0))
	}
	pid := command.Process.Pid
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case err := <-wait:
		if err != nil || stdout.exceeded {
			return ToolResult{}, fmt.Errorf("run read helper: %w", failure.Retry("read_helper_failed", 0))
		}
	case <-ctx.Done():
		terminateProcessGroup(pid)
		<-wait
		return ToolResult{}, ctx.Err()
	}
	response, err := decodeReadHelperFrame(stdout.contents)
	if err != nil {
		return ToolResult{}, fmt.Errorf("decode read helper response: %w", failure.Retry("read_helper_response_invalid", 0))
	}
	if response.Error != nil {
		return correctableError(*response.Error), nil
	}
	return ToolResult{Response: map[string]any{"output": *response.Output}}, nil
}

func (w *localWorkspace) toolEnvironment() []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + w.root + "/.home",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"PWD=" + w.cwd,
		"TMPDIR=" + w.root + "/.tmp",
	}
}

func toolSysProcAttr(uid, gid uint32) *syscall.SysProcAttr {
	attributes := &syscall.SysProcAttr{Setpgid: true}
	if uid != uint32(os.Geteuid()) || gid != uint32(os.Getegid()) {
		attributes.Credential = &syscall.Credential{Uid: uid, Gid: gid, NoSetGroups: true}
	}
	return attributes
}

type limitedBuffer struct {
	contents []byte
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(contents []byte) (int, error) {
	if len(b.contents)+len(contents) > b.limit {
		b.exceeded = true
	}
	if remaining := b.limit - len(b.contents); remaining > 0 {
		if remaining > len(contents) {
			remaining = len(contents)
		}
		b.contents = append(b.contents, contents[:remaining]...)
	}
	return len(contents), nil
}

func decodeReadHelperFrame(contents []byte) (readHelperResponse, error) {
	if len(contents) < 4 {
		return readHelperResponse{}, errors.New("missing read helper frame")
	}
	length := int(binary.BigEndian.Uint32(contents[:4]))
	if length <= 0 || length > maxReadHelperFrame || len(contents) != length+4 {
		return readHelperResponse{}, errors.New("invalid read helper frame length")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents[4:]))
	decoder.DisallowUnknownFields()
	var response readHelperResponse
	if err := decoder.Decode(&response); err != nil {
		return readHelperResponse{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return readHelperResponse{}, errors.New("multiple read helper values")
	}
	if (response.Output == nil) == (response.Error == nil) {
		return readHelperResponse{}, errors.New("read helper response must contain exactly one output or error")
	}
	return response, nil
}

func executeRead(request readRequest) readHelperResponse {
	file, err := os.Open(request.Path)
	if err != nil {
		return readFailure(request.Path, err)
	}
	defer file.Close()
	return readText(request, file)
}

func readText(request readRequest, input io.Reader) readHelperResponse {
	offset := int64(1)
	if request.Offset != nil {
		offset = *request.Offset
	}
	if offset <= 0 {
		return helperError("Invalid offset: must be a positive integer")
	}
	if request.Limit != nil && *request.Limit <= 0 {
		return helperError("Invalid limit: must be a positive integer")
	}

	lines := &streamedTextLines{reader: bufio.NewReaderSize(input, 32<<10)}
	var skipped int64
	for skipped < offset-1 {
		exists, err := lines.skipLine()
		if err != nil {
			return readFailure(request.Path, err)
		}
		if !exists {
			return helperError(fmt.Sprintf("Offset %d is beyond end of file (%d lines total)", offset, skipped))
		}
		skipped++
	}

	selected := make([]string, 0, min(MaxToolLines+1, 64))
	selectedBytes := 0
	var selectedLines int64
	for {
		line, exists, err := lines.nextLine(MaxToolBytes)
		if err != nil {
			return readFailure(request.Path, err)
		}
		if !exists {
			return helperError(fmt.Sprintf("Offset %d is beyond end of file (%d lines total)", offset, skipped))
		}
		if line.tooLong {
			if len(selected) == 0 {
				return oversizedReadLine(request.Path, offset)
			}
			return truncatedRead(offset, headTruncation{
				content: strings.Join(selected, "\n"), lines: len(selected), byBytes: true,
			})
		}

		text := strings.ToValidUTF8(string(line.contents), "�")
		if len(selected) > 0 {
			selectedBytes++
		}
		selected = append(selected, text)
		selectedBytes += len(text)
		selectedLines++
		countedLines := len(selected)
		if text == "" {
			countedLines--
		}
		if selectedBytes > MaxToolBytes || countedLines > MaxToolLines {
			truncated := truncateHead(strings.Join(selected, "\n"))
			if truncated.firstLineExceeds {
				return oversizedReadLine(request.Path, offset)
			}
			return truncatedRead(offset, truncated)
		}

		if request.Limit != nil && selectedLines >= *request.Limit {
			output := strings.Join(selected, "\n")
			if line.terminated {
				output += fmt.Sprintf("\n\n[More lines in file. Use offset=%d to continue.]", offset+selectedLines)
			}
			return helperOutput(output)
		}
		if !line.terminated {
			return helperOutput(strings.Join(selected, "\n"))
		}
	}
}

func readFailure(path string, err error) readHelperResponse {
	return helperError(fmt.Sprintf("Could not read file: %s: %v", path, err))
}

func oversizedReadLine(path string, line int64) readHelperResponse {
	return helperOutput(fmt.Sprintf(
		"[Line %d exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]",
		line, formatSize(MaxToolBytes), line, shellQuote(path), MaxToolBytes,
	))
}

func truncatedRead(offset int64, truncated headTruncation) readHelperResponse {
	endLine := offset + int64(truncated.lines) - 1
	notice := fmt.Sprintf("[Showing lines %d-%d", offset, endLine)
	if truncated.byBytes {
		notice += " (" + formatSize(MaxToolBytes) + " limit)"
	}
	return helperOutput(truncated.content + "\n\n" + notice + fmt.Sprintf(". Use offset=%d to continue.]", endLine+1))
}

type streamedTextLine struct {
	contents   []byte
	terminated bool
	tooLong    bool
}

type streamedTextLines struct {
	reader *bufio.Reader
	done   bool
}

func (r *streamedTextLines) skipLine() (bool, error) {
	if r.done {
		return false, nil
	}
	for {
		_, err := r.reader.ReadSlice('\n')
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			r.done = true
			return true, nil
		default:
			return false, err
		}
	}
}

func (r *streamedTextLines) nextLine(maxBytes int) (streamedTextLine, bool, error) {
	if r.done {
		return streamedTextLine{}, false, nil
	}
	contents := make([]byte, 0, min(maxBytes, 4096))
	for {
		fragment, err := r.reader.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) && !errors.Is(err, io.EOF) {
			return streamedTextLine{}, false, err
		}
		terminated := err == nil
		if terminated {
			fragment = fragment[:len(fragment)-1]
		}
		if len(contents)+len(fragment) > maxBytes {
			return streamedTextLine{tooLong: true}, true, nil
		}
		contents = append(contents, fragment...)
		switch {
		case terminated:
			return streamedTextLine{contents: contents, terminated: true}, true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			r.done = true
			return streamedTextLine{contents: contents}, true, nil
		}
	}
}

func helperOutput(output string) readHelperResponse {
	return readHelperResponse{Output: &output}
}

func helperError(message string) readHelperResponse {
	return readHelperResponse{Error: &message}
}

type headTruncation struct {
	content          string
	lines            int
	byBytes          bool
	firstLineExceeds bool
}

func truncateHead(content string) headTruncation {
	lines := splitCountedLines(content)
	if len(lines) <= MaxToolLines && len(content) <= MaxToolBytes {
		return headTruncation{content: content, lines: len(lines)}
	}
	if len(lines) > 0 && len(lines[0]) > MaxToolBytes {
		return headTruncation{byBytes: true, firstLineExceeds: true}
	}
	selected := make([]string, 0, min(len(lines), MaxToolLines))
	bytesUsed := 0
	byBytes := false
	for index, line := range lines {
		if index >= MaxToolLines {
			break
		}
		lineBytes := len(line)
		if len(selected) > 0 {
			lineBytes++
		}
		if bytesUsed+lineBytes > MaxToolBytes {
			byBytes = true
			break
		}
		selected = append(selected, line)
		bytesUsed += lineBytes
	}
	return headTruncation{content: strings.Join(selected, "\n"), lines: len(selected), byBytes: byBytes}
}

func splitCountedLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func formatSize(bytes int64) string {
	if bytes < 1024 {
		return strconv.FormatInt(bytes, 10) + "B"
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
}

func correctableError(message string) ToolResult {
	return ToolResult{Response: map[string]any{"error": message}}
}

func correctableCategorizedError(category, output string) ToolResult {
	return ToolResult{Response: map[string]any{"error": map[string]any{
		"category": category,
		"output":   output,
	}}}
}
