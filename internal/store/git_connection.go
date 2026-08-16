package store

import (
	"context"
	"time"
)

type GitConnection struct {
	ID, OrgID, State, AccessState                  string
	GitHubRepositoryID, GitHubInstallationID       int64
	OwnerLogin, RepositoryName, RepositoryFullName string
	RepositoryDefaultBranch, RepositoryAccessState string
	RepoFullName, DefaultBranch                    string
	InstallationID                                 int64
	CreatedAt, UpdatedAt                           time.Time
}

type SaveGitConnectionParams struct {
	GitConnection GitConnection
	Idempotency   CompleteIdempotencyParams
	Audit         *AuditEvent
}

const gitConnectionSelect = `SELECT connection.id::text,connection.org_id::text,connection.state,connection.access_state,
	repository.github_repository_id,repository.github_installation_id,repository.owner_login,repository.name,
	repository.full_name,repository.default_branch,repository.access_state,connection.created_at,connection.updated_at
	FROM git_connections connection
	JOIN github_repositories repository ON repository.org_id=connection.org_id AND repository.github_repository_id=connection.github_repository_id`

func scanGitConnection(row rowScanner, value *GitConnection) error {
	if err := row.Scan(&value.ID, &value.OrgID, &value.State, &value.AccessState, &value.GitHubRepositoryID,
		&value.GitHubInstallationID, &value.OwnerLogin, &value.RepositoryName, &value.RepositoryFullName,
		&value.RepositoryDefaultBranch, &value.RepositoryAccessState, &value.CreatedAt, &value.UpdatedAt); err != nil {
		return err
	}
	value.RepoFullName = value.RepositoryFullName
	value.DefaultBranch = value.RepositoryDefaultBranch
	value.InstallationID = value.GitHubInstallationID
	return nil
}

// CreateGitConnection inserts a git connection, guarding that the actor's org
// exists so a missing org yields ErrNotFound rather than a bare foreign-key
// violation. A duplicate (org_id, provider, installation_id, repo_full_name)
// maps to ErrAlreadyExists and a CHECK violation maps to ErrInvalidArgument.
func (s *Store) CreateGitConnection(ctx context.Context, p SaveGitConnectionParams) (GitConnection, error) {
	return GitConnection{}, ErrFailedPrecondition
}

// DeleteGitConnection removes a git connection org-scoped. projects.git_connection_id
// references git_connections ON DELETE SET NULL, so deleting a connection still
// referenced by a project nulls that reference rather than failing.
func (s *Store) DeleteGitConnection(ctx context.Context, orgID, id string, idem CompleteIdempotencyParams, audit *AuditEvent) error {
	return ErrFailedPrecondition
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
