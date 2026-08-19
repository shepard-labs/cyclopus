package store

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestNewDefaultsToPublicSchema(t *testing.T) {
	dataStore := New(nil)
	if dataStore.schema != "public" {
		t.Fatalf("schema = %q, want public", dataStore.schema)
	}
}

func TestNewWithSchemaValidation(t *testing.T) {
	valid := []string{"a", "_private", "tenant_123", strings.Repeat("a", 63)}
	for _, schema := range valid {
		dataStore, err := NewWithSchema(nil, schema)
		if err != nil || dataStore.schema != schema {
			t.Fatalf("NewWithSchema(%q) = %+v, %v", schema, dataStore, err)
		}
	}
	invalid := []string{"", "1tenant", "Tenant", "tenant-name", "tenant.name", "pg_temp", "pg_", "ténant", strings.Repeat("a", 64)}
	for _, schema := range invalid {
		if _, err := NewWithSchema(nil, schema); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("NewWithSchema(%q) error = %v, want ErrInvalidArgument", schema, err)
		}
	}
}

type enableSchemaDB struct {
	DB
	query string
}

func (d *enableSchemaDB) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	d.query = query
	return pgconn.CommandTag{}, nil
}

func TestEnablePhase6SchemaEpochUsesTrustedSchema(t *testing.T) {
	tests := []struct {
		name  string
		store func(*enableSchemaDB) *Store
		query string
	}{
		{name: "default", store: func(db *enableSchemaDB) *Store { return New(db) }, query: `SELECT "public"."enable_phase6_schema_epoch"()`},
		{name: "configured", store: func(db *enableSchemaDB) *Store {
			dataStore, err := NewWithSchema(db, "target_schema")
			if err != nil {
				t.Fatal(err)
			}
			return dataStore
		}, query: `SELECT "target_schema"."enable_phase6_schema_epoch"()`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := &enableSchemaDB{}
			if err := test.store(db).EnablePhase6SchemaEpoch(t.Context()); err != nil {
				t.Fatal(err)
			}
			if db.query != test.query {
				t.Fatalf("query = %q, want %q", db.query, test.query)
			}
		})
	}
}

type githubEventRetryDB struct {
	DB
	commits []error
	begins  atomic.Int32
}

func (d *githubEventRetryDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	index := int(d.begins.Add(1)) - 1
	var commitErr error
	if index < len(d.commits) {
		commitErr = d.commits[index]
	}
	return &githubEventRetryTx{commitErr: commitErr}, nil
}

type githubEventRetryTx struct {
	pgx.Tx
	commitErr error
}

func (t *githubEventRetryTx) Commit(context.Context) error { return t.commitErr }
func (*githubEventRetryTx) Rollback(context.Context) error { return nil }

func TestWithGitHubEventTxRetryFunctionFailureThenSuccess(t *testing.T) {
	db := &githubEventRetryDB{}
	dataStore := New(db)
	var calls atomic.Int32
	err := dataStore.withGitHubEventTxRetry(t.Context(), pgx.TxOptions{}, func(pgx.Tx) error {
		if calls.Add(1) == 1 {
			return &pgconn.PgError{Code: "40001", Message: "retry"}
		}
		return nil
	})
	if err != nil || calls.Load() != 2 || db.begins.Load() != 2 {
		t.Fatalf("retry result = %v, calls=%d begins=%d", err, calls.Load(), db.begins.Load())
	}
}

func TestWithGitHubEventTxRetryCommitFailureThenSuccess(t *testing.T) {
	db := &githubEventRetryDB{commits: []error{&pgconn.PgError{Code: "40P01", Message: "retry"}, nil}}
	dataStore := New(db)
	var calls atomic.Int32
	err := dataStore.withGitHubEventTxRetry(t.Context(), pgx.TxOptions{}, func(pgx.Tx) error {
		calls.Add(1)
		return nil
	})
	if err != nil || calls.Load() != 2 || db.begins.Load() != 2 {
		t.Fatalf("commit retry result = %v, calls=%d begins=%d", err, calls.Load(), db.begins.Load())
	}
}

func TestWithGitHubEventTxRetryExhaustionReturnsPostgresError(t *testing.T) {
	db := &githubEventRetryDB{}
	dataStore := New(db)
	err := dataStore.withGitHubEventTxRetry(t.Context(), pgx.TxOptions{}, func(pgx.Tx) error {
		return &pgconn.PgError{Code: "40001", Message: "retry"}
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40001" || db.begins.Load() != githubEventTxAttempts {
		t.Fatalf("exhaustion error = %v, begins=%d", err, db.begins.Load())
	}
}

func TestWithGitHubEventTxRetryDoesNotRetryDomainError(t *testing.T) {
	db := &githubEventRetryDB{}
	dataStore := New(db)
	err := dataStore.withGitHubEventTxRetry(t.Context(), pgx.TxOptions{}, func(pgx.Tx) error { return ErrLeaseNotOwned })
	if !errors.Is(err, ErrLeaseNotOwned) || db.begins.Load() != 1 {
		t.Fatalf("domain error = %v, begins=%d", err, db.begins.Load())
	}
}

func TestWithGitHubEventTxRetryContextCancellationStopsBackoff(t *testing.T) {
	db := &githubEventRetryDB{}
	dataStore := New(db)
	ctx, cancel := context.WithCancel(t.Context())
	err := dataStore.withGitHubEventTxRetry(ctx, pgx.TxOptions{}, func(pgx.Tx) error {
		cancel()
		return &pgconn.PgError{Code: "40P01", Message: "retry"}
	})
	if !errors.Is(err, context.Canceled) || db.begins.Load() != 1 {
		t.Fatalf("cancellation error = %v, begins=%d", err, db.begins.Load())
	}
}
