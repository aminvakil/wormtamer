package review

import (
	"bytes"
	"encoding/json"
	"html"
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
	Severity       string `json:"severity"`
	Title          string `json:"title"`
	Explanation    string `json:"explanation"`
	Recommendation string `json:"recommendation"`
	Path           string `json:"path"`
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
		for index, finding := range result.Findings {
			body.WriteString("\n**")
			body.WriteString(strconv.Itoa(index + 1))
			body.WriteString(". ")
			body.WriteString(markdownText(finding.Severity))
			body.WriteString(": ")
			body.WriteString(markdownText(finding.Title))
			body.WriteString("**\n\nPath: ")
			body.WriteString(markdownText(finding.Path))
			body.WriteString("\n\nExplanation:\n")
			writeBlockquote(&body, finding.Explanation)
			body.WriteString("\n\nRecommendation:\n")
			writeBlockquote(&body, finding.Recommendation)
			body.WriteByte('\n')
		}
	}
	body.WriteString("\n")
	body.WriteString(marker)
	if body.Len() > maxRenderedNoteBytes {
		return "", failure.Failed("note_body_limit_exceeded")
	}
	return body.String(), nil
}

func containsForbidden(result Result, forbidden []string) bool {
	values := []string{result.Summary}
	for _, finding := range result.Findings {
		values = append(values, finding.Severity, finding.Title, finding.Explanation, finding.Recommendation, finding.Path)
	}
	for _, secret := range forbidden {
		if secret == "" {
			continue
		}
		for _, value := range values {
			if strings.Contains(value, secret) {
				return true
			}
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
		body.WriteString(markdownText(line))
	}
}

func markdownText(value string) string {
	value = html.EscapeString(value)
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
