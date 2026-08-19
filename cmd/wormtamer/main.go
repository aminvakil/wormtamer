package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/aminvakil/wormtamer/internal/config"
	"github.com/aminvakil/wormtamer/internal/diagnostics"
	"github.com/aminvakil/wormtamer/internal/feedback"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/memory"
	"github.com/aminvakil/wormtamer/internal/panel"
	"github.com/aminvakil/wormtamer/internal/publicsource"
	"github.com/aminvakil/wormtamer/internal/reconcile"
	"github.com/aminvakil/wormtamer/internal/repository"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
	"github.com/aminvakil/wormtamer/internal/usage"
	"github.com/aminvakil/wormtamer/internal/webhook"
	"github.com/aminvakil/wormtamer/internal/worker"
)

const shutdownTimeout = 10 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], logger); err != nil {
		logger.Error("wormtamer stopped", "error", bounded(err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, logger *slog.Logger) error {
	return runWithOutput(ctx, args, logger, os.Stdout)
}

func runWithOutput(ctx context.Context, args []string, logger *slog.Logger, output io.Writer) error {
	invocation, err := parseInvocation(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(invocation.configPath)
	if err != nil {
		return err
	}
	logger = slog.New(logLevelHandler{Handler: logger.Handler(), level: configuredLogLevel(cfg.LogLevel)})
	forbidden := []string{cfg.GitLab.WebhookSecret, cfg.GitLab.PersonalAccessToken, cfg.Gemini.APIKey}
	diagnosticRecorder := diagnostics.New(cfg.LogLevel == "debug", forbidden)
	logger = slog.New(diagnostics.NewTeeHandler(logger.Handler(), diagnosticRecorder))
	serviceLogger := logger.With("component", "service")
	if cfg.LogLevel == "debug" {
		serviceLogger.Warn("debug logging enabled; logs include private model prompts, responses, and tool content")
	}
	if cfg.ConfigFileBroadlyRead {
		serviceLogger.Warn("configuration file is readable by group or other users")
	}

	storage, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer storage.Close()
	if invocation.jobs != nil {
		return executeJobsCommand(ctx, storage, *invocation.jobs, output)
	}
	if err := storage.MarkStartedModelGenerationsUnknown(ctx); err != nil {
		return err
	}
	var modelPricing *usage.Pricing
	if cfg.Gemini.BaseURL == "" {
		pricingCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		modelPricing, err = usage.FetchGeminiPricing(pricingCtx, cfg.Gemini.Model)
		cancel()
		if err != nil {
			serviceLogger.Warn("Gemini pricing unavailable; cost estimates disabled", "error", bounded(err.Error()))
			modelPricing = nil
		}
	}
	usageRecorder, err := usage.NewRecorder(storage, modelPricing, forbidden)
	if err != nil {
		return err
	}
	generationRecorder := diagnostics.ObserveGenerations(usageRecorder, diagnosticRecorder)

	webPanel, err := panel.New(storage, panel.Config{
		GitLabBaseURL:                  cfg.GitLab.BaseURL,
		GeminiEndpoint:                 cfg.Gemini.BaseURL,
		GeminiModel:                    cfg.Gemini.Model,
		GeminiThinkingLevel:            cfg.Gemini.ThinkingLevel,
		LogLevel:                       cfg.LogLevel,
		AuthorizedRepositories:         cfg.AuthorizedRepositories,
		ShareAllAuthorizedRepositories: cfg.ShareAllAuthorizedRepositories,
		RepositorySharing:              cfg.RepositorySharing,
		AllowedPublicDomains:           cfg.PublicSources.AllowedDomains,
		PublicGitHubRepositories:       cfg.PublicSources.GitHubRepositories,
	}, logger.With("component", "panel"), diagnosticRecorder)
	if err != nil {
		return err
	}

	workspaceManager, err := repository.NewManager(cfg.DatabasePath + ".workspaces")
	if err != nil {
		return err
	}
	defer workspaceManager.Close()

	gitLabClient, err := gitlab.New(cfg.GitLab.BaseURL, cfg.GitLab.PersonalAccessToken, cfg.AuthorizedRepositories, cfg.RepositorySharing, nil)
	if err != nil {
		return err
	}
	geminiReviewer, err := review.NewGeminiReviewer(ctx, cfg.Gemini.APIKey, cfg.Gemini.BaseURL, cfg.Gemini.Model, cfg.Gemini.ThinkingLevel, forbidden,
		logger.With("component", "review", "job_kind", "review"), generationRecorder, diagnosticRecorder)
	if err != nil {
		return err
	}
	publicClient, err := publicsource.New(cfg.PublicSources.AllowedDomains, cfg.PublicSources.GitHubRepositories, forbidden)
	if err != nil {
		return err
	}
	reviewWorker, err := worker.New(storage, gitLabClient, publicClient,
		cfg.PublicSources.AllowedDomains, cfg.PublicSources.GitHubRepositories,
		workspaceManager, geminiReviewer, logger.With("component", "review_worker", "job_kind", "review"), forbidden)
	if err != nil {
		return err
	}
	memoryEvaluator, err := memory.NewEvaluator(ctx, cfg.Gemini.APIKey, cfg.Gemini.BaseURL, cfg.Gemini.Model, forbidden,
		logger.With("component", "feedback", "job_kind", "feedback"), generationRecorder, diagnosticRecorder)
	if err != nil {
		return err
	}
	feedbackWorker, err := feedback.New(storage, gitLabClient, memoryEvaluator, logger.With("component", "feedback_worker", "job_kind", "feedback"))
	if err != nil {
		return err
	}
	reconciler := reconcile.New(storage, gitLabClient, cfg.GitLab.BaseURL, cfg.AuthorizedRepositories, logger.With("component", "reconciler"))

	ingressHandler := webhook.New(webhook.Config{
		GitLabInstance:         cfg.GitLab.BaseURL,
		WebhookSecret:          cfg.GitLab.WebhookSecret,
		AuthorizedRepositories: cfg.AuthorizedRepositories,
	}, storage, logger.With("component", "webhook"))
	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           serviceRoutes(ingressHandler.Routes(), webPanel.Routes()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          log.New(httpErrorWriter{logger: serviceLogger}, "", 0),
	}

	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for HTTP traffic: %w", err)
	}
	serviceLogger.Info("HTTP server started", "listen_address", bounded(listener.Addr().String()))

	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()
	componentCtx, stopComponents := context.WithCancel(context.Background())
	defer stopComponents()
	workerErrors := make(chan error, 1)
	go func() {
		workerErrors <- reviewWorker.Run(componentCtx)
	}()
	reconcilerErrors := make(chan error, 1)
	go func() {
		reconcilerErrors <- reconciler.Run(componentCtx)
	}()
	feedbackErrors := make(chan error, 1)
	go func() {
		feedbackErrors <- feedbackWorker.Run(componentCtx)
	}()

	var processError error
	serveFinished := false
	workerFinished := false
	reconcilerFinished := false
	feedbackFinished := false
	select {
	case err := <-serveErrors:
		serveFinished = true
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			processError = fmt.Errorf("serve HTTP traffic: %w", err)
		}
	case err := <-workerErrors:
		workerFinished = true
		if err != nil {
			processError = fmt.Errorf("run review worker: %w", err)
		} else {
			processError = errors.New("review worker stopped unexpectedly")
		}
	case err := <-reconcilerErrors:
		reconcilerFinished = true
		if err != nil {
			processError = fmt.Errorf("run reconciler: %w", err)
		} else {
			processError = errors.New("reconciler stopped unexpectedly")
		}
	case err := <-feedbackErrors:
		feedbackFinished = true
		if err != nil {
			processError = fmt.Errorf("run feedback worker: %w", err)
		} else {
			processError = errors.New("feedback worker stopped unexpectedly")
		}
	case <-ctx.Done():
	}

	serviceLogger.Info("HTTP server shutting down")
	stopComponents()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		if processError == nil {
			processError = fmt.Errorf("graceful HTTP shutdown: %w", err)
		}
	}
	if !serveFinished {
		if err := <-serveErrors; err != nil && !errors.Is(err, http.ErrServerClosed) && processError == nil {
			processError = fmt.Errorf("serve HTTP traffic during shutdown: %w", err)
		}
	}
	if !workerFinished {
		if err := <-workerErrors; err != nil && processError == nil {
			processError = fmt.Errorf("stop review worker: %w", err)
		}
	}
	if !reconcilerFinished {
		if err := <-reconcilerErrors; err != nil && processError == nil {
			processError = fmt.Errorf("stop reconciler: %w", err)
		}
	}
	if !feedbackFinished {
		if err := <-feedbackErrors; err != nil && processError == nil {
			processError = fmt.Errorf("stop feedback worker: %w", err)
		}
	}
	serviceLogger.Info("HTTP server stopped")
	return processError
}

func serviceRoutes(ingress, panel http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthcheck", ingress)
	mux.Handle("/webhooks/gitlab", ingress)
	mux.Handle("/", panel)
	return mux
}

type invocation struct {
	configPath string
	jobs       *jobsCommand
}

func parseInvocation(args []string) (invocation, error) {
	flags := flag.NewFlagSet("wormtamer", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to JSON configuration")
	if err := flags.Parse(args); err != nil {
		return invocation{}, fmt.Errorf("parse command line: %w", err)
	}
	if *configPath == "" {
		return invocation{}, errors.New("-config is required")
	}

	result := invocation{configPath: *configPath}
	arguments := flags.Args()
	if len(arguments) == 0 {
		return result, nil
	}
	if arguments[0] != "jobs" {
		return invocation{}, errors.New("unexpected positional arguments")
	}
	if len(arguments) == 2 && arguments[1] == jobsActionListFailed {
		result.jobs = &jobsCommand{action: jobsActionListFailed}
		return result, nil
	}
	if len(arguments) == 4 && arguments[1] == jobsActionRetry &&
		(arguments[2] == store.FailedJobKindReview || arguments[2] == store.FailedJobKindFeedback) {
		jobID, err := strconv.ParseInt(arguments[3], 10, 64)
		if err != nil || jobID <= 0 {
			return invocation{}, errors.New("job ID must be a positive integer")
		}
		result.jobs = &jobsCommand{action: jobsActionRetry, kind: arguments[2], jobID: jobID}
		return result, nil
	}
	return invocation{}, errors.New("invalid jobs command")
}

func configuredLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type logLevelHandler struct {
	slog.Handler
	level slog.Level
}

func (h logLevelHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h logLevelHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	return logLevelHandler{Handler: h.Handler.WithAttrs(attributes), level: h.level}
}

func (h logLevelHandler) WithGroup(name string) slog.Handler {
	return logLevelHandler{Handler: h.Handler.WithGroup(name), level: h.level}
}

func bounded(value string) string {
	const limit = 512
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

type httpErrorWriter struct {
	logger *slog.Logger
}

func (w httpErrorWriter) Write(contents []byte) (int, error) {
	w.logger.Error("HTTP server error", "detail", bounded(string(contents)))
	return len(contents), nil
}
