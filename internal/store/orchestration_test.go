package store

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMapEnqueueOrchestrationError(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		message string
		want    error
	}{
		{name: "inactive scope", code: "23514", message: "orchestration environment scope is inactive", want: ErrFailedPrecondition},
		{name: "stale access", code: "23514", message: "orchestration repository access is inactive or stale", want: ErrFailedPrecondition},
		{name: "idempotency mismatch", code: "23505", message: "orchestration idempotency key was used for a different request", want: ErrConflict},
		{name: "malformed request", code: "22023", message: "invalid workflow path", want: ErrInvalidArgument},
		{name: "function malformed request", code: "23514", message: "invalid orchestration enqueue request", want: ErrInvalidArgument},
		{name: "generation overflow", code: "23514", message: "orchestration desired generation cannot exceed bigint maximum", want: ErrFailedPrecondition},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := mapEnqueueOrchestrationError(&pgconn.PgError{Code: test.code, Message: test.message})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestMapEnqueueOrchestrationErrorPreservesRetryableFailures(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		pgErr := &pgconn.PgError{Code: code, Message: "retry transaction"}
		err := mapEnqueueOrchestrationError(pgErr)
		if !errors.Is(err, pgErr) {
			t.Fatalf("SQLSTATE %s error = %v, want native pg error", code, err)
		}
		if errors.Is(err, ErrFailedPrecondition) || errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("SQLSTATE %s became permanent: %v", code, err)
		}
	}
}
