// Command ingest archives GitHub Audit Events for the navikt organisation to
// the BigQuery Archive, running an Ingest Run on a fixed interval.
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

	"github.com/navikt/galning/internal/config"
	gh "github.com/navikt/galning/internal/github"
	"github.com/navikt/galning/internal/ingest"
	"github.com/navikt/galning/internal/metrics"
	"github.com/navikt/galning/internal/oauth"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := config.FromEnv()
	if err := cfg.ValidateIngest(); err != nil {
		slog.Error("missing configuration", "error", err)
		os.Exit(1)
	}

	metrics.RegisterIngest()

	// HTTP routes.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /internal/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
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
		defer func() {
			if err := ts.Close(); err != nil {
				slog.Warn("close token store", "error", err)
			}
		}()
		store = ts
	}

	// GitHub client — stateless; the token is passed per call from the store.
	ghClient := gh.NewAuditClient()

	// OAuth handler — serves /api/authorize and /api/callback.
	oauthHandler := oauth.NewHandler(cfg.GithubClientID, cfg.GithubClientSecret, cfg.GithubCallbackURL, store)
	mux.HandleFunc("GET /ingest/callback", oauthHandler.Callback)
	mux.HandleFunc("GET /ingest/authorize", oauthHandler.Authorize)

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
			if err := ingest.DryRun(ctx, cfg, ghClient, store); err != nil {
				slog.Error("dry-run listing failed", "error", err)
			}
		}()
		runServer(ctx, server)
		return
	}

	// BigQuery Archive.
	arc, err := ingest.NewBigQueryClient(ctx, cfg.BigQueryProject, cfg.BigQueryDataset, cfg.BigQueryTable)
	if err != nil {
		slog.Error("connect to archive", "error", err)
		os.Exit(1)
	}
	defer arc.Close()

	go ingest.StartLoop(ctx, cfg, arc, ghClient, store)

	runServer(ctx, server)
}

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
