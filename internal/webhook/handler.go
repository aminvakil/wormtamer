package webhook

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aminvakil/wormtamer/internal/store"
)

const (
	maxBodyBytes             = 1 << 20
	maxConcurrentWebhooks    = 4
	overloadRetryAfterSecond = "1"
)

var headSHAPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

type EventStore interface {
	AcceptEvent(context.Context, store.Event) (store.AcceptResult, error)
	AcceptFeedbackEvent(context.Context, store.FeedbackEvent, time.Time) (store.AcceptFeedbackResult, error)
}

type Config struct {
	GitLabInstance         string
	WebhookSecret          string
	AuthorizedRepositories []string
}

type Handler struct {
	gitLabInstance string
	secretDigest   [sha256.Size]byte
	authorized     map[string]struct{}
	store          EventStore
	logger         *slog.Logger
	admission      chan struct{}
	now            func() time.Time
}

type mergeRequestPayload struct {
	ObjectKind string `json:"object_kind"`
	Project    struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	ObjectAttributes struct {
		IID            int64  `json:"iid"`
		Action         string `json:"action"`
		Draft          bool   `json:"draft"`
		WorkInProgress bool   `json:"work_in_progress"`
		LastCommit     struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
}

type notePayload struct {
	ObjectKind string `json:"object_kind"`
	EventType  string `json:"event_type"`
	User       struct {
		ID int64 `json:"id"`
	} `json:"user"`
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	ObjectAttributes struct {
		ID           int64  `json:"id"`
		Internal     bool   `json:"internal"`
		NoteableType string `json:"noteable_type"`
		AuthorID     int64  `json:"author_id"`
		UpdatedAt    string `json:"updated_at"`
		ProjectID    int64  `json:"project_id"`
		NoteableID   int64  `json:"noteable_id"`
		System       bool   `json:"system"`
		Action       string `json:"action"`
	} `json:"object_attributes"`
	MergeRequest struct {
		ID              int64 `json:"id"`
		IID             int64 `json:"iid"`
		TargetProjectID int64 `json:"target_project_id"`
	} `json:"merge_request"`
}

func New(config Config, storage EventStore, logger *slog.Logger) *Handler {
	authorized := make(map[string]struct{}, len(config.AuthorizedRepositories))
	for _, repository := range config.AuthorizedRepositories {
		authorized[repository] = struct{}{}
	}
	return &Handler{
		gitLabInstance: config.GitLabInstance,
		secretDigest:   sha256.Sum256([]byte(config.WebhookSecret)),
		authorized:     authorized,
		store:          storage,
		logger:         logger,
		admission:      make(chan struct{}, maxConcurrentWebhooks),
		now:            time.Now,
	}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthcheck", h.healthcheck)
	mux.HandleFunc("POST /webhooks/gitlab", h.gitLabWebhook)
	return mux
}

func (h *Handler) healthcheck(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

func (h *Handler) gitLabWebhook(w http.ResponseWriter, request *http.Request) {
	if !h.authenticated(request.Header.Get("X-Gitlab-Token")) {
		h.reject(w, http.StatusUnauthorized, "invalid_authentication")
		return
	}

	select {
	case h.admission <- struct{}{}:
		defer func() { <-h.admission }()
	default:
		w.Header().Set("Retry-After", overloadRetryAfterSecond)
		h.rejectWithDelivery(w, http.StatusServiceUnavailable, "ingress_overloaded", deliveryIDFromUUID(request.Header.Get("X-Gitlab-Event-UUID")))
		return
	}

	request.Body = http.MaxBytesReader(w, request.Body, maxBodyBytes)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.rejectWithDelivery(w, http.StatusRequestEntityTooLarge, "body_too_large", deliveryIDFromUUID(request.Header.Get("X-Gitlab-Event-UUID")))
			return
		}
		h.rejectWithDelivery(w, http.StatusBadRequest, "body_read_failed", deliveryIDFromUUID(request.Header.Get("X-Gitlab-Event-UUID")))
		return
	}

	delivery := deliveryID(h.gitLabInstance, request.Header.Get("X-Gitlab-Event-UUID"), body)
	switch request.Header.Get("X-Gitlab-Event") {
	case "Merge Request Hook":
		h.mergeRequestEvent(w, request, body, delivery)
	case "Note Hook":
		h.noteEvent(w, request, body, delivery)
	default:
		h.rejectWithDelivery(w, http.StatusBadRequest, "unsupported_event", delivery)
	}
}

func (h *Handler) mergeRequestEvent(w http.ResponseWriter, request *http.Request, body []byte, delivery string) {
	var payload mergeRequestPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.rejectWithDelivery(w, http.StatusBadRequest, "malformed_json", delivery)
		return
	}
	if !validPayload(payload) {
		h.logger.Warn("webhook rejected",
			"reason", "malformed_merge_request",
			"delivery_id", bounded(delivery),
			"project_id", payload.Project.ID,
			"project_path", bounded(payload.Project.PathWithNamespace),
			"merge_request_iid", payload.ObjectAttributes.IID,
			"head_sha", bounded(payload.ObjectAttributes.LastCommit.ID),
		)
		h.writeStatus(w, http.StatusBadRequest)
		return
	}

	projectPath := payload.Project.PathWithNamespace
	headSHA := strings.ToLower(payload.ObjectAttributes.LastCommit.ID)
	if _, allowed := h.authorized[projectPath]; !allowed {
		h.logger.Warn("webhook rejected",
			"reason", "unauthorized_repository",
			"delivery_id", bounded(delivery),
			"project_id", payload.Project.ID,
			"project_path", bounded(projectPath),
			"merge_request_iid", payload.ObjectAttributes.IID,
			"head_sha", bounded(headSHA),
		)
		h.writeStatus(w, http.StatusForbidden)
		return
	}

	event := store.Event{
		DeliveryID:      delivery,
		GitLabInstance:  h.gitLabInstance,
		ProjectID:       payload.Project.ID,
		ProjectPath:     projectPath,
		MergeRequestIID: payload.ObjectAttributes.IID,
		HeadSHA:         headSHA,
		Action:          payload.ObjectAttributes.Action,
		Payload:         body,
	}
	if payload.ObjectAttributes.Action != "open" {
		event.IgnoredOutcome = store.OutcomeIgnoredAction
	} else if payload.ObjectAttributes.Draft || payload.ObjectAttributes.WorkInProgress {
		event.IgnoredOutcome = store.OutcomeIgnoredDraft
	} else {
		event.QueueReview = true
	}

	result, err := h.store.AcceptEvent(request.Context(), event)
	if err != nil {
		attemptedOutcome := event.IgnoredOutcome
		if event.QueueReview {
			attemptedOutcome = store.OutcomeQueued
		}
		h.logger.Error("webhook persistence failed",
			"reason", "persistence_failed",
			"delivery_id", bounded(event.DeliveryID),
			"project_id", event.ProjectID,
			"project_path", bounded(event.ProjectPath),
			"merge_request_iid", event.MergeRequestIID,
			"head_sha", bounded(event.HeadSHA),
			"outcome", attemptedOutcome,
		)
		h.writeStatus(w, http.StatusInternalServerError)
		return
	}

	h.logger.Info("webhook accepted",
		"delivery_id", bounded(event.DeliveryID),
		"project_id", event.ProjectID,
		"project_path", bounded(event.ProjectPath),
		"merge_request_iid", event.MergeRequestIID,
		"head_sha", bounded(event.HeadSHA),
		"event_id", result.EventID,
		"job_id", result.JobID,
		"outcome", result.Outcome,
		"duplicate_delivery", result.DuplicateDelivery,
	)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) noteEvent(w http.ResponseWriter, request *http.Request, body []byte, delivery string) {
	var payload notePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.rejectWithDelivery(w, http.StatusBadRequest, "malformed_json", delivery)
		return
	}
	updatedAt, valid := validNotePayload(payload)
	if !valid {
		h.logger.Warn("webhook rejected",
			"reason", "malformed_note",
			"delivery_id", bounded(delivery),
			"project_id", payload.Project.ID,
			"project_path", bounded(payload.Project.PathWithNamespace),
			"merge_request_iid", payload.MergeRequest.IID,
			"note_id", payload.ObjectAttributes.ID,
			"actor_id", payload.User.ID,
		)
		h.writeStatus(w, http.StatusBadRequest)
		return
	}
	if _, allowed := h.authorized[payload.Project.PathWithNamespace]; !allowed {
		h.logger.Warn("webhook rejected",
			"reason", "unauthorized_repository",
			"delivery_id", bounded(delivery),
			"project_id", payload.Project.ID,
			"project_path", bounded(payload.Project.PathWithNamespace),
			"merge_request_iid", payload.MergeRequest.IID,
			"note_id", payload.ObjectAttributes.ID,
		)
		h.writeStatus(w, http.StatusForbidden)
		return
	}
	if payload.ObjectAttributes.System || payload.ObjectAttributes.Internal {
		h.logger.Info("webhook ignored",
			"reason", "unsupported_note_visibility",
			"delivery_id", bounded(delivery),
			"project_id", payload.Project.ID,
			"merge_request_iid", payload.MergeRequest.IID,
			"note_id", payload.ObjectAttributes.ID,
		)
		w.WriteHeader(http.StatusOK)
		return
	}

	event := store.FeedbackEvent{
		DeliveryID: delivery, GitLabInstance: h.gitLabInstance,
		ProjectID: payload.Project.ID, ProjectPath: payload.Project.PathWithNamespace,
		MergeRequestIID: payload.MergeRequest.IID, NoteID: payload.ObjectAttributes.ID,
		ActorID: payload.User.ID, Action: payload.ObjectAttributes.Action,
		SourceUpdatedAt: updatedAt,
	}
	result, err := h.store.AcceptFeedbackEvent(request.Context(), event, h.now().UTC())
	if err != nil {
		h.logger.Error("feedback webhook persistence failed",
			"reason", "persistence_failed",
			"delivery_id", bounded(delivery),
			"project_id", event.ProjectID,
			"merge_request_iid", event.MergeRequestIID,
			"note_id", event.NoteID,
		)
		h.writeStatus(w, http.StatusInternalServerError)
		return
	}
	h.logger.Info("feedback webhook accepted",
		"delivery_id", bounded(delivery),
		"project_id", event.ProjectID,
		"merge_request_iid", event.MergeRequestIID,
		"note_id", event.NoteID,
		"actor_id", event.ActorID,
		"event_id", result.EventID,
		"feedback_job_id", result.JobID,
		"outcome", result.Outcome,
		"duplicate_delivery", result.DuplicateDelivery,
	)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) authenticated(provided string) bool {
	providedDigest := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(h.secretDigest[:], providedDigest[:]) == 1
}

func (h *Handler) reject(w http.ResponseWriter, status int, reason string) {
	h.logger.Warn("webhook rejected", "reason", reason)
	h.writeStatus(w, status)
}

func (h *Handler) rejectWithDelivery(w http.ResponseWriter, status int, reason, delivery string) {
	if delivery == "" {
		h.reject(w, status, reason)
		return
	}
	h.logger.Warn("webhook rejected", "reason", reason, "delivery_id", bounded(delivery))
	h.writeStatus(w, status)
}

func (h *Handler) writeStatus(w http.ResponseWriter, status int) {
	http.Error(w, http.StatusText(status), status)
}

func validPayload(payload mergeRequestPayload) bool {
	attributes := payload.ObjectAttributes
	return payload.ObjectKind == "merge_request" &&
		payload.Project.ID > 0 &&
		payload.Project.PathWithNamespace != "" && len(payload.Project.PathWithNamespace) <= 255 &&
		attributes.IID > 0 &&
		attributes.Action != "" && len(attributes.Action) <= 64 &&
		headSHAPattern.MatchString(attributes.LastCommit.ID)
}

func validNotePayload(payload notePayload) (time.Time, bool) {
	attributes := payload.ObjectAttributes
	mergeRequest := payload.MergeRequest
	if payload.ObjectKind != "note" || payload.EventType != "note" || payload.User.ID <= 0 ||
		payload.Project.ID <= 0 || payload.Project.PathWithNamespace == "" || len(payload.Project.PathWithNamespace) > 255 ||
		attributes.ID <= 0 || attributes.NoteableType != "MergeRequest" || attributes.AuthorID != payload.User.ID ||
		attributes.ProjectID != payload.Project.ID || (attributes.Action != "create" && attributes.Action != "update") ||
		mergeRequest.ID <= 0 || mergeRequest.IID <= 0 || attributes.NoteableID != mergeRequest.ID ||
		mergeRequest.TargetProjectID != payload.Project.ID {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700"} {
		updatedAt, err := time.Parse(layout, attributes.UpdatedAt)
		if err == nil {
			return updatedAt.UTC(), true
		}
	}
	return time.Time{}, false
}

func deliveryID(instance, eventUUID string, body []byte) string {
	if delivery := deliveryIDFromUUID(eventUUID); delivery != "" {
		return delivery
	}
	digest := sha256.New()
	_, _ = io.WriteString(digest, instance)
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(body)
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func deliveryIDFromUUID(eventUUID string) string {
	if !validEventUUID(eventUUID) {
		return ""
	}
	return "uuid:" + eventUUID
}

func validEventUUID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func bounded(value string) string {
	const limit = 256
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
