// Package ingest provides the Ingest Run logic and the 5-minute loop.
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/navikt/galning/internal/archive"
	"github.com/navikt/galning/internal/config"
	"github.com/navikt/galning/internal/github"
)

const dryRunLimit = 50

// Run performs a single Ingest Run: derives the Cursor from the Archive,
// fetches new Audit Events from GitHub, and inserts them into the Archive.
func Run(ctx context.Context, cfg config.Config, arc *archive.Archive, ghClient *github.Client) error {
	cursor, err := arc.LatestCursor(ctx, cfg.GithubOrg)
	if err != nil {
		return fmt.Errorf("derive cursor: %w", err)
	}

	events, err := ghClient.AuditEvents(ctx, cfg.GithubOrg, cursor)
	if err != nil {
		return fmt.Errorf("fetch audit events: %w", err)
	}

	if len(events) == 0 {
		slog.Info("no new audit events — archive is up to date")
		return nil
	}

	slog.Info("fetched audit events", "count", len(events))

	if err := arc.Insert(ctx, events); err != nil {
		return fmt.Errorf("insert events: %w", err)
	}

	slog.Info("ingest run complete", "inserted", len(events))
	return nil
}

// DryRun fetches the most recent Audit Events from GitHub and logs them to
// stdout. No events are written to the Archive. Used for local testing.
func DryRun(ctx context.Context, cfg config.Config, ghClient *github.Client) error {
	slog.Info("dry-run: fetching recent audit events", "org", cfg.GithubOrg, "limit", dryRunLimit)

	events, err := ghClient.RecentAuditEvents(ctx, cfg.GithubOrg, dryRunLimit)
	if err != nil {
		return fmt.Errorf("fetch recent audit events: %w", err)
	}

	if len(events) == 0 {
		slog.Info("dry-run: no audit events returned")
		return nil
	}

	for _, e := range events {
		slog.Info("audit event",
			"document_id", e.DocumentID,
			"action", e.Action,
			"actor", e.Actor,
			"created_at", time.UnixMilli(e.CreatedAt).UTC().Format(time.RFC3339),
		)
	}

	slog.Info("dry-run complete", "count", len(events))
	return nil
}

func StartLoop(ctx context.Context, interval time.Duration, cfg config.Config, arc *archive.Archive, ghClient *github.Client) {
	run := func() {
		slog.Info("ingest run starting", "org", cfg.GithubOrg)
		if err := Run(ctx, cfg, arc, ghClient); err != nil {
			slog.Error("ingest run failed", "error", err)
		}
	}

	run() // run immediately on startup

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("ingest loop stopped")
			return
		case <-ticker.C:
			run()
		}
	}
}
