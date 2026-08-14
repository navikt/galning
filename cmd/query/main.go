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

	"cloud.google.com/go/bigquery"
	"github.com/navikt/galning/internal/config"
	gh "github.com/navikt/galning/internal/github"
	"github.com/navikt/galning/internal/logging"
	"github.com/navikt/galning/internal/oauth"
	"github.com/navikt/galning/internal/query"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := config.FromEnv()
	logging.Setup(cfg.LogFormat)

	if err := cfg.ValidateQuery(); err != nil {
		slog.Error("missing configuration", "error", err)
		os.Exit(1)
	}

	bq, err := bigquery.NewClient(ctx, cfg.BigQueryProject)
	if err != nil {
		slog.Error("create bigquery client", "error", err)
		os.Exit(1)
	}
	defer bq.Close()

	querier := query.NewBigQueryQuerier(bq, cfg.BigQueryProject, cfg.BigQueryDataset, cfg.BigQueryTable)
	queryHandler := oauth.NewQueryHandler(
		cfg.GithubClientID, cfg.GithubClientSecret, cfg.GithubCallbackURL,
		oauth.NewSessionStore(),
		gh.NewUserClient(cfg.GithubOrg),
		querier,
	)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /query", queryHandler.Form)
	mux.HandleFunc("GET /query/teams", queryHandler.Teams)
	mux.HandleFunc("GET /query/repos", queryHandler.Repos)
	mux.HandleFunc("GET /query/login", queryHandler.Login)
	mux.HandleFunc("GET /query/callback", queryHandler.Callback)
	mux.HandleFunc("POST /query/run", queryHandler.Run)
	mux.HandleFunc("POST /query/logout", queryHandler.Logout)
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

	slog.Info("query service starting", "callback", cfg.GithubCallbackURL)
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
