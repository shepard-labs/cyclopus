package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	githubadapter "github.com/shepard-labs/cyclopus/internal/adapters/github"
	"github.com/shepard-labs/cyclopus/internal/store"
)

type GitHubEventDisposition string

const (
	GitHubEventComplete            GitHubEventDisposition = "complete"
	GitHubEventRetry               GitHubEventDisposition = "retry"
	GitHubEventFail                GitHubEventDisposition = "fail"
	GitHubEventLeaseLost           GitHubEventDisposition = "lease_lost"
	githubEventCallPinSafetyMargin                        = 5 * time.Second
)

type GitHubEventOutcome struct {
	Disposition GitHubEventDisposition
	Code        string
	RetryAfter  time.Duration
	IntentCount int
}

type GitHubEventInstallation = githubadapter.EventInstallation
type GitHubEventRepository = githubadapter.EventRepository

type GitHubEventClient interface {
	GetInstallation(context.Context, int64) (GitHubEventInstallation, error)
	ListInstallationRepositories(context.Context, int64) ([]GitHubEventRepository, error)
	GetRepository(context.Context, int64, int64) (GitHubEventRepository, error)
	GetBranchRef(context.Context, int64, int64, string) (sha string, exists bool, err error)
	RefreshInstallationCredentials(context.Context, int64) error
}

type GitHubEventStore interface {
	PinGitHubEventExternalCall(context.Context, string, store.LeaseToken, string, time.Duration) (store.LeaseToken, error)
	GetGitHubEventGeneration(context.Context, string, int64) (int64, error)
	ReconcileGitHubEvent(context.Context, store.ReconcileGitHubEventParams) (int64, error)
	RevokeGitHubEvent(context.Context, store.RevokeGitHubEventParams) error
	SnapshotGitHubPush(context.Context, store.SnapshotGitHubPushParams) (store.SnapshotGitHubPushResult, error)
	ExecuteGitHubRoutingIntent(context.Context, string, store.LeaseToken, []store.PreviewIdentity) (string, error)
}

type GitHubEventProcessor struct {
	store                           GitHubEventStore
	github                          GitHubEventClient
	maxIntents                      int
	callPinDuration, requestTimeout time.Duration
	now                             func() time.Time
}

func NewGitHubEventProcessor(dataStore GitHubEventStore, client GitHubEventClient, maxIntents int, callDurations ...time.Duration) (*GitHubEventProcessor, error) {
	callPinDuration, requestTimeout := time.Minute, time.Second
	if len(callDurations) == 2 {
		callPinDuration, requestTimeout = callDurations[0], callDurations[1]
	}
	if dataStore == nil || client == nil || maxIntents < 1 || maxIntents > 10000 || len(callDurations) != 0 && len(callDurations) != 2 || requestTimeout <= 0 || callPinDuration < githubEventCallPinSafetyMargin || requestTimeout > callPinDuration-githubEventCallPinSafetyMargin {
		return nil, errors.New("github event processor dependencies are invalid")
	}
	return &GitHubEventProcessor{store: dataStore, github: client, maxIntents: maxIntents, callPinDuration: callPinDuration, requestTimeout: requestTimeout, now: time.Now}, nil
}

type githubEventEnvelope struct {
	Action       string `json:"action"`
	Ref          string `json:"ref"`
	Before       string `json:"before"`
	After        string `json:"after"`
	Deleted      bool   `json:"deleted"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository          githubPayloadRepository   `json:"repository"`
	RepositoriesAdded   []githubPayloadRepository `json:"repositories_added"`
	RepositoriesRemoved []githubPayloadRepository `json:"repositories_removed"`
}

type githubPayloadRepository struct {
	ID     int64  `json:"id"`
	NodeID string `json:"node_id"`
}

var eventSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (p *GitHubEventProcessor) ProcessDelivery(ctx context.Context, delivery store.GitHubWebhookDelivery) GitHubEventOutcome {
	if delivery.Lease == nil || len(delivery.Payload) == 0 {
		return failEvent("invalid_event")
	}
	lease := *delivery.Lease
	delivery.Lease = &lease
	digest := sha256.Sum256(delivery.Payload)
	if hex.EncodeToString(digest[:]) != delivery.PayloadDigest {
		return failEvent("invalid_event")
	}
	var payload githubEventEnvelope
	decoder := json.NewDecoder(bytes.NewReader(delivery.Payload))
	if err := decoder.Decode(&payload); err != nil {
		return failEvent("invalid_event")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return failEvent("invalid_event")
	}
	if payload.Action != delivery.Action {
		return failEvent("invalid_event")
	}

	switch delivery.Event {
	case "ping":
		return completeEvent("ignored", 0)
	case "installation":
		if !eventIdentityMatches(delivery, payload) {
			return failEvent("invalid_event")
		}
		switch delivery.Action {
		case "created", "unsuspend", "new_permissions_accepted":
			return p.reconcileInstallation(ctx, delivery)
		case "suspend", "deleted":
			kind := strings.TrimSuffix(delivery.Action, "ed")
			if delivery.Action == "deleted" {
				kind = "delete"
			}
			if err := p.store.RevokeGitHubEvent(ctx, revokeEventParams(delivery, "unverified")); err != nil {
				return storeEventError(err)
			}
			authoritative, err, outcome := externalGitHubCall(p, ctx, delivery, func(callCtx context.Context) (GitHubEventInstallation, error) {
				return p.github.GetInstallation(callCtx, *delivery.InstallationID)
			})
			if outcome.Disposition != "" {
				return outcome
			}
			if err == nil {
				if delivery.Action == "suspend" && authoritative.Suspended {
					if err := p.store.RevokeGitHubEvent(ctx, revokeEventParams(delivery, kind)); err != nil {
						return storeEventError(err)
					}
					return completeEvent("processed", 0)
				}
				return p.reconcileInstallation(ctx, delivery)
			} else if outcome := githubEventError(err); outcome.Disposition == GitHubEventRetry {
				return outcome
			} else if delivery.Action == "deleted" && isGitHubStatus(err, http.StatusNotFound) {
				if err := p.store.RevokeGitHubEvent(ctx, revokeEventParams(delivery, kind)); err != nil {
					return storeEventError(err)
				}
			}
			return completeEvent("processed", 0)
		default:
			return completeEvent("ignored", 0)
		}
	case "installation_repositories":
		if !eventIdentityMatches(delivery, payload) {
			return failEvent("invalid_event")
		}
		switch delivery.Action {
		case "added":
			for _, repository := range payload.RepositoriesAdded {
				authoritative, err, outcome := externalGitHubCall(p, ctx, delivery, func(callCtx context.Context) (GitHubEventRepository, error) {
					return p.github.GetRepository(callCtx, *delivery.InstallationID, repository.ID)
				})
				if outcome.Disposition != "" {
					return outcome
				}
				if err != nil {
					return githubEventError(err)
				}
				if authoritative.ID != repository.ID || authoritative.NodeID != repository.NodeID || authoritative.InstallationID != *delivery.InstallationID {
					return failEvent("malformed_response")
				}
			}
			return p.reconcileInstallation(ctx, delivery)
		case "removed":
			for _, repository := range payload.RepositoriesRemoved {
				if repository.ID <= 0 || repository.NodeID == "" {
					return failEvent("invalid_event")
				}
				params := revokeEventParams(delivery, "unverified_remove")
				params.RepositoryID, params.RepositoryNodeID = repository.ID, repository.NodeID
				if err := p.store.RevokeGitHubEvent(ctx, params); err != nil {
					return storeEventError(err)
				}
				authoritative, err, callOutcome := externalGitHubCall(p, ctx, delivery, func(callCtx context.Context) (GitHubEventRepository, error) {
					return p.github.GetRepository(callCtx, *delivery.InstallationID, repository.ID)
				})
				if callOutcome.Disposition != "" {
					return callOutcome
				}
				if err == nil {
					if authoritative.ID != repository.ID || authoritative.NodeID != repository.NodeID || authoritative.InstallationID != *delivery.InstallationID {
						return failEvent("malformed_response")
					}
					return p.reconcileInstallation(ctx, delivery)
				}
				outcome := githubEventError(err)
				if outcome.Disposition == GitHubEventRetry {
					return outcome
				}
				if isGitHubStatus(err, http.StatusNotFound) {
					params.Kind = "remove"
					if err := p.store.RevokeGitHubEvent(ctx, params); err != nil {
						return storeEventError(err)
					}
				}
			}
			return completeEvent("processed", 0)
		default:
			return completeEvent("ignored", 0)
		}
	case "push":
		if !eventIdentityMatches(delivery, payload) {
			return failEvent("invalid_event")
		}
		return p.processPush(ctx, delivery, payload)
	default:
		return completeEvent("ignored", 0)
	}
}

func eventIdentityMatches(delivery store.GitHubWebhookDelivery, payload githubEventEnvelope) bool {
	if delivery.InstallationID == nil || payload.Installation.ID != *delivery.InstallationID {
		return false
	}
	if delivery.RepositoryID == nil {
		return payload.Repository.ID == 0
	}
	return delivery.RepositoryNodeID != nil && payload.Repository.ID == *delivery.RepositoryID && payload.Repository.NodeID == *delivery.RepositoryNodeID
}

func (p *GitHubEventProcessor) reconcileInstallation(ctx context.Context, delivery store.GitHubWebhookDelivery) GitHubEventOutcome {
	expectedGeneration, err := p.store.GetGitHubEventGeneration(ctx, delivery.OrgID, *delivery.InstallationID)
	if err != nil {
		return storeEventError(err)
	}
	installation, repositories, outcome := p.fetchInstallation(ctx, delivery)
	if outcome.Disposition != "" {
		if outcome.Code == "permission_revoked" {
			if err := p.store.RevokeGitHubEvent(ctx, revokeEventParams(delivery, "unverified")); err != nil {
				return storeEventError(err)
			}
		}
		return outcome
	}
	values := make([]store.GitHubRepository, 0, len(repositories))
	for _, repository := range repositories {
		if repository.InstallationID != 0 && repository.InstallationID != installation.ID {
			return failEvent("malformed_response")
		}
		values = append(values, store.GitHubRepository{GitHubRepositoryID: repository.ID, GitHubInstallationID: installation.ID,
			NodeID: repository.NodeID, OwnerLogin: repository.Owner, Name: repository.Name, FullName: repository.FullName,
			Visibility: repository.Visibility, Archived: repository.Archived, DefaultBranch: repository.DefaultBranch})
	}
	_, err = p.store.ReconcileGitHubEvent(ctx, store.ReconcileGitHubEventParams{OrgID: delivery.OrgID, DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, ExpectedGeneration: expectedGeneration,
		Installation: store.GitHubAuthoritativeInstallation{InstallationID: installation.ID, AccountID: installation.AccountID,
			AccountLogin: installation.AccountLogin, AccountType: installation.AccountType, Selection: installation.RepositorySelection, Suspended: installation.Suspended},
		Repositories: values, Audit: eventAudit()})
	if err != nil {
		if errors.Is(err, store.ErrFailedPrecondition) {
			return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "database_unavailable"}
		}
		return storeEventError(err)
	}
	return completeEvent("processed", 0)
}

func (p *GitHubEventProcessor) fetchInstallation(ctx context.Context, delivery store.GitHubWebhookDelivery) (GitHubEventInstallation, []GitHubEventRepository, GitHubEventOutcome) {
	id := *delivery.InstallationID
	installation, err, outcome := externalGitHubCall(p, ctx, delivery, func(callCtx context.Context) (GitHubEventInstallation, error) {
		return p.github.GetInstallation(callCtx, id)
	})
	if outcome.Disposition != "" {
		return GitHubEventInstallation{}, nil, outcome
	}
	if err != nil {
		return GitHubEventInstallation{}, nil, githubEventError(err)
	}
	if installation.ID != id || installation.AccountID <= 0 || installation.AccountLogin == "" {
		return GitHubEventInstallation{}, nil, failEvent("malformed_response")
	}
	repositories, err, outcome := externalGitHubCall(p, ctx, delivery, func(callCtx context.Context) ([]GitHubEventRepository, error) {
		return p.github.ListInstallationRepositories(callCtx, id)
	})
	if outcome.Disposition != "" {
		return GitHubEventInstallation{}, nil, outcome
	}
	if err != nil {
		return GitHubEventInstallation{}, nil, githubEventError(err)
	}
	return installation, repositories, GitHubEventOutcome{}
}

func (p *GitHubEventProcessor) processPush(ctx context.Context, delivery store.GitHubWebhookDelivery, payload githubEventEnvelope) GitHubEventOutcome {
	if !strings.HasPrefix(payload.Ref, "refs/heads/") || payload.Ref != strings.TrimSpace(payload.Ref) {
		return completeEvent("ignored", 0)
	}
	zero := strings.Repeat("0", 40)
	if !eventSHA.MatchString(payload.Before) || !eventSHA.MatchString(payload.After) {
		return failEvent("invalid_event")
	}
	if payload.Deleted {
		if payload.Before == zero || payload.After != zero {
			return failEvent("invalid_event")
		}
	} else if payload.After == zero || payload.Before == payload.After {
		return failEvent("invalid_event")
	}
	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
	branchResult, err, outcome := externalGitHubCall(p, ctx, delivery, func(callCtx context.Context) (struct {
		sha    string
		exists bool
	}, error) {
		sha, exists, err := p.github.GetBranchRef(callCtx, *delivery.InstallationID, *delivery.RepositoryID, branch)
		return struct {
			sha    string
			exists bool
		}{sha, exists}, err
	})
	if outcome.Disposition != "" {
		return outcome
	}
	sha, exists := branchResult.sha, branchResult.exists
	if err != nil {
		outcome := githubEventError(err)
		if outcome.Disposition == GitHubEventComplete && outcome.Code == "permission_revoked" {
			params := revokeEventParams(delivery, "unverified_remove")
			var scoped interface{ GitHubEventAccessScope() string }
			if errors.As(err, &scoped) && scoped.GitHubEventAccessScope() == "installation" {
				params = revokeEventParams(delivery, "unverified")
			} else {
				params.RepositoryID, params.RepositoryNodeID = *delivery.RepositoryID, *delivery.RepositoryNodeID
			}
			if err := p.store.RevokeGitHubEvent(ctx, params); err != nil {
				return storeEventError(err)
			}
		}
		return outcome
	}
	if payload.Deleted {
		if exists {
			return completeEvent("stale_push", 0)
		}
	} else if !exists || sha != payload.After {
		return completeEvent("stale_push", 0)
	}
	eventKind, commit := "push", payload.After
	if payload.Deleted {
		eventKind, commit = "delete", ""
	}
	result, err := p.store.SnapshotGitHubPush(ctx, store.SnapshotGitHubPushParams{DeliveryID: delivery.ID, OrgID: delivery.OrgID,
		InstallationID: *delivery.InstallationID, RepositoryID: *delivery.RepositoryID, RepositoryNodeID: *delivery.RepositoryNodeID,
		Ref: payload.Ref, BeforeSHA: payload.Before, CommitSHA: commit, AuthoritativeSHA: sha, EventKind: eventKind, RefExists: exists, ReceivedAt: delivery.ReceivedAt,
		ObservationSequence: delivery.IngestionSequence, MaxIntents: p.maxIntents, Audit: eventAudit(), DeliveryLease: *delivery.Lease})
	if err != nil {
		return storeEventError(err)
	}
	if payload.Deleted && result.IntentCount == 0 {
		return completeEvent("production_branch_deleted", 0)
	}
	return completeEvent("processed", result.IntentCount)
}

func revokeEventParams(delivery store.GitHubWebhookDelivery, kind string) store.RevokeGitHubEventParams {
	return store.RevokeGitHubEventParams{OrgID: delivery.OrgID, DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease,
		InstallationID: *delivery.InstallationID, Kind: kind, Audit: eventAudit()}
}

func (p *GitHubEventProcessor) ProcessIntent(ctx context.Context, intent store.GitHubRoutingIntent) GitHubEventOutcome {
	if intent.Lease == nil {
		return failEvent("invalid_snapshot")
	}
	result, err := p.store.ExecuteGitHubRoutingIntent(ctx, intent.ID, *intent.Lease, deterministicPreviewIdentities(intent))
	if err != nil {
		if errors.Is(err, store.ErrFailedPrecondition) || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrLeaseNotOwned) {
			return completeEvent("superseded", 0)
		}
		return storeEventError(err)
	}
	return completeEvent(result, 0)
}

func routingKey(intent store.GitHubRoutingIntent) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"github-routing-v1", intent.ID, intent.AppID, intent.Ref, intent.CommitSHA, intent.EventKind, fmt.Sprint(intent.RoutingGeneration)}, "\x00")))
	return "ghr_" + hex.EncodeToString(sum[:])
}

func deterministicPreviewIdentities(intent store.GitHubRoutingIntent) []store.PreviewIdentity {
	values := make([]store.PreviewIdentity, 0, store.PreviewIdentityAttempts)
	for i := range store.PreviewIdentityAttempts {
		sum := sha256.Sum256([]byte(fmt.Sprintf("github-preview-v1\x00%s\x00%d", intent.ID, i)))
		idBytes := append([]byte(nil), sum[:16]...)
		idBytes[6] = (idBytes[6] & 0x0f) | 0x40
		idBytes[8] = (idBytes[8] & 0x3f) | 0x80
		id, _ := uuid.FromBytes(idBytes)
		slug := "gh-" + hex.EncodeToString(sum[16:24])
		values = append(values, store.PreviewIdentity{EnvironmentID: id.String(), Slug: slug, Namespace: "preview-" + slug})
	}
	return values
}

func isGitHubStatus(err error, status int) bool {
	var serviceError *GitHubAPIError
	if errors.As(err, &serviceError) {
		return serviceError.Status == status
	}
	var eventError interface {
		GitHubEventError() (int, bool, time.Duration)
	}
	if errors.As(err, &eventError) {
		code, _, _ := eventError.GitHubEventError()
		return code == status
	}
	return false
}

func externalGitHubCall[T any](p *GitHubEventProcessor, ctx context.Context, delivery store.GitHubWebhookDelivery, call func(context.Context) (T, error)) (T, error, GitHubEventOutcome) {
	var zero T
	lease, err := p.store.PinGitHubEventExternalCall(ctx, delivery.ID, *delivery.Lease, delivery.OrgID, p.callPinDuration)
	if err != nil {
		return zero, nil, externalCallAuthorizationError(err)
	}
	*delivery.Lease = lease
	callCtx, cancel, ok := p.externalCallContext(ctx, lease)
	if !ok {
		return zero, nil, GitHubEventOutcome{Disposition: GitHubEventLeaseLost, Code: "lease_lost"}
	}
	value, err := call(callCtx)
	cancel()
	if !isGitHubStatus(err, http.StatusUnauthorized) && !isGitHubStatus(err, http.StatusConflict) && !isGitHubStatus(err, http.StatusUnprocessableEntity) {
		return value, err, GitHubEventOutcome{}
	}
	lease, pinErr := p.store.PinGitHubEventExternalCall(ctx, delivery.ID, *delivery.Lease, delivery.OrgID, p.callPinDuration)
	if pinErr != nil {
		return zero, nil, externalCallAuthorizationError(pinErr)
	}
	*delivery.Lease = lease
	refreshCtx, refreshCancel, ok := p.externalCallContext(ctx, lease)
	if !ok {
		return zero, nil, GitHubEventOutcome{Disposition: GitHubEventLeaseLost, Code: "lease_lost"}
	}
	refreshErr := p.github.RefreshInstallationCredentials(refreshCtx, *delivery.InstallationID)
	refreshCancel()
	if refreshErr != nil {
		return zero, refreshErr, GitHubEventOutcome{}
	}
	lease, pinErr = p.store.PinGitHubEventExternalCall(ctx, delivery.ID, *delivery.Lease, delivery.OrgID, p.callPinDuration)
	if pinErr != nil {
		return zero, nil, externalCallAuthorizationError(pinErr)
	}
	*delivery.Lease = lease
	retryCtx, retryCancel, ok := p.externalCallContext(ctx, lease)
	if !ok {
		return zero, nil, GitHubEventOutcome{Disposition: GitHubEventLeaseLost, Code: "lease_lost"}
	}
	value, retryErr := call(retryCtx)
	retryCancel()
	if (isGitHubStatus(err, http.StatusConflict) || isGitHubStatus(err, http.StatusUnprocessableEntity)) &&
		(isGitHubStatus(retryErr, http.StatusConflict) || isGitHubStatus(retryErr, http.StatusUnprocessableEntity)) {
		return zero, nil, failEvent("github_conflict")
	}
	return value, retryErr, GitHubEventOutcome{}
}

func (p *GitHubEventProcessor) externalCallContext(ctx context.Context, lease store.LeaseToken) (context.Context, context.CancelFunc, bool) {
	now := p.now()
	deadline := lease.ExpiresAt.Add(-githubEventCallPinSafetyMargin)
	if requestDeadline := now.Add(p.requestTimeout); requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	if !deadline.After(now) {
		return nil, nil, false
	}
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	return callCtx, cancel, true
}

func externalCallAuthorizationError(err error) GitHubEventOutcome {
	if errors.Is(err, store.ErrLeaseNotOwned) {
		return GitHubEventOutcome{Disposition: GitHubEventLeaseLost, Code: "lease_lost"}
	}
	return storeEventError(err)
}

func githubEventError(err error) GitHubEventOutcome {
	if err == nil {
		return GitHubEventOutcome{}
	}
	var api *GitHubAPIError
	if errors.As(err, &api) {
		switch {
		case api.Status == http.StatusTooManyRequests || api.Status == http.StatusForbidden && api.RetryAfter > 0:
			return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_rate_limited", RetryAfter: api.RetryAfter}
		case api.Status == http.StatusUnauthorized || api.Status == http.StatusForbidden || api.Status == http.StatusNotFound:
			return GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "permission_revoked"}
		case api.Status == http.StatusConflict || api.Status == http.StatusUnprocessableEntity:
			return failEvent("github_conflict")
		case api.Status >= 500 || api.Status == 0:
			return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_unavailable", RetryAfter: api.RetryAfter}
		default:
			return failEvent("malformed_response")
		}
	}
	var eventError interface {
		GitHubEventError() (int, bool, time.Duration)
	}
	if errors.As(err, &eventError) {
		status, retryable, retryAfter := eventError.GitHubEventError()
		if retryable {
			if status == http.StatusForbidden && retryAfter <= 0 {
				retryAfter = time.Minute
			}
			code := "github_unavailable"
			if status == http.StatusForbidden || status == http.StatusTooManyRequests {
				code = "github_rate_limited"
			}
			return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: code, RetryAfter: retryAfter}
		}
		switch {
		case status == http.StatusTooManyRequests || status == http.StatusForbidden && retryAfter > 0:
			return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_rate_limited", RetryAfter: retryAfter}
		case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
			return GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "permission_revoked"}
		case status == http.StatusConflict || status == http.StatusUnprocessableEntity:
			return failEvent("github_conflict")
		case status >= 500 || status == 0:
			return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_unavailable", RetryAfter: retryAfter}
		default:
			return failEvent("malformed_response")
		}
	}
	if retryable, retryAfter, code, ok := githubadapter.ErrorDisposition(err); ok {
		if retryable {
			if strings.Contains(code, "429") || strings.Contains(code, "403") {
				return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_rate_limited", RetryAfter: retryAfter}
			}
			return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_unavailable", RetryAfter: retryAfter}
		}
		if code == "malformed_response" {
			return failEvent("malformed_response")
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
		return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_unavailable"}
	}
	return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_unavailable"}
}

func storeEventError(err error) GitHubEventOutcome {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "40001" || pgErr.Code == "40P01" || strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "53") || pgErr.Code == "57P01" {
			return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "database_unavailable"}
		}
		if strings.HasPrefix(pgErr.Code, "22") {
			return failEvent("invalid_event")
		}
	}
	switch {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrInvalidArgument):
		return failEvent("invalid_event")
	case errors.Is(err, store.ErrFailedPrecondition), errors.Is(err, store.ErrNotFound):
		return completeEvent("ignored", 0)
	case errors.Is(err, store.ErrLeaseNotOwned):
		return completeEvent("superseded", 0)
	default:
		return GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "database_unavailable"}
	}
}

func completeEvent(code string, count int) GitHubEventOutcome {
	return GitHubEventOutcome{Disposition: GitHubEventComplete, Code: code, IntentCount: count}
}

func failEvent(code string) GitHubEventOutcome {
	return GitHubEventOutcome{Disposition: GitHubEventFail, Code: code}
}

func eventAudit() *store.AuditEvent {
	id, err := uuid.NewV7()
	if err != nil {
		panic(err)
	}
	return &store.AuditEvent{ID: id.String()}
}
