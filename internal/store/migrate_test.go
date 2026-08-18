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
	want := []string{
		"000001_foundation.up.sql",
		"000002_user_profiles.up.sql",
		"000003_avatar_key_format.up.sql",
		"000004_self_hosted_github_import_epoch.down.sql",
		"000004_self_hosted_github_import_epoch.up.sql",
		"000005_github_setup_state_generation.down.sql",
		"000005_github_setup_state_generation.up.sql",
		"000006_github_installation_audit_id.down.sql",
		"000006_github_installation_audit_id.up.sql",
		"000007_import_workflow_revision.down.sql",
		"000007_import_workflow_revision.up.sql",
		"000008_github_repository_cursor_generation.down.sql",
		"000008_github_repository_cursor_generation.up.sql",
		"000009_registry_canonical_identity.down.sql",
		"000009_registry_canonical_identity.up.sql",
		"000010_github_webhook_routing.down.sql",
		"000010_github_webhook_routing.up.sql",
		"000011_github_unverified_revocation.down.sql",
		"000011_github_unverified_revocation.up.sql",
		"000012_github_webhook_signature_key_version.down.sql",
		"000012_github_webhook_signature_key_version.up.sql",
		"000013_github_webhook_conflict_error.down.sql",
		"000013_github_webhook_conflict_error.up.sql",
		"000014_github_routing_fanout.down.sql",
		"000014_github_routing_fanout.up.sql",
	}
	if len(entries) != len(want) {
		t.Fatalf("embedded migrations = %v, want exactly %v", entries, want)
	}
	for index, name := range want {
		if entries[index].Name() != name {
			t.Fatalf("embedded migrations = %v, want exactly %v", entries, want)
		}
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
	stateGeneration, err := fs.ReadFile(Migrations, "migrations/000005_github_setup_state_generation.up.sql")
	if err != nil {
		t.Fatalf("ReadFile(state generation migration) error = %v", err)
	}
	for _, want := range []string{"ADD COLUMN state_generation", "status = 'expired'", "github_setup_protocol_incompatible", "status <> 'active' OR state_generation IS NOT NULL"} {
		if !strings.Contains(string(stateGeneration), want) {
			t.Errorf("state generation migration does not contain %q", want)
		}
	}
}

func TestMigration000010CommittedCheckpoint(t *testing.T) {
	for name, want := range map[string]string{
		"000010_github_webhook_routing.up.sql":   "68341a5325e7d1b34a985e5193dc309184593bf935db0ad9c16271e8e4711415",
		"000010_github_webhook_routing.down.sql": "86de886fd2376f9f1db4dec3c46aea4f3398e4ad59116a4e160daaa4756804bd",
	} {
		contents, err := fs.ReadFile(Migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(contents)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s checksum = %s, want committed efef5cb checkpoint %s", name, got, want)
		}
	}
	fixture, err := os.ReadFile("testdata/000010_github_webhook_routing.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(fixture)
	if got, want := hex.EncodeToString(sum[:]), "68341a5325e7d1b34a985e5193dc309184593bf935db0ad9c16271e8e4711415"; got != want {
		t.Fatalf("committed 000010 upgrade fixture checksum = %s, want efef5cb checkpoint %s", got, want)
	}
}

func TestMigrationForwardDownAndEpochContract(t *testing.T) {
	for _, name := range []string{
		"migrations/000004_self_hosted_github_import_epoch.up.sql",
		"migrations/000004_self_hosted_github_import_epoch.down.sql",
		"migrations/000005_github_setup_state_generation.up.sql",
		"migrations/000005_github_setup_state_generation.down.sql",
		"migrations/000008_github_repository_cursor_generation.up.sql",
		"migrations/000008_github_repository_cursor_generation.down.sql",
		"migrations/000009_registry_canonical_identity.up.sql",
		"migrations/000009_registry_canonical_identity.down.sql",
	} {
		contents, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", name, err)
		}
		if len(strings.TrimSpace(string(contents))) == 0 {
			t.Fatalf("migration %s is empty", name)
		}
	}
	epoch, err := os.ReadFile("migrations/000004_self_hosted_github_import_epoch.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(epoch), SchemaEpoch) || !strings.Contains(string(epoch), SchemaFingerprint) {
		t.Fatalf("migration 000004 does not establish schema epoch %q and fingerprint %q", SchemaEpoch, SchemaFingerprint)
	}
	stateGeneration, err := os.ReadFile("migrations/000005_github_setup_state_generation.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateGeneration), "schema_epoch") {
		t.Fatal("migration 000005 unexpectedly changes the schema epoch")
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
