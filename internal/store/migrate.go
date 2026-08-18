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
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const migrationLockID int64 = 0x4359434c4f505553

const (
	SchemaEpoch       = "self_hosted_github_import_v1"
	SchemaFingerprint = "95e56712029f59eb49b6b0bd939cfbb2dc62360772731fabec7bf2cb2d7d290e"
)

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

	prepared11, err := prepareMigration11Upgrade(ctx, conn)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		if prepared11 {
			restoreMigration10Guard(context.WithoutCancel(ctx), conn)
		}
		return fmt.Errorf("apply migrations: database is old, missing, or partially applied; back up the database and apply all migrations through 000005 before starting Cyclopus: %w", err)
	}
	return ValidateSchemaEpoch(ctx, conn)
}

func prepareMigration11Upgrade(ctx context.Context, conn *sql.Conn) (bool, error) {
	var version int
	var dirty bool
	err := conn.QueryRowContext(ctx, `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty)
	if errors.Is(err, sql.ErrNoRows) || isUndefinedTable(err) || version != 10 || dirty {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect migration 000011 upgrade checkpoint: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS guard_github_webhook_v10_down ON schema_migrations`); err != nil {
		return false, fmt.Errorf("prepare migration 000011 upgrade: remove 000010 schema_migrations guard: %w", err)
	}
	return true, nil
}

func restoreMigration10Guard(ctx context.Context, conn *sql.Conn) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	var version int
	var dirty bool
	if err := tx.QueryRowContext(ctx, `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 10 && !(version == 11 && dirty) {
		return
	}
	var partialV11 bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND
		((table_name='github_repository_routing_state' OR table_name='github_routing_intents') AND column_name='installation_id'
		 OR table_name='github_webhook_deliveries' AND column_name='ingestion_sequence'))`).Scan(&partialV11); err != nil || partialV11 {
		return
	}
	if _, err := tx.ExecContext(ctx, `CREATE OR REPLACE FUNCTION guard_github_webhook_v10_down() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF EXISTS(SELECT 1 FROM github_webhook_deliveries WHERE status IN('pending','leased') OR verified_payload IS NOT NULL) OR EXISTS(SELECT 1 FROM github_routing_intents WHERE status IN('pending','leased')) THEN RAISE EXCEPTION '000010 rollback blocked: drain webhook deliveries/intents and redact retained payloads before retrying' USING ERRCODE='check_violation'; END IF; RETURN NULL; END $$;
DROP TRIGGER IF EXISTS guard_github_webhook_v10_down ON schema_migrations;
CREATE TRIGGER guard_github_webhook_v10_down BEFORE TRUNCATE ON schema_migrations FOR EACH STATEMENT EXECUTE FUNCTION guard_github_webhook_v10_down();`); err != nil {
		return
	}
	if version == 11 {
		if _, err := tx.ExecContext(ctx, `UPDATE schema_migrations SET version=10,dirty=false WHERE version=11 AND dirty=true`); err != nil {
			return
		}
	}
	_ = tx.Commit()
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

type schemaEpochQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ValidateSchemaEpoch prevents a binary built for the replacement persistence
// model from starting against an old or partially-applied alpha schema.
func ValidateSchemaEpoch(ctx context.Context, db schemaEpochQuerier) error {
	var epoch, fingerprint string
	err := db.QueryRowContext(ctx, `SELECT epoch, fingerprint FROM schema_epoch WHERE singleton = true`).Scan(&epoch, &fingerprint)
	if err != nil {
		return fmt.Errorf("validate schema epoch: database is missing or only partially applied migration 000004; back up the database and apply all migrations before starting Cyclopus: %w", err)
	}
	if epoch != SchemaEpoch || fingerprint != SchemaFingerprint {
		return fmt.Errorf("validate schema epoch: database epoch %q does not match required epoch %q; back up the database and apply migration 000004 with this binary", epoch, SchemaEpoch)
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
