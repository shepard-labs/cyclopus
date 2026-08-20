package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type GitConnection struct {
	ID, OrgID, State, AccessState                  string
	GitHubRepositoryID, GitHubInstallationID       int64
	RepositoryNodeID, OwnerLogin, InstallationID   string
	RepositoryName, RepositoryFullName             string
	RepositoryDefaultBranch, RepositoryAccessState string
	RepositoryPrivate, RepositoryArchived          bool
	RepositorySyncedAt                             *time.Time
	CreatedAt, UpdatedAt                           time.Time
	DisconnectedAt                                 *time.Time
}

const gitConnectionSelect = `SELECT connection.id::text,connection.org_id::text,connection.state,connection.access_state,
		repository.github_repository_id,repository.github_installation_id,installation.id::text,repository.owner_login,repository.name,
		repository.full_name,repository.node_id,repository.visibility='private',repository.archived,repository.default_branch,
		repository.access_state,repository.last_reconciled_at,connection.created_at,connection.updated_at,connection.disconnected_at
	FROM git_connections connection
	JOIN github_repositories repository ON repository.org_id=connection.org_id AND repository.github_repository_id=connection.github_repository_id
	JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id`

func scanGitConnection(row rowScanner, value *GitConnection) error {
	if err := row.Scan(&value.ID, &value.OrgID, &value.State, &value.AccessState, &value.GitHubRepositoryID,
		&value.GitHubInstallationID, &value.InstallationID, &value.OwnerLogin, &value.RepositoryName, &value.RepositoryFullName,
		&value.RepositoryNodeID, &value.RepositoryPrivate, &value.RepositoryArchived, &value.RepositoryDefaultBranch,
		&value.RepositoryAccessState, &value.RepositorySyncedAt, &value.CreatedAt, &value.UpdatedAt, &value.DisconnectedAt); err != nil {
		return err
	}
	return nil
}

func (s *Store) GetGitConnection(ctx context.Context, orgID, id string) (GitConnection, error) {
	var value GitConnection
	err := scanGitConnection(s.db.QueryRow(ctx, gitConnectionSelect+` WHERE connection.org_id=$1 AND connection.id=$2`, orgID, id), &value)
	return value, resourceQueryError("get git connection", err)
}

// ListGitConnections returns every git connection in the org, ordered stably for
// pagination.
func (s *Store) ListGitConnections(ctx context.Context, orgID string) ([]GitConnection, error) {
	rows, err := s.db.Query(ctx, gitConnectionSelect+` WHERE connection.org_id=$1 ORDER BY connection.created_at,connection.id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []GitConnection
	for rows.Next() {
		var value GitConnection
		if err := scanGitConnection(rows, &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

type DisconnectGitConnectionParams struct {
	OrgID, ID   string
	Idempotency CompleteIdempotencyParams
	Audit       *AuditEvent
}

func (s *Store) DisconnectGitConnection(ctx context.Context, params DisconnectGitConnectionParams) (GitConnection, error) {
	var value GitConnection
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, params.OrgID); err != nil {
			return fmt.Errorf("lock disconnect boundary: %w", err)
		}
		var dispatchInProgress bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM orchestration_jobs job
			JOIN apps app ON app.org_id=job.org_id AND app.id=job.app_id
			JOIN projects project ON project.org_id=app.org_id AND project.id=app.project_id
			WHERE project.org_id=$1 AND project.git_connection_id=$2
			AND job.dispatch_call_owner IS NOT NULL AND job.dispatch_call_expires_at>now())`, params.OrgID, params.ID).Scan(&dispatchInProgress); err != nil {
			return err
		}
		if dispatchInProgress {
			return ErrConflict
		}
		var before json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT to_jsonb(connection) FROM git_connections connection
			WHERE connection.org_id=$1 AND connection.id=$2 FOR UPDATE`, params.OrgID, params.ID).Scan(&before); err != nil {
			return mapMutationError("lock git connection", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE apps app SET desired_generation=app.desired_generation+1
			FROM projects project WHERE project.org_id=$1 AND project.git_connection_id=$2
			AND app.org_id=project.org_id AND app.project_id=project.id
			AND EXISTS (SELECT 1 FROM git_connections connection WHERE connection.org_id=$1 AND connection.id=$2 AND connection.state='active')`, params.OrgID, params.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE github_routing_intents intent SET status='dropped',result_code='superseded',error_code=NULL,error_detail=NULL,
			terminal_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
			FROM projects project,apps app WHERE project.org_id=$1 AND project.git_connection_id=$2
			AND app.org_id=project.org_id AND app.project_id=project.id AND intent.org_id=app.org_id AND intent.app_id=app.id
			AND intent.status IN ('pending','leased')`, params.OrgID, params.ID); err != nil {
			return err
		}
		if err := scanGitConnection(tx.QueryRow(ctx, `WITH disconnected AS (
			UPDATE git_connections SET state='disconnected',disconnected_at=COALESCE(disconnected_at,now()),updated_at=CASE WHEN state='active' THEN now() ELSE updated_at END
			WHERE org_id=$1 AND id=$2 RETURNING *)
			SELECT disconnected.id::text,disconnected.org_id::text,disconnected.state,disconnected.access_state,
			repository.github_repository_id,repository.github_installation_id,installation.id::text,repository.owner_login,repository.name,
			repository.full_name,repository.node_id,repository.visibility='private',repository.archived,repository.default_branch,
			repository.access_state,repository.last_reconciled_at,disconnected.created_at,disconnected.updated_at,disconnected.disconnected_at
			FROM disconnected JOIN github_repositories repository ON repository.org_id=disconnected.org_id AND repository.github_repository_id=disconnected.github_repository_id
			JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id`, params.OrgID, params.ID), &value); err != nil {
			return mapMutationError("disconnect git connection", err)
		}
		body, _ := json.Marshal(value)
		params.Idempotency.Body = body
		resourceType, resourceID := "git_connection", value.ID
		params.Idempotency.ResourceType, params.Idempotency.ResourceID = &resourceType, &resourceID
		if err := completeIdempotency(ctx, tx, params.Idempotency); err != nil {
			return err
		}
		after, _ := json.Marshal(value)
		return insertAuditEvent(ctx, tx, params.OrgID, params.Audit, "git_connection.disconnected", "git_connection", value.ID, before, after)
	})
	return value, mapMutationError("disconnect git connection", err)
}
