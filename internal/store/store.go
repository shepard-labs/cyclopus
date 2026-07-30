package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrNotFound           = errors.New("store: not found")
	ErrConflict           = errors.New("store: conflict")
	ErrAlreadyExists      = errors.New("store: already exists")
	ErrFailedPrecondition = errors.New("store: failed precondition")
	ErrLeaseNotOwned      = errors.New("store: lease not owned")
	ErrIdempotencyActive  = errors.New("store: idempotency key is active")
	ErrInvalidArgument    = errors.New("store: invalid argument")
)

func requirePositiveDuration(name string, value time.Duration) error {
	if value <= 0 {
		return fmt.Errorf("%w: %s must be greater than zero", ErrInvalidArgument, name)
	}
	return nil
}

type DB interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Store struct {
	db DB
}

func New(db DB) *Store {
	return &Store{db: db}
}

func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func rowsAffected(commandTag pgconnCommandTag, notFound error) error {
	if commandTag.RowsAffected() == 0 {
		return notFound
	}
	return nil
}

type pgconnCommandTag interface {
	RowsAffected() int64
}
