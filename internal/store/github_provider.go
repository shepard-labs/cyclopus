package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type GitHubApp struct {
	AppID                       *int64
	Slug, Name, HTMLURL, State  string
	OwnerLogin, OwnerType       *string
	HasCredential               bool
	FailureCode, FailureMessage *string
	CreatedAt, UpdatedAt        time.Time
}

func (s *Store) GetGitHubApp(ctx context.Context) (GitHubApp, error) {
	var value GitHubApp
	err := s.db.QueryRow(ctx, `SELECT github_app_id,COALESCE(app_slug,''),COALESCE(app_name,''),COALESCE(app_url,''),state,
		owner_login,owner_type,credential_secret_ref IS NOT NULL,failure_code,failure_message,created_at,updated_at FROM github_app WHERE singleton=true`).Scan(
		&value.AppID, &value.Slug, &value.Name, &value.HTMLURL, &value.State, &value.OwnerLogin, &value.OwnerType,
		&value.HasCredential, &value.FailureCode, &value.FailureMessage, &value.CreatedAt, &value.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitHubApp{}, ErrNotFound
	}
	return value, err
}

type GitHubInstallation struct {
	GitHubInstallationID, AccountID                                    int64
	OrgID, AccountLogin, AccountType, RepositorySelection, AccessState string
	SuspendedAt, DeletedAt, LastReconciledAt                           *time.Time
	CreatedAt, UpdatedAt                                               time.Time
}

const githubInstallationSelect = `SELECT github_installation_id,org_id::text,account_id,account_login,account_type,
	repository_selection,access_state,suspended_at,deleted_at,last_reconciled_at,created_at,updated_at FROM github_installations`

func scanGitHubInstallation(row rowScanner, value *GitHubInstallation) error {
	return row.Scan(&value.GitHubInstallationID, &value.OrgID, &value.AccountID, &value.AccountLogin, &value.AccountType,
		&value.RepositorySelection, &value.AccessState, &value.SuspendedAt, &value.DeletedAt, &value.LastReconciledAt,
		&value.CreatedAt, &value.UpdatedAt)
}

func (s *Store) GetGitHubInstallation(ctx context.Context, orgID string, id int64) (GitHubInstallation, error) {
	var value GitHubInstallation
	err := scanGitHubInstallation(s.db.QueryRow(ctx, githubInstallationSelect+` WHERE org_id=$1 AND github_installation_id=$2`, orgID, id), &value)
	return value, resourceQueryError("get github installation", err)
}

func (s *Store) ListGitHubInstallations(ctx context.Context, orgID string) ([]GitHubInstallation, error) {
	rows, err := s.db.Query(ctx, githubInstallationSelect+` WHERE org_id=$1 ORDER BY created_at,github_installation_id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []GitHubInstallation
	for rows.Next() {
		var value GitHubInstallation
		if err := scanGitHubInstallation(rows, &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

type GitHubRepository struct {
	GitHubRepositoryID, GitHubInstallationID                                          int64
	OrgID, NodeID, OwnerLogin, Name, FullName, Visibility, DefaultBranch, AccessState string
	Archived                                                                          bool
	LastReconciledAt                                                                  *time.Time
	CreatedAt, UpdatedAt                                                              time.Time
}

const githubRepositorySelect = `SELECT github_repository_id,org_id::text,github_installation_id,node_id,owner_login,name,full_name,
	visibility,archived,default_branch,access_state,last_reconciled_at,created_at,updated_at FROM github_repositories`

func scanGitHubRepository(row rowScanner, value *GitHubRepository) error {
	return row.Scan(&value.GitHubRepositoryID, &value.OrgID, &value.GitHubInstallationID, &value.NodeID, &value.OwnerLogin,
		&value.Name, &value.FullName, &value.Visibility, &value.Archived, &value.DefaultBranch, &value.AccessState,
		&value.LastReconciledAt, &value.CreatedAt, &value.UpdatedAt)
}

func (s *Store) ListGitHubRepositories(ctx context.Context, orgID string, installationID int64) ([]GitHubRepository, error) {
	rows, err := s.db.Query(ctx, githubRepositorySelect+` WHERE org_id=$1 AND github_installation_id=$2 ORDER BY created_at,github_repository_id`, orgID, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []GitHubRepository
	for rows.Next() {
		var value GitHubRepository
		if err := scanGitHubRepository(rows, &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

type ImportOperation struct {
	ID, OrgID, State                                                     string
	GitHubRepositoryID                                                   int64
	GitConnectionID, ProjectID, AppID, EnvironmentID, OrchestrationJobID *string
	FailureCode, FailureMessage                                          *string
	CreatedAt, UpdatedAt                                                 time.Time
	CompletedAt                                                          *time.Time
}

func (s *Store) GetImportOperation(ctx context.Context, orgID, id string) (ImportOperation, error) {
	var value ImportOperation
	err := s.db.QueryRow(ctx, `SELECT id::text,org_id::text,state,github_repository_id,git_connection_id::text,project_id::text,
		app_id::text,environment_id::text,orchestration_job_id::text,failure_code,failure_message,created_at,updated_at,completed_at
		FROM import_operations WHERE org_id=$1 AND id=$2`, orgID, id).Scan(&value.ID, &value.OrgID, &value.State,
		&value.GitHubRepositoryID, &value.GitConnectionID, &value.ProjectID, &value.AppID, &value.EnvironmentID,
		&value.OrchestrationJobID, &value.FailureCode, &value.FailureMessage, &value.CreatedAt, &value.UpdatedAt, &value.CompletedAt)
	return value, resourceQueryError("get import operation", err)
}
