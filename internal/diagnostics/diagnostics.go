package diagnostics

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aminvakil/wormtamer/internal/usage"
)

const (
	maxConversationEntries     = 64
	maxConversationRecordBytes = 4 << 20
	maxConversationTotalBytes  = 32 << 20
	maxLogEntries              = 2000
	maxLogRecordBytes          = 16 << 10
	maxLogTotalBytes           = 8 << 20
	maxLogAttributes           = 64
)

const (
	ContentOmittedLimitExceeded = "limit_exceeded"
	RedactedSensitiveContent    = "[redacted sensitive content]"
)

type BufferState struct {
	StartedAt             time.Time
	ContentEnabled        bool
	ConversationEvictions uint64
	LogEvictions          uint64
}

type ConversationFilter struct {
	JobKind        string
	ProjectID      int64
	MergeRequestID int64
	GenerationID   int64
}

type ConversationSnapshot struct {
	State         BufferState
	Conversations []Conversation
}

type Conversation struct {
	JobKind           string
	ReviewJobID       int64
	FeedbackJobID     int64
	WorkflowAttempt   int
	ProjectID         int64
	ProjectPath       string
	MergeRequestID    int64
	StartedAt         time.Time
	UpdatedAt         time.Time
	Generations       []Generation
	SystemInstruction string
	InitialPrompt     string
	Events            []ConversationEvent
	ContentCaptured   bool
	ContentOmitted    string
	byteSize          int
}

type Generation struct {
	ID                     int64
	ReviewTurn             *int
	ConfiguredModel        string
	ResolvedModel          string
	RequestStartedAt       time.Time
	CompletedAt            *time.Time
	CompletionState        string
	LatencyMS              *int64
	FinishReason           string
	StructuredValidation   string
	FinalOnly              bool
	UsageMetadataAvailable bool
}

type ConversationEvent struct {
	Kind         string
	GenerationID int64
	ReviewTurn   *int
	Text         string
	Calls        []FunctionCall
	Responses    []ToolResponse
}

type FunctionCall struct {
	ID        string
	Name      string
	Arguments string
}

type ToolResponse struct {
	ID       string
	Name     string
	Response string
	Denied   bool
}

type ConversationStart struct {
	GenerationID      int64
	ProjectID         int64
	ProjectPath       string
	MergeRequestID    int64
	SystemInstruction string
	Prompt            string
}

type ModelTurn struct {
	GenerationID int64
	ReviewTurn   *int
	Text         string
	Calls        []FunctionCall
}

type ConversationRecorder interface {
	BeginConversation(context.Context, ConversationStart)
	RecordModelTurn(context.Context, ModelTurn)
	RecordToolResponses(context.Context, int64, *int, []ToolResponse)
	RecordDecision(context.Context, int64, string)
}

type LogFilter struct {
	Level          string
	Component      string
	JobKind        string
	ProjectID      int64
	MergeRequestID int64
	GenerationID   int64
}

type LogSnapshot struct {
	State      BufferState
	Events     []LogEvent
	NextBefore uint64
}

type LogEvent struct {
	ID             uint64
	Timestamp      time.Time
	Level          string
	Message        string
	Attributes     []LogAttribute
	Component      string
	JobKind        string
	ProjectID      int64
	MergeRequestID int64
	GenerationID   int64
	ContentOmitted string
	byteSize       int
}

type LogAttribute struct {
	Key   string
	Value string
}

type Recorder struct {
	mu             sync.Mutex
	startedAt      time.Time
	contentEnabled bool
	forbidden      []string

	conversations       []*Conversation
	conversationBytes   int
	conversationEvicted uint64

	logs       []LogEvent
	logBytes   int
	logEvicted uint64
	nextLogID  uint64
}

func New(contentEnabled bool, forbidden []string) *Recorder {
	return &Recorder{
		startedAt: time.Now().UTC(), contentEnabled: contentEnabled,
		forbidden: append([]string(nil), forbidden...),
	}
}

func (r *Recorder) State() BufferState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateLocked()
}

func (r *Recorder) stateLocked() BufferState {
	return BufferState{
		StartedAt: r.startedAt, ContentEnabled: r.contentEnabled,
		ConversationEvictions: r.conversationEvicted, LogEvictions: r.logEvicted,
	}
}

func (r *Recorder) BeginConversation(ctx context.Context, start ConversationStart) {
	if start.GenerationID <= 0 || start.ProjectID <= 0 || start.MergeRequestID <= 0 {
		return
	}
	scope, ok := usage.ScopeFromContext(ctx)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.conversationIndexByGenerationLocked(start.GenerationID)
	if index < 0 {
		conversation := newConversation(scope)
		conversation.Generations = append(conversation.Generations, Generation{
			ID: start.GenerationID, CompletionState: usage.CompletionStarted,
		})
		r.conversations = append(r.conversations, &conversation)
		index = len(r.conversations) - 1
	}
	conversation := cloneConversation(*r.conversations[index])
	r.conversationBytes -= conversation.byteSize
	conversation.ProjectID = start.ProjectID
	conversation.ProjectPath = r.redact(start.ProjectPath)
	conversation.MergeRequestID = start.MergeRequestID
	if r.contentEnabled && conversation.ContentOmitted == "" && !conversation.ContentCaptured {
		conversation.SystemInstruction = r.redact(start.SystemInstruction)
		conversation.InitialPrompt = r.redact(start.Prompt)
		conversation.ContentCaptured = true
	}
	conversation.byteSize = conversationSize(conversation)
	if conversation.byteSize > maxConversationRecordBytes {
		omitConversationContent(&conversation)
	}
	r.conversations[index] = &conversation
	r.conversationBytes += conversation.byteSize
	r.enforceConversationLimitsLocked()
}

func (r *Recorder) RecordModelTurn(ctx context.Context, turn ModelTurn) {
	if !r.contentEnabled || turn.GenerationID <= 0 {
		return
	}
	scope, ok := usage.ScopeFromContext(ctx)
	if !ok {
		return
	}
	event := ConversationEvent{
		Kind: "model", GenerationID: turn.GenerationID, ReviewTurn: cloneInt(turn.ReviewTurn),
		Text: r.redact(turn.Text), Calls: make([]FunctionCall, len(turn.Calls)),
	}
	for index, call := range turn.Calls {
		event.Calls[index] = FunctionCall{
			ID: r.redact(call.ID), Name: r.redact(call.Name), Arguments: r.redact(call.Arguments),
		}
	}
	r.appendConversationEvent(scope, event)
}

func (r *Recorder) RecordToolResponses(ctx context.Context, generationID int64, turn *int, responses []ToolResponse) {
	if !r.contentEnabled || generationID <= 0 || len(responses) == 0 {
		return
	}
	scope, ok := usage.ScopeFromContext(ctx)
	if !ok {
		return
	}
	event := ConversationEvent{
		Kind: "tool", GenerationID: generationID, ReviewTurn: cloneInt(turn),
		Responses: make([]ToolResponse, len(responses)),
	}
	for index, response := range responses {
		event.Responses[index] = ToolResponse{
			ID: r.redact(response.ID), Name: r.redact(response.Name),
			Response: r.redact(response.Response), Denied: response.Denied,
		}
	}
	r.appendConversationEvent(scope, event)
}

func (r *Recorder) RecordDecision(ctx context.Context, generationID int64, text string) {
	if !r.contentEnabled || generationID <= 0 {
		return
	}
	scope, ok := usage.ScopeFromContext(ctx)
	if !ok {
		return
	}
	r.appendConversationEvent(scope, ConversationEvent{
		Kind: "decision", GenerationID: generationID, Text: r.redact(text),
	})
}

func (r *Recorder) appendConversationEvent(scope usage.Scope, event ConversationEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.conversationIndexByGenerationLocked(event.GenerationID)
	if index < 0 || !conversationMatchesScope(*r.conversations[index], scope) {
		return
	}
	conversation := cloneConversation(*r.conversations[index])
	if conversation.ContentOmitted != "" || !conversation.ContentCaptured {
		return
	}
	r.conversationBytes -= conversation.byteSize
	conversation.Events = append(conversation.Events, event)
	conversation.byteSize = conversationSize(conversation)
	if conversation.byteSize > maxConversationRecordBytes {
		omitConversationContent(&conversation)
	}
	r.conversations[index] = &conversation
	r.conversationBytes += conversation.byteSize
	r.enforceConversationLimitsLocked()
}

func (r *Recorder) generationStarted(id int64, start usage.GenerationStart) {
	if id <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	index := -1
	if start.RequestKind == usage.RequestReview && start.Turn != nil && *start.Turn > 0 {
		index = r.conversationIndexLocked(start.Scope)
	}
	if index < 0 {
		conversation := newConversation(start.Scope)
		r.conversations = append(r.conversations, &conversation)
		index = len(r.conversations) - 1
	}
	conversation := cloneConversation(*r.conversations[index])
	r.conversationBytes -= conversation.byteSize
	conversation.Generations = append(conversation.Generations, Generation{
		ID: id, ReviewTurn: cloneInt(start.Turn), ConfiguredModel: r.metadataValue(start.ConfiguredModel, 256),
		RequestStartedAt: start.StartedAt, CompletionState: usage.CompletionStarted, FinalOnly: start.FinalOnly,
	})
	if conversation.StartedAt.IsZero() || start.StartedAt.Before(conversation.StartedAt) {
		conversation.StartedAt = start.StartedAt
	}
	conversation.UpdatedAt = start.StartedAt
	conversation.byteSize = conversationSize(conversation)
	if conversation.byteSize > maxConversationRecordBytes {
		omitConversationContent(&conversation)
	}
	r.conversations[index] = &conversation
	r.conversationBytes += conversation.byteSize
	r.enforceConversationLimitsLocked()
}

func (r *Recorder) generationCompleted(id int64, completion usage.GenerationCompletion) {
	if id <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for conversationIndex, source := range r.conversations {
		for generationIndex, sourceGeneration := range source.Generations {
			if sourceGeneration.ID != id {
				continue
			}
			conversation := cloneConversation(*source)
			generation := &conversation.Generations[generationIndex]
			r.conversationBytes -= conversation.byteSize
			completed := completion.CompletedAt
			latency := completion.Latency.Milliseconds()
			generation.CompletedAt = &completed
			generation.LatencyMS = &latency
			generation.CompletionState = completion.State
			generation.ResolvedModel = r.metadataValue(completion.ResolvedModel, 256)
			generation.FinishReason = r.metadataValue(completion.FinishReason, 128)
			generation.StructuredValidation = r.metadataValue(completion.StructuredValidation, 128)
			generation.UsageMetadataAvailable = completion.UsageMetadataAvailable
			conversation.UpdatedAt = completion.CompletedAt
			conversation.byteSize = conversationSize(conversation)
			if conversation.byteSize > maxConversationRecordBytes {
				omitConversationContent(&conversation)
			}
			r.conversations[conversationIndex] = &conversation
			r.conversationBytes += conversation.byteSize
			r.enforceConversationLimitsLocked()
			return
		}
	}
}

func newConversation(scope usage.Scope) Conversation {
	conversation := Conversation{JobKind: scope.RequestKind, WorkflowAttempt: scope.Attempt}
	if scope.RequestKind == usage.RequestReview {
		conversation.ReviewJobID = scope.ReviewJobID
	} else {
		conversation.FeedbackJobID = scope.FeedbackJobID
	}
	return conversation
}

func (r *Recorder) conversationIndexLocked(scope usage.Scope) int {
	for index := len(r.conversations) - 1; index >= 0; index-- {
		if conversationMatchesScope(*r.conversations[index], scope) {
			return index
		}
	}
	return -1
}

func (r *Recorder) conversationIndexByGenerationLocked(generationID int64) int {
	for index := len(r.conversations) - 1; index >= 0; index-- {
		for _, generation := range r.conversations[index].Generations {
			if generation.ID == generationID {
				return index
			}
		}
	}
	return -1
}

func conversationMatchesScope(conversation Conversation, scope usage.Scope) bool {
	return conversation.JobKind == scope.RequestKind && conversation.WorkflowAttempt == scope.Attempt &&
		conversation.ReviewJobID == scope.ReviewJobID && conversation.FeedbackJobID == scope.FeedbackJobID
}

func omitConversationContent(conversation *Conversation) {
	conversation.SystemInstruction = ""
	conversation.InitialPrompt = ""
	conversation.Events = nil
	conversation.ContentCaptured = false
	conversation.ContentOmitted = ContentOmittedLimitExceeded
	conversation.byteSize = conversationSize(*conversation)
}

func (r *Recorder) enforceConversationLimitsLocked() {
	for len(r.conversations) > maxConversationEntries || r.conversationBytes > maxConversationTotalBytes {
		index := oldestCompletedConversation(r.conversations)
		if index < 0 {
			// Hard ceilings and retention of every active record are mutually exclusive.
			// Production has at most one active review and one active feedback conversation,
			// so this defensive path drops the newest active record.
			index = len(r.conversations) - 1
		}
		r.removeConversationLocked(index)
	}
}

func oldestCompletedConversation(conversations []*Conversation) int {
	for index, conversation := range conversations {
		if !conversationActive(*conversation) {
			return index
		}
	}
	return -1
}

func conversationActive(conversation Conversation) bool {
	return len(conversation.Generations) > 0 &&
		conversation.Generations[len(conversation.Generations)-1].CompletionState == usage.CompletionStarted
}

func (r *Recorder) removeConversationLocked(index int) {
	r.conversationBytes -= r.conversations[index].byteSize
	last := len(r.conversations) - 1
	copy(r.conversations[index:], r.conversations[index+1:])
	r.conversations[last] = nil
	r.conversations = r.conversations[:last]
	r.conversationEvicted++
}

func (r *Recorder) Conversations(filter ConversationFilter) ConversationSnapshot {
	r.mu.Lock()
	state := r.stateLocked()
	selected := make([]*Conversation, 0, len(r.conversations))
	for index := len(r.conversations) - 1; index >= 0; index-- {
		conversation := r.conversations[index]
		if matchesConversation(*conversation, filter) {
			selected = append(selected, conversation)
		}
	}
	r.mu.Unlock()
	snapshot := ConversationSnapshot{State: state, Conversations: make([]Conversation, len(selected))}
	for index, conversation := range selected {
		copy := cloneConversation(*conversation)
		copy.SystemInstruction = ""
		copy.InitialPrompt = ""
		copy.Events = nil
		copy.byteSize = conversationSize(copy)
		snapshot.Conversations[index] = copy
	}
	return snapshot
}

func (r *Recorder) ConversationByGeneration(generationID int64) (Conversation, bool) {
	if generationID <= 0 {
		return Conversation{}, false
	}
	r.mu.Lock()
	var selected *Conversation
	for _, conversation := range r.conversations {
		for _, generation := range conversation.Generations {
			if generation.ID == generationID {
				selected = conversation
				break
			}
		}
		if selected != nil {
			break
		}
	}
	r.mu.Unlock()
	if selected == nil {
		return Conversation{}, false
	}
	return cloneConversation(*selected), true
}

func matchesConversation(conversation Conversation, filter ConversationFilter) bool {
	if filter.JobKind != "" && conversation.JobKind != filter.JobKind ||
		filter.ProjectID > 0 && conversation.ProjectID != filter.ProjectID ||
		filter.MergeRequestID > 0 && conversation.MergeRequestID != filter.MergeRequestID {
		return false
	}
	if filter.GenerationID > 0 {
		for _, generation := range conversation.Generations {
			if generation.ID == filter.GenerationID {
				return true
			}
		}
		return false
	}
	return true
}

func (r *Recorder) addLog(event LogEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextLogID++
	event.ID = r.nextLogID
	event.byteSize = logSize(event)
	if len(event.Attributes) > maxLogAttributes || event.byteSize > maxLogRecordBytes {
		event.Message = ""
		event.Attributes = nil
		if len(event.Component) > 128 {
			event.Component = ""
		}
		if event.JobKind != usage.RequestReview && event.JobKind != usage.RequestFeedback {
			event.JobKind = ""
		}
		if event.Component != "" {
			event.Attributes = append(event.Attributes, LogAttribute{Key: "component", Value: event.Component})
		}
		if event.JobKind != "" {
			event.Attributes = append(event.Attributes, LogAttribute{Key: "job_kind", Value: event.JobKind})
		}
		for _, identity := range []struct {
			key   string
			value int64
		}{
			{key: "project_id", value: event.ProjectID},
			{key: "merge_request_iid", value: event.MergeRequestID},
			{key: "generation_id", value: event.GenerationID},
		} {
			if identity.value > 0 {
				event.Attributes = append(event.Attributes, LogAttribute{Key: identity.key, Value: strconv.FormatInt(identity.value, 10)})
			}
		}
		event.ContentOmitted = ContentOmittedLimitExceeded
		event.byteSize = logSize(event)
	}
	r.logs = append(r.logs, event)
	r.logBytes += event.byteSize
	for len(r.logs) > maxLogEntries || r.logBytes > maxLogTotalBytes {
		r.logBytes -= r.logs[0].byteSize
		r.logs[0] = LogEvent{}
		r.logs = r.logs[1:]
		r.logEvicted++
	}
}

func (r *Recorder) Logs(filter LogFilter, before uint64, limit int) LogSnapshot {
	if limit <= 0 || limit > maxLogEntries {
		limit = maxLogEntries
	}
	r.mu.Lock()
	snapshot := LogSnapshot{State: r.stateLocked()}
	for index := len(r.logs) - 1; index >= 0; index-- {
		event := r.logs[index]
		if before > 0 && event.ID >= before || !matchesLog(event, filter) {
			continue
		}
		if len(snapshot.Events) == limit {
			snapshot.NextBefore = snapshot.Events[len(snapshot.Events)-1].ID
			break
		}
		snapshot.Events = append(snapshot.Events, event)
	}
	r.mu.Unlock()
	for index := range snapshot.Events {
		snapshot.Events[index].Attributes = append([]LogAttribute(nil), snapshot.Events[index].Attributes...)
	}
	return snapshot
}

func matchesLog(event LogEvent, filter LogFilter) bool {
	return (filter.Level == "" || event.Level == filter.Level) &&
		(filter.Component == "" || event.Component == filter.Component) &&
		(filter.JobKind == "" || event.JobKind == filter.JobKind) &&
		(filter.ProjectID == 0 || event.ProjectID == filter.ProjectID) &&
		(filter.MergeRequestID == 0 || event.MergeRequestID == filter.MergeRequestID) &&
		(filter.GenerationID == 0 || event.GenerationID == filter.GenerationID)
}

func (r *Recorder) metadataValue(value string, limit int) string {
	value = r.redact(value)
	if len(value) > limit || !utf8.ValidString(value) {
		return "[invalid metadata omitted]"
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return "[invalid metadata omitted]"
		}
	}
	return value
}

func (r *Recorder) redact(value string) string {
	return Redact(value, r.forbidden)
}

func Redact(value string, forbidden []string) string {
	for _, secret := range forbidden {
		if secret == "" {
			continue
		}
		if strings.Contains(value, secret) {
			return RedactedSensitiveContent
		}
		escaped, err := json.Marshal(secret)
		if err == nil && len(escaped) >= 2 && strings.Contains(value, string(escaped[1:len(escaped)-1])) {
			return RedactedSensitiveContent
		}
	}
	return value
}

func conversationSize(conversation Conversation) int {
	size := 128 + len(conversation.JobKind) + len(conversation.ProjectPath) +
		len(conversation.SystemInstruction) + len(conversation.InitialPrompt) + len(conversation.ContentOmitted)
	for _, generation := range conversation.Generations {
		size += 128 + len(generation.ConfiguredModel) + len(generation.ResolvedModel) +
			len(generation.FinishReason) + len(generation.StructuredValidation)
	}
	for _, event := range conversation.Events {
		size += 64 + len(event.Kind) + len(event.Text)
		for _, call := range event.Calls {
			size += 32 + len(call.ID) + len(call.Name) + len(call.Arguments)
		}
		for _, response := range event.Responses {
			size += 32 + len(response.ID) + len(response.Name) + len(response.Response)
		}
	}
	return size
}

func logSize(event LogEvent) int {
	size := 96 + len(event.Level) + len(event.Message) + len(event.ContentOmitted)
	for _, attribute := range event.Attributes {
		size += 16 + len(attribute.Key) + len(attribute.Value)
	}
	return size
}

func cloneConversation(source Conversation) Conversation {
	copy := source
	copy.Generations = make([]Generation, len(source.Generations))
	for index, generation := range source.Generations {
		copy.Generations[index] = generation
		copy.Generations[index].ReviewTurn = cloneInt(generation.ReviewTurn)
		if generation.CompletedAt != nil {
			value := *generation.CompletedAt
			copy.Generations[index].CompletedAt = &value
		}
		if generation.LatencyMS != nil {
			value := *generation.LatencyMS
			copy.Generations[index].LatencyMS = &value
		}
	}
	copy.Events = make([]ConversationEvent, len(source.Events))
	for index, event := range source.Events {
		copy.Events[index] = event
		copy.Events[index].ReviewTurn = cloneInt(event.ReviewTurn)
		copy.Events[index].Calls = append([]FunctionCall(nil), event.Calls...)
		copy.Events[index].Responses = append([]ToolResponse(nil), event.Responses...)
	}
	return copy
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

type generationRecorder struct {
	durable  usage.GenerationRecorder
	recorder *Recorder
}

func ObserveGenerations(durable usage.GenerationRecorder, recorder *Recorder) usage.GenerationRecorder {
	if recorder == nil {
		return durable
	}
	return &generationRecorder{durable: durable, recorder: recorder}
}

func (r *generationRecorder) Start(ctx context.Context, start usage.GenerationStart) (int64, error) {
	generationID, err := r.durable.Start(ctx, start)
	if err == nil {
		r.recorder.generationStarted(generationID, start)
	}
	return generationID, err
}

func (r *generationRecorder) Complete(ctx context.Context, generationID int64, completion usage.GenerationCompletion) error {
	if err := r.durable.Complete(ctx, generationID, completion); err != nil {
		return err
	}
	r.recorder.generationCompleted(generationID, completion)
	return nil
}
