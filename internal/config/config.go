package config

import (
	"fmt"
	"os"
	"sort"
	"time"
)

type Config struct {
	BigQueryProject string
	BigQueryDataset string
	BigQueryTable   string

	GithubOrg          string
	GithubClientID     string
	GithubClientSecret string
	GithubCallbackURL  string
	GithubTokenSecret  string

	DryRun         bool
	DigestInterval time.Duration
}

func FromEnv() Config {
	return Config{
		BigQueryProject:    os.Getenv("BIGQUERY_PROJECT"),
		BigQueryDataset:    os.Getenv("BIGQUERY_DATASET"),
		BigQueryTable:      os.Getenv("BIGQUERY_TABLE"),
		GithubOrg:          os.Getenv("GITHUB_ORG"),
		GithubClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		GithubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		GithubCallbackURL:  os.Getenv("GITHUB_CALLBACK_URL"),
		GithubTokenSecret:  os.Getenv("GITHUB_TOKEN_SECRET"),
		DryRun:             os.Getenv("DRY_RUN") == "true",
		DigestInterval:     5 * time.Minute,
	}
}

// ValidateIngest checks the vars required by the ingest binary.
func (c Config) ValidateIngest() error {
	required := map[string]string{
		"GITHUB_ORG":           c.GithubOrg,
		"GITHUB_CLIENT_ID":     c.GithubClientID,
		"GITHUB_CLIENT_SECRET": c.GithubClientSecret,
		"GITHUB_CALLBACK_URL":  c.GithubCallbackURL,
	}

	// Dry-run holds tokens and cursor in memory and skips BigQuery.
	if !c.DryRun {
		required["GITHUB_TOKEN_SECRET"] = c.GithubTokenSecret
		required["BIGQUERY_PROJECT"] = c.BigQueryProject
		required["BIGQUERY_DATASET"] = c.BigQueryDataset
		required["BIGQUERY_TABLE"] = c.BigQueryTable
	}

	return missing(required)
}

// ValidateQuery checks the vars required by the query binary.
func (c Config) ValidateQuery() error {
	return missing(map[string]string{
		"GITHUB_ORG":           c.GithubOrg,
		"GITHUB_CLIENT_ID":     c.GithubClientID,
		"GITHUB_CLIENT_SECRET": c.GithubClientSecret,
		"GITHUB_CALLBACK_URL":  c.GithubCallbackURL,
		"BIGQUERY_PROJECT":     c.BigQueryProject,
		"BIGQUERY_DATASET":     c.BigQueryDataset,
		"BIGQUERY_TABLE":       c.BigQueryTable,
	})
}

func missing(required map[string]string) error {
	var absent []string
	for name, val := range required {
		if val == "" {
			absent = append(absent, name)
		}
	}
	if len(absent) > 0 {
		sort.Strings(absent)
		return fmt.Errorf("required env vars not set: %v", absent)
	}
	return nil
}
