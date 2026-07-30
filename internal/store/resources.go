package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Organization struct {
	ID, Name, Slug string
	CreatedAt      time.Time
	DeletedAt      *time.Time
}

type Project struct {
	ID, OrgID, Name, Slug string
	GitConnectionID       *string
	CreatedAt             time.Time
	DeletedAt             *time.Time
}

type App struct {
	ID, OrgID, ProjectID, Name, Slug, HostnameLabel        string
	RegistryID, RegistryURL, ImageRepository, CIWorkflowID *string
	ContainerPort                                          int
	HealthCheckPath, ScalingMode, RuntimeClass             string
	MinReplicas, MaxReplicas, HPATargetCPUPercent          int
	CPURequestMillicores, CPULimitMillicores               int
	MemoryRequestMiB, MemoryLimitMiB                       int
	PreviewsEnabled                                        bool
	MaxPreviewEnvironments, PreviewIdleTTLHours            int
	DeployTimeoutSeconds, ReleaseRetentionCount            int
	CreatedAt                                              time.Time
	DeletedAt                                              *time.Time
}

type Environment struct {
	ID, OrgID, AppID, Branch, Kind, Namespace, PlannedHostname string
	PreviewSlug, CurrentReleaseID, InProgressReleaseID         *string
	CurrentEnvVarRevisionID, PendingCommitSHA                  *string
	CreatedAt, TornDownAt                                      *time.Time
}

type Release struct {
	ImmutableReleaseSnapshot
	Status, FailureReason string
}

func (s *Store) GetCurrentUser(ctx context.Context, orgID, userID string) (User, error) {
	var value User
	err := s.db.QueryRow(ctx,
		`SELECT id::text, org_id::text, email, created_at FROM users WHERE org_id = $1 AND id = $2`, orgID, userID).
		Scan(&value.ID, &value.OrgID, &value.Email, &value.CreatedAt)
	return value, resourceQueryError("get current user", err)
}

func (s *Store) GetOrganization(ctx context.Context, orgID string) (Organization, error) {
	var value Organization
	err := s.db.QueryRow(ctx,
		`SELECT id::text, name, slug, created_at, deleted_at FROM orgs WHERE id = $1 AND deleted_at IS NULL`, orgID).
		Scan(&value.ID, &value.Name, &value.Slug, &value.CreatedAt, &value.DeletedAt)
	return value, resourceQueryError("get organization", err)
}

func (s *Store) ListProjects(ctx context.Context, orgID string, includeDeleted bool) ([]Project, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id::text, org_id::text, name, slug, git_connection_id::text, created_at, deleted_at
		 FROM projects WHERE org_id = $1 AND ($2 OR deleted_at IS NULL) ORDER BY created_at, id`, orgID, includeDeleted)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var values []Project
	for rows.Next() {
		var value Project
		if err := rows.Scan(&value.ID, &value.OrgID, &value.Name, &value.Slug, &value.GitConnectionID, &value.CreatedAt, &value.DeletedAt); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) GetProject(ctx context.Context, orgID, projectID string) (Project, error) {
	var value Project
	err := s.db.QueryRow(ctx,
		`SELECT id::text, org_id::text, name, slug, git_connection_id::text, created_at, deleted_at
		 FROM projects WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`, orgID, projectID).
		Scan(&value.ID, &value.OrgID, &value.Name, &value.Slug, &value.GitConnectionID, &value.CreatedAt, &value.DeletedAt)
	return value, resourceQueryError("get project", err)
}

type SaveProjectParams struct {
	Project     Project
	Idempotency CompleteIdempotencyParams
	Audit       *AuditEvent
}

func (s *Store) CreateProject(ctx context.Context, params SaveProjectParams) (Project, error) {
	var value Project
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO projects (id, org_id, name, slug, git_connection_id)
			SELECT $1, $2, $3, $4, $5 WHERE EXISTS (SELECT 1 FROM orgs WHERE id = $2 AND deleted_at IS NULL)
			RETURNING id::text, org_id::text, name, slug, git_connection_id::text, created_at, deleted_at`, params.Project.ID, params.Project.OrgID, params.Project.Name, params.Project.Slug, params.Project.GitConnectionID).
			Scan(&value.ID, &value.OrgID, &value.Name, &value.Slug, &value.GitConnectionID, &value.CreatedAt, &value.DeletedAt)
		if err != nil {
			return mapMutationError("create project", err)
		}
		if err := finishAcceptedMutation(ctx, tx, params.Project.OrgID, "project", value.ID, &params.Idempotency); err != nil {
			return err
		}
		after, _ := json.Marshal(value)
		return insertAuditEvent(ctx, tx, params.Project.OrgID, params.Audit, "project.created", "project", value.ID, nil, after)
	})
	return value, err
}

func (s *Store) UpdateProject(ctx context.Context, params SaveProjectParams) (Project, error) {
	var value Project
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var before json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT to_jsonb(projects) FROM projects WHERE org_id=$1 AND id=$2 AND deleted_at IS NULL FOR UPDATE`, params.Project.OrgID, params.Project.ID).Scan(&before); err != nil {
			return mapMutationError("lock project", err)
		}
		err := tx.QueryRow(ctx, `UPDATE projects SET name=$3, slug=$4, git_connection_id=$5 WHERE org_id=$1 AND id=$2 AND deleted_at IS NULL
			RETURNING id::text, org_id::text, name, slug, git_connection_id::text, created_at, deleted_at`, params.Project.OrgID, params.Project.ID, params.Project.Name, params.Project.Slug, params.Project.GitConnectionID).
			Scan(&value.ID, &value.OrgID, &value.Name, &value.Slug, &value.GitConnectionID, &value.CreatedAt, &value.DeletedAt)
		if err != nil {
			return mapMutationError("update project", err)
		}
		if err := finishAcceptedMutation(ctx, tx, params.Project.OrgID, "project", value.ID, &params.Idempotency); err != nil {
			return err
		}
		after, _ := json.Marshal(value)
		return insertAuditEvent(ctx, tx, params.Project.OrgID, params.Audit, "project.updated", "project", value.ID, before, after)
	})
	return value, err
}

func (s *Store) ListApps(ctx context.Context, orgID, projectID string, includeDeleted bool) ([]App, error) {
	rows, err := s.db.Query(ctx, appSelect+` WHERE app.org_id=$1 AND app.project_id=$2 AND ($3 OR app.deleted_at IS NULL) ORDER BY app.created_at, app.id`, orgID, projectID, includeDeleted)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	defer rows.Close()
	var values []App
	for rows.Next() {
		value, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) GetApp(ctx context.Context, orgID, appID string) (App, error) {
	row := s.db.QueryRow(ctx, appSelect+` WHERE app.org_id=$1 AND app.id=$2 AND app.deleted_at IS NULL`, orgID, appID)
	value, err := scanApp(row)
	return value, resourceQueryError("get app", err)
}

const appSelect = `SELECT app.id::text, app.org_id::text, app.project_id::text, app.name, app.slug, app.hostname_label,
	app.registry_id::text, registry.registry_url, app.image_repository, app.container_port, app.health_check_path, app.scaling_mode,
	app.min_replicas, app.max_replicas, app.hpa_target_cpu_percent, app.cpu_request_millicores,
	app.cpu_limit_millicores, app.memory_request_mib, app.memory_limit_mib, app.previews_enabled,
	app.max_preview_environments, app.preview_idle_ttl_hours, app.deploy_timeout_seconds,
	app.release_retention_count, app.runtime_class, app.ci_workflow_id, app.created_at, app.deleted_at FROM apps app
	LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id`

type rowScanner interface{ Scan(...any) error }

func scanApp(row rowScanner) (App, error) {
	var v App
	err := row.Scan(&v.ID, &v.OrgID, &v.ProjectID, &v.Name, &v.Slug, &v.HostnameLabel, &v.RegistryID, &v.RegistryURL, &v.ImageRepository, &v.ContainerPort, &v.HealthCheckPath, &v.ScalingMode, &v.MinReplicas, &v.MaxReplicas, &v.HPATargetCPUPercent, &v.CPURequestMillicores, &v.CPULimitMillicores, &v.MemoryRequestMiB, &v.MemoryLimitMiB, &v.PreviewsEnabled, &v.MaxPreviewEnvironments, &v.PreviewIdleTTLHours, &v.DeployTimeoutSeconds, &v.ReleaseRetentionCount, &v.RuntimeClass, &v.CIWorkflowID, &v.CreatedAt, &v.DeletedAt)
	if err != nil {
		return App{}, err
	}
	return v, nil
}

type SaveAppParams struct {
	App         App
	Idempotency CompleteIdempotencyParams
	Audit       *AuditEvent
}

func (s *Store) CreateApp(ctx context.Context, params SaveAppParams) (App, error) {
	return s.saveApp(ctx, params, true)
}
func (s *Store) UpdateApp(ctx context.Context, params SaveAppParams) (App, error) {
	return s.saveApp(ctx, params, false)
}
func (s *Store) saveApp(ctx context.Context, p SaveAppParams, create bool) (App, error) {
	var value App
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var before json.RawMessage
		if create {
			row := tx.QueryRow(ctx, `INSERT INTO apps (id,org_id,project_id,name,slug,hostname_label,registry_id,image_repository,container_port,health_check_path,scaling_mode,min_replicas,max_replicas,hpa_target_cpu_percent,cpu_request_millicores,cpu_limit_millicores,memory_request_mib,memory_limit_mib,previews_enabled,max_preview_environments,preview_idle_ttl_hours,deploy_timeout_seconds,release_retention_count,runtime_class,ci_workflow_id)
			SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25 WHERE EXISTS (SELECT 1 FROM projects WHERE org_id=$2 AND id=$3 AND deleted_at IS NULL) RETURNING id::text`, appArgs(p.App)...)
			var id string
			if err := row.Scan(&id); err != nil {
				return mapMutationError("create app", err)
			}
		} else {
			if err := tx.QueryRow(ctx, `SELECT to_jsonb(apps) FROM apps WHERE org_id=$1 AND id=$2 AND deleted_at IS NULL FOR UPDATE`, p.App.OrgID, p.App.ID).Scan(&before); err != nil {
				return mapMutationError("lock app", err)
			}
			args := appArgs(p.App)
			row := tx.QueryRow(ctx, `UPDATE apps SET project_id=$3,name=$4,slug=$5,hostname_label=$6,registry_id=$7,image_repository=$8,container_port=$9,health_check_path=$10,scaling_mode=$11,min_replicas=$12,max_replicas=$13,hpa_target_cpu_percent=$14,cpu_request_millicores=$15,cpu_limit_millicores=$16,memory_request_mib=$17,memory_limit_mib=$18,previews_enabled=$19,max_preview_environments=$20,preview_idle_ttl_hours=$21,deploy_timeout_seconds=$22,release_retention_count=$23,runtime_class=$24,ci_workflow_id=$25 WHERE org_id=$2 AND id=$1 AND deleted_at IS NULL RETURNING id::text`, args...)
			var id string
			if err := row.Scan(&id); err != nil {
				return mapMutationError("update app", err)
			}
		}
		v, err := scanApp(tx.QueryRow(ctx, appSelect+` WHERE app.org_id=$1 AND app.id=$2`, p.App.OrgID, p.App.ID))
		if err != nil {
			return err
		}
		value = v
		if err := finishAcceptedMutation(ctx, tx, p.App.OrgID, "app", value.ID, &p.Idempotency); err != nil {
			return err
		}
		after, _ := json.Marshal(value)
		action := "app.updated"
		if create {
			action = "app.created"
		}
		return insertAuditEvent(ctx, tx, p.App.OrgID, p.Audit, action, "app", value.ID, before, after)
	})
	return value, err
}
func appArgs(a App) []any {
	return []any{a.ID, a.OrgID, a.ProjectID, a.Name, a.Slug, a.HostnameLabel, a.RegistryID, a.ImageRepository, a.ContainerPort, a.HealthCheckPath, a.ScalingMode, a.MinReplicas, a.MaxReplicas, a.HPATargetCPUPercent, a.CPURequestMillicores, a.CPULimitMillicores, a.MemoryRequestMiB, a.MemoryLimitMiB, a.PreviewsEnabled, a.MaxPreviewEnvironments, a.PreviewIdleTTLHours, a.DeployTimeoutSeconds, a.ReleaseRetentionCount, a.RuntimeClass, a.CIWorkflowID}
}

func (s *Store) ListEnvironments(ctx context.Context, orgID, appID string, includeTornDown bool) ([]Environment, error) {
	rows, err := s.db.Query(ctx, environmentSelect+` WHERE environment.org_id=$1 AND environment.app_id=$2 AND ($3 OR environment.torn_down_at IS NULL) ORDER BY environment.created_at,environment.id`, orgID, appID, includeTornDown)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		var v Environment
		if err := scanEnvironment(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) GetEnvironment(ctx context.Context, orgID, id string) (Environment, error) {
	var v Environment
	err := scanEnvironment(s.db.QueryRow(ctx, environmentSelect+` WHERE environment.org_id=$1 AND environment.id=$2`, orgID, id), &v)
	return v, resourceQueryError("get environment", err)
}

const environmentSelect = `SELECT environment.id::text,environment.org_id::text,environment.app_id::text,environment.branch,environment.kind,environment.preview_slug,environment.namespace,environment.planned_hostname,
	(SELECT release.id::text FROM releases release WHERE release.org_id=environment.org_id AND release.environment_id=environment.id AND release.status='live' ORDER BY release.created_at DESC,release.id DESC LIMIT 1),
	environment.in_progress_release_id::text,environment.current_env_var_revision_id::text,environment.pending_commit_sha,environment.created_at,environment.torn_down_at FROM environments environment`

func scanEnvironment(row rowScanner, v *Environment) error {
	return row.Scan(&v.ID, &v.OrgID, &v.AppID, &v.Branch, &v.Kind, &v.PreviewSlug, &v.Namespace, &v.PlannedHostname, &v.CurrentReleaseID, &v.InProgressReleaseID, &v.CurrentEnvVarRevisionID, &v.PendingCommitSHA, &v.CreatedAt, &v.TornDownAt)
}

func (s *Store) ListReleases(ctx context.Context, orgID, environmentID string, statuses []string) ([]Release, error) {
	rows, err := s.db.Query(ctx, releaseSelect+` WHERE org_id=$1 AND environment_id=$2 AND (cardinality($3::text[])=0 OR status=ANY($3)) ORDER BY created_at DESC,id DESC`, orgID, environmentID, statuses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		v, e := scanRelease(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) GetRelease(ctx context.Context, orgID, id string) (Release, error) {
	v, err := scanRelease(s.db.QueryRow(ctx, releaseSelect+` WHERE org_id=$1 AND id=$2`, orgID, id))
	return v, resourceQueryError("get release", err)
}

const releaseSelect = `SELECT id::text,org_id::text,environment_id::text,image_ref,git_commit_sha,triggered_by_user_id::text,kind,env_var_revision_id::text,config_snapshot,config_hash,created_at,status,COALESCE(failure_reason,'') FROM releases`

func scanRelease(r rowScanner) (Release, error) {
	var v Release
	err := r.Scan(&v.ID, &v.OrgID, &v.EnvironmentID, &v.ImageRef, &v.GitCommitSHA, &v.TriggeredByUserID, &v.Kind, &v.EnvVarRevisionID, &v.ConfigSnapshot, &v.ConfigHash, &v.CreatedAt, &v.Status, &v.FailureReason)
	return v, err
}

type AcceptReleaseParams struct {
	ID, OrgID, EnvironmentID, ImageRef, Kind string
	GitCommitSHA                             *string
	TriggeredByUserID                        *string
	ConfigSnapshot                           json.RawMessage
	ConfigHash                               string
	RollbackTargetID                         *string
	Idempotency                              CompleteIdempotencyParams
	Audit                                    *AuditEvent
}

func (s *Store) AcceptRelease(ctx context.Context, p AcceptReleaseParams) (DeployLease, error) {
	var lease DeployLease
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		imageRef, snapshot, hash := p.ImageRef, p.ConfigSnapshot, p.ConfigHash
		if p.RollbackTargetID != nil {
			err := tx.QueryRow(ctx, `SELECT target.image_ref,target.config_snapshot,target.config_hash FROM (SELECT release.*,row_number() OVER(ORDER BY release.created_at DESC,release.id DESC) retention_rank FROM releases release WHERE release.org_id=$1 AND release.environment_id=$2) target JOIN environments environment ON environment.org_id=target.org_id AND environment.id=target.environment_id JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id WHERE target.id=$3 AND target.status IN ('live','superseded') AND target.retention_rank<=app.release_retention_count`, p.OrgID, p.EnvironmentID, *p.RollbackTargetID).Scan(&imageRef, &snapshot, &hash)
			if err != nil {
				return mapMutationError("select rollback target", err)
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO releases(id,org_id,environment_id,image_ref,git_commit_sha,triggered_by_user_id,kind,config_snapshot,config_hash,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending')`, p.ID, p.OrgID, p.EnvironmentID, imageRef, p.GitCommitSHA, p.TriggeredByUserID, p.Kind, snapshot, hash)
		if err != nil {
			return mapMutationError("insert accepted release", err)
		}
		err = tx.QueryRow(ctx, `UPDATE environments environment SET in_progress_release_id=$3,lock_acquired_at=clock_timestamp(),deploy_recovery_checkpoint=NULL FROM apps app,projects project,orgs org WHERE environment.org_id=$1 AND environment.id=$2 AND environment.in_progress_release_id IS NULL AND environment.torn_down_at IS NULL AND app.org_id=environment.org_id AND app.id=environment.app_id AND app.deleted_at IS NULL AND project.org_id=app.org_id AND project.id=app.project_id AND project.deleted_at IS NULL AND org.id=environment.org_id AND org.deleted_at IS NULL RETURNING environment.org_id::text,environment.id::text,environment.in_progress_release_id::text,environment.lock_acquired_at,environment.lock_acquired_at+make_interval(secs=>app.deploy_timeout_seconds)`, p.OrgID, p.EnvironmentID, p.ID).Scan(&lease.OrgID, &lease.EnvironmentID, &lease.ReleaseID, &lease.Fence, &lease.ExpiresAt)
		if err != nil {
			return mapMutationError("acquire accepted deploy lease", err)
		}
		if err := finishAcceptedMutation(ctx, tx, p.OrgID, "release", p.ID, &p.Idempotency); err != nil {
			return err
		}
		after, _ := json.Marshal(map[string]any{"release_id": p.ID, "status": "pending", "kind": p.Kind})
		return insertAuditEvent(ctx, tx, p.OrgID, p.Audit, "release.accepted", "release", p.ID, nil, after)
	})
	return lease, err
}

func (s *Store) ListAPITokens(ctx context.Context, orgID, userID string) ([]APIToken, error) {
	rows, err := s.db.Query(ctx, `SELECT token.id::text,token.user_id::text,user_account.org_id::text,user_account.email,token.kind,token.name,token.token_prefix,token.scopes,token.expires_at,token.created_at,token.last_used_at,token.revoked_at FROM api_tokens token JOIN users user_account ON user_account.id=token.user_id WHERE user_account.org_id=$1 AND token.user_id=$2 AND token.kind='api' ORDER BY token.created_at,token.id`, orgID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var v APIToken
		if err := rows.Scan(&v.ID, &v.UserID, &v.OrgID, &v.Email, &v.Kind, &v.Name, &v.Prefix, &v.Scopes, &v.ExpiresAt, &v.CreatedAt, &v.LastUsedAt, &v.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) GetReconciliationRun(ctx context.Context, orgID, id string) (ReconciliationRun, error) {
	var v ReconciliationRun
	err := scanReconciliationRun(s.db.QueryRow(ctx, reconciliationSelect+` WHERE org_id=$1 AND id=$2`, orgID, id), &v)
	return v, resourceQueryError("get reconciliation run", err)
}

const reconciliationSelect = `SELECT id::text,org_id::text,actor_user_id::text,scope_kind,scope_id::text,scope_app_id::text,scope_environment_id::text,categories,dry_run,prune,status,plan,report,lease_expires_at,created_at,started_at,completed_at,error_code,error_message FROM reconciliation_runs`

func scanReconciliationRun(row rowScanner, v *ReconciliationRun) error {
	return row.Scan(&v.ID, &v.OrgID, &v.ActorUserID, &v.ScopeKind, &v.ScopeID, &v.ScopeAppID, &v.ScopeEnvironmentID, &v.Categories, &v.DryRun, &v.Prune, &v.Status, &v.Plan, &v.Report, &v.LeaseExpiresAt, &v.CreatedAt, &v.StartedAt, &v.CompletedAt, &v.ErrorCode, &v.ErrorMessage)
}

func (s *Store) ListReconciliationRuns(ctx context.Context, orgID string, appID, environmentID *string) ([]ReconciliationRun, error) {
	rows, err := s.db.Query(ctx, reconciliationSelect+` WHERE org_id=$1 AND ($2::uuid IS NULL OR scope_app_id=$2) AND ($3::uuid IS NULL OR scope_environment_id=$3) ORDER BY created_at DESC,id DESC`, orgID, appID, environmentID)
	if err != nil {
		return nil, fmt.Errorf("list reconciliation runs: %w", err)
	}
	defer rows.Close()
	var values []ReconciliationRun
	for rows.Next() {
		var value ReconciliationRun
		if err := scanReconciliationRun(rows, &value); err != nil {
			return nil, fmt.Errorf("scan reconciliation run: %w", err)
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func finishAcceptedMutation(ctx context.Context, tx pgx.Tx, orgID, resourceType, resourceID string, p *CompleteIdempotencyParams) error {
	p.OrgID = orgID
	p.ResourceType = &resourceType
	p.ResourceID = &resourceID
	return completeIdempotency(ctx, tx, *p)
}
func resourceQueryError(action string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return nil
}
func mapMutationError(action string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return fmt.Errorf("%s: %w", action, ErrAlreadyExists)
		case "23503":
			return fmt.Errorf("%s: %w", action, ErrFailedPrecondition)
		case "23514", "22P02":
			return fmt.Errorf("%s: %w", action, ErrInvalidArgument)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return nil
}
