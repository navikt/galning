// Package oauth provides HTTP handlers for the GitHub OAuth flow.
package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	githubAuthorizeURL   = "https://github.com/login/oauth/authorize"
	githubAccessTokenURL = "https://github.com/login/oauth/access_token" // #nosec G101
	stateExpiry          = 10 * time.Minute
)

// Handler serves the GitHub OAuth authorization and callback endpoints.
type Handler struct {
	clientID     string
	clientSecret string
	callbackURL  string
	store        Store
	httpClient   *http.Client

	mu     sync.Mutex
	states map[string]time.Time // state → expiry
}

// NewHandler creates an OAuth Handler.
func NewHandler(clientID, clientSecret, callbackURL string, store Store) *Handler {
	return &Handler{
		clientID:     clientID,
		clientSecret: clientSecret,
		callbackURL:  callbackURL,
		store:        store,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		states:       make(map[string]time.Time),
	}
}

// Authorize redirects the user to GitHub's OAuth authorization page.
// GET /api/authorize
func (h *Handler) Authorize(w http.ResponseWriter, r *http.Request) {
	state, err := randomState()
	if err != nil {
		http.Error(w, "failed to generate state", http.StatusInternalServerError)
		return
	}

	h.mu.Lock()
	h.pruneStates()
	h.states[state] = time.Now().Add(stateExpiry)
	h.mu.Unlock()

	params := url.Values{
		"client_id":    {h.clientID},
		"redirect_uri": {h.callbackURL},
		"scope":        {"read:audit_log"},
		"state":        {state},
	}
	http.Redirect(w, r, githubAuthorizeURL+"?"+params.Encode(), http.StatusFound)
}

// Callback handles the GitHub OAuth redirect, exchanges the code for tokens,
// and saves them to the TokenStore.
// GET /api/callback
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	errParam := r.URL.Query().Get("error")

	if errParam != "" {
		desc := r.URL.Query().Get("error_description")
		slog.Error("oauth callback error", "error", errParam, "description", desc)
		http.Error(w, "GitHub authorization failed: "+errParam, http.StatusBadRequest)
		return
	}

	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	expiry, ok := h.states[state]
	if ok {
		delete(h.states, state)
	}
	h.mu.Unlock()

	if !ok || time.Now().After(expiry) {
		http.Error(w, "invalid or expired state — please restart the authorization flow", http.StatusBadRequest)
		return
	}

	pair, err := h.exchange(r.Context(), code)
	if err != nil {
		slog.Error("token exchange failed", "error", err)
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}

	if err := h.store.Save(r.Context(), pair); err != nil {
		slog.Error("save tokens failed", "error", err)
		http.Error(w, "failed to save tokens", http.StatusInternalServerError)
		return
	}

	slog.Info("oauth flow complete — tokens saved")
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintln(w, "Authorization complete. GALNING is now active.")
}

// exchange exchanges an authorization code for a TokenPair.
func (h *Handler) exchange(ctx context.Context, code string) (*TokenPair, error) {
	body := url.Values{
		"client_id":     {h.clientID},
		"client_secret": {h.clientSecret},
		"code":          {code},
		"redirect_uri":  {h.callbackURL},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubAccessTokenURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange: status %d: %s", resp.StatusCode, raw)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode exchange response: %w", err)
	}
	if result.Error != "" {
		return nil, fmt.Errorf("github oauth error: %s", result.Error)
	}

	// OAuth App tokens do not expire — ExpiresAt is left as zero value.
	return &TokenPair{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
	}, nil
}

// pruneStates removes expired state entries. Must be called with h.mu held.
func (h *Handler) pruneStates() {
	now := time.Now()
	for s, exp := range h.states {
		if now.After(exp) {
			delete(h.states, s)
		}
	}
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
