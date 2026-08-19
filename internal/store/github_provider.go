package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
		access_state=EXCLUDED.access_state,suspended_at=EXCLUDED.suspended_at,last_api_confirmed_generation=github_installations.reconciliation_generation,
		repository_reconciliation_generation=github_installations.repository_reconciliation_generation + CASE
			WHEN github_installations.repository_selection IS DISTINCT FROM EXCLUDED.repository_selection
			  OR github_installations.access_state IS DISTINCT FROM EXCLUDED.access_state THEN 1 ELSE 0 END,
		last_reconciled_at=now(),updated_at=now()
		WHERE github_installations.org_id=EXCLUDED.org_id AND github_installations.account_id=EXCLUDED.account_id AND github_installations.access_state<>'deleted'
		RETURNING id::text,github_installation_id,org_id::text,account_id,account_login,account_type,repository_selection,access_state,repository_reconciliation_generation,suspended_at,deleted_at,last_reconciled_at,created_at,updated_at`,
		installation.GitHubInstallationID, orgID, installation.AccountID, installation.AccountLogin, installation.AccountType, installation.RepositorySelection,
		installation.AccessState, installation.SuspendedAt), value)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: GitHub installation is already claimed", ErrAlreadyExists)
	}
	return err
}

type GitHubInstallation struct {
	ID                                                                  string
	GitHubInstallationID, AccountID, RepositoryReconciliationGeneration int64
	OrgID, AccountLogin, AccountType, RepositorySelection, AccessState  string
	SuspendedAt, DeletedAt, LastReconciledAt                            *time.Time
	CreatedAt, UpdatedAt                                                time.Time
}

const githubInstallationSelect = `SELECT id::text,github_installation_id,org_id::text,account_id,account_login,account_type,
	repository_selection,access_state,repository_reconciliation_generation,suspended_at,deleted_at,last_reconciled_at,created_at,updated_at FROM github_installations`

func scanGitHubInstallation(row rowScanner, value *GitHubInstallation) error {
	return row.Scan(&value.ID, &value.GitHubInstallationID, &value.OrgID, &value.AccountID, &value.AccountLogin, &value.AccountType,
		&value.RepositorySelection, &value.AccessState, &value.RepositoryReconciliationGeneration, &value.SuspendedAt, &value.DeletedAt, &value.LastReconciledAt,
		&value.CreatedAt, &value.UpdatedAt)
}

func (s *Store) GetGitHubInstallation(ctx context.Context, orgID string, id int64) (GitHubInstallation, error) {
	var value GitHubInstallation
	err := scanGitHubInstallation(s.db.QueryRow(ctx, githubInstallationSelect+` WHERE org_id=$1 AND github_installation_id=$2`, orgID, id), &value)
	return value, resourceQueryError("get github installation", err)
}

func (s *Store) GetGitHubInstallationByID(ctx context.Context, orgID, id string) (GitHubInstallation, error) {
	var value GitHubInstallation
	err := scanGitHubInstallation(s.db.QueryRow(ctx, githubInstallationSelect+` WHERE org_id=$1 AND id=$2`, orgID, id), &value)
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
		last_api_confirmed_generation=reconciliation_generation,repository_reconciliation_generation=repository_reconciliation_generation+1,last_reconciled_at=now(),updated_at=now()
		WHERE org_id=$1 AND github_installation_id=$2 AND access_state<>'deleted'
		RETURNING id::text,github_installation_id,org_id::text,account_id,account_login,account_type,repository_selection,access_state,repository_reconciliation_generation,suspended_at,deleted_at,last_reconciled_at,created_at,updated_at`, orgID, id), value)
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
	GitHubRepositoryID, GitHubInstallationID                                                          int64
	ReconciliationGeneration                                                                          int64
	OrgID, InstallationID, NodeID, OwnerLogin, Name, FullName, Visibility, DefaultBranch, AccessState string
	Archived                                                                                          bool
	LastReconciledAt                                                                                  *time.Time
	CreatedAt, UpdatedAt                                                                              time.Time
}

func (s *Store) GetGitHubRepository(ctx context.Context, orgID string, repositoryID int64) (GitHubRepository, error) {
	var value GitHubRepository
	err := scanGitHubRepository(s.db.QueryRow(ctx, githubRepositorySelect+` WHERE repository.org_id=$1 AND repository.github_repository_id=$2`, orgID, repositoryID), &value)
	return value, resourceQueryError("get github repository", err)
}

func (s *Store) UpsertGitHubRepositories(ctx context.Context, orgID string, installation GitHubInstallation, repositories []GitHubRepository, audit *AuditEvent) ([]GitHubRepository, int64, error) {
	repositories = append([]GitHubRepository(nil), repositories...)
	sort.Slice(repositories, func(i, j int) bool {
		return repositories[i].GitHubRepositoryID < repositories[j].GitHubRepositoryID
	})
	nodeIDs := make([]string, 0, len(repositories))
	for index, repository := range repositories {
		if repository.GitHubRepositoryID <= 0 || repository.NodeID == "" {
			return nil, 0, ErrInvalidArgument
		}
		if index > 0 && repositories[index-1].GitHubRepositoryID == repository.GitHubRepositoryID {
			return nil, 0, ErrInvalidArgument
		}
		nodeIDs = append(nodeIDs, repository.NodeID)
	}
	sort.Strings(nodeIDs)
	for index := 1; index < len(nodeIDs); index++ {
		if nodeIDs[index-1] == nodeIDs[index] {
			return nil, 0, ErrInvalidArgument
		}
	}

	values := make([]GitHubRepository, 0, len(repositories))
	var generation int64
	var identityMismatch bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if installation.OrgID != orgID {
			return ErrFailedPrecondition
		}
		repositoryIDs := make([]int64, len(repositories))
		for index, repository := range repositories {
			repositoryIDs[index] = repository.GitHubRepositoryID
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cyclopus:github_repository:id:' || $1, 0))`, fmt.Sprint(repository.GitHubRepositoryID)); err != nil {
				return err
			}
		}
		for _, nodeID := range nodeIDs {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cyclopus:github_repository:node:' || $1, 0))`, nodeID); err != nil {
				return err
			}
		}

		existingByID := make(map[int64]GitHubRepository, len(repositories))
		existingByNodeID := make(map[string]GitHubRepository, len(repositories))
		rows, err := tx.Query(ctx, githubRepositorySelect+` WHERE repository.github_repository_id=ANY($1) OR repository.node_id=ANY($2)
			ORDER BY repository.github_repository_id`, repositoryIDs, nodeIDs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var existing GitHubRepository
			if err := scanGitHubRepository(rows, &existing); err != nil {
				rows.Close()
				return err
			}
			existingByID[existing.GitHubRepositoryID] = existing
			existingByNodeID[existing.NodeID] = existing
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		involvedInstallationSet := map[int64]struct{}{installation.GitHubInstallationID: {}}
		for _, repository := range repositories {
			if before, ok := existingByID[repository.GitHubRepositoryID]; ok {
				involvedInstallationSet[before.GitHubInstallationID] = struct{}{}
			}
		}
		installationIDs := make([]int64, 0, len(involvedInstallationSet))
		for installationID := range involvedInstallationSet {
			installationIDs = append(installationIDs, installationID)
		}
		sort.Slice(installationIDs, func(i, j int) bool { return installationIDs[i] < installationIDs[j] })
		rows, err = tx.Query(ctx, `SELECT github_installation_id,org_id::text,access_state FROM github_installations
			WHERE github_installation_id=ANY($1) ORDER BY github_installation_id FOR UPDATE`, installationIDs)
		if err != nil {
			return err
		}
		states := make(map[int64]string, len(installationIDs))
		for rows.Next() {
			var id int64
			var lockedOrgID, state string
			if err := rows.Scan(&id, &lockedOrgID, &state); err != nil {
				rows.Close()
				return err
			}
			if lockedOrgID != orgID {
				rows.Close()
				return ErrFailedPrecondition
			}
			states[id] = state
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(states) != len(installationIDs) || states[installation.GitHubInstallationID] != "active" {
			return ErrFailedPrecondition
		}
		rows, err = tx.Query(ctx, githubRepositorySelect+` WHERE repository.github_repository_id=ANY($1) OR repository.node_id=ANY($2)
			ORDER BY repository.github_repository_id FOR UPDATE OF repository`, repositoryIDs, nodeIDs)
		if err != nil {
			return err
		}
		lockedCount := 0
		for rows.Next() {
			var locked GitHubRepository
			if err := scanGitHubRepository(rows, &locked); err != nil {
				rows.Close()
				return err
			}
			before, ok := existingByID[locked.GitHubRepositoryID]
			if !ok || before.GitHubInstallationID != locked.GitHubInstallationID || before.NodeID != locked.NodeID {
				rows.Close()
				return ErrFailedPrecondition
			}
			lockedCount++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if lockedCount != len(existingByID) {
			return ErrFailedPrecondition
		}

		for _, repository := range repositories {
			before, exists := existingByID[repository.GitHubRepositoryID]
			if exists && before.NodeID != repository.NodeID {
				after, _ := json.Marshal(map[string]any{"github_repository_id": repository.GitHubRepositoryID, "mismatch": "node_id"})
				if err := insertAuditEvent(ctx, tx, orgID, audit, "github_repository.identity_mismatch", "github_installation", installation.ID, nil, after); err != nil {
					return err
				}
				identityMismatch = true
				break
			}
			if nodeOwner, ok := existingByNodeID[repository.NodeID]; ok && nodeOwner.GitHubRepositoryID != repository.GitHubRepositoryID {
				after, _ := json.Marshal(map[string]any{"github_repository_id": repository.GitHubRepositoryID, "mismatch": "numeric_id"})
				if err := insertAuditEvent(ctx, tx, orgID, audit, "github_repository.identity_mismatch", "github_installation", installation.ID, nil, after); err != nil {
					return err
				}
				identityMismatch = true
				break
			}
			if exists && before.GitHubInstallationID != installation.GitHubInstallationID && states[before.GitHubInstallationID] == "active" {
				return ErrFailedPrecondition
			}
		}
		if identityMismatch {
			return tx.QueryRow(ctx, `SELECT repository_reconciliation_generation FROM github_installations
				WHERE org_id=$1 AND github_installation_id=$2`, orgID, installation.GitHubInstallationID).Scan(&generation)
		}

		mutatedInstallations := make(map[int64]struct{})
		for _, repository := range repositories {
			before, exists := existingByID[repository.GitHubRepositoryID]
			var value GitHubRepository
			err = scanGitHubRepository(tx.QueryRow(ctx, `WITH saved AS (INSERT INTO github_repositories
				(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,archived,default_branch,access_state,last_api_confirmed_generation,last_reconciled_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'active',1,now())
				ON CONFLICT(github_repository_id) DO UPDATE SET owner_login=EXCLUDED.owner_login,name=EXCLUDED.name,
				full_name=EXCLUDED.full_name,visibility=EXCLUDED.visibility,archived=EXCLUDED.archived,default_branch=EXCLUDED.default_branch,
					github_installation_id=EXCLUDED.github_installation_id,access_state=EXCLUDED.access_state,
					reconciliation_generation=github_repositories.reconciliation_generation+CASE WHEN
						github_repositories.github_installation_id IS DISTINCT FROM EXCLUDED.github_installation_id
						OR github_repositories.owner_login IS DISTINCT FROM EXCLUDED.owner_login
						OR github_repositories.name IS DISTINCT FROM EXCLUDED.name
						OR github_repositories.full_name IS DISTINCT FROM EXCLUDED.full_name
						OR github_repositories.visibility IS DISTINCT FROM EXCLUDED.visibility
						OR github_repositories.archived IS DISTINCT FROM EXCLUDED.archived
						OR github_repositories.default_branch IS DISTINCT FROM EXCLUDED.default_branch
						OR github_repositories.access_state IS DISTINCT FROM EXCLUDED.access_state THEN 1 ELSE 0 END,
					last_api_confirmed_generation=github_repositories.reconciliation_generation+CASE WHEN
						github_repositories.github_installation_id IS DISTINCT FROM EXCLUDED.github_installation_id
						OR github_repositories.owner_login IS DISTINCT FROM EXCLUDED.owner_login
						OR github_repositories.name IS DISTINCT FROM EXCLUDED.name
						OR github_repositories.full_name IS DISTINCT FROM EXCLUDED.full_name
						OR github_repositories.visibility IS DISTINCT FROM EXCLUDED.visibility
						OR github_repositories.archived IS DISTINCT FROM EXCLUDED.archived
						OR github_repositories.default_branch IS DISTINCT FROM EXCLUDED.default_branch
						OR github_repositories.access_state IS DISTINCT FROM EXCLUDED.access_state THEN 1 ELSE 0 END,
					last_reconciled_at=now(),updated_at=CASE
					WHEN github_repositories.github_installation_id IS DISTINCT FROM EXCLUDED.github_installation_id
					  OR github_repositories.owner_login IS DISTINCT FROM EXCLUDED.owner_login
					  OR github_repositories.name IS DISTINCT FROM EXCLUDED.name
					  OR github_repositories.full_name IS DISTINCT FROM EXCLUDED.full_name
					  OR github_repositories.visibility IS DISTINCT FROM EXCLUDED.visibility
					  OR github_repositories.archived IS DISTINCT FROM EXCLUDED.archived
					  OR github_repositories.default_branch IS DISTINCT FROM EXCLUDED.default_branch
					  OR github_repositories.access_state IS DISTINCT FROM EXCLUDED.access_state
					THEN now() ELSE github_repositories.updated_at END
				WHERE github_repositories.org_id=EXCLUDED.org_id AND github_repositories.node_id=EXCLUDED.node_id
					RETURNING *) SELECT saved.github_repository_id,saved.org_id::text,saved.github_installation_id,installation.id::text,saved.node_id,saved.owner_login,saved.name,saved.full_name,saved.visibility,saved.archived,saved.default_branch,saved.access_state,saved.reconciliation_generation,saved.last_reconciled_at,saved.created_at,saved.updated_at FROM saved JOIN github_installations installation ON installation.org_id=saved.org_id AND installation.github_installation_id=saved.github_installation_id`,
				repository.GitHubRepositoryID, orgID, installation.GitHubInstallationID, repository.NodeID, repository.OwnerLogin, repository.Name,
				repository.FullName, repository.Visibility, repository.Archived, repository.DefaultBranch), &value)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrFailedPrecondition
			}
			if err != nil {
				return err
			}
			values = append(values, value)
			mutated := !exists || before.GitHubInstallationID != value.GitHubInstallationID || before.OwnerLogin != value.OwnerLogin ||
				before.Name != value.Name || before.FullName != value.FullName || before.Visibility != value.Visibility || before.Archived != value.Archived ||
				before.DefaultBranch != value.DefaultBranch || before.AccessState != value.AccessState
			if mutated {
				mutatedInstallations[value.GitHubInstallationID] = struct{}{}
				if exists && before.GitHubInstallationID != value.GitHubInstallationID {
					mutatedInstallations[before.GitHubInstallationID] = struct{}{}
				}
			}
			if exists && (before.GitHubInstallationID != value.GitHubInstallationID || before.FullName != value.FullName) {
				action := "github_repository.renamed"
				if before.GitHubInstallationID != value.GitHubInstallationID {
					action = "github_repository.transferred_rebound"
				}
				beforeJSON, _ := json.Marshal(before)
				afterJSON, _ := json.Marshal(value)
				if err := insertAuditEvent(ctx, tx, orgID, audit, action, "github_installation", installation.ID, beforeJSON, afterJSON); err != nil {
					return err
				}
			}
		}
		for installationID := range mutatedInstallations {
			if _, err := tx.Exec(ctx, `UPDATE github_installations SET repository_reconciliation_generation=repository_reconciliation_generation+1
				WHERE org_id=$1 AND github_installation_id=$2`, orgID, installationID); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, `SELECT repository_reconciliation_generation FROM github_installations
			WHERE org_id=$1 AND github_installation_id=$2`, orgID, installation.GitHubInstallationID).Scan(&generation)
	})
	if err == nil && identityMismatch {
		err = ErrFailedPrecondition
	}
	return values, generation, mapMutationError("upsert github repositories", err)
}

const githubRepositorySelect = `SELECT repository.github_repository_id,repository.org_id::text,repository.github_installation_id,installation.id::text,repository.node_id,repository.owner_login,repository.name,repository.full_name,
	repository.visibility,repository.archived,repository.default_branch,repository.access_state,repository.reconciliation_generation,repository.last_reconciled_at,repository.created_at,repository.updated_at FROM github_repositories repository
	JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id`

func scanGitHubRepository(row rowScanner, value *GitHubRepository) error {
	return row.Scan(&value.GitHubRepositoryID, &value.OrgID, &value.GitHubInstallationID, &value.InstallationID, &value.NodeID, &value.OwnerLogin,
		&value.Name, &value.FullName, &value.Visibility, &value.Archived, &value.DefaultBranch, &value.AccessState,
		&value.ReconciliationGeneration, &value.LastReconciledAt, &value.CreatedAt, &value.UpdatedAt)
}

func (s *Store) ListGitHubRepositories(ctx context.Context, orgID string, installationID int64) ([]GitHubRepository, error) {
	rows, err := s.db.Query(ctx, githubRepositorySelect+` WHERE repository.org_id=$1 AND repository.github_installation_id=$2 ORDER BY repository.created_at,repository.github_repository_id`, orgID, installationID)
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

type ImportRepositoryParams struct {
	OperationID, OrgID, ActorUserID, IdempotencyKey, RequestDigest string
	RepositoryID                                                   int64
	ExpectedInstallationID                                         string
	ExpectedGitHubInstallationID, ExpectedInstallationGeneration   int64
	ExpectedRepositoryNodeID                                       string
	ExpectedRepositoryGeneration                                   int64
	Project                                                        Project
	App                                                            App
	Environment                                                    Environment
	// RegistryOperationID/RegistryCandidateGeneration bind a first-credential
	// import to its reserved pending candidate; empty/zero for public imports
	// and for private imports reusing the active credential.
	RegistryOperationID             string
	RegistryCandidateGeneration     int64
	WorkflowRevision, BranchHeadSHA string
	Idempotency                     IdempotencyClaimParams
	Completion                      CompleteIdempotencyParams
	AuditIDs                        []string
	BeforeCommit                    func() error
}

type FailImportRepositoryParams struct {
	OperationID, OrgID, ActorUserID, AuditID, IdempotencyKey, RequestDigest string
	RepositoryID                                                            int64
	FailureCode, FailureMessage                                             string
	Completion                                                              CompleteIdempotencyParams
}

func (s *Store) FailImportGitRepository(ctx context.Context, params FailImportRepositoryParams) (ImportOperation, error) {
	var operation ImportOperation
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO import_operations(id,org_id,idempotency_key,request_digest,state,github_repository_id,failure_code,failure_message,completed_at)
			SELECT $1,$2,$3,$4,'failed',$5,$6,$7,now() FROM api_idempotency_records
			WHERE org_id=$2 AND scope=$8 AND idempotency_key=$3 AND request_hash=$4 AND status='in_progress'
			  AND lease_owner=$9 AND lease_expires_at>clock_timestamp()
			RETURNING id::text,org_id::text,state,github_repository_id,failure_code,failure_message,created_at,updated_at,completed_at`,
			params.OperationID, params.OrgID, params.IdempotencyKey, params.RequestDigest, params.RepositoryID,
			params.FailureCode, params.FailureMessage, params.Completion.Scope, params.Completion.Owner).Scan(&operation.ID,
			&operation.OrgID, &operation.State, &operation.GitHubRepositoryID, &operation.FailureCode,
			&operation.FailureMessage, &operation.CreatedAt, &operation.UpdatedAt, &operation.CompletedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrLeaseNotOwned
			}
			return mapMutationError("create failed import operation", err)
		}
		body, _ := json.Marshal(operation)
		params.Completion.Body = body
		resourceType, resourceID := "import_operation", operation.ID
		params.Completion.ResourceType, params.Completion.ResourceID = &resourceType, &resourceID
		if err := completeIdempotency(ctx, tx, params.Completion); err != nil {
			return err
		}
		after, _ := json.Marshal(operation)
		return insertAuditEvent(ctx, tx, params.OrgID, &AuditEvent{ID: params.AuditID, ActorUserID: &params.ActorUserID}, "github_import.failed", "import_operation", operation.ID, nil, after)
	})
	return operation, err
}

var importBeforeCommitHook func() error

func (s *Store) ImportGitRepository(ctx context.Context, params ImportRepositoryParams) (ImportOperation, IdempotencyResult, error) {
	var operation ImportOperation
	result := IdempotencyResult{Disposition: IdempotencyClaimed, Status: "in_progress"}
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var installationID, installationOrgID, installationState string
		var installationNumericID, installationGeneration int64
		if err := tx.QueryRow(ctx, `SELECT id::text,org_id::text,github_installation_id,access_state,repository_reconciliation_generation
			FROM github_installations WHERE org_id=$1 AND github_installation_id=$2 FOR UPDATE`, params.OrgID, params.ExpectedGitHubInstallationID).Scan(
			&installationID, &installationOrgID, &installationNumericID, &installationState, &installationGeneration); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%s: %w", "installation", ErrFailedPrecondition)
			}
			return err
		}
		if installationOrgID != params.OrgID || (params.ExpectedInstallationID != "" && installationID != params.ExpectedInstallationID) || installationNumericID != params.ExpectedGitHubInstallationID ||
			installationState != "active" || installationGeneration != params.ExpectedInstallationGeneration {
			return ErrFailedPrecondition
		}
		var repositoryOrgID, repositoryInstallationID, repositoryNodeID, repositoryAccessState string
		var repositoryInstallationNumericID, repositoryGeneration int64
		var repositoryArchived bool
		if err := tx.QueryRow(ctx, `SELECT repository.org_id::text,repository.github_installation_id,installation.id::text,repository.node_id,
			repository.access_state,repository.archived,repository.reconciliation_generation FROM github_repositories repository
			JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id
			WHERE repository.org_id=$1 AND repository.github_repository_id=$2 FOR UPDATE OF repository`, params.OrgID, params.RepositoryID).Scan(
			&repositoryOrgID, &repositoryInstallationNumericID, &repositoryInstallationID, &repositoryNodeID, &repositoryAccessState, &repositoryArchived, &repositoryGeneration); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrFailedPrecondition
			}
			return err
		}
		if repositoryOrgID != params.OrgID || (params.ExpectedInstallationID != "" && repositoryInstallationID != params.ExpectedInstallationID) || repositoryInstallationNumericID != params.ExpectedGitHubInstallationID ||
			repositoryNodeID != params.ExpectedRepositoryNodeID || repositoryAccessState != "active" || repositoryArchived || repositoryGeneration != params.ExpectedRepositoryGeneration {
			return ErrFailedPrecondition
		}
		if err := tx.QueryRow(ctx, `INSERT INTO import_operations(id,org_id,idempotency_key,request_digest,state,checkpoint,github_repository_id,lease_owner,lease_expires_at)
			SELECT $1,$2,$3,$4,'committing','request_validated',$5,$6,lease_expires_at
			FROM api_idempotency_records WHERE org_id=$2 AND scope=$7 AND idempotency_key=$3 AND request_hash=$4
			  AND status='in_progress' AND lease_owner=$6 AND lease_expires_at>clock_timestamp()
			RETURNING id::text`, params.OperationID, params.OrgID, params.IdempotencyKey, params.RequestDigest,
			params.RepositoryID, params.Idempotency.Owner, params.Idempotency.Scope).Scan(&operation.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrLeaseNotOwned
			}
			return mapMutationError("create import operation", err)
		}
		var connectionID string
		if err := tx.QueryRow(ctx, `INSERT INTO git_connections(org_id,github_repository_id,state,access_state)
			VALUES($1,$2,'active','active') ON CONFLICT(org_id,github_repository_id) DO UPDATE SET state='active',access_state='active',disconnected_at=NULL,updated_at=now()
			RETURNING id::text`, params.OrgID, params.RepositoryID).Scan(&connectionID); err != nil {
			return err
		}
		params.Project.GitConnectionID = &connectionID
		if err := tx.QueryRow(ctx, `INSERT INTO projects(id,org_id,name,slug,git_connection_id) VALUES($1,$2,$3,$4,$5) RETURNING id::text`,
			params.Project.ID, params.OrgID, params.Project.Name, params.Project.Slug, connectionID).Scan(&params.Project.ID); err != nil {
			return mapMutationError("create imported project", err)
		}
		params.App.ProjectID, params.App.GitHubWorkflowRevision = params.Project.ID, params.WorkflowRevision
		// A private import binds the org GHCR registry row: reusing an active
		// credential fences it ready, while a first create fences the reserved
		// pending candidate this import itself attached. The app then adopts
		// the registry binding atomically inside the same transaction.
		var registryID *string
		if params.App.ImageVisibility == "private" {
			var bound string
			if err := tx.QueryRow(ctx, `SELECT id::text FROM registries
				WHERE org_id=$1 AND provider='ghcr' AND registry_host='ghcr.io'
				  AND (($2='' AND state='ready' AND active_generation=$3 AND active_generation>0 AND active_credential_secret_ref IS NOT NULL
				        AND pending_generation IS NULL AND operation_id IS NULL)
					   OR ($2<>'' AND state IN ('creating','provisioning') AND pending_generation=$3 AND operation_id=NULLIF($4,'')::uuid
					       AND EXISTS (SELECT 1 FROM registry_credential_operations operation
					           WHERE operation.org_id=$1 AND operation.id=NULLIF($4,'')::uuid AND operation.registry_id=registries.id
					             AND operation.kind='import_create' AND operation.status='awaiting_import'
				             AND operation.checkpoint='source_secret_written' AND operation.candidate_generation=$3)))
				FOR UPDATE`, params.OrgID, params.RegistryOperationID, params.RegistryCandidateGeneration, params.RegistryOperationID).Scan(&bound); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrFailedPrecondition
				}
				return err
			}
			registryID = &bound
		}
		params.App.RegistryID = registryID
		// The environment's required credential generation is non-secret
		// binding evidence from the service's reservation decision; the fence
		// above guarantees it matches the registry state this transaction saw.
		if registryID != nil {
			pendingBinding := params.RegistryOperationID != "" && params.Environment.RegistryCredentialState == "pending" &&
				params.Environment.RequiredRegistryCredentialGeneration != nil && params.Environment.ReadyRegistryCredentialGeneration == nil
			readyBinding := params.RegistryOperationID == "" && params.Environment.RegistryCredentialState == "ready" &&
				params.Environment.RequiredRegistryCredentialGeneration != nil && params.Environment.ReadyRegistryCredentialGeneration != nil &&
				*params.Environment.RequiredRegistryCredentialGeneration == *params.Environment.ReadyRegistryCredentialGeneration &&
				params.Environment.RegistryCredentialCheckpoint == "activated"
			if !pendingBinding && !readyBinding {
				return fmt.Errorf("import environment credential binding incomplete: %w", ErrInvalidArgument)
			}
		} else if params.Environment.RegistryCredentialState != "" && params.Environment.RegistryCredentialState != "not_required" {
			return fmt.Errorf("import environment credential binding unexpected: %w", ErrInvalidArgument)
		}
		app := normalizeAppDefaults(params.App)
		if err := tx.QueryRow(ctx, `INSERT INTO apps (id,org_id,project_id,name,slug,hostname_label,registry_id,image_repository,container_port,health_check_path,scaling_mode,min_replicas,max_replicas,hpa_target_cpu_percent,keda_cron_schedules,keda_http_target_pending_requests,cpu_request_millicores,cpu_limit_millicores,memory_request_mib,memory_limit_mib,previews_enabled,max_preview_environments,preview_idle_ttl_hours,deploy_timeout_seconds,release_retention_count,runtime_class,github_workflow_path,github_workflow_revision,production_branch,annotation_policy,image_visibility)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31) RETURNING id::text`, appArgs(app)...).Scan(&params.App.ID); err != nil {
			return mapMutationError("create imported app", err)
		}
		credentialState := params.Environment.RegistryCredentialState
		if credentialState == "" {
			credentialState = "not_required"
		}
		if err := tx.QueryRow(ctx, `INSERT INTO environments(id,org_id,app_id,branch,kind,namespace,planned_hostname,lifecycle_status,required_registry_credential_generation,ready_registry_credential_generation,registry_credential_state,registry_credential_checkpoint)
			VALUES($1,$2,$3,$4,'production',$5,$6,'reserved',$7,$8,$9,NULLIF($10,'')) RETURNING id::text`, params.Environment.ID, params.OrgID, params.App.ID,
			params.Environment.Branch, params.Environment.Namespace, params.Environment.PlannedHostname,
			params.Environment.RequiredRegistryCredentialGeneration, params.Environment.ReadyRegistryCredentialGeneration, credentialState,
			params.Environment.RegistryCredentialCheckpoint).Scan(&params.Environment.ID); err != nil {
			return mapMutationError("create imported environment", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO environment_lifecycle_jobs(org_id,environment_id,kind,status,checkpoint,reason)
			VALUES($1,$2,'provision','queued','reserved','repository_import')`, params.OrgID, params.Environment.ID); err != nil {
			return err
		}
		var job OrchestrationJob
		if err := scanEnqueuedPush(tx.QueryRow(ctx, `SELECT result.orchestration_job_id::text,result.org_id::text,result.app_id::text,result.environment_id::text,
			result.github_repository_id::text,result.github_workflow_path,result.workflow_revision,result.dispatch_ref,result.desired_checkout_sha,result.desired_generation,
			result.idempotency_key,result.pending_github_workflow_path,result.pending_workflow_revision,result.pending_dispatch_ref,result.pending_checkout_sha,
			result.pending_desired_generation,result.pending_idempotency_key,result.orchestration_status,result.accepted_generation,result.enqueue_result
			FROM enqueue_orchestration_push($1,$2,$3,$4,$5,$6,$7,$8,$9) result`, params.OrgID, params.App.ID, params.Environment.ID,
			params.RepositoryID, params.App.GitHubWorkflowPath, params.WorkflowRevision, params.App.ProductionBranch, params.BranchHeadSHA, "import:"+params.OperationID), &job); err != nil {
			return mapEnqueueOrchestrationError(err)
		}
		now := time.Now().UTC()
		operation = ImportOperation{ID: params.OperationID, OrgID: params.OrgID, State: "completed", GitHubRepositoryID: params.RepositoryID,
			GitConnectionID: &connectionID, ProjectID: &params.Project.ID, AppID: &params.App.ID, EnvironmentID: &params.Environment.ID, OrchestrationJobID: &job.ID, CompletedAt: &now}
		if err := tx.QueryRow(ctx, `UPDATE import_operations SET state='completed',checkpoint='completed',git_connection_id=$3,project_id=$4,app_id=$5,
			environment_id=$6,orchestration_job_id=$7,lease_owner=NULL,lease_expires_at=NULL,completed_at=now(),updated_at=now()
			WHERE org_id=$1 AND id=$2 RETURNING created_at,updated_at,completed_at`, params.OrgID, params.OperationID, connectionID, params.Project.ID,
			params.App.ID, params.Environment.ID, job.ID).Scan(&operation.CreatedAt, &operation.UpdatedAt, &operation.CompletedAt); err != nil {
			return err
		}
		ids := append(append([]string(nil), params.AuditIDs...), "", "", "", "", "")
		for index, event := range []struct {
			action, kind, id string
			after            any
		}{
			{"git_connection.connected", "git_connection", connectionID, map[string]any{"github_repository_id": params.RepositoryID}},
			{"project.created", "project", params.Project.ID, params.Project}, {"app.created", "app", params.App.ID, params.App},
			{"environment.created", "environment", params.Environment.ID, params.Environment}, {"github_import.completed", "import_operation", params.OperationID, operation},
		} {
			after, _ := json.Marshal(event.after)
			if err := insertAuditEvent(ctx, tx, params.OrgID, &AuditEvent{ID: ids[index], ActorUserID: &params.ActorUserID}, event.action, event.kind, event.id, nil, after); err != nil {
				return err
			}
		}
		if importBeforeCommitHook != nil {
			if err := importBeforeCommitHook(); err != nil {
				return err
			}
		}
		if params.BeforeCommit != nil {
			if err := params.BeforeCommit(); err != nil {
				return err
			}
		}
		if params.RegistryOperationID != "" {
			tag, err := tx.Exec(ctx, `UPDATE registry_credential_operations SET status='pending',next_attempt_at=clock_timestamp(),updated_at=clock_timestamp()
					WHERE org_id=$1 AND id=NULLIF($2,'')::uuid AND kind='import_create' AND status='awaiting_import' AND checkpoint='source_secret_written'`, params.OrgID, params.RegistryOperationID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrFailedPrecondition
			}
		}
		body, _ := json.Marshal(operation)
		params.Completion.Body, params.Completion.Owner = body, params.Idempotency.Owner
		resourceType, resourceID := "import_operation", params.OperationID
		params.Completion.ResourceType, params.Completion.ResourceID = &resourceType, &resourceID
		return completeIdempotency(ctx, tx, params.Completion)
	})
	return operation, result, mapMutationError("import github repository", err)
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
