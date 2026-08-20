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
		"000015_oci_ghcr_private_pulls.down.sql",
		"000015_oci_ghcr_private_pulls.up.sql",
		"000016_private_resolution_model.down.sql",
		"000016_private_resolution_model.up.sql",
		"000017_registry_validation_references.down.sql",
		"000017_registry_validation_references.up.sql",
		"000018_import_pending_credentials.down.sql",
		"000018_import_pending_credentials.up.sql",
		"000019_registry_credential_fingerprint.down.sql",
		"000019_registry_credential_fingerprint.up.sql",
		"000020_namespace_credential_transitions.down.sql",
		"000020_namespace_credential_transitions.up.sql",
		"000021_release_generation_fencing.down.sql",
		"000021_release_generation_fencing.up.sql",
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

func TestMigration000016SchemaContract(t *testing.T) {
	for name, want := range map[string]string{
		"000016_private_resolution_model.up.sql":   "e34752a02fde1914bace629f0f43a1eccdc4d9fcfff6abb179cb91ac9d5ceec5",
		"000016_private_resolution_model.down.sql": "213df34309d98832ec0f04afd9e490837ef8fc8b76b8071bc50d230f5d3fa72b",
	} {
		contents, err := fs.ReadFile(Migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"apps_registry_source_consistency", "orchestration_oci_valid", "assert_private_resolution_v16_down_safe", "enforce_orchestration_target_and_artifact"} {
			if !strings.Contains(string(contents), required) {
				t.Errorf("%s does not contain %q", name, required)
			}
		}
		sum := sha256.Sum256(contents)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s checksum = %s, want v16 migration fingerprint %s", name, got, want)
		}
	}
}

func TestMigration000017SchemaContract(t *testing.T) {
	for name, want := range map[string]string{
		"000017_registry_validation_references.up.sql":   "c6f8c18bf32e47a4242d756e691edbb9f0ec7e5807d1b88a811271ec508db178",
		"000017_registry_validation_references.down.sql": "72f4360af166b3a88be726dde4390a292381e984c166c83df45836415961d15c",
	} {
		contents, err := fs.ReadFile(Migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"awaiting_source", "reserved", "target_reference", "registry_credential_validation_targets_leased"} {
			if !strings.Contains(string(contents), required) {
				t.Errorf("%s does not contain %q", name, required)
			}
		}
		sum := sha256.Sum256(contents)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s checksum = %s, want v17 migration fingerprint %s", name, got, want)
		}
	}
}

func TestMigration000018SchemaContract(t *testing.T) {
	for name, want := range map[string]string{
		"000018_import_pending_credentials.up.sql":   "3866dace051ff15aeae0b2bdf3baee11580e361fe9e82f6400bcc1f5d345eefd",
		"000018_import_pending_credentials.down.sql": "ab34de7f99c2cac3afa8e2934e89d128e4d7494d1779d8d2a5b5baaeb4340b93",
	} {
		contents, err := fs.ReadFile(Migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"awaiting_import", "activated", "registry_credential_operations_awaiting_import", "assert_registry_generation_consistency"} {
			if !strings.Contains(string(contents), required) {
				t.Errorf("%s does not contain %q", name, required)
			}
		}
		sum := sha256.Sum256(contents)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s checksum = %s, want v18 migration fingerprint %s", name, got, want)
		}
	}
}

func TestMigration000019SchemaContract(t *testing.T) {
	for name, want := range map[string]string{
		"000019_registry_credential_fingerprint.up.sql":   "fe7e6848e66b392dd805b47898b2fbd6b5e8b7f1969d1ba478c90dea7f3b3b70",
		"000019_registry_credential_fingerprint.down.sql": "c92de23bff590788092c9a537319d28c223844639c33c9b581a60ace98ac0144",
	} {
		contents, err := fs.ReadFile(Migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"credential_fingerprint", "actor_user_id", "registry_credential_operation_attribution"} {
			if !strings.Contains(string(contents), required) {
				t.Errorf("%s does not contain %q", name, required)
			}
		}
		if strings.HasSuffix(name, ".up.sql") && !strings.Contains(string(contents), "FOREIGN KEY (org_id,actor_user_id) REFERENCES users(org_id,id)") {
			t.Errorf("%s does not preserve composite tenant actor attribution", name)
		}
		sum := sha256.Sum256(contents)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s checksum = %s, want v19 migration fingerprint %s", name, got, want)
		}
	}
}

func TestMigration000020SchemaContract(t *testing.T) {
	for name, want := range map[string]string{
		"000020_namespace_credential_transitions.up.sql":   "83cd794406ea14a1c291a999c600ab333c041691a0307727ab17c73175de0e2c",
		"000020_namespace_credential_transitions.down.sql": "7b70ed1d11aff52f81679efece75ef4ea24273e40adaaf199bad36bc6a6e2e9d",
	} {
		contents, err := fs.ReadFile(Migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"transition_app_image_visibility", "registry_credential_cleanup_state", "environments_registry_credential_cleanup_due", "environment_credential_row_valid"} {
			if !strings.Contains(string(contents), required) {
				t.Errorf("%s does not contain %q", name, required)
			}
		}
		sum := sha256.Sum256(contents)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s checksum = %s, want v20 migration fingerprint %s", name, got, want)
		}
	}
}

func TestMigration000021SchemaContract(t *testing.T) {
	for name, want := range map[string]string{
		"000021_release_generation_fencing.up.sql":   "0821d0022879a6c79499fc1557b624d4c3cfd02f490038986c00c04c55829155",
		"000021_release_generation_fencing.down.sql": "e28f4d1748741e68c78b802d08a5a118f2d67b9017b55b60b80697135feeaeaa",
	} {
		contents, err := fs.ReadFile(Migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"desired_generation", "orchestration_job_id", "assert_release_generation_v21_down_safe"} {
			if !strings.Contains(string(contents), required) {
				t.Errorf("%s does not contain %q", name, required)
			}
		}
		if strings.HasSuffix(name, ".up.sql") {
			for _, required := range []string{"legacy_unverified", "SET search_path FROM CURRENT", "BEFORE TRUNCATE ON schema_migrations"} {
				if !strings.Contains(string(contents), required) {
					t.Errorf("%s does not contain %q", name, required)
				}
			}
		}
		sum := sha256.Sum256(contents)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s checksum = %s, want v21 migration fingerprint %s", name, got, want)
		}
	}
}

func TestMigration000015SchemaContract(t *testing.T) {
	up, err := fs.ReadFile(Migrations, "migrations/000015_oci_ghcr_private_pulls.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(Migrations, "migrations/000015_oci_ghcr_private_pulls.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"CREATE TABLE registry_credential_generations",
		"CREATE TABLE registry_credential_operations",
		"CREATE TABLE registry_credential_validation_targets",
		"registry_credential_operations_active",
		"orchestration_jobs_oci_due",
		"orchestration_jobs_oci_leased",
		"environments_registry_credential_due",
		"attempts BETWEEN 0 AND 12",
		"oci_attempts BETWEEN 0 AND 20",
		"CREATE FUNCTION registry_row_valid",
		"CREATE FUNCTION enforce_registry_checkpoint",
		"CREATE FUNCTION assert_registry_generation_consistency",
		"CREATE FUNCTION environment_credential_row_valid",
		"CREATE FUNCTION enforce_environment_credential",
		"CREATE FUNCTION assert_dependent_environments",
		"CREATE FUNCTION orchestration_oci_valid",
		"CREATE FUNCTION enforce_orchestration_registry_evidence",
		"CREATE FUNCTION enforce_release_registry_evidence",
		"CREATE TRIGGER registries_checkpoint_valid",
		"CREATE TRIGGER environments_credential_valid",
		"CREATE TRIGGER apps_environments_valid",
		"CREATE TRIGGER registries_environments_valid",
		"CREATE CONSTRAINT TRIGGER registry_generations_environments_valid",
		"CREATE TRIGGER orchestration_jobs_registry_evidence",
		"CREATE TRIGGER releases_registry_evidence",
		"registry_credential_generations_one_active",
		"resolved_registry_id uuid",
		"oci_annotation_status IN ('pending','matched','absent','mismatched','legacy')",
		"assert_oci_ghcr_v15_down_safe",
		"BEFORE TRUNCATE ON schema_migrations",
	} {
		if !strings.Contains(string(up), want) {
			t.Errorf("000015 up does not contain %q", want)
		}
	}
	for _, forbidden := range []string{
		"phase6_enabled",
		"enable_phase6_schema_epoch",
		"schema_epoch_phase6_enable_guard",
		"phase6_registry_row_valid",
		"phase6_environment_credential_row_valid",
		"phase6_orchestration_valid",
		"enforce_phase6_",
		"assert_phase6_",
		"_phase6_",
	} {
		if strings.Contains(string(up), forbidden) {
			t.Errorf("000015 up contains phase6-prefixed object %q", forbidden)
		}
	}
	for _, forbidden := range []string{"dockerconfig", "personal_access_token", "bearer_token", "raw_body bytea"} {
		if strings.Contains(strings.ToLower(string(up)), forbidden) {
			t.Errorf("000015 persists forbidden secret material %q", forbidden)
		}
	}
	if !strings.HasPrefix(string(down), "LOCK TABLE schema_epoch, registries") || !strings.Contains(string(down), "SELECT assert_oci_ghcr_v15_down_safe();") {
		t.Fatal("000015 down must lock in deterministic order and assert safety before DDL")
	}
	for name, want := range map[string]string{
		"000015_oci_ghcr_private_pulls.up.sql":   "48846958668a922da92bc8f3b2cfeed4de36b5a9289b5019ee99a55e9ba74356",
		"000015_oci_ghcr_private_pulls.down.sql": "839e0a980d3a84210539f03fa64c86de3e0e71e4f23782ae2bdbf19e7a3945f4",
	} {
		contents, err := fs.ReadFile(Migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(contents)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s checksum = %s, want v15 migration fingerprint %s", name, got, want)
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
