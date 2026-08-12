package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestEmbeddedMigrations(t *testing.T) {
	entries, err := fs.ReadDir(Migrations, "migrations")
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 3 || entries[0].Name() != "000001_foundation.up.sql" || entries[1].Name() != "000002_user_profiles.up.sql" || entries[2].Name() != "000003_avatar_key_format.up.sql" {
		t.Fatalf("embedded migrations = %v, want baseline and user profile migrations", entries)
	}
	contents, err := fs.ReadFile(Migrations, "migrations/000001_foundation.up.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, want := range []string{
		"CREATE TABLE releases",
		"releases_one_live_per_environment",
		"CREATE TABLE orchestration_jobs",
		"CREATE TABLE environment_lifecycle_jobs",
		"environment_lifecycle_jobs_single_flight",
		"CREATE TABLE preview_lifecycle_requests",
		"policy_blocked",
		"CREATE TABLE api_idempotency_records",
		"environments_active_branch",
		"audit_events_append_only",
		"enforce_reconciliation_scope_single_flight",
		"desired_state_restored",
		"k8s-secret://",
		"UNIQUE (org_id, scope, idempotency_key)",
		"status text NOT NULL DEFAULT 'queued'",
		"response_body::text !~ 'cyp_v1_",
		"before::text !~ 'cyp_v1_",
	} {
		if !strings.Contains(string(contents), want) {
			t.Errorf("migration does not contain %q", want)
		}
	}
	profile, err := fs.ReadFile(Migrations, "migrations/000002_user_profiles.up.sql")
	if err != nil {
		t.Fatalf("ReadFile(user profile migration) error = %v", err)
	}
	for _, want := range []string{"ADD COLUMN first_name", "ADD COLUMN avatar_key", "DROP FUNCTION claim_first_user", "claimed_avatar_key"} {
		if !strings.Contains(string(profile), want) {
			t.Errorf("user profile migration does not contain %q", want)
		}
	}
	avatarFormat, err := fs.ReadFile(Migrations, "migrations/000003_avatar_key_format.up.sql")
	if err != nil {
		t.Fatalf("ReadFile(avatar format migration) error = %v", err)
	}
	if !strings.Contains(string(avatarFormat), "avatar_key ~ '^[a-f0-9]{10}$'") {
		t.Error("avatar format migration must not pin the database to the current asset manifest")
	}
}

func TestGreenfieldBaselineFreezeContract(t *testing.T) {
	marker, err := os.ReadFile("migrations/baseline_frozen")
	if err != nil {
		t.Fatalf("ReadFile(baseline_frozen) error = %v", err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(marker)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("invalid baseline marker line %q", line)
		}
		values[key] = value
	}
	if values["baseline_frozen"] != "true" && values["baseline_frozen"] != "false" {
		t.Fatalf("baseline_frozen = %q, want true or false", values["baseline_frozen"])
	}
	if values["baseline_frozen"] == "false" {
		return
	}

	contents, err := os.ReadFile("migrations/000001_foundation.up.sql")
	if err != nil {
		t.Fatalf("ReadFile(foundation migration) error = %v", err)
	}
	digest := sha256.Sum256(contents)
	want, err := hex.DecodeString(values["baseline_sha256"])
	if err != nil || len(want) != sha256.Size {
		t.Fatalf("baseline_sha256 must be a SHA-256 digest when the baseline is frozen")
	}
	if !strings.EqualFold(hex.EncodeToString(digest[:]), hex.EncodeToString(want)) {
		t.Fatalf("000001_foundation.up.sql changed after baseline_frozen=true")
	}
}

func TestMigrationConfigValidation(t *testing.T) {
	err := (MigrationConfig{}).Validate()
	if err == nil {
		t.Fatal("Validate() error = nil")
	}
	for _, want := range []string{"database URL", "lock timeout", "filesystem"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	err = (MigrationConfig{
		URL:         "://bad",
		LockTimeout: time.Second,
		Migrations:  fstest.MapFS{},
	}).Validate()
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("Validate() error = %v, want invalid URL", err)
	}
}

func TestMigrateWrapsConfigErrors(t *testing.T) {
	err := Migrate(context.Background(), MigrationConfig{})
	if err == nil || !strings.Contains(err.Error(), "validate migration config") {
		t.Fatalf("Migrate() error = %v", err)
	}
}
