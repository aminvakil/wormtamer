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
	"unicode/utf8"

	"github.com/aminvakil/wormtamer/internal/store"
)

const (
	pageSize        = 50
	dashboardRecent = 5
)

//go:embed templates/*.html assets/*.css
var files embed.FS

type Store interface {
	ReadDashboard(context.Context, int) (store.Dashboard, error)
	ListReviewRecords(context.Context, string, int64, int) (store.ReviewRecordsPage, error)
	GetReviewRecord(context.Context, int64) (store.ReviewRecordDetail, error)
	ListFeedbackRecords(context.Context, string, int64, int) (store.FeedbackRecordsPage, error)
	GetFeedbackRecord(context.Context, int64) (store.FeedbackRecordDetail, error)
	ListMemoryRecords(context.Context, *bool, int64, int) (store.MemoryRecordsPage, error)
	ReadUsageReport(context.Context, store.UsageQuery) (store.UsageReport, error)
	GetGenerationRecord(context.Context, int64) (store.GenerationRecord, error)
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
	now       func() time.Time
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
	URL   string
	Count int
}

type metricView struct {
	Label string
	Tone  string
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

type generationView struct {
	Record  store.GenerationRecord
	Project string
	JobURL  string
}

type costView struct {
	Amount          string
	GenerationCount int
}

type usageModelView struct {
	Breakdown store.UsageModelBreakdown
	URL       string
}

type usageProjectView struct {
	Breakdown store.UsageProjectBreakdown
	Project   string
	URL       string
}

type usageKindView struct {
	Breakdown store.UsageKindBreakdown
	URL       string
}

type overviewPage struct {
	Page           string
	Title          string
	Config         configView
	Metrics        []metricView
	ReviewStates   []stateView
	FeedbackStates []stateView
	OldestReview   *time.Time
	OldestFeedback *time.Time
	RecentReviews  []reviewView
	RecentFeedback []feedbackView
}

type reviewsPage struct {
	Page       string
	Title      string
	Records    []reviewView
	State      string
	NextURL    string
	StateLinks []filterLink
}

type reviewDetailPage struct {
	Page            string
	Title           string
	Detail          store.ReviewRecordDetail
	Project         string
	MergeRequestURL string
	PublicationURL  string
	Generations     []generationView
}

type feedbackPage struct {
	Page       string
	Title      string
	Records    []feedbackView
	State      string
	NextURL    string
	StateLinks []filterLink
}

type feedbackDetailPage struct {
	Page            string
	Title           string
	Detail          store.FeedbackRecordDetail
	Project         string
	MergeRequestURL string
	Generations     []generationView
}

type usagePage struct {
	Page            string
	Title           string
	Window          string
	Filters         usageFilters
	Report          store.UsageReport
	WindowLinks     []filterLink
	KindLinks       []filterLink
	ClearFiltersURL string
	Costs           []costView
	Models          []usageModelView
	Projects        []usageProjectView
	Kinds           []usageKindView
	Generations     []generationView
	NextURL         string
}

type generationDetailPage struct {
	Page       string
	Title      string
	Generation generationView
}

type memoryPage struct {
	Page        string
	Title       string
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

type usageFilters struct {
	Window                   string
	RequestKind              string
	ConfiguredModel          string
	ResolvedModel            string
	ResolvedModelUnavailable bool
	ProjectID                int64
	BeforeID                 int64
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
		"formatTime":                formatTime,
		"formatOptionalTime":        formatOptionalTime,
		"formatCompactTime":         formatCompactTime,
		"formatCompactOptionalTime": formatCompactOptionalTime,
		"timeAttribute":             timeAttribute,
		"optionalTimeAttribute":     optionalTimeAttribute,
		"sourceLabel":               sourceLabel,
		"patchStatusLabel":          patchStatusLabel,
		"shortSHA":                  shortSHA,
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
		config: panelConfig(config), gitlabURL: gitLabURL, now: time.Now,
	}, nil
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.overview)
	mux.HandleFunc("GET /reviews", h.reviews)
	mux.HandleFunc("GET /reviews/{jobID}", h.reviewDetail)
	mux.HandleFunc("GET /feedback", h.feedback)
	mux.HandleFunc("GET /feedback/{jobID}", h.feedbackDetail)
	mux.HandleFunc("GET /memory", h.memory)
	mux.HandleFunc("GET /usage", h.usage)
	mux.HandleFunc("GET /usage/{generationID}", h.generationDetail)
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
		Page: "overview", Title: "Overview · Wormtamer", Config: h.config,
		Metrics:        overviewMetrics(dashboard),
		ReviewStates:   reviewStateViews(dashboard.ReviewCounts),
		FeedbackStates: feedbackStateViews(dashboard.FeedbackCounts),
		OldestReview:   dashboard.OldestQueuedReview,
		OldestFeedback: dashboard.OldestQueuedFeedback,
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
		Page: "reviews", Title: "Reviews · Wormtamer",
		Records: h.reviewViews(pageRecords.Records), State: state,
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
		Page: "reviews", Title: fmt.Sprintf("Review #%d · Wormtamer", detail.ID), Detail: detail,
		Project:         projectLabel(detail.ProjectPath, detail.ProjectID),
		MergeRequestURL: mergeRequestURL,
		Generations:     h.generationViews(detail.Generations),
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
		Page: "feedback", Title: "Feedback · Wormtamer",
		Records: h.feedbackViews(pageRecords.Records), State: state,
		StateLinks: stateLinks("/feedback", state, feedbackStates()),
	}
	if pageRecords.NextBefore > 0 {
		page.NextURL = listURL("/feedback", state, pageRecords.NextBefore, "state")
	}
	h.render(w, "feedback", page)
}

func (h *Handler) feedbackDetail(w http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		h.badRequest(w)
		return
	}
	jobID, err := strconv.ParseInt(request.PathValue("jobID"), 10, 64)
	if err != nil || jobID <= 0 {
		h.badRequest(w)
		return
	}
	detail, err := h.store.GetFeedbackRecord(request.Context(), jobID)
	if errors.Is(err, store.ErrFeedbackRecordNotFound) {
		h.writeError(w, http.StatusNotFound)
		return
	}
	if err != nil {
		h.internalError(w, "feedback_detail")
		return
	}
	page := feedbackDetailPage{
		Page: "feedback", Title: fmt.Sprintf("Feedback #%d · Wormtamer", detail.ID), Detail: detail,
		Project:         projectLabel(detail.ProjectPath, detail.ProjectID),
		MergeRequestURL: h.mergeRequestURL(detail.ProjectPath, detail.MergeRequestIID),
		Generations:     h.generationViews(detail.Generations),
	}
	h.render(w, "feedback-detail", page)
}

func (h *Handler) usage(w http.ResponseWriter, request *http.Request) {
	filters, ok := parseUsageQuery(request.URL.Query())
	if !ok {
		h.badRequest(w)
		return
	}
	now := h.now().UTC()
	duration := 24 * time.Hour
	switch filters.Window {
	case "week":
		duration = 7 * 24 * time.Hour
	case "month":
		duration = 30 * 24 * time.Hour
	}
	report, err := h.store.ReadUsageReport(request.Context(), store.UsageQuery{
		Since: now.Add(-duration), Until: now, RequestKind: filters.RequestKind,
		ConfiguredModel: filters.ConfiguredModel, ResolvedModel: filters.ResolvedModel,
		ResolvedModelUnavailable: filters.ResolvedModelUnavailable,
		ProjectID:                filters.ProjectID, BeforeID: filters.BeforeID, Limit: pageSize,
	})
	if err != nil {
		h.internalError(w, "usage")
		return
	}
	page := usagePage{
		Page: "usage", Title: "Model usage · Wormtamer", Window: filters.Window, Filters: filters, Report: report,
		WindowLinks: []filterLink{
			{Label: "24 hours", URL: usageFilterURL(withUsageWindow(filters, "day"), 0), Current: filters.Window == "day"},
			{Label: "7 days", URL: usageFilterURL(withUsageWindow(filters, "week"), 0), Current: filters.Window == "week"},
			{Label: "30 days", URL: usageFilterURL(withUsageWindow(filters, "month"), 0), Current: filters.Window == "month"},
		},
		KindLinks: []filterLink{
			{Label: "All requests", URL: usageFilterURL(withUsageKind(filters, ""), 0), Current: filters.RequestKind == ""},
			{Label: "Reviews", URL: usageFilterURL(withUsageKind(filters, "review"), 0), Current: filters.RequestKind == "review"},
			{Label: "Feedback", URL: usageFilterURL(withUsageKind(filters, "feedback"), 0), Current: filters.RequestKind == "feedback"},
		},
		ClearFiltersURL: usageFilterURL(usageFilters{Window: filters.Window}, 0),
		Generations:     h.generationViews(report.Generations.Records),
	}
	for _, cost := range report.Costs {
		page.Costs = append(page.Costs, costView{
			Amount: formatPicoCost(cost.EstimatedCostPicos), GenerationCount: cost.GenerationCount,
		})
	}
	for _, model := range report.Models {
		filtered := filters
		filtered.ConfiguredModel, filtered.ResolvedModel, filtered.BeforeID = model.ConfiguredModel, model.ResolvedModel, 0
		filtered.ResolvedModelUnavailable = model.ResolvedModel == ""
		page.Models = append(page.Models, usageModelView{Breakdown: model, URL: usageFilterURL(filtered, 0)})
	}
	for _, project := range report.Projects {
		filtered := filters
		filtered.ProjectID, filtered.BeforeID = project.ProjectID, 0
		page.Projects = append(page.Projects, usageProjectView{
			Breakdown: project, Project: projectLabel(project.ProjectPath, project.ProjectID), URL: usageFilterURL(filtered, 0),
		})
	}
	for _, kind := range report.Kinds {
		filtered := filters
		filtered.RequestKind, filtered.BeforeID = kind.RequestKind, 0
		page.Kinds = append(page.Kinds, usageKindView{Breakdown: kind, URL: usageFilterURL(filtered, 0)})
	}
	if report.Generations.NextBefore > 0 {
		page.NextURL = usageFilterURL(filters, report.Generations.NextBefore)
	}
	h.render(w, "usage", page)
}

func (h *Handler) generationDetail(w http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		h.badRequest(w)
		return
	}
	generationID, err := strconv.ParseInt(request.PathValue("generationID"), 10, 64)
	if err != nil || generationID <= 0 {
		h.badRequest(w)
		return
	}
	record, err := h.store.GetGenerationRecord(request.Context(), generationID)
	if errors.Is(err, store.ErrGenerationRecordNotFound) {
		h.writeError(w, http.StatusNotFound)
		return
	}
	if err != nil {
		h.internalError(w, "generation_detail")
		return
	}
	page := generationDetailPage{
		Page: "usage", Title: fmt.Sprintf("Generation #%d · Wormtamer", record.ID),
		Generation: h.generationViews([]store.GenerationRecord{record})[0],
	}
	h.render(w, "generation-detail", page)
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
		Page: "memory", Title: "Runtime memory · Wormtamer",
		Records: h.memoryViews(pageRecords.Records), Active: activeText,
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
	return orderedStateViews(counts, reviewStates(), "/reviews")
}

func feedbackStateViews(counts []store.StateCount) []stateView {
	return orderedStateViews(counts, feedbackStates(), "/feedback")
}

func orderedStateViews(counts []store.StateCount, states []string, path string) []stateView {
	byState := make(map[string]int, len(counts))
	for _, count := range counts {
		byState[count.State] = count.Count
	}
	views := make([]stateView, len(states))
	for index, state := range states {
		views[index] = stateView{
			State: state, Count: byState[state],
			URL: path + "?state=" + url.QueryEscape(state),
		}
	}
	return views
}

func overviewMetrics(dashboard store.Dashboard) []metricView {
	reviews := stateCounts(dashboard.ReviewCounts)
	feedback := stateCounts(dashboard.FeedbackCounts)
	return []metricView{
		{Label: "Waiting", Tone: "waiting", Count: reviews[store.JobQueued] + feedback[store.FeedbackQueued]},
		{Label: "In progress", Tone: "active", Count: reviews[store.JobRunning] + reviews[store.JobPublishing] + feedback[store.FeedbackRunning]},
		{Label: "Failed", Tone: "failed", Count: reviews[store.JobFailed] + feedback[store.FeedbackFailed]},
		{Label: "Active lessons", Tone: "memory", Count: dashboard.ActiveMemoryCount},
	}
}

func stateCounts(counts []store.StateCount) map[string]int {
	values := make(map[string]int, len(counts))
	for _, count := range counts {
		values[count.State] = count.Count
	}
	return values
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

func parseUsageQuery(values url.Values) (usageFilters, bool) {
	if !onlyQueryKeys(values, "window", "kind", "configured_model", "resolved_model", "resolved_model_unavailable", "project_id", "before") {
		return usageFilters{}, false
	}
	for _, key := range []string{"window", "kind", "configured_model", "resolved_model", "resolved_model_unavailable", "project_id", "before"} {
		if len(values[key]) > 1 {
			return usageFilters{}, false
		}
	}
	filters := usageFilters{
		Window: values.Get("window"), RequestKind: values.Get("kind"),
		ConfiguredModel: values.Get("configured_model"), ResolvedModel: values.Get("resolved_model"),
	}
	if filters.Window == "" {
		filters.Window = "day"
	}
	if filters.Window != "day" && filters.Window != "week" && filters.Window != "month" {
		return usageFilters{}, false
	}
	if filters.RequestKind != "" && filters.RequestKind != "review" && filters.RequestKind != "feedback" {
		return usageFilters{}, false
	}
	if !validUsageFilterText(filters.ConfiguredModel) || !validUsageFilterText(filters.ResolvedModel) {
		return usageFilters{}, false
	}
	if value := values.Get("resolved_model_unavailable"); value != "" {
		if value != "true" || filters.ResolvedModel != "" {
			return usageFilters{}, false
		}
		filters.ResolvedModelUnavailable = true
	}
	if value := values.Get("project_id"); value != "" {
		projectID, err := strconv.ParseInt(value, 10, 64)
		if err != nil || projectID <= 0 {
			return usageFilters{}, false
		}
		filters.ProjectID = projectID
	}
	before, ok := parseBefore(values.Get("before"))
	if !ok {
		return usageFilters{}, false
	}
	filters.BeforeID = before
	return filters, true
}

func validUsageFilterText(value string) bool {
	if len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func withUsageWindow(filters usageFilters, window string) usageFilters {
	filters.Window, filters.BeforeID = window, 0
	return filters
}

func withUsageKind(filters usageFilters, kind string) usageFilters {
	filters.RequestKind, filters.BeforeID = kind, 0
	return filters
}

func usageFilterURL(filters usageFilters, before int64) string {
	values := url.Values{}
	if filters.Window != "" && filters.Window != "day" {
		values.Set("window", filters.Window)
	}
	if filters.RequestKind != "" {
		values.Set("kind", filters.RequestKind)
	}
	if filters.ConfiguredModel != "" {
		values.Set("configured_model", filters.ConfiguredModel)
	}
	if filters.ResolvedModelUnavailable {
		values.Set("resolved_model_unavailable", "true")
	} else if filters.ResolvedModel != "" {
		values.Set("resolved_model", filters.ResolvedModel)
	}
	if filters.ProjectID > 0 {
		values.Set("project_id", strconv.FormatInt(filters.ProjectID, 10))
	}
	if before > 0 {
		values.Set("before", strconv.FormatInt(before, 10))
	}
	if len(values) == 0 {
		return "/usage"
	}
	return "/usage?" + values.Encode()
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

func (h *Handler) generationViews(records []store.GenerationRecord) []generationView {
	views := make([]generationView, len(records))
	for index, record := range records {
		jobURL := ""
		if record.ReviewJobID > 0 {
			jobURL = "/reviews/" + strconv.FormatInt(record.ReviewJobID, 10)
		} else if record.FeedbackJobID > 0 {
			jobURL = "/feedback/" + strconv.FormatInt(record.FeedbackJobID, 10)
		}
		views[index] = generationView{
			Record: record, Project: projectLabel(record.ProjectPath, record.ProjectID), JobURL: jobURL,
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

func formatCompactTime(timestamp time.Time) string {
	if timestamp.IsZero() {
		return "—"
	}
	return timestamp.UTC().Format("2006 Jan 02 · 15:04 UTC")
}

func formatCompactOptionalTime(timestamp *time.Time) string {
	if timestamp == nil {
		return "—"
	}
	return formatCompactTime(*timestamp)
}

func timeAttribute(timestamp time.Time) string {
	if timestamp.IsZero() {
		return ""
	}
	return timestamp.UTC().Format(time.RFC3339Nano)
}

func optionalTimeAttribute(timestamp *time.Time) string {
	if timestamp == nil {
		return ""
	}
	return timeAttribute(*timestamp)
}

func sourceLabel(source string) string {
	if source == "reconciled" {
		return "Reconciler"
	}
	if source == "webhook" {
		return "Webhook"
	}
	return source
}

func patchStatusLabel(status string) string {
	switch status {
	case store.PatchIDAvailable:
		return "Patch ID available"
	case store.PatchIDPending:
		return "Patch ID pending"
	case store.PatchIDUnavailable:
		return "Patch ID unavailable"
	default:
		return "Patch ID not fetched"
	}
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

func formatPicoCost(value int64) string {
	const scale = int64(1_000_000_000_000)
	whole := value / scale
	fraction := value % scale
	if fraction == 0 {
		return strconv.FormatInt(whole, 10)
	}
	fractionText := fmt.Sprintf("%012d", fraction)
	fractionText = strings.TrimRight(fractionText, "0")
	return strconv.FormatInt(whole, 10) + "." + fractionText
}
