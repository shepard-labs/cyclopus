package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrNotFound           = errors.New("store: not found")
	ErrConflict           = errors.New("store: conflict")
	ErrAlreadyExists      = errors.New("store: already exists")
	ErrFailedPrecondition = errors.New("store: failed precondition")
	// ErrPreviewCapReached is returned when a preview push is rejected because
	// the app is already at its max_preview_environments cap (§4.3). It wraps
	// ErrFailedPrecondition so existing precondition handling is unaffected,
	// while letting callers detect the cap case specifically for §8.2 logging.
	ErrPreviewCapReached = fmt.Errorf("%w: preview environment cap reached", ErrFailedPrecondition)
	ErrLeaseNotOwned     = errors.New("store: lease not owned")
	ErrIdempotencyActive = errors.New("store: idempotency key is active")
	ErrInvalidArgument   = errors.New("store: invalid argument")
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
	db                  DB
	webhookPayloadCodec GitHubWebhookPayloadCodec
}

func New(db DB) *Store {
	return &Store{db: db}
}

func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return s.withTxOptions(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
}

func (s *Store) withTxOptions(ctx context.Context, options pgx.TxOptions, fn func(pgx.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, options)
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

const githubEventTxAttempts = 3

func (s *Store) withGitHubEventTxRetry(ctx context.Context, options pgx.TxOptions, fn func(pgx.Tx) error) error {
	for attempt := 0; attempt < githubEventTxAttempts; attempt++ {
		tx, err := s.db.BeginTx(ctx, options)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		err = fn(tx)
		if err == nil {
			err = tx.Commit(ctx)
			if err != nil {
				err = fmt.Errorf("commit transaction: %w", err)
			}
		}
		if err == nil {
			return nil
		}
		_ = tx.Rollback(ctx)
		if !isGitHubEventTxRetryable(err) || attempt == githubEventTxAttempts-1 {
			return err
		}
		delay := time.Duration(attempt+1)*time.Millisecond + time.Duration(rand.IntN(3))*time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	panic("unreachable")
}

func isGitHubEventTxRetryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
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
