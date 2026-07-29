package store

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestWithSearchPath(t *testing.T) {
	databaseURL := withSearchPath("postgres://user:password@localhost:5432/cyclopus?sslmode=disable", "store_test_123")
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.RuntimeParams["search_path"], `"store_test_123"`; got != want {
		t.Fatalf("search_path = %q, want %q", got, want)
	}
	if got, want := config.RuntimeParams["sslmode"], ""; got != want {
		t.Fatalf("unexpected sslmode runtime parameter = %q, want %q", got, want)
	}
	if config.TLSConfig != nil {
		t.Fatal("sslmode=disable was not preserved")
	}
}

func withSearchPath(databaseURL, schema string) string {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		panic(fmt.Sprintf("parse PostgreSQL test URL: %v", err))
	}
	config.RuntimeParams["search_path"] = (pgx.Identifier{schema}).Sanitize()
	return connectionString(config)
}

func connectionString(config *pgx.ConnConfig) string {
	parsed, err := url.Parse(config.ConnString())
	if err != nil {
		panic(fmt.Sprintf("parse normalized PostgreSQL test URL: %v", err))
	}
	query := parsed.Query()
	for key, value := range config.RuntimeParams {
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
