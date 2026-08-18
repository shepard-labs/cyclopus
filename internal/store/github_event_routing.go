package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type GitHubAuthoritativeInstallation struct {
	InstallationID int64
	AccountID      int64
	AccountLogin   string
	AccountType    string
	Selection      string
	Suspended      bool
}

type ReconcileGitHubEventParams struct {
	OrgID              string
	DeliveryID         string
	DeliveryLease      LeaseToken
	ExpectedGeneration int64
	Installation       GitHubAuthoritativeInstallation
	Repositories       []GitHubRepository
	Audit              *AuditEvent
}

// ReconcileGitHubEvent atomically refreshes an already-associated installation
// and its complete repository cache. It never creates an organization or
// rebinds a stable repository identity across organizations.
func (s *Store) ReconcileGitHubEvent(ctx context.Context, p ReconcileGitHubEventParams) (int64, error) {
	if p.Installation.InstallationID <= 0 || p.Installation.AccountID <= 0 || p.Installation.AccountLogin == "" ||
		(p.Installation.AccountType != "User" && p.Installation.AccountType != "Organization") ||
		(p.Installation.Selection != "all" && p.Installation.Selection != "selected") {
		return 0, ErrInvalidArgument
	}
	repositories := append([]GitHubRepository(nil), p.Repositories...)
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].GitHubRepositoryID < repositories[j].GitHubRepositoryID })
	for i, repository := range repositories {
		if repository.GitHubRepositoryID <= 0 || repository.NodeID == "" || repository.OwnerLogin == "" || repository.Name == "" || repository.FullName == "" || repository.DefaultBranch == "" {
			return 0, ErrInvalidArgument
		}
		if i > 0 && repositories[i-1].GitHubRepositoryID == repository.GitHubRepositoryID {
			return 0, ErrInvalidArgument
		}
	}

	var generation int64
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := fenceGitHubEventDelivery(ctx, tx, p.DeliveryID, p.DeliveryLease, p.OrgID); err != nil {
			return err
		}
		installationLocks := []int64{p.Installation.InstallationID}
		for _, repository := range repositories {
			var associatedInstallation int64
			err := tx.QueryRow(ctx, `SELECT github_installation_id FROM github_repositories WHERE github_repository_id=$1`, repository.GitHubRepositoryID).Scan(&associatedInstallation)
			if err == nil && associatedInstallation != p.Installation.InstallationID {
				installationLocks = append(installationLocks, associatedInstallation)
			} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		sort.Slice(installationLocks, func(i, j int) bool { return installationLocks[i] < installationLocks[j] })
		if err := lockGitHubInstallations(ctx, tx, installationLocks); err != nil {
			return err
		}
		var installationID string
		var oldState string
		var accountID int64
		var currentGeneration int64
		if err := tx.QueryRow(ctx, `SELECT id::text,access_state,account_id,reconciliation_generation FROM github_installations
				WHERE org_id=$1 AND github_installation_id=$2 FOR UPDATE`, p.OrgID, p.Installation.InstallationID).Scan(&installationID, &oldState, &accountID, &currentGeneration); err != nil {
			return resourceQueryError("lock github event installation", err)
		}
		if accountID != p.Installation.AccountID {
			return ErrConflict
		}
		if p.ExpectedGeneration > 0 && currentGeneration != p.ExpectedGeneration {
			return ErrFailedPrecondition
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('cyclopus.github_authoritative_reconcile','on',true)`); err != nil {
			return err
		}
		state := "active"
		var suspended any
		if p.Installation.Suspended {
			state, suspended = "suspended", "now"
		}
		if _, err := tx.Exec(ctx, `UPDATE github_installations SET account_login=$3,account_type=$4,repository_selection=$5,
			access_state=$6,suspended_at=CASE WHEN $7::boolean THEN COALESCE(suspended_at,clock_timestamp()) ELSE NULL END,
			deleted_at=NULL,
			reconciliation_generation=reconciliation_generation+1,last_api_confirmed_generation=reconciliation_generation+1,
			repository_reconciliation_generation=repository_reconciliation_generation+1,last_reconciled_at=clock_timestamp(),updated_at=clock_timestamp()
			WHERE org_id=$1 AND github_installation_id=$2`, p.OrgID, p.Installation.InstallationID, p.Installation.AccountLogin,
			p.Installation.AccountType, p.Installation.Selection, state, suspended != nil); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT repository_reconciliation_generation FROM github_installations
			WHERE org_id=$1 AND github_installation_id=$2`, p.OrgID, p.Installation.InstallationID).Scan(&generation); err != nil {
			return err
		}

		ids := make([]int64, len(repositories))
		invalidatedInstallations := make(map[int64]struct{})
		for i, repository := range repositories {
			ids[i] = repository.GitHubRepositoryID
			var existingOrg, existingNode, existingInstallationState string
			var existingInstallationID int64
			err := tx.QueryRow(ctx, `SELECT repository.org_id::text,repository.node_id,repository.github_installation_id,installation.access_state
				FROM github_repositories repository JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id
				WHERE repository.github_repository_id=$1 FOR UPDATE OF repository,installation`, repository.GitHubRepositoryID).
				Scan(&existingOrg, &existingNode, &existingInstallationID, &existingInstallationState)
			exists := err == nil
			if err == nil && (existingOrg != p.OrgID || existingNode != repository.NodeID) {
				return ErrConflict
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			var nodeOwnerOrg string
			var nodeOwnerID int64
			err = tx.QueryRow(ctx, `SELECT org_id::text,github_repository_id FROM github_repositories WHERE node_id=$1 FOR UPDATE`, repository.NodeID).Scan(&nodeOwnerOrg, &nodeOwnerID)
			if err == nil && (nodeOwnerOrg != p.OrgID || nodeOwnerID != repository.GitHubRepositoryID) {
				return ErrConflict
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			visibility := repository.Visibility
			if visibility == "" {
				visibility = "private"
			}
			if exists && existingInstallationID != p.Installation.InstallationID {
				if _, invalidated := invalidatedInstallations[existingInstallationID]; !invalidated {
					tag, err := tx.Exec(ctx, `UPDATE github_installations SET reconciliation_generation=reconciliation_generation+1,repository_reconciliation_generation=repository_reconciliation_generation+1,updated_at=clock_timestamp() WHERE org_id=$1 AND github_installation_id=$2`, p.OrgID, existingInstallationID)
					if err != nil {
						return err
					}
					if tag.RowsAffected() == 0 {
						return ErrConflict
					}
					invalidatedInstallations[existingInstallationID] = struct{}{}
				}
				if _, err := tx.Exec(ctx, `UPDATE github_repositories SET github_installation_id=$3,access_state='active',reconciliation_generation=reconciliation_generation+1,last_api_confirmed_generation=reconciliation_generation+1,last_reconciled_at=clock_timestamp(),updated_at=clock_timestamp() WHERE org_id=$1 AND github_repository_id=$2 AND node_id=$4`, p.OrgID, repository.GitHubRepositoryID, p.Installation.InstallationID, repository.NodeID); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE git_connections SET access_state='active',updated_at=clock_timestamp() WHERE org_id=$1 AND github_repository_id=$2`, p.OrgID, repository.GitHubRepositoryID); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO github_repositories
				(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,archived,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation,last_reconciled_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'active',1,1,clock_timestamp())
				ON CONFLICT(github_repository_id) DO UPDATE SET owner_login=EXCLUDED.owner_login,name=EXCLUDED.name,full_name=EXCLUDED.full_name,
				visibility=EXCLUDED.visibility,archived=EXCLUDED.archived,default_branch=EXCLUDED.default_branch,github_installation_id=EXCLUDED.github_installation_id,access_state='active',
				reconciliation_generation=github_repositories.reconciliation_generation+1,last_api_confirmed_generation=github_repositories.reconciliation_generation+1,
				last_reconciled_at=clock_timestamp(),updated_at=clock_timestamp()
				WHERE github_repositories.org_id=EXCLUDED.org_id AND github_repositories.node_id=EXCLUDED.node_id`,
				repository.GitHubRepositoryID, p.OrgID, p.Installation.InstallationID, repository.NodeID, repository.OwnerLogin,
				repository.Name, repository.FullName, visibility, repository.Archived, repository.DefaultBranch); err != nil {
				return mapMutationError("reconcile github event repository", err)
			}
			var repositoryGeneration int64
			if err := tx.QueryRow(ctx, `SELECT reconciliation_generation FROM github_repositories WHERE org_id=$1 AND github_repository_id=$2`, p.OrgID, repository.GitHubRepositoryID).Scan(&repositoryGeneration); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `INSERT INTO github_repository_routing_state(repository_id,org_id,repository_node_id,installation_id,repository_generation,lifecycle_generation)
				VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(repository_id) DO UPDATE SET installation_id=EXCLUDED.installation_id,repository_generation=EXCLUDED.repository_generation,lifecycle_generation=EXCLUDED.lifecycle_generation,updated_at=clock_timestamp()
				WHERE github_repository_routing_state.repository_node_id=EXCLUDED.repository_node_id
				AND github_repository_routing_state.repository_generation<=EXCLUDED.repository_generation
				AND (github_repository_routing_state.installation_id<>EXCLUDED.installation_id OR github_repository_routing_state.lifecycle_generation<=EXCLUDED.lifecycle_generation)`,
				repository.GitHubRepositoryID, p.OrgID, repository.NodeID, p.Installation.InstallationID, repositoryGeneration, currentGeneration+1)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrFailedPrecondition
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE github_repositories SET access_state='inaccessible',reconciliation_generation=reconciliation_generation+1,updated_at=clock_timestamp()
			WHERE org_id=$1 AND github_installation_id=$2 AND access_state<>'deleted' AND NOT (github_repository_id=ANY($3::bigint[]))`, p.OrgID, p.Installation.InstallationID, ids); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE git_connections connection SET access_state=CASE WHEN installation.access_state='active' AND repository.access_state='active' THEN 'active' WHEN installation.access_state='suspended' THEN 'suspended' ELSE 'inaccessible' END,updated_at=clock_timestamp()
			FROM github_repositories repository JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id
			WHERE connection.org_id=$1 AND connection.github_repository_id=repository.github_repository_id AND repository.github_installation_id=$2`, p.OrgID, p.Installation.InstallationID); err != nil {
			return err
		}
		after, _ := json.Marshal(map[string]any{"installation_id": p.Installation.InstallationID, "repository_count": len(repositories), "generation": generation, "state": state})
		return insertAuditEvent(ctx, tx, p.OrgID, p.Audit, "github_installation.webhook_reconciled", "github_installation", installationID, nil, after)
	})
	return generation, err
}

func lockGitHubInstallations(ctx context.Context, tx pgx.Tx, installationIDs []int64) error {
	for i, installationID := range installationIDs {
		if i > 0 && installationIDs[i-1] == installationID {
			continue
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cyclopus:github_installation:' || $1::bigint::text, 0))`, installationID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetGitHubEventGeneration(ctx context.Context, orgID string, installationID int64) (int64, error) {
	var generation int64
	err := s.db.QueryRow(ctx, `SELECT reconciliation_generation FROM github_installations WHERE org_id=$1 AND github_installation_id=$2`, orgID, installationID).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return generation, err
}

type RevokeGitHubEventParams struct {
	OrgID, Kind, RepositoryNodeID string
	DeliveryID                    string
	DeliveryLease                 LeaseToken
	InstallationID, RepositoryID  int64
	Audit                         *AuditEvent
}

// RevokeGitHubEvent immediately blocks routing while retaining imported metadata.
// Repeating a destructive event is successful convergence.
func (s *Store) RevokeGitHubEvent(ctx context.Context, p RevokeGitHubEventParams) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := fenceGitHubEventDelivery(ctx, tx, p.DeliveryID, p.DeliveryLease, p.OrgID); err != nil {
			return err
		}
		if err := lockGitHubInstallations(ctx, tx, []int64{p.InstallationID}); err != nil {
			return err
		}
		var targetID string
		switch p.Kind {
		case "suspend", "delete", "unverified":
			state := "suspended"
			if p.Kind == "delete" {
				state = "deleted"
			} else if p.Kind == "unverified" {
				state = "unverified_revocation"
			}
			err := tx.QueryRow(ctx, `UPDATE github_installations SET access_state=$3,
				suspended_at=CASE WHEN $3='suspended' THEN COALESCE(suspended_at,clock_timestamp()) ELSE NULL END,
				deleted_at=CASE WHEN $3='deleted' THEN COALESCE(deleted_at,clock_timestamp()) ELSE deleted_at END,
				reconciliation_generation=reconciliation_generation+1,repository_reconciliation_generation=repository_reconciliation_generation+1,updated_at=clock_timestamp()
				WHERE org_id=$1 AND github_installation_id=$2 AND (($3<>'deleted' AND $3<>'unverified_revocation') OR access_state<>'deleted') RETURNING id::text`, p.OrgID, p.InstallationID, state).Scan(&targetID)
			if errors.Is(err, pgx.ErrNoRows) {
				err = tx.QueryRow(ctx, `SELECT id::text FROM github_installations WHERE org_id=$1 AND github_installation_id=$2`, p.OrgID, p.InstallationID).Scan(&targetID)
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE git_connections connection SET access_state=CASE
						WHEN installation.access_state='deleted' OR repository.access_state='deleted' THEN 'deleted'
						WHEN installation.access_state='suspended' THEN 'suspended'
						WHEN installation.access_state='unverified_revocation' OR repository.access_state='unverified_revocation' THEN 'unverified_revocation'
						WHEN repository.access_state='inaccessible' THEN 'inaccessible'
						ELSE 'active' END,updated_at=clock_timestamp()
					FROM github_repositories repository
					JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id
					WHERE connection.org_id=$1 AND connection.github_repository_id=repository.github_repository_id AND repository.github_installation_id=$2`, p.OrgID, p.InstallationID)
			if err != nil {
				return err
			}
		case "remove", "unverified_remove":
			state := "inaccessible"
			if p.Kind == "unverified_remove" {
				state = "unverified_revocation"
			}
			err := tx.QueryRow(ctx, `WITH changed AS (UPDATE github_repositories SET access_state=$5,reconciliation_generation=reconciliation_generation+1,updated_at=clock_timestamp()
					WHERE org_id=$1 AND github_installation_id=$2 AND github_repository_id=$3 AND node_id=$4
					  AND NOT ($5='unverified_revocation' AND access_state='deleted') RETURNING github_installation_id)
					SELECT installation.id::text FROM changed JOIN github_installations installation ON installation.org_id=$1 AND installation.github_installation_id=changed.github_installation_id`, p.OrgID, p.InstallationID, p.RepositoryID, p.RepositoryNodeID, state).Scan(&targetID)
			if errors.Is(err, pgx.ErrNoRows) {
				err = tx.QueryRow(ctx, `SELECT installation.id::text FROM github_repositories repository
						JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id
						WHERE repository.org_id=$1 AND repository.github_installation_id=$2 AND repository.github_repository_id=$3 AND repository.node_id=$4`, p.OrgID, p.InstallationID, p.RepositoryID, p.RepositoryNodeID).Scan(&targetID)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE git_connections connection SET access_state=CASE
						WHEN installation.access_state='deleted' OR repository.access_state='deleted' THEN 'deleted'
						WHEN installation.access_state='suspended' THEN 'suspended'
						WHEN installation.access_state='unverified_revocation' OR repository.access_state='unverified_revocation' THEN 'unverified_revocation'
						WHEN repository.access_state='inaccessible' THEN 'inaccessible'
						ELSE 'active' END,updated_at=clock_timestamp()
					FROM github_repositories repository
					JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id
					WHERE connection.org_id=$1 AND connection.github_repository_id=$2 AND repository.org_id=connection.org_id AND repository.github_repository_id=connection.github_repository_id`, p.OrgID, p.RepositoryID); err != nil {
				return err
			}
		default:
			return ErrInvalidArgument
		}
		after, _ := json.Marshal(map[string]any{"kind": p.Kind, "installation_id": p.InstallationID, "repository_id": p.RepositoryID})
		return insertAuditEvent(ctx, tx, p.OrgID, p.Audit, "github_access.webhook_revoked", "github_installation", targetID, nil, after)
	})
}

type SnapshotGitHubPushParams struct {
	DeliveryID, OrgID, RepositoryNodeID, Ref, BeforeSHA, CommitSHA, AuthoritativeSHA, EventKind string
	InstallationID, RepositoryID                                                                int64
	RefExists                                                                                   bool
	ReceivedAt                                                                                  time.Time
	ObservationSequence                                                                         int64
	MaxIntents                                                                                  int
	Audit                                                                                       *AuditEvent
	DeliveryLease                                                                               LeaseToken
	Failpoint                                                                                   func(string) error
}

type SnapshotGitHubPushResult struct {
	RoutingGeneration int64
	IntentCount       int
}

// SnapshotGitHubPush is the accepted consistency boundary: repository/ref
// authority, generation advancement, eligibility, and intent insertion commit
// together while the repository/ref rows are locked.
func (s *Store) SnapshotGitHubPush(ctx context.Context, p SnapshotGitHubPushParams) (SnapshotGitHubPushResult, error) {
	if p.MaxIntents < 1 || p.MaxIntents > 10000 || (p.EventKind != "push" && p.EventKind != "delete") {
		return SnapshotGitHubPushResult{}, ErrInvalidArgument
	}
	if p.EventKind == "push" && (!p.RefExists || p.AuthoritativeSHA != p.CommitSHA) || p.EventKind == "delete" && p.RefExists {
		return SnapshotGitHubPushResult{}, ErrFailedPrecondition
	}
	var result SnapshotGitHubPushResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := fenceGitHubEventDelivery(ctx, tx, p.DeliveryID, p.DeliveryLease, p.OrgID); err != nil {
			return err
		}
		var repositoryGeneration, lifecycleGeneration int64
		var receivedAt time.Time
		var observationSequence int64
		var installationState, repositoryState, nodeID string
		var archived bool
		if err := tx.QueryRow(ctx, `SELECT repository.reconciliation_generation,installation.reconciliation_generation,installation.access_state,repository.access_state,repository.node_id,repository.archived
			FROM github_repositories repository JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id
			WHERE repository.org_id=$1 AND repository.github_installation_id=$2 AND repository.github_repository_id=$3 FOR UPDATE OF repository,installation`,
			p.OrgID, p.InstallationID, p.RepositoryID).Scan(&repositoryGeneration, &lifecycleGeneration, &installationState, &repositoryState, &nodeID, &archived); err != nil {
			return resourceQueryError("lock github push repository", err)
		}
		if nodeID != p.RepositoryNodeID || installationState != "active" || repositoryState != "active" || archived {
			return ErrFailedPrecondition
		}
		if err := tx.QueryRow(ctx, `SELECT received_at,ingestion_sequence FROM github_webhook_deliveries WHERE id=$1 AND org_id=$2 AND repository_id=$3 AND repository_node_id=$4`, p.DeliveryID, p.OrgID, p.RepositoryID, p.RepositoryNodeID).Scan(&receivedAt, &observationSequence); err != nil {
			return resourceQueryError("lock github push delivery", err)
		}
		if p.ObservationSequence > 0 && p.ObservationSequence != observationSequence {
			return ErrFailedPrecondition
		}
		tag, err := tx.Exec(ctx, `INSERT INTO github_repository_routing_state(repository_id,org_id,repository_node_id,installation_id,repository_generation,lifecycle_generation)
			VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(repository_id) DO UPDATE SET installation_id=EXCLUDED.installation_id,repository_generation=EXCLUDED.repository_generation,lifecycle_generation=EXCLUDED.lifecycle_generation,updated_at=clock_timestamp()
			WHERE github_repository_routing_state.repository_node_id=EXCLUDED.repository_node_id AND github_repository_routing_state.repository_generation<=EXCLUDED.repository_generation
			AND (github_repository_routing_state.installation_id<>EXCLUDED.installation_id OR github_repository_routing_state.lifecycle_generation<=EXCLUDED.lifecycle_generation)`,
			p.RepositoryID, p.OrgID, p.RepositoryNodeID, p.InstallationID, repositoryGeneration, lifecycleGeneration)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrFailedPrecondition
		}
		if _, err := tx.Exec(ctx, `INSERT INTO github_ref_routing_state(repository_id,ref) VALUES($1,$2) ON CONFLICT DO NOTHING`, p.RepositoryID, p.Ref); err != nil {
			return err
		}
		var previousSHA *string
		var previousExists bool
		var previousGeneration int64
		var previousObservationSequence *int64
		if err := tx.QueryRow(ctx, `SELECT validated_sha,ref_exists,routing_generation,last_observation_sequence FROM github_ref_routing_state WHERE repository_id=$1 AND ref=$2 FOR UPDATE`, p.RepositoryID, p.Ref).
			Scan(&previousSHA, &previousExists, &previousGeneration, &previousObservationSequence); err != nil {
			return err
		}
		if previousObservationSequence != nil && observationSequence <= *previousObservationSequence {
			return ErrFailedPrecondition
		}
		observationKind := "validated"
		if previousGeneration > 0 && p.BeforeSHA != "" {
			zero := "0000000000000000000000000000000000000000"
			matchesPredecessor := previousExists && previousSHA != nil && p.BeforeSHA == *previousSHA || !previousExists && p.BeforeSHA == zero
			if !matchesPredecessor {
				observationKind = "resynchronized_gap"
			}
		}
		zero := "0000000000000000000000000000000000000000"
		afterSHA := p.CommitSHA
		if p.EventKind == "delete" {
			afterSHA = zero
		}
		if tag, err := tx.Exec(ctx, `INSERT INTO github_ref_transition_observations(repository_id,ref,before_sha,after_sha,event_kind,delivery_id,received_at,observation_sequence,observation_kind) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(delivery_id) DO NOTHING`, p.RepositoryID, p.Ref, p.BeforeSHA, afterSHA, p.EventKind, p.DeliveryID, receivedAt, observationSequence, observationKind); err != nil {
			return err
		} else if tag.RowsAffected() == 0 {
			return ErrFailedPrecondition
		}
		var refLifecycle int64
		var sha any
		if p.EventKind == "push" {
			sha = p.CommitSHA
		}
		if err := tx.QueryRow(ctx, `UPDATE github_ref_routing_state SET routing_generation=routing_generation+1,
			lifecycle_generation=lifecycle_generation+CASE WHEN ref_exists IS DISTINCT FROM $3 THEN 1 ELSE 0 END,
				validated_sha=$4,ref_exists=$3,last_delivery_id=$5,last_observed_received_at=$6,last_observation_sequence=$7,updated_at=clock_timestamp() WHERE repository_id=$1 AND ref=$2
				RETURNING routing_generation,lifecycle_generation`, p.RepositoryID, p.Ref, p.RefExists, sha, p.DeliveryID, receivedAt, observationSequence).Scan(&result.RoutingGeneration, &refLifecycle); err != nil {
			return err
		}
		productionDeleteCount := 0
		if p.EventKind == "delete" {
			if err := tx.QueryRow(ctx, `SELECT count(*)
					FROM apps app JOIN projects project ON project.org_id=app.org_id AND project.id=app.project_id
					JOIN git_connections connection ON connection.org_id=project.org_id AND connection.id=project.git_connection_id
					WHERE app.org_id=$1 AND connection.github_repository_id=$2 AND app.deleted_at IS NULL AND project.deleted_at IS NULL
					AND connection.state='active' AND connection.access_state='active' AND app.github_workflow_path IS NOT NULL
					AND app.production_branch IS NOT NULL AND ('refs/heads/'||app.production_branch)=$3
					AND EXISTS (SELECT 1 FROM environments environment WHERE environment.org_id=app.org_id AND environment.app_id=app.id
						AND environment.kind='production' AND environment.branch=app.production_branch AND environment.lifecycle_status='ready' AND environment.torn_down_at IS NULL)`,
				p.OrgID, p.RepositoryID, p.Ref).Scan(&productionDeleteCount); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT app.id::text,COALESCE(environment.id::text,''),app.github_workflow_path,app.desired_generation,
			('refs/heads/'||app.production_branch)=$4 AS production_ref
			FROM apps app JOIN projects project ON project.org_id=app.org_id AND project.id=app.project_id
			JOIN git_connections connection ON connection.org_id=project.org_id AND connection.id=project.git_connection_id
			LEFT JOIN LATERAL (SELECT candidate.id FROM environments candidate WHERE candidate.org_id=app.org_id AND candidate.app_id=app.id
				AND candidate.kind='production' AND candidate.branch=app.production_branch AND candidate.lifecycle_status='ready' AND candidate.torn_down_at IS NULL
				ORDER BY candidate.id LIMIT 1) environment ON true
			WHERE app.org_id=$1 AND connection.github_repository_id=$2 AND app.deleted_at IS NULL AND project.deleted_at IS NULL
			AND connection.state='active' AND connection.access_state='active' AND app.github_workflow_path IS NOT NULL AND app.production_branch IS NOT NULL
				AND ((('refs/heads/'||app.production_branch)=$4 AND environment.id IS NOT NULL)
				 OR (('refs/heads/'||app.production_branch)<>$4 AND (app.previews_enabled OR $3='delete')))
				AND NOT ($3='delete' AND ('refs/heads/'||app.production_branch)=$4)
				ORDER BY app.id LIMIT $5`, p.OrgID, p.RepositoryID, p.EventKind, p.Ref, p.MaxIntents+1)
		if err != nil {
			return err
		}
		type eligibleIntent struct {
			appID, environmentID, workflow string
			configGeneration               int64
			production                     bool
		}
		var eligible []eligibleIntent
		for rows.Next() {
			var intent eligibleIntent
			if err := rows.Scan(&intent.appID, &intent.environmentID, &intent.workflow, &intent.configGeneration, &intent.production); err != nil {
				rows.Close()
				return err
			}
			eligible = append(eligible, intent)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if p.Failpoint != nil {
			if err := p.Failpoint("after_eligibility"); err != nil {
				return err
			}
		}
		for _, intent := range eligible {
			tag, err := tx.Exec(ctx, `INSERT INTO github_routing_intents(delivery_id,org_id,repository_id,repository_node_id,installation_id,app_id,environment_id,ref,commit_sha,event_kind,routing_generation,repository_generation,ref_lifecycle_generation,config_generation,workflow_path,production_ref)
				VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid,$8,NULLIF($9,''),$10,$11,$12,$13,$14,$15,$16)
				ON CONFLICT(delivery_id,app_id,ref,event_kind,routing_generation) DO NOTHING`, p.DeliveryID, p.OrgID, p.RepositoryID,
				p.RepositoryNodeID, p.InstallationID, intent.appID, intent.environmentID, p.Ref, p.CommitSHA, p.EventKind, result.RoutingGeneration,
				repositoryGeneration, refLifecycle, intent.configGeneration, intent.workflow, intent.production)
			if err != nil {
				return err
			}
			result.IntentCount += int(tag.RowsAffected())
			if result.IntentCount > p.MaxIntents {
				return fmt.Errorf("%w: github routing fanout limit", ErrFailedPrecondition)
			}
			if result.IntentCount == 1 && p.Failpoint != nil {
				if err := p.Failpoint("after_first_intent"); err != nil {
					return err
				}
			}
		}
		auditAfter := map[string]any{"repository_id": p.RepositoryID, "routing_generation": result.RoutingGeneration, "intent_count": result.IntentCount}
		if p.EventKind == "delete" {
			auditAfter["production_branch_deleted_count"] = productionDeleteCount
			auditAfter["production_branch_delete_outcome"] = "recorded_no_teardown"
		}
		after, _ := json.Marshal(auditAfter)
		return insertAuditEvent(ctx, tx, p.OrgID, p.Audit, "github_push.snapshotted", "github_webhook_delivery", p.DeliveryID, nil, after)
	})
	return result, err
}

func (s *Store) ExecuteGitHubRoutingIntent(ctx context.Context, id string, token LeaseToken, identities []PreviewIdentity) (string, error) {
	result := "processed"
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var intent GitHubRoutingIntent
		var valid bool
		dropped := false
		err := tx.QueryRow(ctx, `SELECT intent.org_id::text,intent.repository_id,intent.repository_node_id,intent.app_id::text,COALESCE(intent.environment_id::text,''),intent.ref,COALESCE(intent.commit_sha,''),intent.event_kind,intent.routing_generation,intent.repository_generation,intent.ref_lifecycle_generation,intent.config_generation,intent.workflow_path,intent.production_ref,
			CASE intent.event_kind
			WHEN 'push' THEN intent.routing_generation=ref.routing_generation AND intent.repository_generation=repository_state.repository_generation AND repository_state.repository_generation=github_repository.reconciliation_generation
				AND intent.ref_lifecycle_generation=ref.lifecycle_generation AND repository_state.lifecycle_generation=installation.reconciliation_generation AND intent.config_generation=app.desired_generation AND intent.workflow_path=app.github_workflow_path
				AND app.deleted_at IS NULL AND project.deleted_at IS NULL AND connection.state='active' AND connection.access_state='active'
				AND intent.installation_id=repository_state.installation_id AND repository_state.installation_id=installation.github_installation_id
				AND github_repository.github_installation_id=installation.github_installation_id AND github_repository.access_state='active' AND NOT github_repository.archived AND installation.access_state='active'
				AND intent.production_ref=(intent.ref='refs/heads/'||app.production_branch)
				AND ((intent.production_ref AND environment.id=intent.environment_id AND environment.kind='production' AND environment.branch=app.production_branch AND environment.lifecycle_status='ready' AND environment.torn_down_at IS NULL)
				 OR (NOT intent.production_ref AND intent.environment_id IS NULL AND app.previews_enabled))
				AND ref.ref_exists AND ref.validated_sha=intent.commit_sha
				WHEN 'delete' THEN intent.ref_lifecycle_generation=ref.lifecycle_generation AND NOT ref.ref_exists
					AND intent.installation_id=github_repository.github_installation_id AND intent.installation_id=repository_state.installation_id
					AND NOT intent.production_ref AND intent.environment_id IS NULL
				AND EXISTS (SELECT 1 FROM environments preview WHERE preview.org_id=intent.org_id AND preview.app_id=intent.app_id
					AND preview.kind='preview' AND preview.branch=substring(intent.ref from 12) AND preview.torn_down_at IS NULL)
			ELSE false END
			AND github_repository.node_id=intent.repository_node_id
		FROM github_routing_intents intent
		JOIN github_repository_routing_state repository_state ON repository_state.repository_id=intent.repository_id
		JOIN github_ref_routing_state ref ON ref.repository_id=intent.repository_id AND ref.ref=intent.ref
		JOIN apps app ON app.org_id=intent.org_id AND app.id=intent.app_id
		JOIN projects project ON project.org_id=app.org_id AND project.id=app.project_id
		JOIN git_connections connection ON connection.org_id=project.org_id AND connection.id=project.git_connection_id AND connection.github_repository_id=intent.repository_id
		JOIN github_repositories github_repository ON github_repository.org_id=intent.org_id AND github_repository.github_repository_id=intent.repository_id
		JOIN github_installations installation ON installation.org_id=github_repository.org_id AND installation.github_installation_id=github_repository.github_installation_id
		LEFT JOIN environments environment ON environment.org_id=intent.org_id AND environment.app_id=intent.app_id AND environment.id=intent.environment_id
			WHERE intent.id=$1 AND intent.status='leased' AND intent.lease_owner=$2 AND intent.lease_epoch=$3 AND intent.lease_expires_at>clock_timestamp()
			FOR UPDATE OF intent,repository_state,ref,app,project,connection,github_repository,installation`, id, token.Owner, token.Epoch).Scan(
			&intent.OrgID, &intent.RepositoryID, &intent.RepositoryNodeID, &intent.AppID, &intent.EnvironmentID, &intent.Ref, &intent.CommitSHA, &intent.EventKind,
			&intent.RoutingGeneration, &intent.RepositoryGeneration, &intent.RefLifecycleGeneration, &intent.ConfigGeneration, &intent.WorkflowPath, &intent.ProductionRef, &valid)
		if errors.Is(err, pgx.ErrNoRows) {
			tag, updateErr := tx.Exec(ctx, `UPDATE github_routing_intents SET status='dropped',result_code='superseded',error_code=NULL,error_detail=NULL,terminal_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp()`, id, token.Owner, token.Epoch)
			if updateErr != nil {
				return updateErr
			}
			if tag.RowsAffected() == 0 {
				return ErrLeaseNotOwned
			}
			result = "superseded"
			return nil
		}
		if err != nil {
			return err
		}
		if !valid {
			result, dropped = "superseded", true
		}

		if !dropped {
			key := githubRoutingKey(id, intent.AppID, intent.Ref, intent.CommitSHA, intent.EventKind, intent.RoutingGeneration)
			branch := strings.TrimPrefix(intent.Ref, "refs/heads/")
			nested := New(githubRoutingTxDB{tx})
			switch {
			case intent.EventKind == "delete":
				_, err = nested.RequestPreviewTeardown(ctx, PreviewTeardownParams{OrgID: intent.OrgID, AppID: intent.AppID, Branch: branch, IdempotencyKey: key, EventKind: "delete", Reason: "branch_deleted", Audit: &AuditEvent{ID: githubRoutingAuditID(id)}})
				if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
					result, dropped, err = "ignored", true, nil
				}
			case intent.ProductionRef:
				if intent.EnvironmentID == "" {
					result, dropped = "ignored", true
					break
				}
				_, err = nested.EnqueueOrCoalesceOrchestration(ctx, EnqueueOrchestrationParams{OrgID: intent.OrgID, RepositoryID: fmt.Sprint(intent.RepositoryID), WorkflowID: intent.WorkflowPath, GitRef: intent.Ref, EnvironmentID: intent.EnvironmentID, CommitSHA: intent.CommitSHA, WorkflowRevision: intent.CommitSHA, IdempotencyKey: key})
			case intent.EventKind == "push":
				if len(identities) == 0 {
					return ErrInvalidArgument
				}
				_, err = nested.AcceptPreviewPush(ctx, PreviewPushParams{OrgID: intent.OrgID, AppID: intent.AppID, Branch: branch, CommitSHA: intent.CommitSHA, IdempotencyKey: key, OrchestrationJobID: identities[0].EnvironmentID, Identities: identities})
				if errors.Is(err, ErrPreviewCapReached) || errors.Is(err, ErrFailedPrecondition) {
					result, dropped, err = "ignored", true, nil
				}
			default:
				return ErrInvalidArgument
			}
			if err != nil {
				return err
			}
		}
		status := "completed"
		if dropped {
			status = "dropped"
		}
		tag, err := tx.Exec(ctx, `UPDATE github_routing_intents SET status=$4,result_code=$5,error_code=NULL,error_detail=NULL,terminal_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp()`, id, token.Owner, token.Epoch, status, result)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseNotOwned
		}
		return nil
	})
	return result, err
}

func fenceGitHubEventDelivery(ctx context.Context, tx pgx.Tx, id string, token LeaseToken, orgID string) error {
	var owned bool
	err := tx.QueryRow(ctx, `SELECT status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp()
		FROM github_webhook_deliveries WHERE id=$1 AND org_id=$4 FOR UPDATE`, id, token.Owner, token.Epoch, orgID).Scan(&owned)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !owned {
		return ErrLeaseNotOwned
	}
	return err
}

type githubRoutingTxDB struct{ pgx.Tx }

func (d githubRoutingTxDB) BeginTx(ctx context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return d.Tx.Begin(ctx)
}
func (d githubRoutingTxDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return d.Tx.Exec(ctx, sql, args...)
}

func githubRoutingKey(id, appID, ref, commitSHA, eventKind string, generation int64) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"github-routing-v1", id, appID, ref, commitSHA, eventKind, fmt.Sprint(generation)}, "\x00")))
	return "ghr_" + hex.EncodeToString(sum[:])
}

func githubRoutingAuditID(id string) string {
	sum := sha256.Sum256([]byte("github-routing-audit-v1\x00" + id))
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
