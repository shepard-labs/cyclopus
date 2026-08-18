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

	"github.com/jackc/pgx/v5/pgconn"
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
		{"unauthorized after refresh", &githubadapter.APIError{StatusCode: http.StatusUnauthorized}, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "permission_revoked"}},
		{"retryable forbidden defaults delay", &githubadapter.APIError{StatusCode: http.StatusForbidden, Retryable: true}, GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_rate_limited", RetryAfter: time.Minute}},
		{"permission forbidden", &githubadapter.APIError{StatusCode: http.StatusForbidden}, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "permission_revoked"}},
		{"not found", &githubadapter.APIError{StatusCode: http.StatusNotFound}, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "permission_revoked"}},
		{"conflict after revalidation", &githubadapter.APIError{StatusCode: http.StatusConflict}, GitHubEventOutcome{Disposition: GitHubEventFail, Code: "github_conflict"}},
		{"unprocessable after revalidation", &githubadapter.APIError{StatusCode: http.StatusUnprocessableEntity}, GitHubEventOutcome{Disposition: GitHubEventFail, Code: "github_conflict"}},
		{"server unavailable", &githubadapter.APIError{StatusCode: http.StatusInternalServerError}, GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_unavailable"}},
		{"unexpected client response", &githubadapter.APIError{StatusCode: http.StatusBadRequest}, GitHubEventOutcome{Disposition: GitHubEventFail, Code: "malformed_response"}},
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
	installation, _, outcome := processor.fetchInstallation(t.Context(), eventDelivery("installation", "created", `{"action":"created","installation":{"id":42}}`, false))
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
	_, _, outcome := processor.fetchInstallation(t.Context(), eventDelivery("installation", "created", `{"action":"created","installation":{"id":42}}`, false))
	want := GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "permission_revoked"}
	if outcome != want || client.refreshes != 1 || client.installationCalls != 2 {
		t.Fatalf("outcome=%+v refreshes=%d calls=%d", outcome, client.refreshes, client.installationCalls)
	}
}

func TestGitHubConflictRevalidation(t *testing.T) {
	tests := []struct {
		name   string
		errors []error
		want   GitHubEventOutcome
	}{
		{"409 succeeds after revalidation", []error{&githubadapter.APIError{StatusCode: http.StatusConflict}}, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "processed"}},
		{"422 succeeds after revalidation", []error{&githubadapter.APIError{StatusCode: http.StatusUnprocessableEntity}}, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "processed"}},
		{"409 remains conflict", []error{&githubadapter.APIError{StatusCode: http.StatusConflict}, &githubadapter.APIError{StatusCode: http.StatusConflict}}, GitHubEventOutcome{Disposition: GitHubEventFail, Code: "github_conflict"}},
		{"422 remains conflict", []error{&githubadapter.APIError{StatusCode: http.StatusUnprocessableEntity}, &githubadapter.APIError{StatusCode: http.StatusUnprocessableEntity}}, GitHubEventOutcome{Disposition: GitHubEventFail, Code: "github_conflict"}},
		{"conflict changes to rate limit", []error{&githubadapter.APIError{StatusCode: http.StatusConflict}, &githubadapter.APIError{StatusCode: http.StatusTooManyRequests, Retryable: true, RetryAfter: 2 * time.Minute}}, GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_rate_limited", RetryAfter: 2 * time.Minute}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataStore := &eventStoreStub{}
			client := &eventClientStub{installation: activeEventInstallation(), installationErrors: test.errors}
			processor, _ := NewGitHubEventProcessor(dataStore, client, 10)
			got := processor.ProcessDelivery(t.Context(), eventDelivery("installation", "created", `{"action":"created","installation":{"id":42}}`, false))
			if got != test.want || client.refreshes != 1 || client.installationCalls != 2 {
				t.Fatalf("outcome=%+v refreshes=%d calls=%d", got, client.refreshes, client.installationCalls)
			}
		})
	}
}

func TestGitHubExternalCallsArePersistentlyFenced(t *testing.T) {
	sha := strings.Repeat("a", 40)
	before := strings.Repeat("b", 40)
	tests := []struct {
		name     string
		delivery store.GitHubWebhookDelivery
		client   *eventClientStub
		failAt   int
		calls    int
		want     []string
	}{
		{"get installation", eventDelivery("installation", "created", `{"action":"created","installation":{"id":42}}`, false), &eventClientStub{installation: activeEventInstallation()}, 1, 0, []string{"authorize"}},
		{"list repositories", eventDelivery("installation", "created", `{"action":"created","installation":{"id":42}}`, false), &eventClientStub{installation: activeEventInstallation()}, 2, 1, []string{"authorize", "get_installation", "authorize"}},
		{"get repository each item", eventDelivery("installation_repositories", "added", `{"action":"added","installation":{"id":42},"repositories_added":[{"id":99,"node_id":"R_99"},{"id":99,"node_id":"R_99"}]}`, false), &eventClientStub{}, 2, 1, []string{"authorize", "get_repository", "authorize"}},
		{"get branch ref", eventDelivery("push", "", `{"installation":{"id":42},"repository":{"id":99,"node_id":"R_99"},"ref":"refs/heads/main","before":"`+before+`","after":"`+sha+`"}`, true), &eventClientStub{branchSHA: sha, branchExists: true}, 1, 0, []string{"authorize"}},
		{"refresh credentials", eventDelivery("installation", "created", `{"action":"created","installation":{"id":42}}`, false), &eventClientStub{installationErrors: []error{&githubadapter.APIError{StatusCode: http.StatusUnauthorized}}}, 2, 1, []string{"authorize", "get_installation", "authorize"}},
		{"retry original call", eventDelivery("installation", "created", `{"action":"created","installation":{"id":42}}`, false), &eventClientStub{installationErrors: []error{&githubadapter.APIError{StatusCode: http.StatusUnauthorized}}}, 3, 2, []string{"authorize", "get_installation", "authorize", "refresh_credentials", "authorize"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trace := []string{}
			test.client.trace = &trace
			dataStore := &eventStoreStub{authorizeFailAt: test.failAt, trace: &trace}
			processor, _ := NewGitHubEventProcessor(dataStore, test.client, 10)
			got := processor.ProcessDelivery(t.Context(), test.delivery)
			if got != (GitHubEventOutcome{Disposition: GitHubEventLeaseLost, Code: "lease_lost"}) || test.client.externalCalls != test.calls || strings.Join(trace, ",") != strings.Join(test.want, ",") {
				t.Fatalf("outcome=%+v external calls=%d trace=%v, want %d/%v", got, test.client.externalCalls, trace, test.calls, test.want)
			}
		})
	}
}

func TestGitHubStoreErrorClassification(t *testing.T) {
	tests := []struct {
		code string
		want GitHubEventOutcome
	}{
		{"40001", GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "database_unavailable"}},
		{"40P01", GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "database_unavailable"}},
		{"08006", GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "database_unavailable"}},
		{"23505", GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "database_unavailable"}},
		{"23503", GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "database_unavailable"}},
		{"23514", GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "database_unavailable"}},
		{"22P02", GitHubEventOutcome{Disposition: GitHubEventFail, Code: "invalid_event"}},
	}
	for _, test := range tests {
		if got := storeEventError(&pgconn.PgError{Code: test.code, Message: "secret raw database error"}); got != test.want {
			t.Fatalf("code %s outcome=%+v, want %+v", test.code, got, test.want)
		}
	}
	for _, test := range []struct {
		err  error
		want GitHubEventOutcome
	}{
		{store.ErrConflict, GitHubEventOutcome{Disposition: GitHubEventFail, Code: "invalid_event"}},
		{store.ErrInvalidArgument, GitHubEventOutcome{Disposition: GitHubEventFail, Code: "invalid_event"}},
		{store.ErrFailedPrecondition, GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "ignored"}},
	} {
		if got := storeEventError(test.err); got != test.want {
			t.Fatalf("domain error outcome=%+v, want %+v", got, test.want)
		}
	}
}

func TestGitHubEventProcessorValidatesCallPinDuration(t *testing.T) {
	for _, durations := range [][]time.Duration{{time.Second}, {time.Second, 0}, {5 * time.Second, time.Nanosecond}, {15*time.Second - time.Nanosecond, 10 * time.Second}} {
		if _, err := NewGitHubEventProcessor(&eventStoreStub{}, &eventClientStub{}, 10, durations...); err == nil {
			t.Fatalf("durations %v accepted", durations)
		}
	}
	if _, err := NewGitHubEventProcessor(&eventStoreStub{}, &eventClientStub{}, 10, 15*time.Second, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubExternalCallDeadlineUsesPinnedExpiryAndInjectedClock(t *testing.T) {
	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name           string
		requestTimeout time.Duration
		pinExpiry      time.Time
		pinDelay       time.Duration
		wantDeadline   time.Time
		wantCalls      int
		wantOutcome    GitHubEventOutcome
	}{
		{name: "request timeout is earlier", requestTimeout: 4 * time.Second, pinExpiry: base.Add(12 * time.Second), wantDeadline: base.Add(4 * time.Second), wantCalls: 1, wantOutcome: GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "processed"}},
		{name: "pinned expiry margin is earlier", requestTimeout: 10 * time.Second, pinExpiry: base.Add(12 * time.Second), wantDeadline: base.Add(7 * time.Second), wantCalls: 1, wantOutcome: GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "processed"}},
		{name: "no call window remains", requestTimeout: 10 * time.Second, pinExpiry: base.Add(5 * time.Second), wantCalls: 0, wantOutcome: GitHubEventOutcome{Disposition: GitHubEventLeaseLost, Code: "lease_lost"}},
		{name: "pin database delay consumes call window", requestTimeout: 10 * time.Second, pinExpiry: base.Add(10 * time.Second), pinDelay: 3 * time.Second, wantDeadline: base.Add(5 * time.Second), wantCalls: 1, wantOutcome: GitHubEventOutcome{Disposition: GitHubEventComplete, Code: "processed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := base
			dataStore := &eventStoreStub{pinExpiry: test.pinExpiry, pin: func() { now = now.Add(test.pinDelay) }}
			client := &eventClientStub{installation: GitHubEventInstallation{ID: 42, Suspended: true}}
			processor, err := NewGitHubEventProcessor(dataStore, client, 10, 15*time.Second, test.requestTimeout)
			if err != nil {
				t.Fatal(err)
			}
			processor.now = func() time.Time { return now }
			got := processor.ProcessDelivery(t.Context(), eventDelivery("installation", "suspend", `{"action":"suspend","installation":{"id":42}}`, false))
			if got != test.wantOutcome || client.externalCalls != test.wantCalls {
				t.Fatalf("outcome=%+v external calls=%d, want %+v/%d", got, client.externalCalls, test.wantOutcome, test.wantCalls)
			}
			if test.wantCalls > 0 && (len(client.deadlines) != 1 || !client.deadlines[0].Equal(test.wantDeadline)) {
				t.Fatalf("API deadlines = %v, want [%s]", client.deadlines, test.wantDeadline)
			}
		})
	}
}

func TestGitHubExternalCallContextCancelsAtPinnedExpiryMargin(t *testing.T) {
	start := time.Now()
	dataStore := &eventStoreStub{pinExpiry: start.Add(githubEventCallPinSafetyMargin + 80*time.Millisecond)}
	client := &eventClientStub{installationCall: func(ctx context.Context) (GitHubEventInstallation, error) {
		<-ctx.Done()
		return GitHubEventInstallation{}, ctx.Err()
	}}
	processor, err := NewGitHubEventProcessor(dataStore, client, 10, 10*time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	outcome := processor.ProcessDelivery(t.Context(), eventDelivery("installation", "suspend", `{"action":"suspend","installation":{"id":42}}`, false))
	elapsed := time.Since(start)
	if outcome != (GitHubEventOutcome{Disposition: GitHubEventRetry, Code: "github_unavailable"}) || client.externalCalls != 1 {
		t.Fatalf("outcome=%+v external calls=%d", outcome, client.externalCalls)
	}
	if elapsed < 40*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("API context canceled after %s, want near 80ms pinned-expiry margin", elapsed)
	}
}

func TestGitHubReclaimedDeliveryCannotStartExternalCall(t *testing.T) {
	entered, reclaimed := make(chan struct{}), make(chan struct{})
	dataStore := &eventStoreStub{authorize: func() error {
		close(entered)
		<-reclaimed
		return store.ErrLeaseNotOwned
	}}
	client := &eventClientStub{installation: activeEventInstallation()}
	processor, _ := NewGitHubEventProcessor(dataStore, client, 10)
	done := make(chan GitHubEventOutcome, 1)
	go func() {
		done <- processor.ProcessDelivery(t.Context(), eventDelivery("installation", "created", `{"action":"created","installation":{"id":42}}`, false))
	}()
	<-entered
	close(reclaimed)
	if got := <-done; got != (GitHubEventOutcome{Disposition: GitHubEventLeaseLost, Code: "lease_lost"}) || client.externalCalls != 0 {
		t.Fatalf("outcome=%+v external calls=%d", got, client.externalCalls)
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
			if got.Kind != test.wantKind || got.DeliveryID != delivery.ID || got.DeliveryLease.Owner != delivery.Lease.Owner || got.DeliveryLease.Epoch != delivery.Lease.Epoch ||
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
	externalCalls      int
	trace              *[]string
	deadlines          []time.Time
	installationCall   func(context.Context) (GitHubEventInstallation, error)
}

func (c *eventClientStub) record(call string) {
	c.externalCalls++
	if c.trace != nil {
		*c.trace = append(*c.trace, call)
	}
}

func (c *eventClientStub) GetInstallation(ctx context.Context, _ int64) (GitHubEventInstallation, error) {
	c.record("get_installation")
	c.installationCalls++
	if deadline, ok := ctx.Deadline(); ok {
		c.deadlines = append(c.deadlines, deadline)
	}
	if c.installationCall != nil {
		return c.installationCall(ctx)
	}
	if len(c.installationErrors) > 0 {
		err := c.installationErrors[0]
		c.installationErrors = c.installationErrors[1:]
		return GitHubEventInstallation{}, err
	}
	return c.installation, nil
}
func (c *eventClientStub) ListInstallationRepositories(context.Context, int64) ([]GitHubEventRepository, error) {
	c.record("list_repositories")
	return c.repositories, nil
}
func (c *eventClientStub) GetRepository(context.Context, int64, int64) (GitHubEventRepository, error) {
	c.record("get_repository")
	if c.repositoryError != nil {
		return GitHubEventRepository{}, c.repositoryError
	}
	return activeEventRepository(), nil
}
func (c *eventClientStub) GetBranchRef(context.Context, int64, int64, string) (string, bool, error) {
	c.record("get_branch_ref")
	return c.branchSHA, c.branchExists, c.branchError
}
func (c *eventClientStub) RefreshInstallationCredentials(context.Context, int64) error {
	c.record("refresh_credentials")
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
	authorizeCalls  int
	authorizeFailAt int
	trace           *[]string
	authorize       func() error
	orchestrations  []store.EnqueueOrchestrationParams
	previewError    error
	previewCalls    int
	teardownError   error
	teardownCalls   int
	executeResult   string
	executeError    error
	revokeError     error
	pinExpiry       time.Time
	pin             func()
}

func (s *eventStoreStub) PinGitHubEventExternalCall(_ context.Context, _ string, token store.LeaseToken, _ string, duration time.Duration) (store.LeaseToken, error) {
	s.authorizeCalls++
	if s.pin != nil {
		s.pin()
	}
	if s.trace != nil {
		*s.trace = append(*s.trace, "authorize")
	}
	if s.authorizeFailAt > 0 && s.authorizeCalls == s.authorizeFailAt {
		return store.LeaseToken{}, store.ErrLeaseNotOwned
	}
	if s.authorize != nil {
		if err := s.authorize(); err != nil {
			return store.LeaseToken{}, err
		}
	}
	if s.authorizeError != nil {
		return store.LeaseToken{}, s.authorizeError
	}
	if s.pinExpiry.IsZero() {
		token.ExpiresAt = time.Now().Add(duration)
	} else {
		token.ExpiresAt = s.pinExpiry
	}
	return token, nil
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
