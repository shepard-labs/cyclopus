package auth

import (
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestAuthSearchPathURL(t *testing.T) {
	databaseURL := authSearchPathURL(t, "postgres://user:password@localhost:5432/cyclopus?sslmode=disable", "auth_test_123")
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.RuntimeParams["search_path"], `"auth_test_123"`; got != want {
		t.Fatalf("search_path = %q, want %q", got, want)
	}
	if got, want := config.RuntimeParams["sslmode"], ""; got != want {
		t.Fatalf("unexpected sslmode runtime parameter = %q, want %q", got, want)
	}
	if config.TLSConfig != nil {
		t.Fatal("sslmode=disable was not preserved")
	}
}

func authSearchPathURL(t *testing.T, databaseURL, schema string) string {
	t.Helper()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = (pgx.Identifier{schema}).Sanitize()
	parsed, err := url.Parse(config.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for key, value := range config.RuntimeParams {
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
