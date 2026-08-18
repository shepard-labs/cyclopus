package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	githubadapter "github.com/shepard-labs/cyclopus/internal/adapters/github"
	"github.com/shepard-labs/cyclopus/internal/store"
)

func TestGitHubEventErrorClassification(t *testing.T) {
	retryAfter := 2 * time.Minute
	tests := []struct {
		name string
		err  error
		want GitHubEventOutcome
	}{
		{"network", timeoutError{}, GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_unavailable"}},
		{"server error", &githubadapter.APIError{StatusCode: http.StatusBadGateway, Retryable: true}, GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_unavailable"}},
		{"rate limit", &githubadapter.APIError{StatusCode: http.StatusTooManyRequests, Retryable: true, RetryAfter: retryAfter}, GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_rate_limited", RetryAfter: retryAfter}},
		{"retryable forbidden defaults delay", &githubadapter.APIError{StatusCode: http.StatusForbidden, Retryable: true}, GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_rate_limited", RetryAfter: time.Minute}},
		{"permission forbidden", &githubadapter.APIError{StatusCode: http.StatusForbidden}, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "permission_revoked"}},
		{"not found", &githubadapter.APIError{StatusCode: http.StatusNotFound}, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "permission_revoked"}},
		{"conflict", &githubadapter.APIError{StatusCode: http.StatusConflict}, GitHubEventOutcome{Disposition: GitHubEventFail, Code: "invalid_event"}},
		{"unprocessable", &githubadapter.APIError{StatusCode: http.StatusUnprocessableEntity}, GitHubEventOutcome{Disposition: GitHubEventFail, Code: "invalid_event"}},
		{"malformed", &githubadapter.PermanentJobError{Code: "malformed_response", Err: errors.New("bad")}, GitHubEventOutcome{Disposition: GitHubEventFail, Code: "malformed_response"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := githubEventError(test.err); got != test.want {
				t.Fatalf("outcome = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestGitHubInstallationUnauthorizedRefreshesOnce(t *testing.T) {
	client := &eventClientStub{installationErrors: []error{&githubadapter.APIError{StatusCode: http.StatusUnauthorized}}, installation: activeEventInstallation()}
	processor, err := NewGitHubEventProcessor(&eventStoreStub{}, client, 10)
	if err != nil {
		t.Fatal(err)
	}
	installation, _, outcome := processor.fetchInstallation(t.Context(), 42)
	if outcome.Disposition != "" || installation.ID != 42 || client.refreshes != 1 || client.installationCalls != 2 {
		t.Fatalf("fetch = %+v %+v; refreshes=%d calls=%d", installation, outcome, client.refreshes, client.installationCalls)
	}
}

func TestGitHubInstallationSecondUnauthorizedAfterRefreshIsRevoked(t *testing.T) {
	client := &eventClientStub{installationErrors: []error{
		&githubadapter.APIError{StatusCode: http.StatusUnauthorized},
		&githubadapter.APIError{StatusCode: http.StatusUnauthorized},
	}}
	processor, err := NewGitHubEventProcessor(&eventStoreStub{}, client, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, _, outcome := processor.fetchInstallation(t.Context(), 42)
	want := GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "permission_revoked"}
	if outcome != want || client.refreshes != 1 || client.installationCalls != 2 {
		t.Fatalf("outcome=%+v refreshes=%d calls=%d", outcome, client.refreshes, client.installationCalls)
	}
}

func TestGitHubPermissionRevocationStoreFailureRetries(t *testing.T) {
	dataStore := &eventStoreStub{revokeError: errors.New("database unavailable")}
	client := &eventClientStub{installationErrors: []error{
		&githubadapter.APIError{StatusCode: http.StatusUnauthorized},
		&githubadapter.APIError{StatusCode: http.StatusUnauthorized},
	}}
	processor, _ := NewGitHubEventProcessor(dataStore, client, 10)
	outcome := processor.ProcessDelivery(t.Context(), eventDelivery("installation", "created", `{"action":"created","installation":{"id":42}}`, false))
	if outcome.Disposition != GitHubEventRetry || outcome.Code != "database_unavailable" || len(dataStore.revocations) != 1 {
		t.Fatalf("outcome=%+v revocations=%d", outcome, len(dataStore.revocations))
	}
}

func TestGitHubDestructiveEventsPersistUnverifiedRevocation(t *testing.T) {
	tests := []struct {
		name, event, action, payload, wantKind string
	}{
		{"installation suspend", "installation", "suspend", `{"action":"suspend","installation":{"id":42}}`, "unverified"},
		{"installation delete", "installation", "deleted", `{"action":"deleted","installation":{"id":42}}`, "unverified"},
		{"repository remove", "installation_repositories", "removed", `{"action":"removed","installation":{"id":42},"repositories_removed":[{"id":99,"node_id":"R_99"}]}`, "unverified_remove"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataStore := &eventStoreStub{}
			client := &eventClientStub{installationErrors: []error{&githubadapter.APIError{StatusCode: http.StatusForbidden}}, repositoryError: &githubadapter.APIError{StatusCode: http.StatusForbidden}}
			processor, err := NewGitHubEventProcessor(dataStore, client, 10)
			if err != nil {
				t.Fatal(err)
			}
			delivery := eventDelivery(test.event, test.action, test.payload, false)
			outcome := processor.ProcessDelivery(t.Context(), delivery)
			if outcome.Disposition != GitHubEventComplete || outcome.Code != "processed" || len(dataStore.revocations) != 1 || dataStore.revocations[0].Kind != test.wantKind {
				t.Fatalf("outcome=%+v revocations=%+v", outcome, dataStore.revocations)
			}
		})
	}
}

func TestGitHubUnverifiedRevocationRecoversFromAuthoritativeState(t *testing.T) {
	tests := []struct {
		name, event, action, payload string
		repository                   bool
	}{
		{"unsuspend", "installation", "unsuspend", `{"action":"unsuspend","installation":{"id":42}}`, false},
		{"repository add", "installation_repositories", "added", `{"action":"added","installation":{"id":42},"repositories_added":[{"id":99,"node_id":"R_99"}]}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataStore := &eventStoreStub{}
			client := &eventClientStub{installation: activeEventInstallation(), repositories: []GitHubEventRepository{activeEventRepository()}}
			processor, _ := NewGitHubEventProcessor(dataStore, client, 10)
			outcome := processor.ProcessDelivery(t.Context(), eventDelivery(test.event, test.action, test.payload, test.repository))
			if outcome.Disposition != GitHubEventComplete || outcome.Code != "processed" || dataStore.reconciliations != 1 {
				t.Fatalf("outcome=%+v reconciliations=%d", outcome, dataStore.reconciliations)
			}
		})
	}
}

func TestGitHubSupportedPayloadValidationAndUnsupportedIgnored(t *testing.T) {
	processor, _ := NewGitHubEventProcessor(&eventStoreStub{}, &eventClientStub{}, 10)
	tests := []struct {
		name     string
		delivery store.GitHubWebhookDelivery
		want     GitHubEventOutcome
	}{
		{"digest mismatch", eventDelivery("installation", "created", `{"action":"created","installation":{"id":42}}`, false), GitHubEventOutcome{Disposition: GitHubEventFail, Code: "invalid_event"}},
		{"action mismatch", eventDelivery("installation", "created", `{"action":"deleted","installation":{"id":42}}`, false), GitHubEventOutcome{Disposition: GitHubEventFail, Code: "invalid_event"}},
		{"invalid removed repository", eventDelivery("installation_repositories", "removed", `{"action":"removed","installation":{"id":42},"repositories_removed":[{"id":0,"node_id":""}]}`, false), GitHubEventOutcome{Disposition: GitHubEventFail, Code: "invalid_event"}},
		{"unsupported action", eventDelivery("installation", "renamed", `{"action":"renamed","installation":{"id":42}}`, false), GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "ignored"}},
		{"unsupported event", eventDelivery("issues", "opened", `{"action":"opened","installation":{"id":42}}`, false), GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "ignored"}},
	}
	tests[0].delivery.PayloadDigest = strings.Repeat("0", 64)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := processor.ProcessDelivery(t.Context(), test.delivery); got != test.want {
				t.Fatalf("outcome = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestGitHubPushStalenessAndDeletionOutcomes(t *testing.T) {
	sha := strings.Repeat("a", 40)
	before := strings.Repeat("b", 40)
	tests := []struct {
		name, payload, authoritativeSHA string
		exists, snapshot                bool
		intentCount                     int
		want                            GitHubEventOutcome
	}{
		{"current push", `{"installation":{"id":42},"repository":{"id":99,"node_id":"R_99"},"ref":"refs/heads/main","before":"` + before + `","after":"` + sha + `"}`, sha, true, true, 2, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "processed", IntentCount: 2}},
		{"head mismatch", `{"installation":{"id":42},"repository":{"id":99,"node_id":"R_99"},"ref":"refs/heads/main","before":"` + before + `","after":"` + sha + `"}`, strings.Repeat("b", 40), true, false, 0, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "stale_push"}},
		{"normal ref missing", `{"installation":{"id":42},"repository":{"id":99,"node_id":"R_99"},"ref":"refs/heads/main","before":"` + before + `","after":"` + sha + `"}`, "", false, false, 0, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "stale_push"}},
		{"stale deletion after recreate", `{"installation":{"id":42},"repository":{"id":99,"node_id":"R_99"},"ref":"refs/heads/main","before":"` + before + `","after":"0000000000000000000000000000000000000000","deleted":true}`, sha, true, false, 0, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "stale_push"}},
		{"production deletion", `{"installation":{"id":42},"repository":{"id":99,"node_id":"R_99"},"ref":"refs/heads/main","before":"` + before + `","after":"0000000000000000000000000000000000000000","deleted":true}`, "", false, true, 0, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "production_branch_deleted"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataStore := &eventStoreStub{snapshotResult: store.SnapshotGitHubPushResult{IntentCount: test.intentCount}}
			client := &eventClientStub{branchSHA: test.authoritativeSHA, branchExists: test.exists}
			processor, _ := NewGitHubEventProcessor(dataStore, client, 10)
			got := processor.ProcessDelivery(t.Context(), eventDelivery("push", "", test.payload, true))
			if got != test.want || dataStore.snapshots != boolInt(test.snapshot) {
				t.Fatalf("outcome=%+v snapshots=%d, want %+v/%t", got, dataStore.snapshots, test.want, test.snapshot)
			}
		})
	}
}

func TestGitHubPushPermissionFailurePersistsFencedRevocation(t *testing.T) {
	sha := strings.Repeat("a", 40)
	before := strings.Repeat("b", 40)
	payload := `{"installation":{"id":42},"repository":{"id":99,"node_id":"R_99"},"ref":"refs/heads/main","before":"` + before + `","after":"` + sha + `"}`
	for _, test := range []struct {
		name, scope, wantKind string
	}{
		{name: "repository access", scope: "repository", wantKind: "unverified_remove"},
		{name: "installation authentication", scope: "installation", wantKind: "unverified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataStore := &eventStoreStub{}
			client := &eventClientStub{branchError: &githubadapter.APIError{StatusCode: http.StatusForbidden, AccessScope: test.scope}}
			processor, _ := NewGitHubEventProcessor(dataStore, client, 10)
			delivery := eventDelivery("push", "", payload, true)
			outcome := processor.ProcessDelivery(t.Context(), delivery)
			if outcome != (GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "permission_revoked"}) || len(dataStore.revocations) != 1 {
				t.Fatalf("outcome=%+v revocations=%+v", outcome, dataStore.revocations)
			}
			got := dataStore.revocations[0]
			if got.Kind != test.wantKind || got.DeliveryID != delivery.ID || got.DeliveryLease != *delivery.Lease ||
				(test.scope == "repository" && (got.RepositoryID != 99 || got.RepositoryNodeID != "R_99")) {
				t.Fatalf("revocation=%+v", got)
			}
		})
	}
}

func TestGitHubPushPermissionRevocationStoreFailureRetries(t *testing.T) {
	sha := strings.Repeat("a", 40)
	payload := `{"installation":{"id":42},"repository":{"id":99,"node_id":"R_99"},"ref":"refs/heads/main","before":"` + strings.Repeat("b", 40) + `","after":"` + sha + `"}`
	dataStore := &eventStoreStub{revokeError: errors.New("database unavailable")}
	client := &eventClientStub{branchError: &githubadapter.APIError{StatusCode: http.StatusNotFound, AccessScope: "repository"}}
	processor, _ := NewGitHubEventProcessor(dataStore, client, 10)
	outcome := processor.ProcessDelivery(t.Context(), eventDelivery("push", "", payload, true))
	if outcome.Disposition != GitHubEventRetry || outcome.Code != "database_unavailable" || len(dataStore.revocations) != 1 {
		t.Fatalf("outcome=%+v revocations=%+v", outcome, dataStore.revocations)
	}
}

func TestGitHubIntentRoutesStoredSnapshotAndDropsStale(t *testing.T) {
	lease := &store.LeaseToken{Owner: "worker", Epoch: 3, ExpiresAt: time.Now().Add(time.Minute)}
	base := store.GitHubRoutingIntent{ID: "10000000-0000-0000-0000-000000000010", OrgID: "10000000-0000-0000-0000-000000000002", RepositoryID: 99, RepositoryNodeID: "R_99", AppID: "10000000-0000-0000-0000-000000000011", EnvironmentID: "10000000-0000-0000-0000-000000000012", Ref: "refs/heads/main", CommitSHA: strings.Repeat("a", 40), EventKind: "push", RoutingGeneration: 7, WorkflowPath: ".github/workflows/exact.yml", ProductionRef: true, Lease: lease}
	t.Run("production exact snapshot", func(t *testing.T) {
		dataStore := &eventStoreStub{}
		processor, _ := NewGitHubEventProcessor(dataStore, &eventClientStub{}, 10)
		outcome := processor.ProcessIntent(t.Context(), base)
		if outcome.Code != "processed" {
			t.Fatalf("outcome=%+v", outcome)
		}
	})
	t.Run("stale generation drops before side effect", func(t *testing.T) {
		dataStore := &eventStoreStub{executeError: store.ErrFailedPrecondition}
		processor, _ := NewGitHubEventProcessor(dataStore, &eventClientStub{}, 10)
		outcome := processor.ProcessIntent(t.Context(), base)
		if outcome.Code != "superseded" {
			t.Fatalf("outcome=%+v orchestrations=%d", outcome, len(dataStore.orchestrations))
		}
	})
	t.Run("production without environment is ignored", func(t *testing.T) {
		dataStore := &eventStoreStub{executeResult: "ignored"}
		intent := base
		intent.EnvironmentID = ""
		processor, _ := NewGitHubEventProcessor(dataStore, &eventClientStub{}, 10)
		outcome := processor.ProcessIntent(t.Context(), intent)
		if outcome.Code != "ignored" {
			t.Fatalf("outcome=%+v orchestrations=%d", outcome, len(dataStore.orchestrations))
		}
	})
	t.Run("preview cap is isolated", func(t *testing.T) {
		dataStore := &eventStoreStub{}
		intent := base
		intent.EnvironmentID, intent.ProductionRef, intent.Ref = "", false, "refs/heads/feature/cap"
		processor, _ := NewGitHubEventProcessor(dataStore, &eventClientStub{}, 10)
		outcome := processor.ProcessIntent(t.Context(), intent)
		if outcome.Code != "processed" {
			t.Fatalf("outcome=%+v previews=%d orchestrations=%d", outcome, dataStore.previewCalls, len(dataStore.orchestrations))
		}
	})
	t.Run("deletion converges no match", func(t *testing.T) {
		dataStore := &eventStoreStub{}
		intent := base
		intent.EnvironmentID, intent.ProductionRef, intent.EventKind, intent.CommitSHA, intent.Ref = "", false, "delete", "", "refs/heads/feature/gone"
		processor, _ := NewGitHubEventProcessor(dataStore, &eventClientStub{}, 10)
		outcome := processor.ProcessIntent(t.Context(), intent)
		if outcome.Code != "processed" {
			t.Fatalf("outcome=%+v teardown=%d", outcome, dataStore.teardownCalls)
		}
	})
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout secret payload" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

type eventClientStub struct {
	installation       GitHubEventInstallation
	repositories       []GitHubEventRepository
	installationErrors []error
	repositoryError    error
	installationCalls  int
	refreshes          int
	branchSHA          string
	branchExists       bool
	branchError        error
}

func (c *eventClientStub) GetInstallation(context.Context, int64) (GitHubEventInstallation, error) {
	c.installationCalls++
	if len(c.installationErrors) > 0 {
		err := c.installationErrors[0]
		c.installationErrors = c.installationErrors[1:]
		return GitHubEventInstallation{}, err
	}
	return c.installation, nil
}
func (c *eventClientStub) ListInstallationRepositories(context.Context, int64) ([]GitHubEventRepository, error) {
	return c.repositories, nil
}
func (c *eventClientStub) GetRepository(context.Context, int64, int64) (GitHubEventRepository, error) {
	if c.repositoryError != nil {
		return GitHubEventRepository{}, c.repositoryError
	}
	return activeEventRepository(), nil
}
func (c *eventClientStub) GetBranchRef(context.Context, int64, int64, string) (string, bool, error) {
	return c.branchSHA, c.branchExists, c.branchError
}
func (c *eventClientStub) RefreshInstallationCredentials(context.Context, int64) error {
	c.refreshes++
	return nil
}

type eventStoreStub struct {
	revocations     []store.RevokeGitHubEventParams
	reconciliations int
	snapshotResult  store.SnapshotGitHubPushResult
	snapshotParams  store.SnapshotGitHubPushParams
	snapshots       int
	authorizeError  error
	orchestrations  []store.EnqueueOrchestrationParams
	previewError    error
	previewCalls    int
	teardownError   error
	teardownCalls   int
	executeResult   string
	executeError    error
	revokeError     error
}

func (s *eventStoreStub) GetGitHubEventGeneration(context.Context, string, int64) (int64, error) {
	return 1, nil
}

func (s *eventStoreStub) ReconcileGitHubEvent(context.Context, store.ReconcileGitHubEventParams) (int64, error) {
	s.reconciliations++
	return 1, nil
}
func (s *eventStoreStub) RevokeGitHubEvent(_ context.Context, p store.RevokeGitHubEventParams) error {
	s.revocations = append(s.revocations, p)
	return s.revokeError
}
func (s *eventStoreStub) SnapshotGitHubPush(_ context.Context, p store.SnapshotGitHubPushParams) (store.SnapshotGitHubPushResult, error) {
	s.snapshots++
	s.snapshotParams = p
	return s.snapshotResult, nil
}
func (s *eventStoreStub) AuthorizeGitHubRoutingIntent(context.Context, store.GitHubRoutingIntent, store.LeaseToken) error {
	return s.authorizeError
}
func (s *eventStoreStub) ExecuteGitHubRoutingIntent(context.Context, string, store.LeaseToken, []store.PreviewIdentity) (string, error) {
	if s.executeResult == "" {
		s.executeResult = "processed"
	}
	return s.executeResult, s.executeError
}
func (s *eventStoreStub) EnqueueOrCoalesceOrchestration(_ context.Context, p store.EnqueueOrchestrationParams) (store.OrchestrationJob, error) {
	s.orchestrations = append(s.orchestrations, p)
	return store.OrchestrationJob{}, nil
}
func (s *eventStoreStub) AcceptPreviewPush(context.Context, store.PreviewPushParams) (store.PreviewPushResult, error) {
	s.previewCalls++
	return store.PreviewPushResult{}, s.previewError
}
func (s *eventStoreStub) RequestPreviewTeardown(context.Context, store.PreviewTeardownParams) (store.Environment, error) {
	s.teardownCalls++
	return store.Environment{}, s.teardownError
}

func activeEventInstallation() GitHubEventInstallation {
	return GitHubEventInstallation{ID: 42, AccountID: 43, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected"}
}
func activeEventRepository() GitHubEventRepository {
	return GitHubEventRepository{ID: 99, InstallationID: 42, NodeID: "R_99", Owner: "acme", Name: "api", FullName: "acme/api", Visibility: "private", DefaultBranch: "main"}
}
func eventDelivery(event, action, payload string, repository bool) store.GitHubWebhookDelivery {
	installationID := int64(42)
	value := store.GitHubWebhookDelivery{ID: "10000000-0000-0000-0000-000000000001", Event: event, Action: action, OrgID: "10000000-0000-0000-0000-000000000002", InstallationID: &installationID, Payload: []byte(payload), Lease: &store.LeaseToken{Owner: "worker", Epoch: 1, ExpiresAt: time.Now().Add(time.Minute)}}
	if repository {
		repositoryID, nodeID := int64(99), "R_99"
		value.RepositoryID, value.RepositoryNodeID = &repositoryID, &nodeID
	}
	digest := sha256.Sum256(value.Payload)
	value.PayloadDigest = hex.EncodeToString(digest[:])
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
