package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/navikt/galning/internal/oauth"
)

type userTeam struct {
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Organization struct {
		Login string `json:"login"`
	} `json:"organization"`
}

type teamRepo struct {
	FullName string `json:"full_name"`
}

// UserClient resolves a user's teams in the organisation and the repositories
// each team grants access to.
type UserClient struct {
	org        string
	httpClient *http.Client
}

// NewUserClient returns a UserTeams scoped to the given organisation.
func NewUserClient(org string) *UserClient {
	return &UserClient{
		org:        org,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// excludedTeamSlugs are org-wide teams that every user is a member of and that
// would only add noise to the picker.
var excludedTeamSlugs = map[string]bool{
	"nav-it-github-users": true, // "NAV IT Github users"
}

// Teams returns the teams the user belongs to in the configured organisation,
// sorted by name. Org-wide teams that everyone belongs to are excluded.
func (u *UserClient) Teams(ctx context.Context, token string) ([]oauth.Team, error) {
	nextURL := fmt.Sprintf("%s/user/teams?per_page=%d", apiBase, pageSize)

	var teams []oauth.Team
	for nextURL != "" {
		body, next, err := u.get(ctx, token, nextURL)
		if err != nil {
			return nil, err
		}

		var page []userTeam
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode user teams response: %w", err)
		}
		for _, t := range page {
			if t.Organization.Login == u.org && !excludedTeamSlugs[t.Slug] {
				teams = append(teams, oauth.Team{Slug: t.Slug, Name: t.Name})
			}
		}
		nextURL = next
	}

	sort.Slice(teams, func(i, j int) bool { return teams[i].Name < teams[j].Name })
	return teams, nil
}

// TeamRepos returns the full names (owner/repo) of the repositories the given
// team can access, sorted alphabetically.
func (u *UserClient) TeamRepos(ctx context.Context, token, teamSlug string) ([]string, error) {
	nextURL := fmt.Sprintf("%s/orgs/%s/teams/%s/repos?per_page=%d", apiBase, u.org, teamSlug, pageSize)

	var repos []string
	for nextURL != "" {
		body, next, err := u.get(ctx, token, nextURL)
		if err != nil {
			return nil, err
		}

		var page []teamRepo
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode team repos response: %w", err)
		}
		for _, r := range page {
			repos = append(repos, r.FullName)
		}
		nextURL = next
	}

	sort.Strings(repos)
	return repos, nil
}

// get performs an authenticated GET and returns the body and the next-page URL.
func (u *UserClient) get(ctx context.Context, token, url string) (body []byte, next string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close() // #nosec G104

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read %s response: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("request %s: status %d: %s", url, resp.StatusCode, b)
	}
	return b, parseLinkNext(resp.Header.Get("Link")), nil
}
