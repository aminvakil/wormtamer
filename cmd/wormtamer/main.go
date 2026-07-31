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
	"syscall"
	"time"

	"github.com/aminvakil/wormtamer/internal/config"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/reconcile"
	"github.com/aminvakil/wormtamer/internal/repository"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
	"github.com/aminvakil/wormtamer/internal/webhook"
	"github.com/aminvakil/wormtamer/internal/worker"
)

const shutdownTimeout = 10 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], logger); err != nil {
		logger.Error("wormtamer stopped", "error", bounded(err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, logger *slog.Logger) error {
	configPath, err := parseConfigPath(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.ConfigFileBroadlyRead {
		logger.Warn("configuration file is readable by group or other users")
	}

	storage, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer storage.Close()
	workspaceManager, err := repository.NewManager(cfg.DatabasePath + ".workspaces")
	if err != nil {
		return err
	}
	defer workspaceManager.Close()

	gitLabClient, err := gitlab.New(cfg.GitLab.BaseURL, cfg.GitLab.PersonalAccessToken, cfg.AuthorizedRepositories, nil)
	if err != nil {
		return err
	}
	geminiReviewer, err := review.NewGeminiReviewer(ctx, cfg.Gemini.APIKey, cfg.Gemini.Model, []string{
		cfg.GitLab.WebhookSecret, cfg.GitLab.PersonalAccessToken, cfg.Gemini.APIKey,
	})
	if err != nil {
		return err
	}
	reviewWorker, err := worker.New(storage, gitLabClient, workspaceManager, geminiReviewer, logger, []string{
		cfg.GitLab.WebhookSecret, cfg.GitLab.PersonalAccessToken, cfg.Gemini.APIKey,
	})
	if err != nil {
		return err
	}
	reconciler := reconcile.New(storage, gitLabClient, cfg.GitLab.BaseURL, cfg.AuthorizedRepositories, logger)

	handler := webhook.New(webhook.Config{
		GitLabInstance:         cfg.GitLab.BaseURL,
		WebhookSecret:          cfg.GitLab.WebhookSecret,
		AuthorizedRepositories: cfg.AuthorizedRepositories,
	}, storage, logger)
	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          log.New(httpErrorWriter{logger: logger}, "", 0),
	}

	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for HTTP traffic: %w", err)
	}
	logger.Info("HTTP server started", "listen_address", bounded(listener.Addr().String()))

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

	var processError error
	serveFinished := false
	workerFinished := false
	reconcilerFinished := false
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
	case <-ctx.Done():
	}

	logger.Info("HTTP server shutting down")
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
	logger.Info("HTTP server stopped")
	return processError
}

func parseConfigPath(args []string) (string, error) {
	flags := flag.NewFlagSet("wormtamer", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to JSON configuration")
	if err := flags.Parse(args); err != nil {
		return "", fmt.Errorf("parse command line: %w", err)
	}
	if flags.NArg() != 0 {
		return "", errors.New("unexpected positional arguments")
	}
	if *configPath == "" {
		return "", errors.New("-config is required")
	}
	return *configPath, nil
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
