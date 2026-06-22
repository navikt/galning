package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/navikt/galning/internal/oauth"
)

const (
	apiBase               = "https://api.github.com"
	pageSize              = 100
	rateLimitMinRemaining = 100 // sleep proactively when fewer requests remain
)

// AuditEvent is a single Audit Event returned by the GitHub audit log API.
// Common fields are extracted; the full raw payload is preserved in Raw.
type AuditEvent struct {
	DocumentID    string          `json:"_document_id"`
	Action        string          `json:"action"`
	Actor         string          `json:"actor"`
	ActorIP       string          `json:"actor_ip"`
	CreatedAt     int64           `json:"@timestamp"` // milliseconds since epoch
	Org           string          `json:"org"`
	Repo          string          `json:"repo"`
	User          string          `json:"user"`
	OperationType string          `json:"operation_type"`
	Raw           json.RawMessage `json:"-"`
}

// Client fetches audit events from GitHub using a user access token.
type Client struct {
	store      oauth.Store
	httpClient *http.Client
}

// NewClient constructs a Client backed by the given Store.
func NewClient(store oauth.Store) *Client {
	return &Client{
		store:      store,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// token returns the access token from the store.
// Returns an error if no token has been stored yet (OAuth flow not completed).
func (c *Client) token(ctx context.Context) (string, error) {
	pair, err := c.store.Load(ctx)
	if err != nil {
		return "", fmt.Errorf("load tokens: %w", err)
	}
	if pair == nil {
		return "", fmt.Errorf("not authorised — visit /internal/api/authorize to complete the GitHub OAuth flow")
	}
	return pair.AccessToken, nil
}

// AuditEvents fetches Audit Events for org since afterCursor, calling fn for
// each page of results. Pass an empty string to fetch from the start of
// GitHub's retention window. Events are delivered oldest-first.
// If fn returns an error, fetching stops and that error is returned.
// Rate limit headers are respected: the loop sleeps proactively when fewer
// than rateLimitMinRemaining requests remain, and retries once on 403/429.
func (c *Client) AuditEvents(ctx context.Context, org, afterCursor string, fn func([]AuditEvent) error) error {
	token, err := c.token(ctx)
	if err != nil {
		return err
	}

	nextURL := fmt.Sprintf("%s/orgs/%s/audit-log?per_page=%d&order=asc", apiBase, org, pageSize)
	if afterCursor != "" {
		nextURL += "&after=" + afterCursor
	}

	for nextURL != "" {
		resp, body, err := c.doWithRetry(ctx, token, nextURL)
		if err != nil {
			return err
		}

		var rawEvents []json.RawMessage
		if err := json.Unmarshal(body, &rawEvents); err != nil {
			return fmt.Errorf("unmarshal audit log page: %w", err)
		}
		if len(rawEvents) == 0 {
			break
		}

		page := make([]AuditEvent, 0, len(rawEvents))
		for _, raw := range rawEvents {
			var e AuditEvent
			if err := json.Unmarshal(raw, &e); err != nil {
				return fmt.Errorf("unmarshal audit event: %w", err)
			}
			e.Raw = raw
			page = append(page, e)
		}

		slog.Info("fetched page", "count", len(page))

		if err := fn(page); err != nil {
			return err
		}

		// Proactive throttle: sleep until reset if running low on requests.
		if remaining, reset, ok := parseRateLimitHeaders(resp.Header); ok {
			if remaining < rateLimitMinRemaining {
				if err := sleepUntilReset(ctx, reset, "proactive rate limit pause"); err != nil {
					return err
				}
			}
		}

		nextURL = parseLinkNext(resp.Header.Get("Link"))
	}

	return nil
}

// doWithRetry performs a GET request and retries once on 403/429 rate limit
// responses after sleeping until the reset time indicated in the headers.
func (c *Client) doWithRetry(ctx context.Context, token, url string) (*http.Response, []byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch audit log page: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close() // #nosec G104
		if err != nil {
			return nil, nil, fmt.Errorf("read audit log response: %w", err)
		}

		if resp.StatusCode == http.StatusOK {
			return resp, body, nil
		}

		if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) && attempt == 0 {
			reset := parseResetHeader(resp.Header)
			if err := sleepUntilReset(ctx, reset, fmt.Sprintf("rate limited (status %d), waiting for reset", resp.StatusCode)); err != nil {
				return nil, nil, err
			}
			continue
		}

		return nil, nil, fmt.Errorf("audit log: status %d: %s", resp.StatusCode, body)
	}

	return nil, nil, fmt.Errorf("audit log: still rate limited after retry")
}

// parseRateLimitHeaders extracts x-ratelimit-remaining and x-ratelimit-reset.
func parseRateLimitHeaders(h http.Header) (remaining int, reset time.Time, ok bool) {
	remStr := h.Get("x-ratelimit-remaining")
	resetStr := h.Get("x-ratelimit-reset")
	if remStr == "" || resetStr == "" {
		return 0, time.Time{}, false
	}
	rem, err := strconv.Atoi(remStr)
	if err != nil {
		return 0, time.Time{}, false
	}
	resetEpoch, err := strconv.ParseInt(resetStr, 10, 64)
	if err != nil {
		return 0, time.Time{}, false
	}
	return rem, time.Unix(resetEpoch, 0), true
}

// parseResetHeader returns the reset time from rate limit headers, preferring
// retry-after (seconds) over x-ratelimit-reset (epoch), falling back to 60s.
func parseResetHeader(h http.Header) time.Time {
	if retryAfter := h.Get("retry-after"); retryAfter != "" {
		if secs, err := strconv.Atoi(retryAfter); err == nil {
			return time.Now().Add(time.Duration(secs) * time.Second)
		}
	}
	if _, reset, ok := parseRateLimitHeaders(h); ok {
		return reset
	}
	return time.Now().Add(60 * time.Second) // safe fallback
}

// sleepUntilReset blocks until the reset time or ctx is cancelled.
func sleepUntilReset(ctx context.Context, reset time.Time, reason string) error {
	wait := time.Until(reset)
	if wait <= 0 {
		return nil
	}
	slog.Warn("rate limit: sleeping", "reason", reason, "wait", wait.Round(time.Second))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// RecentAuditEvents fetches the n most recent Audit Events for org, newest-first.
// It fetches a single page only — no pagination.
func (c *Client) RecentAuditEvents(ctx context.Context, org string, n int) ([]AuditEvent, error) {
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}

	u := fmt.Sprintf("%s/orgs/%s/audit-log?per_page=%d&order=desc", apiBase, org, n)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch recent audit events: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close() // #nosec G104
	if err != nil {
		return nil, fmt.Errorf("read recent audit events response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("recent audit events: status %d: %s", resp.StatusCode, body)
	}

	var rawEvents []json.RawMessage
	if err := json.Unmarshal(body, &rawEvents); err != nil {
		return nil, fmt.Errorf("unmarshal recent audit events: %w", err)
	}

	events := make([]AuditEvent, 0, len(rawEvents))
	for _, raw := range rawEvents {
		var e AuditEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("unmarshal audit event: %w", err)
		}
		e.Raw = raw
		events = append(events, e)
	}
	return events, nil
}

// parseLinkNext extracts the URL from a GitHub Link header's rel="next" entry.
func parseLinkNext(link string) string {
	if link == "" {
		return ""
	}
	for _, part := range splitLink(link) {
		u, rel := parseLinkPart(part)
		if rel == "next" {
			return u
		}
	}
	return ""
}

func splitLink(link string) []string {
	var parts []string
	depth := 0
	start := 0
	for i, ch := range link {
		switch ch {
		case '<':
			depth++
		case '>':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, link[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, link[start:])
}

func parseLinkPart(part string) (u, rel string) {
	lt, gt := -1, -1
	for i, ch := range part {
		if ch == '<' && lt == -1 {
			lt = i
		}
		if ch == '>' && gt == -1 {
			gt = i
		}
	}
	if lt == -1 || gt == -1 || gt <= lt {
		return "", ""
	}
	u = part[lt+1 : gt]
	rest := part[gt+1:]
	const relKey = `rel="`
	idx := indexOf(rest, relKey)
	if idx == -1 {
		return u, ""
	}
	rest = rest[idx+len(relKey):]
	end := indexOf(rest, `"`)
	if end == -1 {
		return u, ""
	}
	return u, rest[:end]
}

func indexOf(s, sub string) int {
	for i := range s {
		if i+len(sub) <= len(s) && s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
