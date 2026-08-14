// Package query provides BigQuery-backed querying of the Archive.
package query

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// maxRows caps the number of Audit Events a single Query returns. When a
// result is truncated to this many rows, Result.Truncated is true.
const maxRows = 50_000

// AuditEvent is a single result row returned by a Query.
type AuditEvent struct {
	DocumentID    string          `json:"document_id"`
	Action        string          `json:"action"`
	Actor         string          `json:"actor"`
	CreatedAt     time.Time       `json:"created_at"`
	Repo          string          `json:"repo"`
	User          string          `json:"user"`
	OperationType string          `json:"operation_type"`
	Raw           json.RawMessage `json:"raw"`
}

// Filters holds the user-supplied Query parameters.
type Filters struct {
	Repos  []string
	Action ActionFilter
	From   time.Time
	To     time.Time
}

// ActionFilter restricts which Audit Event actions a Query returns.
type ActionFilter struct {
	// Exact matches action exactly. When set, the preset and prefixes are
	// ignored — used for the single-action "Custom" filter.
	Exact string
	// Preset selects the built-in Riksrevisjonen compliance filter.
	Preset bool
}

// riksrevisjonenPrefixActions match any action with this prefix (prefix.*).
var riksrevisjonenPrefixActions = []string{
	"protected_branch",
	"repository_ruleset",
	"repository_branch_protection_evaluation",
}

// riksrevisjonenExactActions match these actions exactly.
var riksrevisjonenExactActions = []string{
	"repo.update_member",
	"repo.remove_member",
	"repo.add_member",
	"team.add_repository",
	"team.remove_repository",
	"team.update_repository_permission",
}

// riksrevisjonenExcludedActions are removed even though they match a prefix.
var riksrevisjonenExcludedActions = []string{
	"protected_branch.rejected_ref_update",
}

// Result is the full Query result returned to the caller.
type Result struct {
	Query     ResultQuery  `json:"query"`
	SQL       string       `json:"sql"`
	Count     int          `json:"count"`
	Truncated bool         `json:"truncated"`
	Events    []AuditEvent `json:"events"`
}

// ResultQuery echoes back the filters that were applied.
type ResultQuery struct {
	Repos  []string `json:"repos"`
	Action string   `json:"action,omitempty"`
	From   string   `json:"from,omitempty"`
	To     string   `json:"to,omitempty"`
}

// describe renders the action filter as a human-readable string for the result
// echo ("custom" action value, or "riksrevisjonen" for the preset).
func (f ActionFilter) describe() string {
	if f.Exact != "" {
		return f.Exact
	}
	if f.Preset {
		return "riksrevisjonen"
	}
	return ""
}

// Querier executes a Query against the Archive.
type Querier interface {
	Query(ctx context.Context, f Filters) (*Result, error)
}

// actionClause builds the WHERE fragment for the action filter. It returns an
// empty string when no action filtering applies.
//
// The preset lists are hardcoded package constants — not user input — so they
// are safely interpolated as quoted string literals. The custom single-action
// filter is passed via the @action query parameter instead.
func actionClause(f ActionFilter) string {
	switch {
	case f.Exact != "":
		return "  AND action = @action"
	case f.Preset:
		var ors []string
		for _, p := range riksrevisjonenPrefixActions {
			ors = append(ors, fmt.Sprintf("STARTS_WITH(action, '%s.')", p))
		}
		ors = append(ors, "action IN ("+quoteAll(riksrevisjonenExactActions)+")")
		var clause strings.Builder
		clause.WriteString(" AND (")
		clause.WriteString(strings.Join(ors, " OR "))
		clause.WriteString(")")
		for _, ex := range riksrevisjonenExcludedActions {
			fmt.Fprintf(&clause, " AND action != '%s'", ex)
		}
		return clause.String()
	default:
		return ""
	}
}

// quoteAll single-quotes each string for use as a SQL string literal. Only
// used with the hardcoded preset constants, never with user input.
func quoteAll(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = "'" + v + "'"
	}
	return strings.Join(quoted, ", ")
}

// BigQueryQuerier implements Querier against a BigQuery Archive table.
type BigQueryQuerier struct {
	client    *bigquery.Client
	projectID string
	datasetID string
	tableID   string
}

// NewBigQueryQuerier returns a BigQueryQuerier for the given Archive table.
func NewBigQueryQuerier(client *bigquery.Client, projectID, datasetID, tableID string) *BigQueryQuerier {
	return &BigQueryQuerier{
		client:    client,
		projectID: projectID,
		datasetID: datasetID,
		tableID:   tableID,
	}
}

// Query executes a parameterised query against the Archive and returns up to
// maxRows Audit Events ordered newest-first.
func (q *BigQueryQuerier) Query(ctx context.Context, f Filters) (*Result, error) {
	from := fmt.Sprintf("`%s.%s.%s`", q.projectID, q.datasetID, q.tableID)
	sql := fmt.Sprintf(`
SELECT
  document_id,
  action,
  actor,
  created_at,
  repo,
  user,
  operation_type,
  raw
FROM %s
WHERE repo IN UNNEST(@repos)
%s
  AND (@from_zero OR created_at >= @from)
  AND (@to_zero   OR created_at <= @to)
QUALIFY ROW_NUMBER() OVER (PARTITION BY document_id) = 1
ORDER BY created_at DESC
LIMIT %d`,
		from, actionClause(f.Action), maxRows)

	fromZero := f.From.IsZero()
	toZero := f.To.IsZero()

	fromVal := f.From
	if fromZero {
		fromVal = time.Time{}
	}
	toVal := f.To
	if toZero {
		toVal = time.Time{}
	}

	bqQuery := q.client.Query(sql)
	bqQuery.Parameters = []bigquery.QueryParameter{
		{Name: "repos", Value: f.Repos},
		{Name: "action", Value: f.Action.Exact},
		{Name: "from_zero", Value: fromZero},
		{Name: "to_zero", Value: toZero},
		{Name: "from", Value: fromVal},
		{Name: "to", Value: toVal},
	}

	it, err := bqQuery.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("run query: %w", err)
	}

	var events []AuditEvent
	for {
		var row struct {
			DocumentID    string    `bigquery:"document_id"`
			Action        string    `bigquery:"action"`
			Actor         string    `bigquery:"actor"`
			CreatedAt     time.Time `bigquery:"created_at"`
			Repo          string    `bigquery:"repo"`
			User          string    `bigquery:"user"`
			OperationType string    `bigquery:"operation_type"`
			Raw           string    `bigquery:"raw"`
		}
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read query row: %w", err)
		}

		// Raw is stored as a JSON string in BigQuery; re-wrap it as json.RawMessage.
		events = append(events, AuditEvent{
			DocumentID:    row.DocumentID,
			Action:        row.Action,
			Actor:         row.Actor,
			CreatedAt:     row.CreatedAt,
			Repo:          row.Repo,
			User:          row.User,
			OperationType: row.OperationType,
			Raw:           json.RawMessage(row.Raw),
		})
	}

	result := &Result{
		// Normalise whitespace so the SQL is a single line, easy to copy-paste.
		SQL: strings.Join(strings.Fields(sql), " "),
		Query: ResultQuery{
			Repos:  f.Repos,
			Action: f.Action.describe(),
		},
		Count:     len(events),
		Truncated: len(events) >= maxRows,
		Events:    events,
	}
	if !f.From.IsZero() {
		result.Query.From = f.From.Format(time.DateOnly)
	}
	if !f.To.IsZero() {
		result.Query.To = f.To.Format(time.DateOnly)
	}
	if events == nil {
		result.Events = []AuditEvent{}
	}

	return result, nil
}
