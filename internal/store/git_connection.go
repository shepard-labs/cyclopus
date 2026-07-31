package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// GitConnection is one row of the git_connections table: a GitHub App
// installation bound to a single repository (§6). WebhookSecretRef is a
// k8s-secret://…#… REFERENCE to the webhook signing secret — the raw secret is
// never stored in Postgres.
type GitConnection struct {
	ID, OrgID, Provider, RepoFullName, DefaultBranch, WebhookSecretRef string
	InstallationID                                                     int64
	CreatedAt                                                          time.Time
}

type SaveGitConnectionParams struct {
	GitConnection GitConnection
	Idempotency   CompleteIdempotencyParams
	Audit         *AuditEvent
}

const gitConnectionSelect = `SELECT id::text,org_id::text,provider,installation_id,repo_full_name,default_branch,webhook_secret_ref,created_at FROM git_connections`

func scanGitConnection(row rowScanner, value *GitConnection) error {
	return row.Scan(&value.ID, &value.OrgID, &value.Provider, &value.InstallationID, &value.RepoFullName,
		&value.DefaultBranch, &value.WebhookSecretRef, &value.CreatedAt)
}

// CreateGitConnection inserts a git connection, guarding that the actor's org
// exists so a missing org yields ErrNotFound rather than a bare foreign-key
// violation. A duplicate (org_id, provider, installation_id, repo_full_name)
// maps to ErrAlreadyExists and a CHECK violation maps to ErrInvalidArgument.
func (s *Store) CreateGitConnection(ctx context.Context, p SaveGitConnectionParams) (GitConnection, error) {
	var value GitConnection
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := scanGitConnection(tx.QueryRow(ctx, `INSERT INTO git_connections(id,org_id,provider,installation_id,repo_full_name,default_branch,webhook_secret_ref)
			SELECT $1,$2,$3,$4,$5,$6,$7 WHERE EXISTS(SELECT 1 FROM orgs WHERE id=$2)
			RETURNING id::text,org_id::text,provider,installation_id,repo_full_name,default_branch,webhook_secret_ref,created_at`,
			p.GitConnection.ID, p.GitConnection.OrgID, p.GitConnection.Provider, p.GitConnection.InstallationID,
			p.GitConnection.RepoFullName, p.GitConnection.DefaultBranch, p.GitConnection.WebhookSecretRef), &value); err != nil {
			return mapMutationError("create git connection", err)
		}
		if err := finishAcceptedMutation(ctx, tx, value.OrgID, "git_connection", value.ID, &p.Idempotency); err != nil {
			return err
		}
		after, _ := json.Marshal(value)
		return insertAuditEvent(ctx, tx, value.OrgID, p.Audit, "git_connection.created", "git_connection", value.ID, nil, after)
	})
	return value, err
}

// DeleteGitConnection removes a git connection org-scoped. projects.git_connection_id
// references git_connections ON DELETE SET NULL, so deleting a connection still
// referenced by a project nulls that reference rather than failing.
func (s *Store) DeleteGitConnection(ctx context.Context, orgID, id string, idem CompleteIdempotencyParams, audit *AuditEvent) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var before json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT to_jsonb(git_connections) FROM git_connections WHERE org_id=$1 AND id=$2 FOR UPDATE`, orgID, id).Scan(&before); err != nil {
			return mapMutationError("lock git connection", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM git_connections WHERE org_id=$1 AND id=$2`, orgID, id); err != nil {
			return mapMutationError("delete git connection", err)
		}
		if err := completeIdempotency(ctx, tx, idem); err != nil {
			return err
		}
		return insertAuditEvent(ctx, tx, orgID, audit, "git_connection.deleted", "git_connection", id, before, nil)
	})
}

func (s *Store) GetGitConnection(ctx context.Context, orgID, id string) (GitConnection, error) {
	var value GitConnection
	err := scanGitConnection(s.db.QueryRow(ctx, gitConnectionSelect+` WHERE org_id=$1 AND id=$2`, orgID, id), &value)
	return value, resourceQueryError("get git connection", err)
}

// ListGitConnections returns every git connection in the org, ordered stably for
// pagination.
func (s *Store) ListGitConnections(ctx context.Context, orgID string) ([]GitConnection, error) {
	rows, err := s.db.Query(ctx, gitConnectionSelect+` WHERE org_id=$1 ORDER BY created_at,id`, orgID)
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
