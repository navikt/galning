package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"

	gh "github.com/navikt/galning/internal/github"
)

// Schema column names mirror the BigQuery table definition.
// The table is partitioned on created_at (DATE) and clustered on action.

// Row is the BigQuery row shape for a single Audit Event.
type Row struct {
	DocumentID    string    `bigquery:"document_id"`
	Action        string    `bigquery:"action"`
	Actor         string    `bigquery:"actor"`
	ActorIP       string    `bigquery:"actor_ip"`
	CreatedAt     time.Time `bigquery:"created_at"`
	Org           string    `bigquery:"org"`
	Repo          string    `bigquery:"repo"`
	User          string    `bigquery:"user"`
	OperationType string    `bigquery:"operation_type"`
	Raw           string    `bigquery:"raw"`
}

// Save implements bigquery.ValueSaver so we can use InsertAll.
func (r Row) Save() (map[string]bigquery.Value, string, error) {
	return map[string]bigquery.Value{
		"document_id":    r.DocumentID,
		"action":         r.Action,
		"actor":          r.Actor,
		"actor_ip":       r.ActorIP,
		"created_at":     r.CreatedAt,
		"org":            r.Org,
		"repo":           r.Repo,
		"user":           r.User,
		"operation_type": r.OperationType,
		"raw":            r.Raw,
	}, r.DocumentID, nil // use document_id as the insert dedup key
}

// Schema is the BigQuery table schema for the Archive.
var Schema = bigquery.Schema{
	{Name: "document_id", Type: bigquery.StringFieldType, Required: true},
	{Name: "action", Type: bigquery.StringFieldType, Required: true},
	{Name: "actor", Type: bigquery.StringFieldType},
	{Name: "actor_ip", Type: bigquery.StringFieldType},
	{Name: "created_at", Type: bigquery.TimestampFieldType, Required: true},
	{Name: "org", Type: bigquery.StringFieldType, Required: true},
	{Name: "repo", Type: bigquery.StringFieldType},
	{Name: "user", Type: bigquery.StringFieldType},
	{Name: "operation_type", Type: bigquery.StringFieldType},
	{Name: "raw", Type: bigquery.JSONFieldType, Required: true},
}

// Archive wraps the BigQuery table for the compliance Archive.
type Archive struct {
	table *bigquery.Table
	bq    *bigquery.Client
}

// New connects to BigQuery and returns an Archive for the given table.
// It creates the table if it does not exist, with date partitioning on
// created_at and clustering on action.
func New(ctx context.Context, projectID, datasetID, tableID string) (*Archive, error) {
	bq, err := bigquery.NewClient(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("bigquery client: %w", err)
	}

	table := bq.Dataset(datasetID).Table(tableID)

	// Create table if it does not exist.
	meta := &bigquery.TableMetadata{
		Schema: Schema,
		TimePartitioning: &bigquery.TimePartitioning{
			Type:  bigquery.DayPartitioningType,
			Field: "created_at",
		},
		Clustering: &bigquery.Clustering{
			Fields: []string{"action"},
		},
	}
	if err := table.Create(ctx, meta); err != nil {
		// If the table already exists, ignore the error.
		if !isAlreadyExists(err) {
			return nil, fmt.Errorf("create table: %w", err)
		}
	}

	return &Archive{table: table, bq: bq}, nil
}

// LatestCursor queries the Archive for the document_id of the most recently
// inserted Audit Event for the given org. Returns an empty string if the
// Archive is empty (first Ingest Run).
func (a *Archive) LatestCursor(ctx context.Context, org string) (string, error) {
	query := a.bq.Query(fmt.Sprintf(
		"SELECT document_id FROM `%s.%s.%s` WHERE org = @org ORDER BY created_at DESC LIMIT 1",
		a.table.ProjectID, a.table.DatasetID, a.table.TableID,
	))
	query.Parameters = []bigquery.QueryParameter{
		{Name: "org", Value: org},
	}

	it, err := query.Read(ctx)
	if err != nil {
		return "", fmt.Errorf("query cursor: %w", err)
	}

	var row struct {
		DocumentID string `bigquery:"document_id"`
	}
	if err := it.Next(&row); err != nil {
		if err == iterator.Done {
			return "", nil // empty archive — first run
		}
		return "", fmt.Errorf("read cursor row: %w", err)
	}
	return row.DocumentID, nil
}

// Insert inserts a batch of Audit Events into the Archive.
func (a *Archive) Insert(ctx context.Context, events []gh.AuditEvent) error {
	if len(events) == 0 {
		return nil
	}

	inserter := a.table.Inserter()
	inserter.SkipInvalidRows = false
	inserter.IgnoreUnknownValues = false

	rows := make([]*Row, 0, len(events))
	for _, e := range events {
		row, err := toRow(e)
		if err != nil {
			return fmt.Errorf("convert event %s: %w", e.DocumentID, err)
		}
		rows = append(rows, row)
	}

	if err := inserter.Put(ctx, rows); err != nil {
		return fmt.Errorf("insert rows: %w", err)
	}
	return nil
}

// Close releases the BigQuery client resources.
func (a *Archive) Close() error {
	return a.bq.Close()
}

// toRow converts a GitHub AuditEvent to a BigQuery Row.
func toRow(e gh.AuditEvent) (*Row, error) {
	rawJSON, err := json.Marshal(e.Raw)
	if err != nil {
		return nil, err
	}

	// GitHub audit log timestamps are milliseconds since epoch.
	var createdAt time.Time
	if e.CreatedAt != 0 {
		createdAt = time.UnixMilli(e.CreatedAt).UTC()
	} else {
		createdAt = time.Now().UTC()
	}

	return &Row{
		DocumentID:    e.DocumentID,
		Action:        e.Action,
		Actor:         e.Actor,
		ActorIP:       e.ActorIP,
		CreatedAt:     createdAt,
		Org:           e.Org,
		Repo:          e.Repo,
		User:          e.User,
		OperationType: e.OperationType,
		Raw:           string(rawJSON),
	}, nil
}

// isAlreadyExists reports whether a BigQuery error is a "table already exists" error.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	return containsCode(err, 409)
}

func containsCode(err error, code int) bool {
	type coder interface{ Code() int }
	if c, ok := err.(coder); ok && c.Code() == code {
		return true
	}
	// google.golang.org/api/googleapi errors embed the code differently
	type googleErr interface {
		Error() string
	}
	_ = googleErr(nil)
	return false
}
