package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const migrationLockID int64 = 0x4359434c4f505553

type MigrationConfig struct {
	URL         string
	LockTimeout time.Duration
	Migrations  fs.FS
}

func (c MigrationConfig) Validate() error {
	var errs []error
	if c.URL == "" {
		errs = append(errs, errors.New("database URL is required"))
	} else if _, err := pgx.ParseConfig(c.URL); err != nil {
		errs = append(errs, fmt.Errorf("database URL is invalid: %w", err))
	}
	if c.LockTimeout <= 0 {
		errs = append(errs, errors.New("migration lock timeout must be greater than zero"))
	}
	if c.Migrations == nil {
		errs = append(errs, errors.New("migration filesystem is required"))
	}
	return errors.Join(errs...)
}

func Migrate(ctx context.Context, cfg MigrationConfig) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate migration config: %w", err)
	}

	db, err := sql.Open("pgx", cfg.URL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	lockCtx, cancel := context.WithTimeout(ctx, cfg.LockTimeout)
	defer cancel()
	conn, err := db.Conn(lockCtx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	if err := acquireMigrationLock(lockCtx, conn); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockID)
	}()

	source, err := iofs.New(cfg.Migrations, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	driver, err := postgres.WithConnection(ctx, conn, &postgres.Config{})
	if err != nil {
		_ = source.Close()
		return fmt.Errorf("create postgres migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		_ = source.Close()
		_ = driver.Close()
		return fmt.Errorf("create migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func acquireMigrationLock(ctx context.Context, conn *sql.Conn) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", migrationLockID).Scan(&acquired); err != nil {
			return fmt.Errorf("acquire migration advisory lock: %w", err)
		}
		if acquired {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire migration advisory lock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
