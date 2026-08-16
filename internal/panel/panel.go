package panel

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aminvakil/wormtamer/internal/store"
)

const (
	pageSize        = 50
	dashboardRecent = 10
)

//go:embed templates/*.html assets/*.css
var files embed.FS

type Store interface {
	ReadDashboard(context.Context, int) (store.Dashboard, error)
	ListReviewRecords(context.Context, string, int64, int) (store.ReviewRecordsPage, error)
	GetReviewRecord(context.Context, int64) (store.ReviewRecordDetail, error)
	ListFeedbackRecords(context.Context, string, int64, int) (store.FeedbackRecordsPage, error)
	ListMemoryRecords(context.Context, *bool, int64, int) (store.MemoryRecordsPage, error)
}

type Config struct {
	GitLabBaseURL                  string
	GeminiEndpoint                 string
	GeminiModel                    string
	GeminiThinkingLevel            string
	LogLevel                       string
	AuthorizedRepositories         []string
	ShareAllAuthorizedRepositories bool
	RepositorySharing              map[string][]string
	AllowedPublicDomains           []string
	PublicGitHubRepositories       []string
}

type Handler struct {
	store     Store
	logger    *slog.Logger
	templates *template.Template
	css       []byte
	config    configView
	gitlabURL *url.URL
}

type configView struct {
	GitLabBaseURL            string
	GeminiEndpoint           string
	GeminiModel              string
	GeminiThinkingLevel      string
	LogLevel                 string
	AuthorizedRepositories   []string
	SharingMode              string
	RepositorySharing        []sharingView
	AllowedPublicDomains     []string
	PublicGitHubRepositories []string
}

type sharingView struct {
	Repository string
	Related    []string
}

type stateView struct {
	State string
	Count int
}

type reviewView struct {
	Record          store.ReviewRecord
	Project         string
	MergeRequestURL string
}

type feedbackView struct {
	Record          store.FeedbackRecord
	Project         string
	MergeRequestURL string
}

type memoryView struct {
	Record     store.MemoryRecord
	Project    string
	SourceLink string
}

type overviewPage struct {
	Page           string
	Config         configView
	ReviewStates   []stateView
	FeedbackStates []stateView
	OldestReview   *time.Time
	OldestFeedback *time.Time
	ActiveMemories int
	RecentReviews  []reviewView
	RecentFeedback []feedbackView
}

type reviewsPage struct {
	Page       string
	Records    []reviewView
	State      string
	NextURL    string
	StateLinks []filterLink
}

type reviewDetailPage struct {
	Page            string
	Detail          store.ReviewRecordDetail
	Project         string
	MergeRequestURL string
	PublicationURL  string
}

type feedbackPage struct {
	Page       string
	Records    []feedbackView
	State      string
	NextURL    string
	StateLinks []filterLink
}

type memoryPage struct {
	Page        string
	Records     []memoryView
	Active      string
	NextURL     string
	FilterLinks []filterLink
}

type filterLink struct {
	Label   string
	URL     string
	Current bool
}

func New(storage Store, config Config, logger *slog.Logger) (*Handler, error) {
	if storage == nil {
		return nil, errors.New("panel store is required")
	}
	gitLabURL, err := url.Parse(config.GitLabBaseURL)
	if err != nil || (gitLabURL.Scheme != "http" && gitLabURL.Scheme != "https") || gitLabURL.Host == "" {
		return nil, errors.New("invalid panel GitLab base URL")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	templates, err := template.New("panel").Funcs(template.FuncMap{
		"formatTime":         formatTime,
		"formatOptionalTime": formatOptionalTime,
		"shortSHA":           shortSHA,
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse panel templates: %w", err)
	}
	css, err := files.ReadFile("assets/panel.css")
	if err != nil {
		return nil, fmt.Errorf("read panel stylesheet: %w", err)
	}
	return &Handler{
		store: storage, logger: logger, templates: templates, css: css,
		config: panelConfig(config), gitlabURL: gitLabURL,
	}, nil
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.overview)
	mux.HandleFunc("GET /reviews", h.reviews)
	mux.HandleFunc("GET /reviews/{jobID}", h.reviewDetail)
	mux.HandleFunc("GET /feedback", h.feedback)
	mux.HandleFunc("GET /memory", h.memory)
	mux.HandleFunc("GET /assets/panel.css", h.stylesheet)
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		setSecurityHeaders(w)
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, request)
	})
}

func (h *Handler) overview(w http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		h.badRequest(w)
		return
	}
	dashboard, err := h.store.ReadDashboard(request.Context(), dashboardRecent)
	if err != nil {
		h.internalError(w, "overview")
		return
	}
	page := overviewPage{
		Page: "overview", Config: h.config,
		ReviewStates:   reviewStateViews(dashboard.ReviewCounts),
		FeedbackStates: feedbackStateViews(dashboard.FeedbackCounts),
		OldestReview:   dashboard.OldestQueuedReview,
		OldestFeedback: dashboard.OldestQueuedFeedback,
		ActiveMemories: dashboard.ActiveMemoryCount,
		RecentReviews:  h.reviewViews(dashboard.RecentReviews),
		RecentFeedback: h.feedbackViews(dashboard.RecentFeedback),
	}
	h.render(w, "overview", page)
}

func (h *Handler) reviews(w http.ResponseWriter, request *http.Request) {
	state, before, ok := parseListQuery(request.URL.Query(), reviewStates())
	if !ok {
		h.badRequest(w)
		return
	}
	pageRecords, err := h.store.ListReviewRecords(request.Context(), state, before, pageSize)
	if err != nil {
		h.internalError(w, "reviews")
		return
	}
	page := reviewsPage{
		Page: "reviews", Records: h.reviewViews(pageRecords.Records), State: state,
		StateLinks: stateLinks("/reviews", state, reviewStates()),
	}
	if pageRecords.NextBefore > 0 {
		page.NextURL = listURL("/reviews", state, pageRecords.NextBefore, "state")
	}
	h.render(w, "reviews", page)
}

func (h *Handler) reviewDetail(w http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		h.badRequest(w)
		return
	}
	jobID, err := strconv.ParseInt(request.PathValue("jobID"), 10, 64)
	if err != nil || jobID <= 0 {
		h.badRequest(w)
		return
	}
	detail, err := h.store.GetReviewRecord(request.Context(), jobID)
	if errors.Is(err, store.ErrReviewRecordNotFound) {
		h.writeError(w, http.StatusNotFound)
		return
	}
	if err != nil {
		h.internalError(w, "review_detail")
		return
	}
	mergeRequestURL := h.mergeRequestURL(detail.ProjectPath, detail.MergeRequestIID)
	page := reviewDetailPage{
		Page: "reviews", Detail: detail,
		Project:         projectLabel(detail.ProjectPath, detail.ProjectID),
		MergeRequestURL: mergeRequestURL,
	}
	if mergeRequestURL != "" && detail.GitLabNoteID > 0 {
		page.PublicationURL = mergeRequestURL + "#note_" + strconv.FormatInt(detail.GitLabNoteID, 10)
	}
	h.render(w, "review-detail", page)
}

func (h *Handler) feedback(w http.ResponseWriter, request *http.Request) {
	state, before, ok := parseListQuery(request.URL.Query(), feedbackStates())
	if !ok {
		h.badRequest(w)
		return
	}
	pageRecords, err := h.store.ListFeedbackRecords(request.Context(), state, before, pageSize)
	if err != nil {
		h.internalError(w, "feedback")
		return
	}
	page := feedbackPage{
		Page: "feedback", Records: h.feedbackViews(pageRecords.Records), State: state,
		StateLinks: stateLinks("/feedback", state, feedbackStates()),
	}
	if pageRecords.NextBefore > 0 {
		page.NextURL = listURL("/feedback", state, pageRecords.NextBefore, "state")
	}
	h.render(w, "feedback", page)
}

func (h *Handler) memory(w http.ResponseWriter, request *http.Request) {
	activeText, active, before, ok := parseMemoryQuery(request.URL.Query())
	if !ok {
		h.badRequest(w)
		return
	}
	pageRecords, err := h.store.ListMemoryRecords(request.Context(), active, before, pageSize)
	if err != nil {
		h.internalError(w, "memory")
		return
	}
	page := memoryPage{
		Page: "memory", Records: h.memoryViews(pageRecords.Records), Active: activeText,
		FilterLinks: []filterLink{
			{Label: "All", URL: "/memory", Current: activeText == ""},
			{Label: "Active", URL: "/memory?active=true", Current: activeText == "true"},
			{Label: "Inactive", URL: "/memory?active=false", Current: activeText == "false"},
		},
	}
	if pageRecords.NextBefore > 0 {
		page.NextURL = listURL("/memory", activeText, pageRecords.NextBefore, "active")
	}
	h.render(w, "memory", page)
}

func (h *Handler) stylesheet(w http.ResponseWriter, _ *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(h.css)
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	var output bytes.Buffer
	if err := h.templates.ExecuteTemplate(&output, name, data); err != nil {
		h.logger.Error("panel rendering failed", "page", name, "reason", "template_failed")
		h.writeError(w, http.StatusInternalServerError)
		return
	}
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = output.WriteTo(w)
}

func (h *Handler) badRequest(w http.ResponseWriter) {
	h.writeError(w, http.StatusBadRequest)
}

func (h *Handler) internalError(w http.ResponseWriter, page string) {
	h.logger.Error("panel request failed", "page", page, "reason", "persistence_failed")
	h.writeError(w, http.StatusInternalServerError)
}

func (h *Handler) writeError(w http.ResponseWriter, status int) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, http.StatusText(status), status)
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func panelConfig(config Config) configView {
	view := configView{
		GitLabBaseURL:            config.GitLabBaseURL,
		GeminiEndpoint:           config.GeminiEndpoint,
		GeminiModel:              config.GeminiModel,
		GeminiThinkingLevel:      config.GeminiThinkingLevel,
		LogLevel:                 config.LogLevel,
		AuthorizedRepositories:   append([]string(nil), config.AuthorizedRepositories...),
		AllowedPublicDomains:     append([]string(nil), config.AllowedPublicDomains...),
		PublicGitHubRepositories: append([]string(nil), config.PublicGitHubRepositories...),
	}
	if view.GeminiEndpoint == "" {
		view.GeminiEndpoint = "Gemini Developer API"
	}
	if config.ShareAllAuthorizedRepositories {
		view.SharingMode = "All authorized repositories"
		return view
	}
	view.SharingMode = "Directional rules"
	for _, repository := range config.AuthorizedRepositories {
		related := config.RepositorySharing[repository]
		if len(related) == 0 {
			continue
		}
		view.RepositorySharing = append(view.RepositorySharing, sharingView{
			Repository: repository,
			Related:    append([]string(nil), related...),
		})
	}
	return view
}

func reviewStateViews(counts []store.StateCount) []stateView {
	return orderedStateViews(counts, reviewStates())
}

func feedbackStateViews(counts []store.StateCount) []stateView {
	return orderedStateViews(counts, feedbackStates())
}

func orderedStateViews(counts []store.StateCount, states []string) []stateView {
	byState := make(map[string]int, len(counts))
	for _, count := range counts {
		byState[count.State] = count.Count
	}
	views := make([]stateView, len(states))
	for index, state := range states {
		views[index] = stateView{State: state, Count: byState[state]}
	}
	return views
}

func reviewStates() []string {
	return []string{store.JobQueued, store.JobRunning, store.JobPublishing, store.JobCompleted, store.JobFailed, store.JobObsolete}
}

func feedbackStates() []string {
	return []string{store.FeedbackQueued, store.FeedbackRunning, store.FeedbackCompleted, store.FeedbackFailed}
}

func stateLinks(path, current string, states []string) []filterLink {
	links := []filterLink{{Label: "All", URL: path, Current: current == ""}}
	for _, state := range states {
		links = append(links, filterLink{
			Label: strings.ToUpper(state[:1]) + state[1:],
			URL:   path + "?state=" + url.QueryEscape(state), Current: current == state,
		})
	}
	return links
}

func parseListQuery(values url.Values, states []string) (string, int64, bool) {
	if !onlyQueryKeys(values, "state", "before") || len(values["state"]) > 1 || len(values["before"]) > 1 {
		return "", 0, false
	}
	state := values.Get("state")
	if state != "" && !contains(states, state) {
		return "", 0, false
	}
	before, ok := parseBefore(values.Get("before"))
	return state, before, ok
}

func parseMemoryQuery(values url.Values) (string, *bool, int64, bool) {
	if !onlyQueryKeys(values, "active", "before") || len(values["active"]) > 1 || len(values["before"]) > 1 {
		return "", nil, 0, false
	}
	activeText := values.Get("active")
	var active *bool
	switch activeText {
	case "":
	case "true":
		value := true
		active = &value
	case "false":
		value := false
		active = &value
	default:
		return "", nil, 0, false
	}
	before, ok := parseBefore(values.Get("before"))
	return activeText, active, before, ok
}

func onlyQueryKeys(values url.Values, allowed ...string) bool {
	for key := range values {
		if !contains(allowed, key) {
			return false
		}
	}
	return true
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func parseBefore(value string) (int64, bool) {
	if value == "" {
		return 0, true
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil && parsed > 0
}

func listURL(path, filter string, before int64, filterName string) string {
	values := url.Values{"before": {strconv.FormatInt(before, 10)}}
	if filter != "" {
		values.Set(filterName, filter)
	}
	return path + "?" + values.Encode()
}

func (h *Handler) reviewViews(records []store.ReviewRecord) []reviewView {
	views := make([]reviewView, len(records))
	for index, record := range records {
		views[index] = reviewView{
			Record: record, Project: projectLabel(record.ProjectPath, record.ProjectID),
			MergeRequestURL: h.mergeRequestURL(record.ProjectPath, record.MergeRequestIID),
		}
	}
	return views
}

func (h *Handler) feedbackViews(records []store.FeedbackRecord) []feedbackView {
	views := make([]feedbackView, len(records))
	for index, record := range records {
		views[index] = feedbackView{
			Record: record, Project: projectLabel(record.ProjectPath, record.ProjectID),
			MergeRequestURL: h.mergeRequestURL(record.ProjectPath, record.MergeRequestIID),
		}
	}
	return views
}

func (h *Handler) memoryViews(records []store.MemoryRecord) []memoryView {
	views := make([]memoryView, len(records))
	for index, record := range records {
		views[index] = memoryView{
			Record: record, Project: projectLabel(record.ProjectPath, record.ProjectID),
			SourceLink: h.gitLabLink(record.SourceURL),
		}
	}
	return views
}

func (h *Handler) mergeRequestURL(projectPath string, iid int64) string {
	if projectPath == "" || iid <= 0 {
		return ""
	}
	copy := *h.gitlabURL
	copy.Path = strings.TrimSuffix(copy.Path, "/") + "/" + projectPath + "/-/merge_requests/" + strconv.FormatInt(iid, 10)
	copy.RawPath = ""
	copy.RawQuery = ""
	copy.Fragment = ""
	return copy.String()
}

func (h *Handler) gitLabLink(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Scheme != h.gitlabURL.Scheme || parsed.Host != h.gitlabURL.Host {
		return ""
	}
	basePath := strings.TrimSuffix(h.gitlabURL.Path, "/")
	if basePath != "" && parsed.Path != basePath && !strings.HasPrefix(parsed.Path, basePath+"/") {
		return ""
	}
	return parsed.String()
}

func projectLabel(path string, projectID int64) string {
	if path != "" {
		return path
	}
	return "Project #" + strconv.FormatInt(projectID, 10)
}

func formatTime(timestamp time.Time) string {
	if timestamp.IsZero() {
		return "—"
	}
	return timestamp.UTC().Format("2006-01-02 15:04:05 UTC")
}

func formatOptionalTime(timestamp *time.Time) string {
	if timestamp == nil {
		return "—"
	}
	return formatTime(*timestamp)
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
