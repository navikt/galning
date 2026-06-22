package config

import (
	"fmt"
	"os"
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

	DryRun bool
}

func FromEnv() (Config, error) {
	cfg := Config{
		BigQueryProject:    os.Getenv("BIGQUERY_PROJECT"),
		BigQueryDataset:    os.Getenv("BIGQUERY_DATASET"),
		BigQueryTable:      os.Getenv("BIGQUERY_TABLE"),
		GithubOrg:          os.Getenv("GITHUB_ORG"),
		GithubClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		GithubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		GithubCallbackURL:  os.Getenv("GITHUB_CALLBACK_URL"),
		GithubTokenSecret:  os.Getenv("GITHUB_TOKEN_SECRET"),
		DryRun:             os.Getenv("DRY_RUN") == "true",
	}

	required := map[string]string{
		"GITHUB_ORG":           cfg.GithubOrg,
		"GITHUB_CLIENT_ID":     cfg.GithubClientID,
		"GITHUB_CLIENT_SECRET": cfg.GithubClientSecret,
		"GITHUB_CALLBACK_URL":  cfg.GithubCallbackURL,
	}
	if !cfg.DryRun {
		required["GITHUB_TOKEN_SECRET"] = cfg.GithubTokenSecret
		required["BIGQUERY_PROJECT"] = cfg.BigQueryProject
		required["BIGQUERY_DATASET"] = cfg.BigQueryDataset
		required["BIGQUERY_TABLE"] = cfg.BigQueryTable
	}

	var missing []string
	for name, val := range required {
		if val == "" {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return Config{}, fmt.Errorf("required env vars not set: %v", missing)
	}

	return cfg, nil
}
