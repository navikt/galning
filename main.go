package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/navikt/galning/internal/archive"
	"github.com/navikt/galning/internal/config"
	gh "github.com/navikt/galning/internal/github"
	"github.com/navikt/galning/internal/ingest"
	"github.com/navikt/galning/internal/oauth"
)

const ingestInterval = 5 * time.Minute

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("missing configuration", "error", err)
		os.Exit(1)
	}

	// In dry-run mode, tokens and cursor are held in memory only.
	var store oauth.Store
	if cfg.DryRun {
		store = oauth.NewInMemoryStore()
	} else {
		ts, err := oauth.NewTokenStore(ctx, cfg.GithubTokenSecret)
		if err != nil {
			slog.Error("create token store", "error", err)
			os.Exit(1)
		}
		store = ts
	}

	// GitHub client — uses user access token from the store.
	ghClient := gh.NewClient(store)

	// OAuth handler — serves /api/authorize and /api/callback.
	oauthHandler := oauth.NewHandler(cfg.GithubClientID, cfg.GithubClientSecret, cfg.GithubCallbackURL, store)

	// HTTP routes.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/callback", oauthHandler.Callback)
	mux.HandleFunc("GET /internal/api/authorize", oauthHandler.Authorize)
	mux.HandleFunc("GET /internal/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	if cfg.DryRun {
		slog.Info("dry-run mode — BigQuery skipped; serving OAuth endpoints and listing recent audit events")
		go func() {
			for {
				pair, err := store.Load(ctx)
				if err != nil {
					slog.Warn("dry-run: failed to check token", "error", err)
					return
				}
				if pair != nil {
					break
				}
				slog.Info("dry-run: no token yet — complete the OAuth flow first", "url", "http://localhost:8080/internal/api/authorize")
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
			if err := ingest.DryRun(ctx, cfg, ghClient); err != nil {
				slog.Error("dry-run listing failed", "error", err)
			}
		}()
		runServer(ctx, server)
		return
	}

	// BigQuery Archive.
	arc, err := archive.New(ctx, cfg.BigQueryProject, cfg.BigQueryDataset, cfg.BigQueryTable)
	if err != nil {
		slog.Error("connect to archive", "error", err)
		os.Exit(1)
	}
	defer arc.Close()

	// Start the ingest loop in the background.
	go ingest.StartLoop(ctx, ingestInterval, cfg, arc, ghClient, store)

	runServer(ctx, server)
}

// runServer starts the HTTP server and blocks until ctx is cancelled,
// then performs a graceful shutdown with a 30-second deadline.
func runServer(ctx context.Context, server *http.Server) {
	go func() {
		slog.Info("http server starting")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("http server shutdown error", "error", err)
	}
	slog.Info("shutdown complete")
}
