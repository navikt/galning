// Package oauth manages the GitHub OAuth token pair used for API access.
// Tokens are persisted as a JSON secret version in Google Secret Manager
// so they survive pod restarts without requiring Kubernetes Secret access.
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TokenPair holds a GitHub OAuth user access token and the pagination cursor
// for the audit log. Both are persisted together in Secret Manager.
// OAuth App tokens do not expire and have no refresh token.
// ExpiresAt and RefreshToken are retained for backwards compatibility with any
// token already persisted in Secret Manager, but are not used.
type TokenPair struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Cursor       string    `json:"cursor,omitempty"`
}

// Store is the interface for loading and saving the OAuth TokenPair.
type Store interface {
	Load(ctx context.Context) (*TokenPair, error)
	Save(ctx context.Context, pair *TokenPair) error
}

// TokenStore reads and writes the OAuth TokenPair to Google Secret Manager.
// Each Save adds a new secret version; Load reads the latest version.
// The secret resource must already exist — the store only adds versions.
type TokenStore struct {
	client     *secretmanager.Client
	secretName string // full resource name: projects/{project}/secrets/{name}
}

// NewTokenStore creates a TokenStore backed by Google Secret Manager.
// secretName is the fully-qualified resource name:
//
//	projects/PROJECT_ID/secrets/SECRET_NAME
func NewTokenStore(ctx context.Context, secretName string) (*TokenStore, error) {
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create secret manager client: %w", err)
	}
	return &TokenStore{client: client, secretName: secretName}, nil
}

// Load reads the latest TokenPair from Secret Manager.
// Returns nil without error if no version exists yet (first run before OAuth).
func (s *TokenStore) Load(ctx context.Context) (*TokenPair, error) {
	req := &secretmanagerpb.AccessSecretVersionRequest{
		Name: s.secretName + "/versions/latest",
	}
	result, err := s.client.AccessSecretVersion(ctx, req)
	if err != nil {
		if isNotFound(err) {
			return nil, nil // no version yet — not yet authorised
		}
		return nil, fmt.Errorf("access secret version: %w", err)
	}

	var pair TokenPair
	if err := json.Unmarshal(result.Payload.Data, &pair); err != nil {
		return nil, fmt.Errorf("unmarshal token pair: %w", err)
	}
	return &pair, nil
}

// Save adds a new version of the secret containing the TokenPair.
func (s *TokenStore) Save(ctx context.Context, pair *TokenPair) error {
	data, err := json.Marshal(pair) // #nosec G117
	if err != nil {
		return fmt.Errorf("marshal token pair: %w", err)
	}

	req := &secretmanagerpb.AddSecretVersionRequest{
		Parent: s.secretName,
		Payload: &secretmanagerpb.SecretPayload{
			Data: data,
		},
	}
	if _, err := s.client.AddSecretVersion(ctx, req); err != nil {
		return fmt.Errorf("add secret version: %w", err)
	}
	return nil
}

// Close releases the Secret Manager client resources.
func (s *TokenStore) Close() error {
	return s.client.Close()
}

// InMemoryStore holds the OAuth TokenPair in memory only.
// It is used in dry-run mode where no Secret Manager access is needed.
type InMemoryStore struct {
	mu   sync.RWMutex
	pair *TokenPair
}

// NewInMemoryStore returns an InMemoryStore with no token set.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{}
}

// Load returns the current in-memory TokenPair, or nil if none has been saved yet.
func (s *InMemoryStore) Load(_ context.Context) (*TokenPair, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pair, nil
}

// Save stores the TokenPair in memory.
func (s *InMemoryStore) Save(_ context.Context, pair *TokenPair) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pair = pair
	return nil
}

func isNotFound(err error) bool {
	return status.Code(err) == codes.NotFound
}
