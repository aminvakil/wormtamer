package review

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aminvakil/wormtamer/internal/failure"
)

const (
	maxResultBytes       = 64 << 10
	maxSummaryCharacters = 2000
	maxFindings          = 20
	maxTitleCharacters   = 200
	maxDetailCharacters  = 2000
	maxPathBytes         = 1024
	maxRenderedNoteBytes = 64 << 10
)

type Result struct {
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

type Finding struct {
	ID             string `json:"-"`
	Severity       string `json:"severity"`
	Title          string `json:"title"`
	Explanation    string `json:"explanation"`
	Recommendation string `json:"recommendation"`
	Path           string `json:"path"`
}

func ReviewID(gitLabInstance string, projectID, mergeRequestIID int64, headSHA string) string {
	return scopedID("WT-R-", "wormtamer:review:v1", gitLabInstance, strconv.FormatInt(projectID, 10),
		strconv.FormatInt(mergeRequestIID, 10), strings.ToLower(headSHA))
}

func ValidReviewID(id string) bool {
	return validScopedID(id, "WT-R-")
}

func FindingID(gitLabInstance string, projectID, mergeRequestIID int64, headSHA string, ordinal int) string {
	return scopedID("WT-F-", "wormtamer:finding:v1", gitLabInstance, strconv.FormatInt(projectID, 10),
		strconv.FormatInt(mergeRequestIID, 10), strings.ToLower(headSHA), strconv.Itoa(ordinal))
}

func ValidFindingID(id string) bool {
	return validScopedID(id, "WT-F-")
}

func scopedID(prefix string, values ...string) string {
	digest := sha256.New()
	for _, value := range values {
		_, _ = digest.Write([]byte(value))
		_, _ = digest.Write([]byte{0})
	}
	return prefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest.Sum(nil)[:16])
}

func validScopedID(id, prefix string) bool {
	if len(id) != 31 || !strings.HasPrefix(id, prefix) {
		return false
	}
	_, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(id[5:])
	return err == nil
}

func DecodeAndValidate(contents []byte, changedPaths map[string]struct{}, forbidden []string) (Result, []byte, error) {
	if len(contents) == 0 || len(contents) > maxResultBytes {
		return Result{}, nil, failure.Retry("invalid_model_output", 0)
	}
	var result Result
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Result{}, nil, failure.Retry("invalid_model_output", 0)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Result{}, nil, failure.Retry("invalid_model_output", 0)
	}
	if err := validateResult(&result, changedPaths); err != nil {
		return Result{}, nil, err
	}
	if containsForbidden(result, forbidden) {
		return Result{}, nil, failure.Retry("sensitive_model_output", 0)
	}
	canonical, err := json.Marshal(result)
	if err != nil || len(canonical) > maxResultBytes {
		return Result{}, nil, failure.Retry("invalid_model_output", 0)
	}
	return result, canonical, nil
}

func DecodeStored(contents []byte) (Result, error) {
	result, _, err := DecodeAndValidate(contents, nil, nil)
	return result, err
}

func validateResult(result *Result, changedPaths map[string]struct{}) error {
	result.Summary = strings.TrimSpace(result.Summary)
	if !validText(result.Summary, maxSummaryCharacters, true) || result.Findings == nil || len(result.Findings) > maxFindings {
		return failure.Retry("invalid_model_output", 0)
	}
	for index := range result.Findings {
		finding := &result.Findings[index]
		finding.Severity = strings.TrimSpace(finding.Severity)
		finding.Title = strings.TrimSpace(finding.Title)
		finding.Explanation = strings.TrimSpace(finding.Explanation)
		finding.Recommendation = strings.TrimSpace(finding.Recommendation)
		finding.Path = strings.TrimSpace(finding.Path)
		switch finding.Severity {
		case "low", "medium", "high", "critical":
		default:
			return failure.Retry("invalid_model_output", 0)
		}
		if !validText(finding.Title, maxTitleCharacters, false) ||
			!validText(finding.Explanation, maxDetailCharacters, true) ||
			!validText(finding.Recommendation, maxDetailCharacters, true) ||
			finding.Path == "" || len(finding.Path) > maxPathBytes || hasControl(finding.Path, false) {
			return failure.Retry("invalid_model_output", 0)
		}
		if changedPaths != nil {
			if _, present := changedPaths[finding.Path]; !present {
				return failure.Retry("invalid_model_output", 0)
			}
		}
	}
	return nil
}

func RenderNote(result Result, marker string, forbidden []string) (string, error) {
	if containsForbidden(result, forbidden) {
		return "", failure.Failed("sensitive_model_output")
	}

	var body strings.Builder
	body.WriteString("## Wormtamer review\n\n")
	writeBlockquote(&body, result.Summary)
	if len(result.Findings) == 0 {
		body.WriteString("\n\nNo actionable findings.\n")
	} else {
		body.WriteString("\n\n### Findings\n")
		seenIDs := make(map[string]struct{}, len(result.Findings))
		for index, finding := range result.Findings {
			if !ValidFindingID(finding.ID) {
				return "", failure.Failed("invalid_finding_identifier")
			}
			if _, exists := seenIDs[finding.ID]; exists {
				return "", failure.Failed("duplicate_finding_identifier")
			}
			seenIDs[finding.ID] = struct{}{}
			body.WriteString("\n**")
			body.WriteString(strconv.Itoa(index + 1))
			body.WriteString(". ")
			body.WriteString(markdownText(finding.Severity))
			body.WriteString(": ")
			body.WriteString(markdownInlineText(finding.Title))
			body.WriteString("** · Finding ID: `")
			body.WriteString(finding.ID)
			body.WriteString("`\n\nPath: ")
			writeCodeSpan(&body, finding.Path)
			body.WriteString("\n\nExplanation:\n")
			writeBlockquote(&body, finding.Explanation)
			body.WriteString("\n\nRecommendation:\n")
			writeBlockquote(&body, finding.Recommendation)
			body.WriteByte('\n')
		}
	}
	body.WriteString("\n")
	body.WriteString(marker)
	rendered := body.String()
	if containsForbiddenText(rendered, forbidden) {
		return "", failure.Failed("sensitive_model_output")
	}
	if len(rendered) > maxRenderedNoteBytes {
		return "", failure.Failed("note_body_limit_exceeded")
	}
	return rendered, nil
}

func containsForbidden(result Result, forbidden []string) bool {
	values := []string{result.Summary}
	for _, finding := range result.Findings {
		values = append(values, finding.Severity, finding.Title, finding.Explanation, finding.Recommendation, finding.Path)
	}
	for _, value := range values {
		if containsForbiddenText(value, forbidden) || containsForbiddenText(inlineCodeVisibleText(value), forbidden) {
			return true
		}
	}
	return false
}

func containsForbiddenText(value string, forbidden []string) bool {
	for _, secret := range forbidden {
		if secret != "" && strings.Contains(value, secret) {
			return true
		}
	}
	return false
}

func validText(value string, maxCharacters int, multiline bool) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= maxCharacters && !hasControl(value, multiline)
}

func hasControl(value string, multiline bool) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			if multiline && (character == '\n' || character == '\t') {
				continue
			}
			return true
		}
	}
	return false
}

func writeBlockquote(body *strings.Builder, value string) {
	for index, line := range strings.Split(value, "\n") {
		if index > 0 {
			body.WriteByte('\n')
		}
		body.WriteString("> ")
		body.WriteString(markdownInlineText(line))
	}
}

func markdownInlineText(value string) string {
	var rendered strings.Builder
	walkInlineCode(value,
		func(text string) { rendered.WriteString(markdownText(text)) },
		func(code string) { writeCodeSpan(&rendered, code) },
	)
	return rendered.String()
}

func inlineCodeVisibleText(value string) string {
	var visible strings.Builder
	for index, line := range strings.Split(value, "\n") {
		if index > 0 {
			visible.WriteByte('\n')
		}
		walkInlineCode(line,
			func(text string) { visible.WriteString(text) },
			func(code string) { visible.WriteString(code) },
		)
	}
	return visible.String()
}

func walkInlineCode(value string, writeText, writeCode func(string)) {
	for len(value) > 0 {
		opening := strings.IndexByte(value, '`')
		if opening < 0 {
			writeText(value)
			return
		}
		if !isolatedBacktick(value, opening) {
			runEnd := backtickRunEnd(value, opening)
			writeText(value[:runEnd])
			value = value[runEnd:]
			continue
		}
		closingOffset := strings.IndexByte(value[opening+1:], '`')
		if closingOffset < 0 {
			writeText(value)
			return
		}
		closing := opening + 1 + closingOffset
		if !isolatedBacktick(value, closing) {
			runEnd := backtickRunEnd(value, closing)
			writeText(value[:runEnd])
			value = value[runEnd:]
			continue
		}
		writeText(value[:opening])
		writeCode(value[opening+1 : closing])
		value = value[closing+1:]
	}
}

func isolatedBacktick(value string, index int) bool {
	return (index == 0 || value[index-1] != '`') &&
		(index == len(value)-1 || value[index+1] != '`')
}

func backtickRunEnd(value string, index int) int {
	for index < len(value) && value[index] == '`' {
		index++
	}
	return index
}

func writeCodeSpan(body *strings.Builder, value string) {
	longestRun := 0
	currentRun := 0
	for _, character := range value {
		if character == '`' {
			currentRun++
			if currentRun > longestRun {
				longestRun = currentRun
			}
			continue
		}
		currentRun = 0
	}
	delimiter := strings.Repeat("`", longestRun+1)
	body.WriteString(delimiter)
	if strings.Trim(value, " ") != "" &&
		(strings.HasPrefix(value, " ") || strings.HasSuffix(value, " ") ||
			strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`")) {
		body.WriteByte(' ')
		body.WriteString(value)
		body.WriteByte(' ')
	} else {
		body.WriteString(value)
	}
	body.WriteString(delimiter)
}

func markdownText(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	var escaped strings.Builder
	for _, character := range value {
		if strings.ContainsRune(`\\`+"`*_{}[]()#+-.!|>~", character) {
			escaped.WriteByte('\\')
		}
		if character == '@' {
			escaped.WriteRune('@')
			escaped.WriteRune('\u200b')
			continue
		}
		escaped.WriteRune(character)
	}
	return escaped.String()
}
