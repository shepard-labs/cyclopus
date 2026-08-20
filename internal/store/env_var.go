package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shepard-labs/cyclopus/internal/secretref"
)

type MutateEnvVarParams struct {
	OrgID             string
	EnvironmentID     string
	EnvVarID          string
	Key               string
	SecretRef         string
	ReleaseID         string
	TriggeredByUserID *string
	Idempotency       CompleteIdempotencyParams
	Audit             *AuditEvent
}

type EnvironmentVariableValue struct {
	Key       string
	SecretRef string
	IsSecret  bool
}

type EnvironmentVariableRevision struct {
	ID, OrgID, EnvironmentID, CreatedByUserID string
	Revision                                  int64
	Variables                                 []EnvironmentVariableValue
	CreatedAt                                 time.Time
}

type MutateEnvVarResult struct {
	Lease          DeployLease
	ConfigSnapshot json.RawMessage
	ConfigHash     string
}

func (s *Store) MutateEnvVarAndCreateRelease(ctx context.Context, params MutateEnvVarParams) (MutateEnvVarResult, error) {
	return s.ReplaceEnvVarsAndCreateRelease(ctx, ReplaceEnvVarsParams{OrgID: params.OrgID, EnvironmentID: params.EnvironmentID, Variables: []EnvironmentVariableValue{{Key: params.Key, SecretRef: params.SecretRef, IsSecret: true}}, ReleaseID: params.ReleaseID, TriggeredByUserID: params.TriggeredByUserID, Idempotency: params.Idempotency, Audit: params.Audit, Merge: true})
}

type ReplaceEnvVarsParams struct {
	OrgID, EnvironmentID, ReleaseID string
	Variables                       []EnvironmentVariableValue
	TriggeredByUserID               *string
	Idempotency                     CompleteIdempotencyParams
	Audit                           *AuditEvent
	Merge                           bool
}

func (s *Store) ReplaceEnvVarsAndCreateRelease(ctx context.Context, params ReplaceEnvVarsParams) (MutateEnvVarResult, error) {
	var result MutateEnvVarResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var imageRef, namespace string
		var gitCommitSHA *string
		var liveSnapshot json.RawMessage
		var registryID *string
		var registryGeneration *int64
		var visibility string
		var desiredGeneration int64
		if err := tx.QueryRow(ctx, `
			SELECT release.image_ref, release.git_commit_sha, release.config_snapshot, environment.namespace,
			  app.registry_id::text,environment.ready_registry_credential_generation,environment.desired_generation,app.image_visibility
			FROM environments AS environment
			JOIN apps AS app ON app.org_id = environment.org_id AND app.id = environment.app_id
			JOIN projects AS project ON project.org_id = app.org_id AND project.id = app.project_id
			JOIN orgs AS org ON org.id = environment.org_id
			JOIN releases AS release ON release.org_id = environment.org_id
			  AND release.environment_id = environment.id AND release.status = 'live'
			WHERE environment.org_id = $1 AND environment.id = $2
			  AND environment.lifecycle_status='ready' AND environment.torn_down_at IS NULL AND environment.in_progress_release_id IS NULL
			  AND app.desired_generation=environment.desired_generation
			  AND ((app.image_visibility='public' AND app.registry_id IS NULL
			    AND environment.registry_credential_state='not_required'
			    AND environment.required_registry_credential_generation IS NULL AND environment.ready_registry_credential_generation IS NULL)
			   OR (app.image_visibility='private' AND app.registry_id IS NOT NULL
			    AND environment.registry_credential_state='ready'
			    AND environment.required_registry_credential_generation=environment.ready_registry_credential_generation
			    AND EXISTS(SELECT 1 FROM registries registry JOIN registry_credential_generations generation
			      ON generation.org_id=registry.org_id AND generation.registry_id=registry.id
			      AND generation.generation=registry.active_generation AND generation.state='active'
			      WHERE registry.org_id=app.org_id AND registry.id=app.registry_id
			      AND registry.state='ready' AND registry.active_generation=environment.ready_registry_credential_generation)))
			  AND app.deleted_at IS NULL AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			FOR UPDATE OF environment`, params.OrgID, params.EnvironmentID).Scan(&imageRef, &gitCommitSHA, &liveSnapshot,
			&namespace, &registryID, &registryGeneration, &desiredGeneration, &visibility); err == pgx.ErrNoRows {
			return ErrConflict
		} else if err != nil {
			return fmt.Errorf("lock environment for env-var mutation: %w", err)
		}
		for _, variable := range params.Variables {
			parsed, err := secretref.Parse(variable.SecretRef)
			if err != nil || parsed.Namespace != namespace {
				return ErrInvalidArgument
			}
		}

		var before json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('id',id,'key',key,'secret_ref',secret_ref,'is_secret',is_secret) ORDER BY key),'[]'::jsonb) FROM env_vars WHERE org_id=$1 AND environment_id=$2`, params.OrgID, params.EnvironmentID).Scan(&before); err != nil {
			return fmt.Errorf("read env-var audit state: %w", err)
		}
		if !params.Merge {
			if _, err := tx.Exec(ctx, `DELETE FROM env_vars WHERE org_id=$1 AND environment_id=$2`, params.OrgID, params.EnvironmentID); err != nil {
				return fmt.Errorf("replace env vars: %w", err)
			}
		}
		for _, variable := range params.Variables {
			if _, err := tx.Exec(ctx, `INSERT INTO env_vars (id,org_id,environment_id,key,secret_ref,is_secret) VALUES (uuidv7(),$1,$2,$3,$4,$5) ON CONFLICT (environment_id,key) DO UPDATE SET secret_ref=EXCLUDED.secret_ref,is_secret=EXCLUDED.is_secret WHERE env_vars.org_id=EXCLUDED.org_id`, params.OrgID, params.EnvironmentID, variable.Key, variable.SecretRef, variable.IsSecret); err != nil {
				return fmt.Errorf("upsert env-var secret reference: %w", err)
			}
		}

		envelope, err := DecodeReleaseSnapshot(liveSnapshot)
		if err != nil {
			return fmt.Errorf("decode live release snapshot: %w", err)
		}
		var revisionID string
		var revisionNumber int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO env_var_revisions (org_id, environment_id, revision, created_by_user_id)
			SELECT $1, $2, COALESCE(MAX(revision), 0) + 1, $3
			FROM env_var_revisions WHERE org_id = $1 AND environment_id = $2
			RETURNING id::text, revision`, params.OrgID, params.EnvironmentID,
			params.TriggeredByUserID).Scan(&revisionID, &revisionNumber); err != nil {
			return fmt.Errorf("create env-var revision: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO env_var_revision_entries (org_id, environment_id, env_var_revision_id, key, secret_ref, is_secret)
			SELECT org_id, environment_id, $3, key, secret_ref, is_secret
			FROM env_vars WHERE org_id = $1 AND environment_id = $2`, params.OrgID,
			params.EnvironmentID, revisionID); err != nil {
			return fmt.Errorf("snapshot env-var revision entries: %w", err)
		}
		var variables []ReleaseEnvironment
		rows, err := tx.Query(ctx, `SELECT key, secret_ref, is_secret
			FROM env_var_revision_entries WHERE env_var_revision_id = $1 ORDER BY key`, revisionID)
		if err != nil {
			return fmt.Errorf("read env-var revision: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var variable ReleaseEnvironment
			if err := rows.Scan(&variable.Key, &variable.SecretRef, &variable.IsSecret); err != nil {
				return fmt.Errorf("scan env-var revision: %w", err)
			}
			variables = append(variables, variable)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read env-var revision: %w", err)
		}
		envHash := ComputeEnvVarRevisionHash(variables)
		envelope.EnvVarRevision = &EnvVarRevision{Version: EnvVarRevisionVersion, ID: revisionID, Revision: revisionNumber, Hash: envHash, Variables: variables}
		destinationName := EnvDestinationSecretName(envelope.DesiredState.Deployment.Identity.Name, revisionID)
		entries := make([]SecretEntryIntent, len(variables))
		for index, variable := range variables {
			entries[index] = SecretEntryIntent{Key: variable.Key, SecretRef: variable.SecretRef}
		}
		envelope.DesiredState.Secret = &SecretIntent{
			Version: 1,
			Identity: ObjectIdentity{
				Name: destinationName, Namespace: namespace,
				Labels: cloneMap(envelope.DesiredState.Deployment.Identity.Labels),
			},
			Immutable: true,
			Entries:   entries,
		}
		envelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{destinationName}
		envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-revision-id"] = revisionID
		envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = envHash
		result.ConfigSnapshot, result.ConfigHash, err = EncodeReleaseSnapshot(envelope)
		if err != nil {
			return err
		}
		result.ConfigSnapshot, result.ConfigHash, err = NormalizeReleaseSnapshotContext(result.ConfigSnapshot, ReleaseSnapshotContext{
			ImageVisibility: visibility,
			ImmutableImage:  imageRef,
			ReleaseID:       &params.ReleaseID,
		})
		if err != nil {
			return fmt.Errorf("normalize env-change snapshot context: %w", err)
		}

		if _, err := tx.Exec(ctx, `INSERT INTO releases (
			id, org_id, environment_id, image_ref, git_commit_sha, triggered_by_user_id,
			kind, env_var_revision_id, config_snapshot, config_hash, status,
			registry_credential_registry_id,registry_credential_generation,desired_generation,provenance
		) VALUES ($1, $2, $3, $4, $5, $6, 'env_change', $7, $8, $9, 'pending',$10,$11,$12,'contextual')`,
			params.ReleaseID, params.OrgID, params.EnvironmentID, imageRef, gitCommitSHA,
			params.TriggeredByUserID, revisionID, result.ConfigSnapshot, result.ConfigHash, registryID,
			registryGeneration, desiredGeneration); err != nil {
			return fmt.Errorf("insert env-change release: %w", err)
		}
		if err := tx.QueryRow(ctx, `
			UPDATE environments AS environment
			SET in_progress_release_id = $3, lock_acquired_at = clock_timestamp()
			FROM apps AS app, projects AS project, orgs AS org
			WHERE environment.org_id = $1 AND environment.id = $2
			  AND environment.in_progress_release_id IS NULL
			  AND environment.lifecycle_status='ready' AND environment.desired_generation=$4
			  AND app.desired_generation=$4
			  AND app.org_id = environment.org_id AND app.id = environment.app_id
			  AND project.org_id = app.org_id AND project.id = app.project_id
			  AND org.id = environment.org_id
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			RETURNING environment.org_id::text, environment.id::text,
				environment.in_progress_release_id::text, environment.lock_acquired_at,
				environment.lock_acquired_at + make_interval(secs => app.deploy_timeout_seconds)`,
			params.OrgID, params.EnvironmentID, params.ReleaseID, desiredGeneration).Scan(&result.Lease.OrgID,
			&result.Lease.EnvironmentID, &result.Lease.ReleaseID, &result.Lease.Fence,
			&result.Lease.ExpiresAt); err == pgx.ErrNoRows {
			return ErrConflict
		} else if err != nil {
			return fmt.Errorf("acquire env-change deploy lease: %w", err)
		}
		result.Lease.DesiredGeneration = desiredGeneration

		resourceType := "release"
		params.Idempotency.OrgID = params.OrgID
		params.Idempotency.ResourceType = &resourceType
		params.Idempotency.ResourceID = &params.ReleaseID
		if err := completeIdempotency(ctx, tx, params.Idempotency); err != nil {
			return err
		}
		after, err := json.Marshal(map[string]any{"variables": params.Variables, "release_id": params.ReleaseID})
		if err != nil {
			return fmt.Errorf("marshal env-var audit state: %w", err)
		}
		return insertAuditEvent(ctx, tx, params.OrgID, params.Audit,
			"environment_variable.upserted", "environment", params.EnvironmentID, before, after)
	})
	return result, err
}

func (s *Store) GetCurrentEnvironmentVariableRevision(ctx context.Context, orgID, environmentID string) (EnvironmentVariableRevision, error) {
	var revision EnvironmentVariableRevision
	err := s.db.QueryRow(ctx, `SELECT revision.id::text,revision.org_id::text,revision.environment_id::text,revision.revision,COALESCE(revision.created_by_user_id::text,''),revision.created_at FROM environments environment JOIN env_var_revisions revision ON revision.id=environment.current_env_var_revision_id AND revision.environment_id=environment.id WHERE environment.org_id=$1 AND environment.id=$2 AND environment.torn_down_at IS NULL`, orgID, environmentID).Scan(&revision.ID, &revision.OrgID, &revision.EnvironmentID, &revision.Revision, &revision.CreatedByUserID, &revision.CreatedAt)
	if err != nil {
		return revision, resourceQueryError("get current environment variable revision", err)
	}
	rows, err := s.db.Query(ctx, `SELECT key,secret_ref,is_secret FROM env_var_revision_entries WHERE org_id=$1 AND environment_id=$2 AND env_var_revision_id=$3 ORDER BY key`, orgID, environmentID, revision.ID)
	if err != nil {
		return revision, fmt.Errorf("list environment variable revision entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var value EnvironmentVariableValue
		if err := rows.Scan(&value.Key, &value.SecretRef, &value.IsSecret); err != nil {
			return revision, err
		}
		revision.Variables = append(revision.Variables, value)
	}
	return revision, rows.Err()
}
