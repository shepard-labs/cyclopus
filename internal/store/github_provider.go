package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type GitHubAuditContext struct {
	OrgID  string
	Event  *AuditEvent
	Source string
}

type GitHubApp struct {
	AppID, OwnerID                         *int64
	Slug, Name, HTMLURL, WebhookURL, State string
	OwnerLogin, OwnerType                  *string
	CredentialSecretRef                    *string
	SetupSessionID                         string
	HasCredential                          bool
	FailureCode, FailureMessage            *string
	CreatedAt, UpdatedAt                   time.Time
}

func (s *Store) GetGitHubApp(ctx context.Context) (GitHubApp, error) {
	var value GitHubApp
	err := s.db.QueryRow(ctx, `SELECT github_app_id,owner_id,COALESCE(app_slug,''),COALESCE(app_name,''),COALESCE(app_url,''),state,
		owner_login,owner_type,credential_secret_ref,credential_secret_ref IS NOT NULL,COALESCE(setup_session_id::text,''),failure_code,failure_message,created_at,updated_at FROM github_app WHERE singleton=true`).Scan(
		&value.AppID, &value.OwnerID, &value.Slug, &value.Name, &value.HTMLURL, &value.State, &value.OwnerLogin, &value.OwnerType,
		&value.CredentialSecretRef, &value.HasCredential, &value.SetupSessionID, &value.FailureCode, &value.FailureMessage, &value.CreatedAt, &value.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitHubApp{}, ErrNotFound
	}
	return value, err
}

type GitHubSetupSession struct {
	ID, StateHash, StateGeneration, FlowKind, UserID, SessionHash, Status, Checkpoint string
	OrgID, InstallationPurpose                                                        *string
	ExpiresAt                                                                         time.Time
}

func (s *Store) BeginGitHubSetupSession(ctx context.Context, stateHash, generation, flowKind, userID, sessionHash string, orgID, purpose *string, expiresAt time.Time, audit GitHubAuditContext) (GitHubSetupSession, error) {
	var value GitHubSetupSession
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if flowKind == "app_setup" {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('github_app_setup'))`); err != nil {
				return fmt.Errorf("lock github app setup: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE github_setup_sessions SET status='expired',updated_at=now()
				WHERE flow_kind='app_setup' AND status='active' AND expires_at <= now()`); err != nil {
				return fmt.Errorf("expire github app setup sessions: %w", err)
			}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO github_setup_sessions(state_hash,state_generation,flow_kind,initiating_user_id,initiating_session_hash,org_id,installation_purpose,status,checkpoint,expires_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,'active','created',$8)
			ON CONFLICT(state_hash) DO UPDATE SET updated_at=github_setup_sessions.updated_at
			RETURNING id::text,state_hash,state_generation,flow_kind,initiating_user_id::text,initiating_session_hash,org_id::text,installation_purpose,status,COALESCE(checkpoint,''),expires_at`,
			stateHash, generation, flowKind, userID, sessionHash, orgID, purpose, expiresAt).Scan(&value.ID, &value.StateHash, &value.StateGeneration, &value.FlowKind, &value.UserID, &value.SessionHash,
			&value.OrgID, &value.InstallationPurpose, &value.Status, &value.Checkpoint, &value.ExpiresAt); err != nil {
			return err
		}
		return insertAuditEvent(ctx, tx, audit.OrgID, audit.Event, githubSetupAuditAction(value.FlowKind, "started"), "github_setup_session", value.ID, nil, githubSetupAuditSnapshot(value, audit.Source))
	})
	if err != nil {
		return GitHubSetupSession{}, mapMutationError("begin github setup session", err)
	}
	return value, nil
}

type BeginGitHubSetupSessionParams struct {
	StateHash, Generation, FlowKind, UserID, SessionHash string
	OrgID, Purpose                                       *string
	StartedAt, ExpiresAt                                 time.Time
	Idempotency                                          IdempotencyClaimParams
	Completion                                           CompleteIdempotencyParams
	Audit                                                GitHubAuditContext
	BeforeCommit                                         func() error
	RequiredAppState                                     string
}

func (s *Store) BeginGitHubSetupSessionIdempotent(ctx context.Context, params BeginGitHubSetupSessionParams) (GitHubSetupSession, IdempotencyResult, error) {
	var value GitHubSetupSession
	var result IdempotencyResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		result, err = claimIdempotency(ctx, tx, params.Idempotency)
		if err != nil || result.Disposition != IdempotencyClaimed {
			return err
		}
		if params.RequiredAppState != "" {
			var appState, appSlug string
			err := tx.QueryRow(ctx, `SELECT state,COALESCE(app_slug,'') FROM github_app WHERE singleton=true`).Scan(&appState, &appSlug)
			if params.RequiredAppState == "not_ready" {
				if err == nil && appState == "ready" {
					return ErrAlreadyExists
				}
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			} else if err != nil || appState != params.RequiredAppState || appSlug == "" {
				return ErrFailedPrecondition
			}
		}
		if params.FlowKind == "app_setup" {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('github_app_setup'))`); err != nil {
				return fmt.Errorf("lock github app setup: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE github_setup_sessions SET status='expired',checkpoint='completed',failure_code='github_setup_expired',failure_message='Setup session expired before completion.',updated_at=now()
				WHERE flow_kind='app_setup' AND status='active' AND expires_at <= now()`); err != nil {
				return fmt.Errorf("expire github app setup sessions: %w", err)
			}
		}
		err = tx.QueryRow(ctx, `INSERT INTO github_setup_sessions(state_hash,state_generation,flow_kind,initiating_user_id,initiating_session_hash,org_id,installation_purpose,status,checkpoint,expires_at,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,'active','created',$8,$9,$9)
			RETURNING id::text,state_hash,state_generation,flow_kind,initiating_user_id::text,initiating_session_hash,org_id::text,installation_purpose,status,COALESCE(checkpoint,''),expires_at`,
			params.StateHash, params.Generation, params.FlowKind, params.UserID, params.SessionHash, params.OrgID, params.Purpose, params.ExpiresAt, params.StartedAt).Scan(&value.ID, &value.StateHash, &value.StateGeneration, &value.FlowKind, &value.UserID, &value.SessionHash,
			&value.OrgID, &value.InstallationPurpose, &value.Status, &value.Checkpoint, &value.ExpiresAt)
		if err != nil {
			return fmt.Errorf("begin github setup session: %w", err)
		}
		params.Completion.Owner = params.Idempotency.Owner
		if err := completeIdempotency(ctx, tx, params.Completion); err != nil {
			return err
		}
		after := githubSetupAuditSnapshot(value, params.Audit.Source)
		if err := insertAuditEvent(ctx, tx, params.Audit.OrgID, params.Audit.Event, githubSetupAuditAction(value.FlowKind, "started"), "github_setup_session", value.ID, nil, after); err != nil {
			return err
		}
		if params.BeforeCommit != nil {
			return params.BeforeCommit()
		}
		return nil
	})
	return value, result, err
}

func (s *Store) RedirectGitHubSetupSession(ctx context.Context, stateHash, userID, sessionHash, flowKind string, now time.Time, audit GitHubAuditContext) (GitHubSetupSession, error) {
	var value GitHubSetupSession
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE github_setup_sessions SET checkpoint='redirected',updated_at=$5
		WHERE state_hash=$1 AND initiating_user_id=$2 AND initiating_session_hash=$3 AND flow_kind=$4
		  AND status='active' AND checkpoint='created' AND expires_at>$5
		RETURNING id::text,state_hash,state_generation,flow_kind,initiating_user_id::text,initiating_session_hash,org_id::text,installation_purpose,status,COALESCE(checkpoint,''),expires_at`,
			stateHash, userID, sessionHash, flowKind, now).Scan(&value.ID, &value.StateHash, &value.StateGeneration, &value.FlowKind, &value.UserID, &value.SessionHash,
			&value.OrgID, &value.InstallationPurpose, &value.Status, &value.Checkpoint, &value.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFailedPrecondition
		}
		if err != nil {
			return err
		}
		return insertAuditEvent(ctx, tx, audit.OrgID, audit.Event, githubSetupAuditAction(value.FlowKind, "checkpointed"), "github_setup_session", value.ID, nil, githubSetupAuditSnapshot(value, audit.Source))
	})
	return value, err
}

func (s *Store) GitHubSetupSessionByState(ctx context.Context, stateHash, flowKind string, now time.Time) (GitHubSetupSession, error) {
	var value GitHubSetupSession
	err := s.db.QueryRow(ctx, `SELECT id::text,state_hash,state_generation,flow_kind,initiating_user_id::text,initiating_session_hash,
		org_id::text,installation_purpose,status,COALESCE(checkpoint,''),expires_at FROM github_setup_sessions
		WHERE state_hash=$1 AND flow_kind=$2 AND status='active' AND checkpoint='redirected' AND expires_at>$3`, stateHash, flowKind, now).Scan(
		&value.ID, &value.StateHash, &value.StateGeneration, &value.FlowKind, &value.UserID, &value.SessionHash,
		&value.OrgID, &value.InstallationPurpose, &value.Status, &value.Checkpoint, &value.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitHubSetupSession{}, ErrFailedPrecondition
	}
	return value, err
}

func (s *Store) ConsumeGitHubSetupSession(ctx context.Context, stateHash, userID, sessionHash, flowKind string, now time.Time, audit GitHubAuditContext) (GitHubSetupSession, error) {
	var value GitHubSetupSession
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE github_setup_sessions SET status='consumed',checkpoint='callback_validated',consumed_at=$5,updated_at=$5
			WHERE state_hash=$1 AND initiating_user_id=$2 AND initiating_session_hash=$3 AND flow_kind=$4 AND status='active' AND checkpoint='redirected' AND expires_at>$5
			RETURNING id::text,state_hash,state_generation,flow_kind,initiating_user_id::text,initiating_session_hash,org_id::text,installation_purpose,status,COALESCE(checkpoint,''),expires_at`,
			stateHash, userID, sessionHash, flowKind, now).Scan(&value.ID, &value.StateHash, &value.StateGeneration, &value.FlowKind, &value.UserID, &value.SessionHash,
			&value.OrgID, &value.InstallationPurpose, &value.Status, &value.Checkpoint, &value.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFailedPrecondition
		}
		if err != nil {
			return err
		}
		return insertAuditEvent(ctx, tx, audit.OrgID, audit.Event, githubSetupAuditAction(value.FlowKind, "checkpointed"), "github_setup_session", value.ID, nil, githubSetupAuditSnapshot(value, audit.Source))
	})
	return value, err
}

func (s *Store) ValidateGitHubSetupSession(ctx context.Context, stateHash, userID, sessionHash, flowKind string, now time.Time) error {
	var persisted, status string
	var expiresAt time.Time
	err := s.db.QueryRow(ctx, `SELECT state_hash,status,expires_at FROM github_setup_sessions WHERE state_hash=$1 AND initiating_user_id=$2 AND initiating_session_hash=$3
		AND flow_kind=$4`, stateHash, userID, sessionHash, flowKind).Scan(&persisted, &status, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFailedPrecondition
	}
	if err != nil {
		return err
	}
	if status != "active" || !expiresAt.After(now) {
		return ErrFailedPrecondition
	}
	return nil
}

func (s *Store) StartGitHubAppExchange(ctx context.Context, sessionID string, audit GitHubAuditContext) error {
	return s.mutateGitHubApp(ctx, sessionID, audit, "github_app.setup_checkpointed", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO github_app(singleton,state,checkpoint,setup_session_id) VALUES(true,'exchanging','exchange_started',$1)
			ON CONFLICT(singleton) DO UPDATE SET state='exchanging',checkpoint='exchange_started',setup_session_id=$1,failure_code=NULL,failure_message=NULL,updated_at=now()
			WHERE github_app.state <> 'ready'`, sessionID)
		if err != nil {
			return err
		}
		return rowsAffected(tag, ErrFailedPrecondition)
	})
}

type GitHubAppMetadata struct {
	AppID, OwnerID                             int64
	Slug, Name, HTMLURL, OwnerLogin, OwnerType string
}

func (s *Store) RecordGitHubAppMetadata(ctx context.Context, sessionID string, m GitHubAppMetadata, audit GitHubAuditContext) error {
	return s.mutateGitHubApp(ctx, sessionID, audit, "github_app.setup_checkpointed", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE github_app SET github_app_id=$2,app_slug=$3,app_name=$4,app_url=$5,owner_id=$6,owner_login=$7,owner_type=$8,
		state='secret_pending',checkpoint='metadata_received',updated_at=now() WHERE singleton=true AND setup_session_id=$1`, sessionID, m.AppID, m.Slug, m.Name, m.HTMLURL, m.OwnerID, m.OwnerLogin, m.OwnerType)
		if err != nil {
			return err
		}
		return rowsAffected(tag, ErrFailedPrecondition)
	})
}

func (s *Store) MarkGitHubAppSecretWrite(ctx context.Context, sessionID string, written bool, audit GitHubAuditContext) error {
	checkpoint := "secret_write_started"
	if written {
		checkpoint = "secret_written"
	}
	return s.mutateGitHubApp(ctx, sessionID, audit, "github_app.setup_checkpointed", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE github_app SET checkpoint=$2,updated_at=now() WHERE singleton=true AND setup_session_id=$1`, sessionID, checkpoint)
		if err != nil {
			return err
		}
		return rowsAffected(tag, ErrFailedPrecondition)
	})
}

func (s *Store) ActivateGitHubApp(ctx context.Context, sessionID, secretRef string, audit GitHubAuditContext) error {
	return s.mutateGitHubApp(ctx, sessionID, audit, "github_app.setup_ready", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE github_app SET credential_secret_ref=$2,state='ready',checkpoint='ready_committed',ready_at=now(),updated_at=now()
		WHERE singleton=true AND setup_session_id=$1 AND checkpoint='secret_written'`, sessionID, secretRef)
		if err != nil {
			return err
		}
		return rowsAffected(tag, ErrFailedPrecondition)
	})
}

func (s *Store) FailGitHubAppSetup(ctx context.Context, sessionID, code, message string, orphan bool, audit GitHubAuditContext) error {
	state := "failed"
	checkpoint := "exchange_started"
	if orphan {
		state, checkpoint = "orphaned", "orphan_recorded"
	}
	if len(message) > 8192 {
		message = message[:8192]
	}
	action := "github_app.setup_failed"
	if orphan {
		action = "github_app.setup_orphaned"
	}
	return s.mutateGitHubApp(ctx, sessionID, audit, action, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE github_app SET state=$2,checkpoint=$3,failure_code=$4,failure_message=$5,
		orphan_cleanup_url=CASE WHEN github_app_id IS NULL THEN NULL ELSE app_url END,orphaned_at=CASE WHEN $2='orphaned' THEN now() ELSE NULL END,updated_at=now()
		WHERE singleton=true AND setup_session_id=$1 AND state NOT IN ('failed','orphaned')`, sessionID, state, checkpoint, code, message)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNoGitHubMutation
		}
		return nil
	})
}

func (s *Store) MarkGitHubAppUnavailable(ctx context.Context, sessionID, code, message string, audit GitHubAuditContext) error {
	if len(message) > 8192 {
		message = message[:8192]
	}
	return s.mutateGitHubApp(ctx, sessionID, audit, "github_app.setup_failed", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE github_app SET state='failed',checkpoint='orphan_recorded',credential_secret_ref=NULL,ready_at=NULL,
		failure_code=$2,failure_message=$3,updated_at=now() WHERE singleton=true AND setup_session_id=$1`, sessionID, code, message)
		if err != nil {
			return err
		}
		return rowsAffected(tag, ErrFailedPrecondition)
	})
}

var errNoGitHubMutation = errors.New("no github mutation")

func (s *Store) mutateGitHubApp(ctx context.Context, sessionID string, audit GitHubAuditContext, action string, mutate func(pgx.Tx) error) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if audit.OrgID == "" {
			if err := tx.QueryRow(ctx, `SELECT users.org_id::text FROM github_setup_sessions session JOIN users ON users.id=session.initiating_user_id WHERE session.id=$1`, sessionID).Scan(&audit.OrgID); err != nil {
				return err
			}
		}
		before, _ := githubAppAuditSnapshotTx(ctx, tx, sessionID, audit.Source)
		if err := mutate(tx); err != nil {
			if errors.Is(err, errNoGitHubMutation) {
				return nil
			}
			return err
		}
		after, err := githubAppAuditSnapshotTx(ctx, tx, sessionID, audit.Source)
		if err != nil {
			return err
		}
		return insertAuditEvent(ctx, tx, audit.OrgID, audit.Event, action, "github_app", sessionID, before, after)
	})
}

func githubAppAuditSnapshotTx(ctx context.Context, tx pgx.Tx, sessionID, source string) (json.RawMessage, error) {
	var state, checkpoint string
	var appID, ownerID *int64
	var slug, name, appURL, ownerLogin, ownerType, failureCode *string
	err := tx.QueryRow(ctx, `SELECT state,COALESCE(checkpoint,''),github_app_id,owner_id,app_slug,app_name,app_url,owner_login,owner_type,failure_code FROM github_app WHERE singleton=true AND setup_session_id=$1`, sessionID).
		Scan(&state, &checkpoint, &appID, &ownerID, &slug, &name, &appURL, &ownerLogin, &ownerType, &failureCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"state": state, "checkpoint": checkpoint, "app_id": appID, "owner_id": ownerID, "slug": slug, "name": name, "html_url": appURL, "owner_login": ownerLogin, "owner_type": ownerType, "failure_code": failureCode, "source": source})
}

func githubSetupAuditSnapshot(value GitHubSetupSession, source string) json.RawMessage {
	encoded, _ := json.Marshal(map[string]any{"flow_kind": value.FlowKind, "status": value.Status, "checkpoint": value.Checkpoint, "expires_at": value.ExpiresAt, "source": source})
	return encoded
}

func githubSetupAuditAction(flowKind, stage string) string {
	if flowKind == "installation" {
		return "github_installation.setup_" + stage
	}
	return "github_app.setup_" + stage
}

func (s *Store) UpsertGitHubInstallation(ctx context.Context, orgID string, installation GitHubInstallation, audit GitHubAuditContext, setupSessionID string) (GitHubInstallation, error) {
	var value GitHubInstallation
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := upsertGitHubInstallation(ctx, tx, orgID, installation, &value)
		if err != nil {
			return err
		}
		after := githubInstallationAuditSnapshot(value, audit.Source, setupSessionID)
		return insertAuditEvent(ctx, tx, orgID, audit.Event, "github_installation.claimed", "github_installation", value.ID, nil, after)
	})
	return value, err
}

func upsertGitHubInstallation(ctx context.Context, tx pgx.Tx, orgID string, installation GitHubInstallation, value *GitHubInstallation) error {
	err := scanGitHubInstallation(tx.QueryRow(ctx, `INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,
		suspended_at,last_api_confirmed_generation,last_reconciled_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,1,now())
		ON CONFLICT(github_installation_id) DO UPDATE SET account_login=EXCLUDED.account_login,account_type=EXCLUDED.account_type,repository_selection=EXCLUDED.repository_selection,
		access_state=EXCLUDED.access_state,suspended_at=EXCLUDED.suspended_at,last_api_confirmed_generation=github_installations.reconciliation_generation,last_reconciled_at=now(),updated_at=now()
		WHERE github_installations.org_id=EXCLUDED.org_id AND github_installations.account_id=EXCLUDED.account_id AND github_installations.access_state<>'deleted'
		RETURNING id::text,github_installation_id,org_id::text,account_id,account_login,account_type,repository_selection,access_state,suspended_at,deleted_at,last_reconciled_at,created_at,updated_at`,
		installation.GitHubInstallationID, orgID, installation.AccountID, installation.AccountLogin, installation.AccountType, installation.RepositorySelection,
		installation.AccessState, installation.SuspendedAt), value)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: GitHub installation is already claimed", ErrAlreadyExists)
	}
	return err
}

type GitHubInstallation struct {
	ID                                                                 string
	GitHubInstallationID, AccountID                                    int64
	OrgID, AccountLogin, AccountType, RepositorySelection, AccessState string
	SuspendedAt, DeletedAt, LastReconciledAt                           *time.Time
	CreatedAt, UpdatedAt                                               time.Time
}

const githubInstallationSelect = `SELECT id::text,github_installation_id,org_id::text,account_id,account_login,account_type,
	repository_selection,access_state,suspended_at,deleted_at,last_reconciled_at,created_at,updated_at FROM github_installations`

func scanGitHubInstallation(row rowScanner, value *GitHubInstallation) error {
	return row.Scan(&value.ID, &value.GitHubInstallationID, &value.OrgID, &value.AccountID, &value.AccountLogin, &value.AccountType,
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

func markGitHubInstallationDeleted(ctx context.Context, tx pgx.Tx, orgID string, id int64, value *GitHubInstallation) error {
	err := scanGitHubInstallation(tx.QueryRow(ctx, `UPDATE github_installations SET access_state='deleted',suspended_at=NULL,deleted_at=COALESCE(deleted_at,now()),
		last_api_confirmed_generation=reconciliation_generation,last_reconciled_at=now(),updated_at=now()
		WHERE org_id=$1 AND github_installation_id=$2 AND access_state<>'deleted'
		RETURNING id::text,github_installation_id,org_id::text,account_id,account_login,account_type,repository_selection,access_state,suspended_at,deleted_at,last_reconciled_at,created_at,updated_at`, orgID, id), value)
	if errors.Is(err, pgx.ErrNoRows) {
		return scanGitHubInstallation(tx.QueryRow(ctx, githubInstallationSelect+` WHERE org_id=$1 AND github_installation_id=$2`, orgID, id), value)
	}
	return err
}

type SaveGitHubInstallationIdempotentParams struct {
	OrgID        string
	Installation GitHubInstallation
	Idempotency  IdempotencyClaimParams
	Completion   CompleteIdempotencyParams
	Audit        GitHubAuditContext
	BeforeCommit func() error
}

func (s *Store) SaveGitHubInstallationIdempotent(ctx context.Context, params SaveGitHubInstallationIdempotentParams) (GitHubInstallation, IdempotencyResult, error) {
	var value GitHubInstallation
	var result IdempotencyResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		result.Disposition = IdempotencyClaimed
		var err error
		var before GitHubInstallation
		if err := scanGitHubInstallation(tx.QueryRow(ctx, githubInstallationSelect+` WHERE org_id=$1 AND github_installation_id=$2 FOR UPDATE`, params.OrgID, params.Installation.GitHubInstallationID), &before); err != nil {
			return resourceQueryError("lock github installation", err)
		}
		if params.Installation.AccessState == "deleted" {
			err = markGitHubInstallationDeleted(ctx, tx, params.OrgID, params.Installation.GitHubInstallationID, &value)
		} else {
			err = upsertGitHubInstallation(ctx, tx, params.OrgID, params.Installation, &value)
		}
		if err != nil {
			return err
		}
		body, _ := json.Marshal(value)
		params.Completion.Body = body
		params.Completion.Owner = params.Idempotency.Owner
		if err := completeIdempotency(ctx, tx, params.Completion); err != nil {
			return err
		}
		action := "github_installation.refreshed"
		switch {
		case value.AccessState == "deleted":
			action = "github_installation.deleted"
		case value.AccessState == "suspended":
			action = "github_installation.suspended"
		case before.AccessState != "active" && value.AccessState == "active":
			action = "github_installation.restored"
		}
		if err := insertAuditEvent(ctx, tx, params.OrgID, params.Audit.Event, action, "github_installation", value.ID,
			githubInstallationAuditSnapshot(before, params.Audit.Source, ""), githubInstallationAuditSnapshot(value, params.Audit.Source, "")); err != nil {
			return err
		}
		if params.BeforeCommit != nil {
			return params.BeforeCommit()
		}
		return nil
	})
	return value, result, err
}

func githubInstallationAuditSnapshot(value GitHubInstallation, source, setupSessionID string) json.RawMessage {
	encoded, _ := json.Marshal(map[string]any{"github_installation_id": value.GitHubInstallationID, "account_id": value.AccountID, "account_login": value.AccountLogin,
		"account_type": value.AccountType, "repository_selection": value.RepositorySelection, "access_state": value.AccessState, "suspended_at": value.SuspendedAt,
		"deleted_at": value.DeletedAt, "setup_session_id": setupSessionID, "source": source})
	return encoded
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
