package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/navikt/galning/internal/query"
)

const sessionCookieName = "galning_session"

// SessionStore manages Query Sessions. *MemorySessions implements it.
type SessionStore interface {
	New(token string) (string, error)
	Lookup(id string) *Session
	Delete(id string)
}

// TeamLister resolves a user's teams and the repos each team grants access to.
// *github.UserTeams implements it.
type TeamLister interface {
	Teams(ctx context.Context, token string) ([]Team, error)
	TeamRepos(ctx context.Context, token, teamSlug string) ([]string, error)
}

// QueryHandler serves the login-first query UI: it authenticates the user via
// GitHub OAuth, keeps the user token in a server-side Query Session, renders a
// query form, and runs the Archive Query against BigQuery scoped to the repos
// of a single team the user picks, returning the result as a downloadable JSON
// file.
//
// The user token lives only in the session and is used solely to derive the
// user's teams and their repos — it is never persisted.
type QueryHandler struct {
	clientID     string
	clientSecret string
	callbackURL  string
	sessions     SessionStore
	teamLister   TeamLister
	querier      query.Querier
	httpClient   *http.Client
	states       *stateSet
}

// NewQueryHandler creates a QueryHandler.
func NewQueryHandler(
	clientID, clientSecret, callbackURL string,
	sessions SessionStore,
	teams TeamLister,
	querier query.Querier,
) *QueryHandler {
	return &QueryHandler{
		clientID:     clientID,
		clientSecret: clientSecret,
		callbackURL:  callbackURL,
		sessions:     sessions,
		teamLister:   teams,
		querier:      querier,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		states:       newStateSet(),
	}
}

// sessionFromRequest resolves the session cookie to a Session, or nil.
func (h *QueryHandler) sessionFromRequest(r *http.Request) (string, *Session) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return "", nil
	}
	sess := h.sessions.Lookup(c.Value)
	if sess == nil {
		return "", nil
	}
	return c.Value, sess
}

// requireSession wraps a handler that needs an authenticated Query Session.
// Unauthenticated requests are redirected to the login flow.
func (h *QueryHandler) requireSession(next func(w http.ResponseWriter, r *http.Request, sess *Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, sess := h.sessionFromRequest(r)
		if sess == nil {
			http.Redirect(w, r, "/query/login", http.StatusSeeOther)
			return
		}
		next(w, r, sess)
	}
}

// Login starts the GitHub OAuth flow for the query UI.
// GET /query/login
func (h *QueryHandler) Login(w http.ResponseWriter, r *http.Request) {
	state, err := h.states.add()
	if err != nil {
		http.Error(w, "failed to generate state", http.StatusInternalServerError)
		return
	}

	params := url.Values{
		"client_id":    {h.clientID},
		"redirect_uri": {h.callbackURL},
		"scope":        {"repo read:org"}, // read:org is required to list the user's teams and their repos
		"state":        {state},
	}
	http.Redirect(w, r, githubAuthorizeURL+"?"+params.Encode(), http.StatusFound)
}

// Callback handles the GitHub OAuth redirect for the query flow: it exchanges
// the code for a user token, opens a Query Session, and sets the session
// cookie. GET /query/callback
func (h *QueryHandler) Callback(w http.ResponseWriter, r *http.Request) {
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		desc := r.URL.Query().Get("error_description")
		slog.Error("query oauth callback error", "error", errParam, "description", desc)
		http.Error(w, "GitHub authorization failed: "+errParam, http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	if !h.states.consume(state) {
		http.Error(w, "invalid or expired state — please restart the login flow", http.StatusBadRequest)
		return
	}

	token, err := h.exchange(r.Context(), code)
	if err != nil {
		slog.Error("query token exchange failed", "error", err)
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}

	id, err := h.sessions.New(token)
	if err != nil {
		slog.Error("create session failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.setSessionCookie(w, id, int(sessionTTL.Seconds()))
	http.Redirect(w, r, "/query", http.StatusSeeOther)
}

// setSessionCookie writes the session cookie. Secure is set only when the
// callback URL is HTTPS — over plain HTTP (local development) browsers reject
// Secure cookies. HttpOnly and SameSite=Lax are always set.
func (h *QueryHandler) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{ // #nosec G124 — Secure intentionally off for localhost (http) callbacks
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(h.callbackURL),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	})
}

// Form renders the query form.
// GET /query
func (h *QueryHandler) Form(w http.ResponseWriter, r *http.Request) {
	h.requireSession(func(w http.ResponseWriter, r *http.Request, _ *Session) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, queryFormHTML)
	})(w, r)
}

// Teams returns the teams the user belongs to as JSON, for the team picker.
// The list is fetched from GitHub on first call and cached in the session.
// GET /query/teams
func (h *QueryHandler) Teams(w http.ResponseWriter, r *http.Request) {
	h.requireSession(func(w http.ResponseWriter, r *http.Request, sess *Session) {
		teams, err := h.teams(r.Context(), sess)
		if err != nil {
			slog.Error("list teams failed", "error", err)
			http.Error(w, "could not list your teams", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(map[string][]Team{"teams": teams}); err != nil {
			slog.Error("encode teams", "error", err)
		}
	})(w, r)
}

// Repos returns the repos of a single team as JSON, for the repo picker shown
// after the user picks a team. The team must be one of the user's own teams.
// Repos are fetched from GitHub on first call and cached per team in the session.
// GET /query/repos?team=<slug>
func (h *QueryHandler) Repos(w http.ResponseWriter, r *http.Request) {
	h.requireSession(func(w http.ResponseWriter, r *http.Request, sess *Session) {
		team := r.URL.Query().Get("team")
		if team == "" {
			http.Error(w, "a team is required", http.StatusBadRequest)
			return
		}

		userTeams, err := h.teams(r.Context(), sess)
		if err != nil {
			slog.Error("list teams failed", "error", err)
			http.Error(w, "could not list your teams", http.StatusBadGateway)
			return
		}
		if !hasTeam(userTeams, team) {
			slog.Warn("team not in user's teams", "team", team)
			http.Error(w, "you are not a member of that team", http.StatusForbidden)
			return
		}

		repos, err := h.teamRepos(r.Context(), sess, team)
		if err != nil {
			slog.Error("list team repos failed", "team", team, "error", err)
			http.Error(w, "could not list the team's repositories", http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(map[string][]string{"repos": repos}); err != nil {
			slog.Error("encode repos", "error", err)
		}
	})(w, r)
}

// Run executes the Archive Query and streams the result as a downloadable JSON
// file. The query is scoped to the repos of the single team the user picked;
// an optional repo filter narrows it further (silently intersected). The
// picked team must be one of the user's own teams.
// POST /query/run
func (h *QueryHandler) Run(w http.ResponseWriter, r *http.Request) {
	h.requireSession(func(w http.ResponseWriter, r *http.Request, sess *Session) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form submission", http.StatusBadRequest)
			return
		}

		team := r.PostForm.Get("team")
		if team == "" {
			http.Error(w, "a team is required", http.StatusBadRequest)
			return
		}

		// The picked team must be one of the user's own teams — otherwise a
		// user could probe the repos of teams they don't belong to.
		userTeams, err := h.teams(r.Context(), sess)
		if err != nil {
			slog.Error("list teams failed", "error", err)
			http.Error(w, "could not list your teams", http.StatusBadGateway)
			return
		}
		if !hasTeam(userTeams, team) {
			slog.Warn("team not in user's teams", "team", team)
			http.Error(w, "you are not a member of that team", http.StatusForbidden)
			return
		}

		teamRepos, err := h.teamRepos(r.Context(), sess, team)
		if err != nil {
			slog.Error("list team repos failed", "team", team, "error", err)
			http.Error(w, "could not list the team's repositories", http.StatusBadGateway)
			return
		}

		// An optional repo filter narrows the team's repos; repos outside the
		// team's set are silently dropped. No checked boxes = the whole team.
		repos := teamRepos
		if filter := r.PostForm["repo"]; len(filter) > 0 {
			repos = intersect(filter, teamRepos)
		}
		if len(repos) == 0 {
			http.Error(w, "none of the specified repositories are accessible to that team", http.StatusBadRequest)
			return
		}

		filters, err := parseFilters(queryFilters{
			Repos:        repos,
			ActionFilter: r.PostForm.Get("action_filter"),
			Action:       r.PostForm.Get("action"),
			From:         r.PostForm.Get("from"),
			To:           r.PostForm.Get("to"),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		result, err := h.querier.Query(r.Context(), filters)
		if err != nil {
			slog.Error("archive query failed", "error", err)
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}

		filename := fmt.Sprintf("audit-events-%s.json", time.Now().UTC().Format("20060102-150405"))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			slog.Error("encode query result", "error", err)
		}
	})(w, r)
}

// Logout deletes the Query Session and clears the cookie.
// POST /query/logout
func (h *QueryHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if id, _ := h.sessionFromRequest(r); id != "" {
		h.sessions.Delete(id)
	}
	h.setSessionCookie(w, "", -1)
	http.Redirect(w, r, "/query", http.StatusSeeOther)
}

// teams returns the user's teams, fetching and caching them in the session on
// first use.
func (h *QueryHandler) teams(ctx context.Context, sess *Session) ([]Team, error) {
	if cached := sess.CachedTeams(); cached != nil {
		return cached, nil
	}

	teams, err := h.teamLister.Teams(ctx, sess.Token)
	if err != nil {
		return nil, err
	}
	sess.SetTeams(teams)

	return teams, nil
}

// teamRepos returns the repos the given team can access, fetching and caching
// them in the session on first use.
func (h *QueryHandler) teamRepos(ctx context.Context, sess *Session, teamSlug string) ([]string, error) {
	if cached := sess.CachedTeamRepos(teamSlug); cached != nil {
		return cached, nil
	}

	repos, err := h.teamLister.TeamRepos(ctx, sess.Token, teamSlug)
	if err != nil {
		return nil, err
	}
	sess.SetTeamRepos(teamSlug, repos)

	return repos, nil
}

// hasTeam reports whether slug is among the user's teams.
func hasTeam(teams []Team, slug string) bool {
	for _, t := range teams {
		if t.Slug == slug {
			return true
		}
	}
	return false
}

// intersect returns the elements of typed that are also in allowed, preserving
// order and dropping duplicates.
func intersect(typed, allowed []string) []string {
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[a] = true
	}
	seen := make(map[string]bool, len(typed))
	var out []string
	for _, t := range typed {
		if set[t] && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// exchange exchanges an authorization code for an access token.
// Returns the raw access token string — it is stored only in the session.
func (h *QueryHandler) exchange(ctx context.Context, code string) (string, error) {
	body := url.Values{
		"client_id":     {h.clientID},
		"client_secret": {h.clientSecret},
		"code":          {code},
		"redirect_uri":  {h.callbackURL},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubAccessTokenURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("exchange: status %d: %s", resp.StatusCode, raw)
	}

	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decode exchange response: %w", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("github oauth error: %s", result.Error)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("empty access token in exchange response")
	}
	return result.AccessToken, nil
}

// queryFilters holds the user-supplied Query parameters from the form.
type queryFilters struct {
	Repos        []string
	ActionFilter string
	Action       string
	From         string
	To           string
}

// parseFilters converts queryFilters (strings) into query.Filters (typed values).
func parseFilters(f queryFilters) (query.Filters, error) {
	out := query.Filters{
		Repos:  f.Repos,
		Action: parseActionFilter(f.ActionFilter, f.Action),
	}
	if f.From != "" {
		t, err := time.Parse(time.DateOnly, f.From)
		if err != nil {
			return query.Filters{}, fmt.Errorf("invalid from date %q — use YYYY-MM-DD", f.From)
		}
		out.From = t
	}
	if f.To != "" {
		t, err := time.Parse(time.DateOnly, f.To)
		if err != nil {
			return query.Filters{}, fmt.Errorf("invalid to date %q — use YYYY-MM-DD", f.To)
		}
		// Include the full to-day by moving to end of day.
		out.To = t.Add(24*time.Hour - time.Nanosecond)
	}
	return out, nil
}

// parseActionFilter maps the form's action_filter choice + custom value to a
// query.ActionFilter. "riksrevisjonen" selects the compliance preset, "custom"
// uses the single action text field, and anything else ("all") applies no
// action filter.
func parseActionFilter(filter, custom string) query.ActionFilter {
	switch filter {
	case "riksrevisjonen":
		return query.ActionFilter{Preset: true}
	case "custom":
		return query.ActionFilter{Exact: custom}
	default:
		return query.ActionFilter{}
	}
}

// isHTTPS reports whether the given URL uses the https scheme.
func isHTTPS(rawurl string) bool {
	u, err := url.Parse(rawurl)
	return err == nil && u.Scheme == "https"
}

// queryFormHTML is the query form. The user picks a team (loaded from
// /query/teams); that team's repos are then fetched (/query/repos?team=) and
// shown as a checkbox list. The user narrows the selection with the filter box
// or by unchecking repos, and downloads the matching Audit Events as JSON.
const queryFormHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><text y='.9em' font-size='90'>📜</text></svg>">
  <title>GALNING — Query Audit Events</title>
  <style>
    body { font-family: monospace; max-width: 640px; margin: 2rem auto; padding: 0 1rem; }
    label { display: block; margin-top: 1rem; font-weight: bold; }
    input[type=text], input[type=date], select { width: 100%; box-sizing: border-box; font-family: monospace; font-size: 1rem; padding: 0.3rem; }
    small { color: #555; font-weight: normal; }
    button { margin-top: 1.5rem; padding: 0.5rem 1.5rem; font-size: 1rem; cursor: pointer; }
    button:disabled { cursor: not-allowed; opacity: 0.6; }
    .topbar { display: flex; justify-content: space-between; align-items: center; }
    .topbar form { margin: 0; }
    .topbar button { margin: 0; padding: 0.25rem 0.75rem; font-size: 0.85rem; }
    .msg { color: #555; font-style: italic; margin: 0.25rem 0; }
    .msg.error { color: #b00; font-style: normal; }
    .repolist { max-height: 16rem; overflow-y: auto; border: 1px solid #ccc; padding: 0.25rem; }
    .repo { display: block; padding: 0.15rem 0.3rem; font-weight: normal; cursor: pointer; }
    .repo:hover { background: #f0f0f0; }
    .repo input { margin-right: 0.4rem; }
    .repo.checked { background: #e6f0ff; }
    #repocount { color: #555; font-weight: normal; }
    .repotools { display: flex; gap: 0.5rem; align-items: center; }
    .repotools input { flex: 1; }
    .repotools button { margin: 0; padding: 0.3rem 0.75rem; font-size: 0.85rem; white-space: nowrap; }
    details { margin-top: 0.5rem; border: 1px solid #ccc; padding: 0.4rem 0.6rem; font-size: 0.9rem; }
    details summary { cursor: pointer; font-weight: bold; }
    details ul { margin: 0.4rem 0 0.2rem; padding-left: 1.2rem; }
    details li { margin: 0.1rem 0; }
    .hidden { display: none; }
  </style>
</head>
<body>
  <div class="topbar">
    <h1>Query Audit Events</h1>
    <form action="/query/logout" method="post"><button type="submit">Log out</button></form>
  </div>
  <p>Results are returned as a downloadable JSON file, scoped to the repositories of the team you pick.</p>
  <p><small>Results are limited to 50&thinsp;000 events (newest first). Narrow the date range or repo selection if a result is truncated.</small></p>
  <form action="/query/run" method="post">
    <label for="team">Team</label>
    <p class="msg" id="teammsg">Loading your teams…</p>
    <select id="team" name="team" required disabled></select>

    <div id="repoblock" class="hidden">
      <label for="repofilter">Repositories <small>(uncheck to exclude — all are included by default)</small> <span id="repocount"></span></label>
      <p class="msg" id="repomsg">Loading repositories…</p>
      <div class="repotools">
        <input id="repofilter" type="text" placeholder="Type to filter…" autocomplete="off">
        <button type="button" id="selectall">Select all</button>
        <button type="button" id="selectnone">Unselect all</button>
      </div>
      <div id="repos" class="repolist" role="group" aria-label="Repositories"></div>
    </div>

    <label for="action_filter">Actions</label>
    <select id="action_filter" name="action_filter">
      <option value="riksrevisjonen" selected>Riksrevisjonen (compliance)</option>
      <option value="all">All actions</option>
      <option value="custom">Custom</option>
    </select>
    <div id="customaction" class="hidden">
      <label for="action">Action <small>(exact, e.g. repo.create)</small></label>
      <input id="action" name="action" type="text" placeholder="repo.create">
    </div>

    <details id="filterinfo">
      <summary>What does the Riksrevisjonen filter include?</summary>
      <ul>
        <li><strong>protected_branch.*</strong> (except <code>rejected_ref_update</code>)</li>
        <li><strong>repository_ruleset.*</strong></li>
        <li><strong>repository_branch_protection_evaluation.*</strong></li>
        <li><code>repo.update_member</code></li>
        <li><code>repo.remove_member</code></li>
        <li><code>repo.add_member</code></li>
        <li><code>team.add_repository</code></li>
        <li><code>team.remove_repository</code></li>
        <li><code>team.update_repository_permission</code></li>
      </ul>
    </details>

    <label for="from">From <small>(optional)</small></label>
    <input id="from" name="from" type="date">

    <label for="to">To <small>(optional)</small></label>
    <input id="to" name="to" type="date">

    <button type="submit" id="submit" disabled>Download JSON</button>
  </form>
  <script>
    (function() {
      var team = document.getElementById('team');
      var submit = document.getElementById('submit');
      var teamMsg = document.getElementById('teammsg');
      var repoBlock = document.getElementById('repoblock');
      var repoMsg = document.getElementById('repomsg');
      var repoList = document.getElementById('repos');
      var repoCount = document.getElementById('repocount');
      var filter = document.getElementById('repofilter');
      var selectAll = document.getElementById('selectall');
      var selectNone = document.getElementById('selectnone');
      var actionFilter = document.getElementById('action_filter');
      var customAction = document.getElementById('customaction');
      var filterInfo = document.getElementById('filterinfo');

      // Show the custom action input only when "Custom" is selected, and the
      // filter info block only for the Riksrevisjonen preset.
      actionFilter.addEventListener('change', function() {
        customAction.classList.toggle('hidden', actionFilter.value !== 'custom');
        filterInfo.classList.toggle('hidden', actionFilter.value !== 'riksrevisjonen');
      });

      // Update the "N of M selected" counter.
      function refreshCount() {
        var boxes = repoList.querySelectorAll('input[type=checkbox]');
        var n = 0;
        boxes.forEach(function(cb) { if (cb.checked) { n++; } });
        repoCount.textContent = boxes.length === 0 ? '' : '(' + n + ' of ' + boxes.length + ' selected)';
      }

      // Check or uncheck every repo checkbox at once.
      function setAll(checked) {
        repoList.querySelectorAll('input[type=checkbox]').forEach(function(cb) {
          cb.checked = checked;
          cb.closest('.repo').classList.toggle('checked', checked);
        });
        filter.value = ''; // clear the filter so the new selection is visible
        filter.dispatchEvent(new Event('input'));
        refreshCount();
      }
      selectAll.addEventListener('click', function() { setAll(true); });
      selectNone.addEventListener('click', function() { setAll(false); });

      // Filter: hide unchecked rows that don't match; never hide checked rows.
      filter.addEventListener('input', function() {
        var q = filter.value.toLowerCase();
        repoList.querySelectorAll('.repo').forEach(function(row) {
          var cb = row.querySelector('input[type=checkbox]');
          var matches = q === '' || cb.value.toLowerCase().indexOf(q) !== -1;
          row.style.display = (cb.checked || matches) ? '' : 'none';
        });
      });

      // Toggle styling + counter when a checkbox changes (event delegation).
      repoList.addEventListener('change', function(e) {
        if (e.target && e.target.type === 'checkbox') {
          e.target.closest('.repo').classList.toggle('checked', e.target.checked);
          refreshCount();
        }
      });

      // When a team is picked, fetch its repos and build the checkbox list.
      team.addEventListener('change', function() {
        var slug = team.value;
        submit.disabled = true;
        repoList.innerHTML = '';
        repoCount.textContent = '';
        filter.value = '';
        if (slug === '') {
          repoBlock.classList.add('hidden');
          return;
        }
        repoBlock.classList.remove('hidden');
        repoMsg.textContent = 'Loading repositories…';
        repoMsg.classList.remove('error');
        repoMsg.style.display = '';

        fetch('/query/repos?team=' + encodeURIComponent(slug))
          .then(function(resp) {
            if (!resp.ok) { throw new Error('HTTP ' + resp.status); }
            return resp.json();
          })
          .then(function(data) {
            var repos = data.repos || [];
            repos.forEach(function(name) {
              var label = document.createElement('label');
              label.className = 'repo checked';
              var cb = document.createElement('input');
              cb.type = 'checkbox';
              cb.name = 'repo';
              cb.value = name;
              cb.checked = true;
              label.appendChild(cb);
              label.appendChild(document.createTextNode(name));
              repoList.appendChild(label);
            });
            if (repos.length === 0) {
              repoMsg.textContent = 'That team has no accessible repositories.';
              repoMsg.classList.add('error');
              return;
            }
            repoMsg.style.display = 'none';
            refreshCount();
            submit.disabled = false;
          })
          .catch(function(err) {
            repoMsg.textContent = 'Could not load repositories (' + err.message + '). Try picking the team again.';
            repoMsg.classList.add('error');
          });
      });

      // Load the user's teams and populate the picker.
      fetch('/query/teams')
        .then(function(resp) {
          if (!resp.ok) { throw new Error('HTTP ' + resp.status); }
          return resp.json();
        })
        .then(function(data) {
          var teams = data.teams || [];
          var placeholder = document.createElement('option');
          placeholder.value = '';
          placeholder.textContent = teams.length === 0 ? 'You are not a member of any team' : 'Select a team…';
          team.appendChild(placeholder);
          teams.forEach(function(t) {
            var opt = document.createElement('option');
            opt.value = t.slug;
            opt.textContent = t.name;
            team.appendChild(opt);
          });
          if (teams.length === 0) {
            teamMsg.textContent = 'You are not a member of any team in this organisation.';
            teamMsg.classList.add('error');
            return;
          }
          teamMsg.style.display = 'none';
          team.disabled = false;
        })
        .catch(function(err) {
          teamMsg.textContent = 'Could not load your teams (' + err.message + '). Try reloading the page.';
          teamMsg.classList.add('error');
        });
    })();
  </script>
</body>
</html>
`
