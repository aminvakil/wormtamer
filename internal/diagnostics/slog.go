package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"time"
)

type teeHandler struct {
	primary  slog.Handler
	recorder *Recorder
	bound    []scopedAttribute
	groups   []string
}

type scopedAttribute struct {
	attribute slog.Attr
	groups    []string
}

var conversationLogFields = map[string]struct{}{
	"system_instruction": {},
	"prompt":             {},
	"response":           {},
	"arguments":          {},
	"result":             {},
}

var conversationLogMessages = map[string]struct{}{
	"Gemini review prompt":      {},
	"Gemini review response":    {},
	"Gemini review tool call":   {},
	"Gemini review tool result": {},
	"Gemini feedback prompt":    {},
	"Gemini feedback response":  {},
}

func NewTeeHandler(primary slog.Handler, recorder *Recorder) slog.Handler {
	if recorder == nil {
		return primary
	}
	return &teeHandler{primary: primary, recorder: recorder}
}

func (h *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.primary.Enabled(ctx, level)
}

func (h *teeHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.primary.Handle(ctx, record)
	h.observe(record)
	return err
}

func (h *teeHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	copy := h.clone()
	copy.primary = h.primary.WithAttrs(attributes)
	for _, attribute := range attributes {
		copy.bound = append(copy.bound, scopedAttribute{
			attribute: attribute, groups: append([]string(nil), h.groups...),
		})
	}
	return copy
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	copy := h.clone()
	copy.primary = h.primary.WithGroup(name)
	copy.groups = append(copy.groups, name)
	return copy
}

func (h *teeHandler) clone() *teeHandler {
	return &teeHandler{
		primary: h.primary, recorder: h.recorder,
		bound:  append([]scopedAttribute(nil), h.bound...),
		groups: append([]string(nil), h.groups...),
	}
}

func (h *teeHandler) observe(record slog.Record) {
	collector := attributeCollector{recorder: h.recorder}
	for _, bound := range h.bound {
		collector.add(bound.groups, bound.attribute, 0)
	}
	record.Attrs(func(attribute slog.Attr) bool {
		collector.add(h.groups, attribute, 0)
		return !collector.exceeded
	})
	attributes := collector.attributes
	if _, contentEvent := conversationLogMessages[record.Message]; contentEvent {
		filtered := make([]LogAttribute, 0, len(attributes)+1)
		for _, attribute := range attributes {
			if _, private := conversationLogFields[path.Base(strings.ReplaceAll(attribute.Key, ".", "/"))]; !private {
				filtered = append(filtered, attribute)
			}
		}
		attributes = append(filtered, LogAttribute{Key: "diagnostic_content", Value: "see correlated conversation"})
	}
	event := LogEvent{
		Timestamp: record.Time.UTC(), Level: strings.ToLower(record.Level.String()),
		Message: h.recorder.redact(record.Message), Attributes: attributes,
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	for _, attribute := range attributes {
		switch attribute.Key {
		case "component":
			event.Component = attribute.Value
		case "job_kind":
			event.JobKind = attribute.Value
		case "project_id":
			event.ProjectID, _ = strconv.ParseInt(attribute.Value, 10, 64)
		case "merge_request_iid":
			event.MergeRequestID, _ = strconv.ParseInt(attribute.Value, 10, 64)
		case "generation_id":
			event.GenerationID, _ = strconv.ParseInt(attribute.Value, 10, 64)
		}
	}
	if collector.exceeded {
		event.Attributes = make([]LogAttribute, maxLogAttributes+1)
	}
	h.recorder.addLog(event)
}

type attributeCollector struct {
	recorder   *Recorder
	attributes []LogAttribute
	exceeded   bool
}

func (c *attributeCollector) add(groups []string, attribute slog.Attr, depth int) {
	if c.exceeded || attribute.Equal(slog.Attr{}) {
		return
	}
	if depth > 16 {
		c.exceeded = true
		return
	}
	value := attribute.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		nestedGroups := groups
		if attribute.Key != "" {
			nestedGroups = append(append([]string(nil), groups...), attribute.Key)
		}
		for _, nested := range value.Group() {
			c.add(nestedGroups, nested, depth+1)
		}
		return
	}
	if len(c.attributes) >= maxLogAttributes {
		c.exceeded = true
		return
	}
	keyParts := append([]string(nil), groups...)
	if attribute.Key != "" {
		keyParts = append(keyParts, attribute.Key)
	}
	key := c.recorder.redact(strings.Join(keyParts, "."))
	c.attributes = append(c.attributes, LogAttribute{Key: key, Value: c.value(value)})
}

func (c *attributeCollector) value(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		return c.recorder.redact(value.String())
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(value.Bool())
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return value.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindAny:
		encoded, err := json.Marshal(value.Any())
		if err != nil {
			return "[unencodable attribute]"
		}
		return c.recorder.redact(string(encoded))
	case slog.KindLogValuer:
		return c.value(value.Resolve())
	default:
		return c.recorder.redact(fmt.Sprint(value.Any()))
	}
}
