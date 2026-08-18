//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGitHubPushDurableFanoutPostgres18(t *testing.T) {
	t.Run("expired manifest lease is reclaimed and stale attempt is fenced", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := snapshotGitHubFanoutCount(t, dataStore, "50000000-0000-0000-0000-000000000010", "refs/heads/feature/reclaim", strings.Repeat("0", 40), strings.Repeat("1", 40), 2, 2, 2)
		first := claimOneGitHubFanout(t, dataStore, "crashed-worker", time.Minute)
		if first.Attempts != 1 {
			t.Fatalf("first manifest attempt = %d, want 1", first.Attempts)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_fanout_manifests SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
			t.Fatal(err)
		}
		second := claimOneGitHubFanout(t, New(pool), "replacement-worker", time.Minute)
		if second.ID != first.ID || second.Attempts != 2 || second.Lease.Epoch != first.Lease.Epoch+1 {
			t.Fatalf("reclaimed manifest = %+v, first = %+v", second, first)
		}
		staleErrors := []error{
			func() error {
				_, err := dataStore.ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: first.ID, ManifestLease: *first.Lease, BatchLimit: 2})
				return err
			}(),
			func() error {
				_, err := dataStore.RenewGitHubRoutingFanoutManifest(t.Context(), first.ID, *first.Lease, time.Minute)
				return err
			}(),
			dataStore.RetryGitHubRoutingFanoutManifest(t.Context(), first.ID, *first.Lease, 0, "retry", "stale"),
			dataStore.FailGitHubRoutingFanoutManifest(t.Context(), first.ID, *first.Lease, "internal", "stale"),
		}
		for operation, err := range staleErrors {
			if !errors.Is(err, ErrLeaseNotOwned) {
				t.Fatalf("stale manifest operation %d error = %v", operation, err)
			}
		}
		result, err := New(pool).ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: second.ID, ManifestLease: *second.Lease, BatchLimit: 2, Audit: &AuditEvent{ID: "51000000-0000-0000-0000-000000000110"}})
		if err != nil || !result.Terminal || result.InsertedCount != 2 {
			t.Fatalf("replacement continuation = %+v, %v", result, err)
		}
		assertFanoutState(t, pool, delivery.ID, "completed", 2, 2, 0, 2)
	})

	t.Run("expired final batch rolls back before blocked reclaim", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := snapshotGitHubFanoutCount(t, dataStore, "50000000-0000-0000-0000-000000000016", "refs/heads/feature/final-fence", strings.Repeat("0", 40), strings.Repeat("7", 40), 2, 2, 2)
		manifest := claimOneGitHubFanout(t, dataStore, "expiring-final-worker", 100*time.Millisecond)
		paused, resume := make(chan struct{}), make(chan struct{})
		staleDone := make(chan error, 1)
		staleAuditID := "51000000-0000-0000-0000-000000000116"
		go func() {
			_, err := New(pool).ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{
				ManifestID: manifest.ID, ManifestLease: *manifest.Lease, BatchLimit: 2,
				Audit: &AuditEvent{ID: staleAuditID}, Failpoint: func(point string) error {
					if point == "before_manifest_completion" {
						close(paused)
						<-resume
					}
					return nil
				},
			})
			staleDone <- err
		}()
		<-paused
		if _, err := pool.Exec(t.Context(), `SELECT pg_sleep(0.15)`); err != nil {
			t.Fatal(err)
		}

		claimed, err := New(pool).ClaimGitHubRoutingFanoutManifests(t.Context(), "blocked-replacement", time.Minute, 1)
		if err != nil || len(claimed) != 0 {
			t.Fatalf("skip-locked claim while final transaction paused = %+v, %v", claimed, err)
		}

		close(resume)
		if err := <-staleDone; !errors.Is(err, ErrLeaseNotOwned) {
			t.Fatalf("expired final continuation error = %v", err)
		}
		replacement := claimOneGitHubFanout(t, New(pool), "replacement-final-worker", time.Minute)
		if replacement.ID != manifest.ID || replacement.Lease.Epoch != manifest.Lease.Epoch+1 {
			t.Fatalf("replacement manifest = %+v, stale = %+v", replacement, manifest)
		}

		assertFanoutState(t, pool, delivery.ID, "leased", 2, 0, 2, 0)
		var staleAudits int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE id=$1`, staleAuditID).Scan(&staleAudits); err != nil || staleAudits != 0 {
			t.Fatalf("stale audit count = %d, %v", staleAudits, err)
		}
		result, err := New(pool).ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: replacement.ID, ManifestLease: *replacement.Lease, BatchLimit: 2, Audit: &AuditEvent{ID: "51000000-0000-0000-0000-000000000117"}})
		if err != nil || !result.Terminal || result.InsertedCount != 2 {
			t.Fatalf("replacement final continuation = %+v, %v", result, err)
		}
		assertFanoutState(t, pool, delivery.ID, "completed", 2, 2, 0, 2)
	})

	t.Run("expired attempt twelve is swept terminal and never claimed thirteen", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := snapshotGitHubFanoutCount(t, dataStore, "50000000-0000-0000-0000-000000000011", "refs/heads/feature/exhausted", strings.Repeat("0", 40), strings.Repeat("2", 40), 2, 2, 2)
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_fanout_manifests SET attempts=11 WHERE delivery_id=$1`, delivery.ID); err != nil {
			t.Fatal(err)
		}
		attemptTwelve := claimOneGitHubFanout(t, dataStore, "attempt-twelve", time.Minute)
		if attemptTwelve.Attempts != 12 {
			t.Fatalf("manifest attempt = %d, want 12", attemptTwelve.Attempts)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_fanout_manifests SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, attemptTwelve.ID); err != nil {
			t.Fatal(err)
		}
		swept, err := dataStore.FailExpiredExhaustedGitHubEventLeases(t.Context(), time.Hour, 100)
		if err != nil || swept.Fanouts != 1 {
			t.Fatalf("exhausted sweep = %+v, %v", swept, err)
		}
		claimed, err := dataStore.ClaimGitHubRoutingFanoutManifests(t.Context(), "attempt-thirteen", time.Minute, 1)
		if err != nil || len(claimed) != 0 {
			t.Fatalf("attempt thirteen claims = %+v, %v", claimed, err)
		}
		var status, code string
		var attempts int
		var terminalAt time.Time
		if err := pool.QueryRow(t.Context(), `SELECT status,attempts,error_code,terminal_at FROM github_routing_fanout_manifests WHERE id=$1`, attemptTwelve.ID).Scan(&status, &attempts, &code, &terminalAt); err != nil || status != "failed" || attempts != 12 || code != "attempts_exhausted" || terminalAt.IsZero() {
			t.Fatalf("exhausted manifest = %s/%d/%s/%s, %v", status, attempts, code, terminalAt, err)
		}
		assertGitHubFanoutCandidates(t, pool, delivery.ID, 0)
	})

	t.Run("terminal transitions delete candidates and preserve exact intents", func(t *testing.T) {
		for _, test := range []struct {
			name        string
			terminal    func(*testing.T, *Store, *pgxpool.Pool, GitHubWebhookDelivery)
			wantIntents int
		}{
			{name: "final batch complete", wantIntents: 2, terminal: func(t *testing.T, dataStore *Store, _ *pgxpool.Pool, delivery GitHubWebhookDelivery) {
				result, err := continueClaimedGitHubFanout(t, dataStore, delivery.ID, "complete", 2, &AuditEvent{ID: "59100000-0000-0000-0000-000000000001"}, nil)
				if err != nil || !result.Terminal || result.InsertedCount != 2 {
					t.Fatalf("final continuation = %+v, %v", result, err)
				}
			}},
			{name: "explicit fail", terminal: func(t *testing.T, dataStore *Store, _ *pgxpool.Pool, _ GitHubWebhookDelivery) {
				manifest := claimOneGitHubFanout(t, dataStore, "fail", time.Minute)
				if err := dataStore.FailGitHubRoutingFanoutManifest(t.Context(), manifest.ID, *manifest.Lease, "invalid_snapshot", "invalid_snapshot"); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "retry attempt twelve", terminal: func(t *testing.T, dataStore *Store, pool *pgxpool.Pool, _ GitHubWebhookDelivery) {
				if _, err := pool.Exec(t.Context(), `UPDATE github_routing_fanout_manifests SET attempts=11`); err != nil {
					t.Fatal(err)
				}
				manifest := claimOneGitHubFanout(t, dataStore, "retry-twelve", time.Minute)
				if manifest.Attempts != 12 {
					t.Fatalf("attempt = %d, want 12", manifest.Attempts)
				}
				if err := dataStore.RetryGitHubRoutingFanoutManifest(t.Context(), manifest.ID, *manifest.Lease, 0, "database_unavailable", "database_unavailable"); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				dataStore, pool := newGitHubFanoutFixture(t)
				delivery := snapshotGitHubFanoutCount(t, dataStore, "59000000-0000-0000-0000-000000000001", "refs/heads/feature/terminal", strings.Repeat("0", 40), strings.Repeat("1", 40), 2, 2, 2)
				assertGitHubFanoutCandidates(t, pool, delivery.ID, 2)
				test.terminal(t, dataStore, pool, delivery)
				assertGitHubFanoutCandidates(t, pool, delivery.ID, 0)
				assertGitHubFanoutIntents(t, pool, delivery.ID, test.wantIntents)
			})
		}
	})

	t.Run("zero fanout completes without candidates or intents", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		if _, err := pool.Exec(t.Context(), `UPDATE apps SET previews_enabled=false,production_branch='other'`); err != nil {
			t.Fatal(err)
		}
		delivery := receiveDelivery(t, dataStore, webhookParams("59000000-0000-0000-0000-000000000002", strings.Repeat("2", 64), `{}`))
		delivery = claimOne(t, dataStore, "zero", time.Minute)
		result, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/feature/zero", BeforeSHA: strings.Repeat("0", 40), CommitSHA: strings.Repeat("2", 40), AuthoritativeSHA: strings.Repeat("2", 40), EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2, Audit: &AuditEvent{ID: "59100000-0000-0000-0000-000000000002"}})
		if err != nil || !result.Terminal || result.ManifestStatus != "completed" || result.TotalCount != 0 || result.InsertedCount != 0 {
			t.Fatalf("zero snapshot = %+v, %v", result, err)
		}
		assertGitHubFanoutCandidates(t, pool, delivery.ID, 0)
		assertGitHubFanoutIntents(t, pool, delivery.ID, 0)
	})

	t.Run("pending and leased manifests retain candidates", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := snapshotGitHubFanoutCount(t, dataStore, "59000000-0000-0000-0000-000000000003", "refs/heads/feature/active", strings.Repeat("0", 40), strings.Repeat("3", 40), 2, 1, 2)
		assertGitHubFanoutCandidates(t, pool, delivery.ID, 2)
		if deleted, err := dataStore.DeleteTerminalGitHubRoutingFanoutCandidates(t.Context(), 100); err != nil || deleted != 0 {
			t.Fatalf("pending cleanup = %d, %v", deleted, err)
		}
		manifest := claimOneGitHubFanout(t, dataStore, "leased", time.Minute)
		if deleted, err := dataStore.DeleteTerminalGitHubRoutingFanoutCandidates(t.Context(), 100); err != nil || deleted != 0 {
			t.Fatalf("leased cleanup = %d, %v", deleted, err)
		}
		if manifest.DeliveryID != delivery.ID {
			t.Fatalf("leased delivery = %q, want %q", manifest.DeliveryID, delivery.ID)
		}
		assertGitHubFanoutCandidates(t, pool, delivery.ID, 2)
	})

	t.Run("continuation superseded deletes candidates without adding intents", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := snapshotGitHubFanoutCount(t, dataStore, "59000000-0000-0000-0000-000000000007", "refs/heads/feature/continuation-superseded", strings.Repeat("0", 40), strings.Repeat("7", 40), 2, 2, 2)
		manifest := claimOneGitHubFanout(t, dataStore, "continuation-superseded", time.Minute)
		if _, err := pool.Exec(t.Context(), `UPDATE github_ref_routing_state SET validated_sha=$3,routing_generation=routing_generation+1 WHERE repository_id=$1 AND ref=$2`, githubWebhookRepo, "refs/heads/feature/continuation-superseded", strings.Repeat("8", 40)); err != nil {
			t.Fatal(err)
		}
		result, err := dataStore.ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: manifest.ID, ManifestLease: *manifest.Lease, BatchLimit: 2})
		if err != nil || !result.Terminal || !result.Superseded || result.InsertedCount != 0 {
			t.Fatalf("superseded continuation = %+v, %v", result, err)
		}
		assertGitHubFanoutCandidates(t, pool, delivery.ID, 0)
		assertGitHubFanoutIntents(t, pool, delivery.ID, 0)
	})

	t.Run("terminal idempotent snapshot read repairs leftover candidates", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := snapshotGitHubFanoutCount(t, dataStore, "59000000-0000-0000-0000-000000000004", "refs/heads/feature/read-repair", strings.Repeat("0", 40), strings.Repeat("4", 40), 2, 2, 2)
		manifest := claimOneGitHubFanout(t, dataStore, "read-repair", time.Minute)
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_fanout_manifests SET status='completed',inserted_count=total_count,cursor_app_id=eligibility_cutoff_app_id,terminal_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, manifest.ID); err != nil {
			t.Fatal(err)
		}
		result, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/feature/read-repair", BeforeSHA: strings.Repeat("0", 40), CommitSHA: strings.Repeat("4", 40), AuthoritativeSHA: strings.Repeat("4", 40), EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2})
		if err != nil || !result.Terminal || result.ManifestStatus != "completed" || result.TotalCount != 2 {
			t.Fatalf("terminal idempotent read = %+v, %v", result, err)
		}
		assertGitHubFanoutCandidates(t, pool, delivery.ID, 0)
	})

	t.Run("bounded cleanup repairs leftovers and skips concurrent active continuation", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		terminal := snapshotGitHubFanoutCount(t, dataStore, "59000000-0000-0000-0000-000000000005", "refs/heads/feature/repair", strings.Repeat("0", 40), strings.Repeat("5", 40), 2, 1, 2)
		terminalManifest := claimOneGitHubFanout(t, dataStore, "seed-terminal", time.Minute)
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_fanout_manifests SET status='failed',error_code='seeded',terminal_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, terminalManifest.ID); err != nil {
			t.Fatal(err)
		}
		active := snapshotGitHubFanoutCount(t, dataStore, "59000000-0000-0000-0000-000000000006", "refs/heads/feature/active-cleanup", strings.Repeat("0", 40), strings.Repeat("6", 40), 2, 1, 2)
		activeManifest := claimOneGitHubFanout(t, dataStore, "active-continuation", time.Minute)
		locked, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := New(pool).ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: activeManifest.ID, ManifestLease: *activeManifest.Lease, BatchLimit: 1, Failpoint: func(point string) error {
				if point == "after_routing_lock" {
					close(locked)
					<-release
				}
				return nil
			}})
			done <- err
		}()
		<-locked
		deleted, err := dataStore.DeleteTerminalGitHubRoutingFanoutCandidates(t.Context(), 1)
		if err != nil || deleted != 2 {
			t.Fatalf("bounded cleanup = %d, %v, want two rows from one manifest", deleted, err)
		}
		assertGitHubFanoutCandidates(t, pool, terminal.ID, 0)
		assertGitHubFanoutCandidates(t, pool, active.ID, 2)
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		assertGitHubFanoutCandidates(t, pool, active.ID, 2)
		assertGitHubFanoutIntents(t, pool, active.ID, 1)
	})

	t.Run("eligibility materialization does not lock delivery renewal", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := receiveDelivery(t, dataStore, webhookParams("50000000-0000-0000-0000-000000000012", strings.Repeat("3", 64), `{}`))
		delivery = claimOne(t, dataStore, "snapshot-worker", time.Minute)
		paused, resume := make(chan struct{}), make(chan struct{})
		result := make(chan error, 1)
		go func() {
			_, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/feature/nonblocking", BeforeSHA: strings.Repeat("0", 40), CommitSHA: strings.Repeat("3", 40), AuthoritativeSHA: strings.Repeat("3", 40), EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2, Failpoint: func(point string) error {
				if point == "after_eligibility" {
					close(paused)
					<-resume
				}
				return nil
			}})
			result <- err
		}()
		<-paused
		renewCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		renewed, err := New(pool).RenewGitHubWebhookDelivery(renewCtx, delivery.ID, *delivery.Lease, time.Minute)
		if err != nil || renewed.Epoch != delivery.Lease.Epoch || !renewed.ExpiresAt.After(delivery.Lease.ExpiresAt) {
			t.Fatalf("renew while materialization paused = %+v, %v", renewed, err)
		}
		close(resume)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		assertFanoutState(t, pool, delivery.ID, "pending", 2, 0, 2, 0)
	})

	t.Run("expired delivery at final fence rolls materialization back", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := receiveDelivery(t, dataStore, webhookParams("50000000-0000-0000-0000-000000000013", strings.Repeat("4", 64), `{}`))
		delivery = claimOne(t, dataStore, "expiring-snapshot", 100*time.Millisecond)
		paused, resume := make(chan struct{}), make(chan struct{})
		result := make(chan error, 1)
		auditID := "51000000-0000-0000-0000-000000000113"
		go func() {
			_, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/feature/expired-fence", BeforeSHA: strings.Repeat("0", 40), CommitSHA: strings.Repeat("4", 40), AuthoritativeSHA: strings.Repeat("4", 40), EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2, Audit: &AuditEvent{ID: auditID}, Failpoint: func(point string) error {
				if point == "after_eligibility" {
					close(paused)
					<-resume
				}
				return nil
			}})
			result <- err
		}()
		<-paused
		if _, err := pool.Exec(t.Context(), `SELECT pg_sleep(0.15)`); err != nil {
			t.Fatal(err)
		}
		close(resume)
		if err := <-result; !errors.Is(err, ErrLeaseNotOwned) {
			t.Fatalf("expired final fence error = %v", err)
		}
		var observations, manifests, candidates, intents, audits int
		if err := pool.QueryRow(t.Context(), `SELECT
			(SELECT count(*) FROM github_ref_transition_observations WHERE delivery_id=$1),
			(SELECT count(*) FROM github_routing_fanout_manifests WHERE delivery_id=$1),
			(SELECT count(*) FROM github_routing_fanout_candidates WHERE manifest_id IN (SELECT id FROM github_routing_fanout_manifests WHERE delivery_id=$1)),
			(SELECT count(*) FROM github_routing_intents WHERE delivery_id=$1),
			(SELECT count(*) FROM audit_events WHERE id=$2)`, delivery.ID, auditID).Scan(&observations, &manifests, &candidates, &intents, &audits); err != nil {
			t.Fatal(err)
		}
		if observations != 0 || manifests != 0 || candidates != 0 || intents != 0 || audits != 0 {
			t.Fatalf("expired snapshot partial state = %d/%d/%d/%d/%d", observations, manifests, candidates, intents, audits)
		}
	})

	t.Run("continuation and newer snapshot share routing then manifest lock order", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		seedGitHubFanoutApps(t, pool, 249, 7000)
		old := snapshotGitHubFanout(t, dataStore, "50000000-0000-0000-0000-000000000014", "refs/heads/feature/interleaving", strings.Repeat("0", 40), strings.Repeat("5", 40), 251, 100)
		oldClaim := claimOneGitHubFanout(t, dataStore, "old-continuation", time.Minute)
		locked, release := make(chan struct{}), make(chan struct{})
		oldDone := make(chan error, 1)
		go func() {
			_, err := New(pool).ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: oldClaim.ID, ManifestLease: *oldClaim.Lease, BatchLimit: 100, Failpoint: func(point string) error {
				if point == "after_routing_lock" {
					close(locked)
					<-release
				}
				return nil
			}})
			oldDone <- err
		}()
		<-locked
		newerDelivery := receiveDelivery(t, dataStore, webhookParams("50000000-0000-0000-0000-000000000015", strings.Repeat("6", 64), `{}`))
		newerDelivery = claimOne(t, dataStore, "newer-snapshot", time.Minute)
		newDone := make(chan error, 1)
		go func() {
			_, err := New(pool).SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: newerDelivery.ID, DeliveryLease: *newerDelivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/feature/interleaving", BeforeSHA: strings.Repeat("5", 40), CommitSHA: strings.Repeat("6", 40), AuthoritativeSHA: strings.Repeat("6", 40), EventKind: "push", RefExists: true, MaxFanout: 251, FanoutBatch: 100})
			newDone <- err
		}()
		close(release)
		for name, done := range map[string]<-chan error{"old continuation": oldDone, "newer snapshot": newDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s error = %v", name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s deadlocked", name)
			}
		}
		var oldStatus, newStatus string
		if err := pool.QueryRow(t.Context(), `SELECT
			(SELECT status FROM github_routing_fanout_manifests WHERE delivery_id=$1),
			(SELECT status FROM github_routing_fanout_manifests WHERE delivery_id=$2)`, old.ID, newerDelivery.ID).Scan(&oldStatus, &newStatus); err != nil {
			t.Fatal(err)
		}
		if oldStatus != "superseded" || newStatus != "pending" {
			t.Fatalf("interleaved manifest statuses = old:%s new:%s", oldStatus, newStatus)
		}
		assertGitHubFanoutCandidates(t, pool, old.ID, 0)
		assertGitHubFanoutCandidates(t, pool, newerDelivery.ID, 251)
	})

	t.Run("251 candidates continue 100 100 51 across restarts without duplicates", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		seedGitHubFanoutApps(t, pool, 249, 1000)
		delivery := snapshotGitHubFanout(t, dataStore, "50000000-0000-0000-0000-000000000001", "refs/heads/feature/batch", strings.Repeat("0", 40), strings.Repeat("a", 40), 251, 100)
		assertFanoutState(t, pool, delivery.ID, "pending", 251, 0, 251, 0)

		for index, want := range []int{100, 200, 251} {
			restarted := New(pool)
			result, err := continueClaimedGitHubFanout(t, restarted, delivery.ID, fmt.Sprintf("batch-%d", index+1), 100, &AuditEvent{ID: fmt.Sprintf("51000000-0000-0000-0000-%012d", index+1)}, nil)
			if err != nil || result.TotalCount != 251 || result.InsertedCount != want || result.Terminal != (want == 251) {
				t.Fatalf("continuation %d = %+v, %v", index+1, result, err)
			}
			status := "pending"
			if want == 251 {
				status = "completed"
			}
			wantCandidates := 251
			if want == 251 {
				wantCandidates = 0
			}
			assertFanoutState(t, pool, delivery.ID, status, 251, want, wantCandidates, want)
		}
		var unique int
		if err := pool.QueryRow(t.Context(), `SELECT count(DISTINCT app_id) FROM github_routing_intents WHERE delivery_id=$1`, delivery.ID).Scan(&unique); err != nil || unique != 251 {
			t.Fatalf("unique intent apps = %d, %v", unique, err)
		}
		manifest := githubFanoutManifestByDelivery(t, New(pool), delivery.ID)
		result, err := New(pool).ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: manifest.ID, ManifestLease: LeaseToken{Owner: "terminal-reader"}, BatchLimit: 100, Audit: &AuditEvent{ID: "51000000-0000-0000-0000-000000000099"}})
		if err != nil || !result.Terminal || result.InsertedCount != 251 {
			t.Fatalf("duplicate continuation = %+v, %v", result, err)
		}
	})

	t.Run("concurrent workers and failpoint preserve cursor transaction boundary", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		seedGitHubFanoutApps(t, pool, 249, 2000)
		delivery := snapshotGitHubFanout(t, dataStore, "50000000-0000-0000-0000-000000000002", "refs/heads/feature/concurrent", strings.Repeat("0", 40), strings.Repeat("b", 40), 251, 100)
		boom := errors.New("after intent insert")
		failedClaim := claimOneGitHubFanout(t, New(pool), "failpoint", time.Minute)
		_, err := New(pool).ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: failedClaim.ID, ManifestLease: *failedClaim.Lease, BatchLimit: 100, Failpoint: func(point string) error {
			if point == "after_intent_batch" {
				return boom
			}
			return nil
		}})
		if !errors.Is(err, boom) {
			t.Fatalf("failpoint error = %v", err)
		}
		if err := dataStore.RetryGitHubRoutingFanoutManifest(t.Context(), failedClaim.ID, *failedClaim.Lease, 0, "retry", "retry"); err != nil {
			t.Fatal(err)
		}
		assertFanoutState(t, pool, delivery.ID, "pending", 251, 0, 251, 0)

		claims := make(chan []GitHubRoutingFanoutManifest, 2)
		var wg sync.WaitGroup
		for index := range 2 {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				claimed, err := New(pool).ClaimGitHubRoutingFanoutManifests(t.Context(), fmt.Sprintf("concurrent-%d", index), time.Minute, 1)
				if err != nil {
					t.Errorf("claim: %v", err)
				}
				claims <- claimed
			}(index)
		}
		wg.Wait()
		close(claims)
		var claimed GitHubRoutingFanoutManifest
		empty := 0
		for result := range claims {
			if len(result) == 0 {
				empty++
			} else {
				claimed = result[0]
			}
		}
		if claimed.ID == "" || empty != 1 {
			t.Fatalf("concurrent claims = claimed:%q empty:%d, want one exclusive claim", claimed.ID, empty)
		}
		result, err := New(pool).ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: claimed.ID, ManifestLease: *claimed.Lease, BatchLimit: 100})
		if err != nil || result.InsertedCount != 100 || result.Terminal {
			t.Fatalf("exclusive continuation = %+v, %v", result, err)
		}
		assertFanoutState(t, pool, delivery.ID, "pending", 251, 100, 251, 100)
		result, err = continueClaimedGitHubFanout(t, New(pool), delivery.ID, "restart-2", 100, nil, nil)
		if err != nil || result.InsertedCount != 200 || result.Terminal {
			t.Fatalf("second restart = %+v, %v", result, err)
		}
		result, err = continueClaimedGitHubFanout(t, New(pool), delivery.ID, "restart-3", 100, &AuditEvent{ID: "51000000-0000-0000-0000-000000000099"}, nil)
		if err != nil || !result.Terminal || result.InsertedCount != 251 {
			t.Fatalf("restart after committed batches = %+v, %v", result, err)
		}
		assertFanoutState(t, pool, delivery.ID, "completed", 251, 251, 0, 251)
	})

	t.Run("old worker remains fenced while replacement commits first fanout batch", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := snapshotGitHubFanoutCount(t, dataStore, "50000000-0000-0000-0000-000000000017", "refs/heads/feature/active-reclaim", strings.Repeat("0", 40), strings.Repeat("8", 40), 2, 1, 2)
		old := claimOneGitHubFanout(t, dataStore, "old-active-fanout", time.Minute)
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_fanout_manifests SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
			t.Fatal(err)
		}
		fresh := claimOneGitHubFanout(t, New(pool), "replacement-fanout", time.Minute)
		committed, release := make(chan struct{}), make(chan struct{})
		freshDone := make(chan error, 1)
		go func() {
			_, err := New(pool).ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: fresh.ID, ManifestLease: *fresh.Lease, BatchLimit: 1, Failpoint: func(point string) error {
				if point == "after_intent_batch" {
					close(committed)
					<-release
				}
				return nil
			}})
			freshDone <- err
		}()
		<-committed
		close(release)
		if err := <-freshDone; err != nil {
			t.Fatal(err)
		}
		if _, err := dataStore.ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: old.ID, ManifestLease: *old.Lease, BatchLimit: 1}); !errors.Is(err, ErrLeaseNotOwned) {
			t.Fatalf("old active fanout error = %v", err)
		}
		assertFanoutState(t, pool, delivery.ID, "pending", 2, 1, 2, 1)
		result, err := continueClaimedGitHubFanout(t, New(pool), delivery.ID, "replacement-final", 1, &AuditEvent{ID: "51000000-0000-0000-0000-000000000118"}, nil)
		if err != nil || !result.Terminal || result.InsertedCount != 2 {
			t.Fatalf("replacement final batch = %+v, %v", result, err)
		}
		assertFanoutState(t, pool, delivery.ID, "completed", 2, 2, 0, 2)
	})

	t.Run("materialized candidates are immutable and execution revalidates", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		seedGitHubFanoutApps(t, pool, 249, 3000)
		delivery := snapshotGitHubFanout(t, dataStore, "50000000-0000-0000-0000-000000000003", "refs/heads/feature/immutable", strings.Repeat("0", 40), strings.Repeat("c", 40), 251, 100)
		if _, err := pool.Exec(t.Context(), `UPDATE apps SET desired_generation=desired_generation+1,github_workflow_path='.github/workflows/changed.yml' WHERE id=$1`, githubWebhookAppA); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch)
			VALUES('5fffffff-0000-0000-0000-000000000001',$1,'10000000-0000-0000-0000-000000000010','Late','late','late','ghcr.io/acme/late','.github/workflows/deploy.yml','main')`, githubWebhookOrg); err != nil {
			t.Fatal(err)
		}
		for {
			result, err := continueClaimedGitHubFanout(t, New(pool), delivery.ID, "immutable", 100, &AuditEvent{ID: "51000000-0000-0000-0000-000000000100"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Terminal {
				break
			}
		}
		var candidates, intents, late int
		if err := pool.QueryRow(t.Context(), `SELECT
			(SELECT count(*) FROM github_routing_fanout_candidates WHERE manifest_id=(SELECT id FROM github_routing_fanout_manifests WHERE delivery_id=$1)),
			(SELECT count(*) FROM github_routing_intents WHERE delivery_id=$1),
			(SELECT count(*) FROM github_routing_intents WHERE delivery_id=$1 AND app_id='5fffffff-0000-0000-0000-000000000001')`, delivery.ID).Scan(&candidates, &intents, &late); err != nil {
			t.Fatal(err)
		}
		if candidates != 0 || intents != 251 || late != 0 {
			t.Fatalf("materialized candidates/intents/late = %d/%d/%d", candidates, intents, late)
		}
		claimed := claimAllGitHubRoutingIntents(t, dataStore, "stale-executor")
		var stale GitHubRoutingIntent
		for _, intent := range claimed {
			if intent.AppID == githubWebhookAppA {
				stale = intent
			} else if err := dataStore.CompleteGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, "ignored"); err != nil {
				t.Fatal(err)
			}
		}
		if stale.ID == "" {
			t.Fatal("changed app intent was not materialized")
		}
		if result, err := dataStore.ExecuteGitHubRoutingIntent(t.Context(), stale.ID, *stale.Lease, []PreviewIdentity{{EnvironmentID: "60000000-0000-0000-0000-000000000001", Slug: "immutable", Namespace: "immutable"}}); err != nil || result != "superseded" {
			t.Fatalf("execute changed app = %q, %v", result, err)
		}
	})

	t.Run("newer push supersedes pending manifest and old intents drop", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		seedGitHubFanoutApps(t, pool, 249, 4000)
		old := snapshotGitHubFanout(t, dataStore, "50000000-0000-0000-0000-000000000004", "refs/heads/feature/supersede", strings.Repeat("0", 40), strings.Repeat("d", 40), 251, 100)
		if _, err := continueClaimedGitHubFanout(t, dataStore, old.ID, "old", 100, nil, nil); err != nil {
			t.Fatal(err)
		}
		newer := snapshotGitHubFanout(t, dataStore, "50000000-0000-0000-0000-000000000005", "refs/heads/feature/supersede", strings.Repeat("d", 40), strings.Repeat("e", 40), 251, 100)
		oldManifest := githubFanoutManifestByDelivery(t, New(pool), old.ID)
		oldResult, err := New(pool).ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: oldManifest.ID, ManifestLease: LeaseToken{Owner: "terminal-reader"}, BatchLimit: 100})
		if err != nil || !oldResult.Terminal || !oldResult.Superseded || oldResult.InsertedCount != 100 {
			t.Fatalf("old continuation = %+v, %v", oldResult, err)
		}
		for {
			result, err := continueClaimedGitHubFanout(t, New(pool), newer.ID, "newer", 100, &AuditEvent{ID: "51000000-0000-0000-0000-000000000101"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Terminal {
				break
			}
		}
		claimed := claimAllGitHubRoutingIntents(t, dataStore, "supersede-executor")
		oldDropped := 0
		for _, intent := range claimed {
			if intent.DeliveryID != old.ID {
				if err := dataStore.CompleteGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, "ignored"); err != nil {
					t.Fatal(err)
				}
				continue
			}
			result, err := dataStore.ExecuteGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, []PreviewIdentity{{EnvironmentID: fmt.Sprintf("61000000-0000-0000-0000-%012d", oldDropped+1), Slug: fmt.Sprintf("old-%d", oldDropped+1), Namespace: fmt.Sprintf("old-%d", oldDropped+1)}})
			if err != nil || result != "superseded" {
				t.Fatalf("old intent execute = %q, %v", result, err)
			}
			oldDropped++
		}
		var newCount, oldPending int
		if err := pool.QueryRow(t.Context(), `SELECT
			(SELECT count(*) FROM github_routing_intents WHERE delivery_id=$1),
			(SELECT count(*) FROM github_routing_intents WHERE delivery_id=$2 AND status IN ('pending','leased'))`, newer.ID, old.ID).Scan(&newCount, &oldPending); err != nil {
			t.Fatal(err)
		}
		if newCount != 251 || oldDropped != 100 || oldPending != 0 {
			t.Fatalf("new intents/old dropped/old pending = %d/%d/%d", newCount, oldDropped, oldPending)
		}
		assertGitHubFanoutCandidates(t, pool, old.ID, 0)
		assertGitHubFanoutCandidates(t, pool, newer.ID, 0)
	})

	t.Run("max total is exact and overflow is atomic", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		seedGitHubFanoutApps(t, pool, 249, 5000)
		exact := snapshotGitHubFanout(t, dataStore, "50000000-0000-0000-0000-000000000006", "refs/heads/feature/exact", strings.Repeat("0", 40), strings.Repeat("f", 40), 251, 50)
		assertFanoutState(t, pool, exact.ID, "pending", 251, 0, 251, 0)
		overflow := receiveDelivery(t, dataStore, webhookParams("50000000-0000-0000-0000-000000000007", strings.Repeat("7", 64), `{}`))
		overflow = claimOne(t, dataStore, "overflow", time.Minute)
		_, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: overflow.ID, DeliveryLease: *overflow.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/feature/overflow", BeforeSHA: strings.Repeat("0", 40), CommitSHA: strings.Repeat("1", 40), AuthoritativeSHA: strings.Repeat("1", 40), EventKind: "push", RefExists: true, MaxFanout: 250, FanoutBatch: 50, Audit: &AuditEvent{ID: "52000000-0000-0000-0000-000000000007"}})
		if !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("overflow error = %v", err)
		}
		var manifests, candidates, intents, observations, audits int
		if err := pool.QueryRow(t.Context(), `SELECT
			(SELECT count(*) FROM github_routing_fanout_manifests WHERE delivery_id=$1),
			(SELECT count(*) FROM github_routing_fanout_candidates WHERE manifest_id IN (SELECT id FROM github_routing_fanout_manifests WHERE delivery_id=$1)),
			(SELECT count(*) FROM github_routing_intents WHERE delivery_id=$1),
			(SELECT count(*) FROM github_ref_transition_observations WHERE delivery_id=$1),
			(SELECT count(*) FROM audit_events WHERE id='52000000-0000-0000-0000-000000000007')`, overflow.ID).Scan(&manifests, &candidates, &intents, &observations, &audits); err != nil {
			t.Fatal(err)
		}
		if manifests != 0 || candidates != 0 || intents != 0 || observations != 0 || audits != 0 {
			t.Fatalf("overflow partial state = %d/%d/%d/%d/%d", manifests, candidates, intents, observations, audits)
		}
	})
}

func TestGitHubPushDurableFanoutConfiguredLimitPostgres18(t *testing.T) {
	dataStore, pool := newGitHubFanoutFixture(t)
	seedGitHubFanoutApps(t, pool, 998, 10000)
	delivery := snapshotGitHubFanoutCount(t, dataStore, "55000000-0000-0000-0000-000000000001", "refs/heads/feature/load", strings.Repeat("0", 40), strings.Repeat("9", 40), 1000, 100, 1000)
	started := time.Now()
	for {
		result, err := continueClaimedGitHubFanout(t, New(pool), delivery.ID, "load", 100, &AuditEvent{ID: "51000000-0000-0000-0000-000000000102"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.Terminal {
			break
		}
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("1000-app durable fanout took %s, want bounded transaction loop under 30s", elapsed)
	}
	assertFanoutState(t, pool, delivery.ID, "completed", 1000, 1000, 0, 1000)
	assertPlanUsesIndex(t, pool, "pending fanout", "github_routing_fanout_manifests_pending", `SELECT id FROM github_routing_fanout_manifests WHERE status='pending' ORDER BY updated_at,id LIMIT 50`)
	assertPlanUsesIndex(t, pool, "fanout candidate cursor", "github_routing_fanout_candidates_pkey", `SELECT app_id FROM github_routing_fanout_candidates WHERE manifest_id='55000000-0000-0000-0000-000000000099' AND app_id>'00000000-0000-0000-0000-000000000000' ORDER BY app_id LIMIT 50`)
}

func TestGitHubDeleteDurableFanoutSkipsProductionWithoutStarvationPostgres18(t *testing.T) {
	dataStore, pool := newGitHubFanoutFixture(t)
	const ref = "refs/heads/deleted-many"
	if _, err := pool.Exec(t.Context(), `UPDATE apps SET production_branch='deleted-many' WHERE id=$1`, githubWebhookAppA); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE environments SET branch='deleted-many' WHERE id=$1`, githubWebhookEnv); err != nil {
		t.Fatal(err)
	}
	for i := range 150 {
		appID := fmt.Sprintf("56000000-0000-0000-0000-%012d", i+1)
		environmentID := fmt.Sprintf("57000000-0000-0000-0000-%012d", i+1)
		if _, err := pool.Exec(t.Context(), `INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch,previews_enabled)
			VALUES($1,$2,'10000000-0000-0000-0000-000000000010',$3,$4,$4,$5,'.github/workflows/deploy.yml','deleted-many',true)`, appID, githubWebhookOrg, fmt.Sprintf("Production %d", i), fmt.Sprintf("production-%d", i), fmt.Sprintf("ghcr.io/acme/production-%d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO environments(id,org_id,app_id,branch,kind,namespace,planned_hostname) VALUES($1,$2,$3,'deleted-many','production',$4,$5)`, environmentID, githubWebhookOrg, appID, fmt.Sprintf("production-%d", i), fmt.Sprintf("production-%d.example.test", i)); err != nil {
			t.Fatal(err)
		}
	}
	seedGitHubFanoutApps(t, pool, 149, 6000)
	delivery := receiveDelivery(t, dataStore, webhookParams("58000000-0000-0000-0000-000000000001", strings.Repeat("8", 64), `{}`))
	delivery = claimOne(t, dataStore, "delete-many", time.Minute)
	result, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: ref, BeforeSHA: strings.Repeat("a", 40), EventKind: "delete", RefExists: false, MaxFanout: 151, FanoutBatch: 50, Audit: &AuditEvent{ID: "58000000-0000-0000-0000-000000000002"}})
	if err != nil || result.TotalCount != 150 || result.InsertedCount != 0 {
		t.Fatalf("delete snapshot = %+v, %v", result, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE apps SET production_branch='renamed-after-snapshot' WHERE id IN ($1,'56000000-0000-0000-0000-000000000001')`, githubWebhookAppA); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE apps SET github_workflow_path='.github/workflows/renamed.yml' WHERE id='56000000-0000-0000-0000-000000000002'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE environments SET torn_down_at=clock_timestamp() WHERE id='57000000-0000-0000-0000-000000000003'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch,previews_enabled)
		VALUES('56000000-0000-0000-0000-000000000999',$1,'10000000-0000-0000-0000-000000000010','Late Production','late-production','late-production','ghcr.io/acme/late-production','.github/workflows/deploy.yml','deleted-many',true)`, githubWebhookOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO environments(id,org_id,app_id,branch,kind,namespace,planned_hostname)
		VALUES('57000000-0000-0000-0000-000000000999',$1,'56000000-0000-0000-0000-000000000999','deleted-many','production','late-production','late-production.example.test')`, githubWebhookOrg); err != nil {
		t.Fatal(err)
	}
	for !result.Terminal {
		continued, err := continueClaimedGitHubFanout(t, New(pool), delivery.ID, "delete", 50, &AuditEvent{ID: "58000000-0000-0000-0000-000000000003"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		result.TotalCount, result.InsertedCount, result.Terminal = continued.TotalCount, continued.InsertedCount, continued.Terminal
	}
	const originalProductionDeletes = 151
	var intents, productionIntents, audits, manifestProductionDeletes, auditProductionDeletes int
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM github_routing_intents WHERE delivery_id=$1),
		(SELECT count(*) FROM github_routing_intents WHERE delivery_id=$1 AND production_ref),
		(SELECT count(*) FROM audit_events WHERE id='58000000-0000-0000-0000-000000000003'),
		(SELECT production_delete_count FROM github_routing_fanout_manifests WHERE delivery_id=$1),
		(SELECT (after->>'production_branch_deleted_count')::int FROM audit_events WHERE id='58000000-0000-0000-0000-000000000003')`, delivery.ID).Scan(&intents, &productionIntents, &audits, &manifestProductionDeletes, &auditProductionDeletes); err != nil {
		t.Fatal(err)
	}
	if intents != 150 || productionIntents != 0 || audits != 1 || manifestProductionDeletes != originalProductionDeletes || auditProductionDeletes != originalProductionDeletes {
		t.Fatalf("delete intents/production/audits/manifest aggregate/audit aggregate = %d/%d/%d/%d/%d", intents, productionIntents, audits, manifestProductionDeletes, auditProductionDeletes)
	}
}

func TestGitHubCrashBoundaryMasterMatrixPostgres18(t *testing.T) {
	t.Run("B1 delivery reclaimed before API pin prevents external call", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := receiveDelivery(t, dataStore, webhookParams("5a000000-0000-0000-0000-000000000001", strings.Repeat("1", 64), `{}`))
		old := claimOne(t, dataStore, "old-before-pin", time.Minute)
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, delivery.ID); err != nil {
			t.Fatal(err)
		}
		_ = claimOne(t, dataStore, "replacement-before-pin", time.Minute)
		githubCalls := 0
		if _, err := dataStore.PinGitHubEventExternalCall(t.Context(), old.ID, *old.Lease, githubWebhookOrg, time.Minute); !errors.Is(err, ErrLeaseNotOwned) {
			t.Fatalf("old pin error = %v", err)
		} else if err == nil {
			githubCalls++
		}
		if githubCalls != 0 {
			t.Fatalf("GitHub calls = %d, want 0", githubCalls)
		}
	})

	t.Run("B2 validated old delivery becomes stale before snapshot", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		old := receiveDelivery(t, dataStore, webhookParams("5a000000-0000-0000-0000-000000000002", strings.Repeat("2", 64), `{}`))
		old = claimOne(t, dataStore, "old-validated", time.Minute)
		newer := receiveDelivery(t, dataStore, webhookParams("5a000000-0000-0000-0000-000000000003", strings.Repeat("3", 64), `{}`))
		newer = claimOne(t, dataStore, "newer-snapshot", time.Minute)
		ref, shaA, shaB := "refs/heads/crash-stale", strings.Repeat("a", 40), strings.Repeat("b", 40)
		if _, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: newer.ID, DeliveryLease: *newer.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: ref, BeforeSHA: strings.Repeat("0", 40), CommitSHA: shaB, AuthoritativeSHA: shaB, EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2}); err != nil {
			t.Fatal(err)
		}
		if _, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: old.ID, DeliveryLease: *old.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: ref, BeforeSHA: strings.Repeat("0", 40), CommitSHA: shaA, AuthoritativeSHA: shaA, EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2}); !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("old snapshot error = %v", err)
		}
		assertGitHubFanoutIntents(t, pool, old.ID, 0)
		assertGitHubFanoutCandidates(t, pool, old.ID, 0)
	})

	t.Run("B3 snapshot crash before delivery terminal reuses manifest exactly once", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := snapshotGitHubFanoutCount(t, dataStore, "5a000000-0000-0000-0000-000000000004", "refs/heads/crash-manifest", strings.Repeat("0", 40), strings.Repeat("4", 40), 2, 2, 2)
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, delivery.ID); err != nil {
			t.Fatal(err)
		}
		reclaimed := claimOne(t, dataStore, "delivery-after-crash", time.Minute)
		result, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: reclaimed.ID, DeliveryLease: *reclaimed.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/crash-manifest", BeforeSHA: strings.Repeat("0", 40), CommitSHA: strings.Repeat("4", 40), AuthoritativeSHA: strings.Repeat("4", 40), EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2})
		if err != nil || result.TotalCount != 2 || result.ManifestStatus != "pending" {
			t.Fatalf("reclaimed snapshot = %+v, %v", result, err)
		}
		completeGitHubFanout(t, dataStore, "manifest-after-crash", time.Minute, 2, &AuditEvent{ID: "5c000000-0000-0000-0000-000000000004"})
		assertGitHubFanoutIntents(t, pool, delivery.ID, 2)
	})

	t.Run("B4 committed first fanout batch resumes exact intents", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := snapshotGitHubFanoutCount(t, dataStore, "5a000000-0000-0000-0000-000000000005", "refs/heads/crash-fanout", strings.Repeat("0", 40), strings.Repeat("5", 40), 2, 1, 2)
		first := claimOneGitHubFanout(t, dataStore, "fanout-before-crash", time.Minute)
		result, err := dataStore.ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: first.ID, ManifestLease: *first.Lease, BatchLimit: 1})
		if err != nil || result.InsertedCount != 1 || result.Terminal {
			t.Fatalf("first batch = %+v, %v", result, err)
		}
		second := claimOneGitHubFanout(t, New(pool), "fanout-after-crash", time.Minute)
		result, err = New(pool).ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: second.ID, ManifestLease: *second.Lease, BatchLimit: 1, Audit: &AuditEvent{ID: "5c000000-0000-0000-0000-000000000005"}})
		if err != nil || result.InsertedCount != 2 || !result.Terminal {
			t.Fatalf("resumed batch = %+v, %v", result, err)
		}
		assertGitHubFanoutIntents(t, pool, delivery.ID, 2)
	})

	t.Run("B5 reclaimed intent fences old atomic execute and applies once", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := snapshotGitHubFanoutCount(t, dataStore, "5a000000-0000-0000-0000-000000000006", "refs/heads/main", strings.Repeat("0", 40), strings.Repeat("6", 40), 2, 2, 1)
		completeGitHubFanout(t, dataStore, "intent-fanout", time.Minute, 2, &AuditEvent{ID: "5c000000-0000-0000-0000-000000000006"})
		old := claimOneIntentForApp(t, dataStore, githubWebhookAppA, "intent-before-crash")
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_intents SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
			t.Fatal(err)
		}
		fresh := claimOneIntentForApp(t, New(pool), githubWebhookAppA, "intent-after-crash")
		var identities []PreviewIdentity
		if _, err := dataStore.ExecuteGitHubRoutingIntent(t.Context(), old.ID, *old.Lease, identities); !errors.Is(err, ErrLeaseNotOwned) {
			t.Fatalf("old execute error = %v", err)
		}
		if result, err := New(pool).ExecuteGitHubRoutingIntent(t.Context(), fresh.ID, *fresh.Lease, identities); err != nil || result != "processed" {
			t.Fatalf("replacement execute = %q, %v", result, err)
		}
		var jobs int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM orchestration_jobs WHERE environment_id=$1`, githubWebhookEnv).Scan(&jobs); err != nil || jobs != 1 {
			t.Fatalf("effective production jobs = %d, %v", jobs, err)
		}
		_ = delivery
	})

	t.Run("B6 atomic side effects survive lost result for production preview and teardown", func(t *testing.T) {
		for _, effect := range []string{"production orchestration", "preview environment", "teardown job"} {
			t.Run(effect, func(t *testing.T) {
				dataStore, pool := newGitHubFanoutFixture(t)
				key := "crash-effect-" + strings.ReplaceAll(effect, " ", "-")
				switch effect {
				case "production orchestration":
					params := EnqueueOrchestrationParams{OrgID: githubWebhookOrg, RepositoryID: fmt.Sprint(githubWebhookRepo), WorkflowID: ".github/workflows/deploy.yml", GitRef: "refs/heads/main", EnvironmentID: githubWebhookEnv, CommitSHA: strings.Repeat("8", 40), WorkflowRevision: strings.Repeat("8", 40), IdempotencyKey: key}
					if _, err := dataStore.EnqueueOrCoalesceOrchestration(t.Context(), params); err != nil {
						t.Fatal(err)
					}
					if _, err := New(pool).EnqueueOrCoalesceOrchestration(t.Context(), params); err != nil {
						t.Fatal(err)
					}
				case "preview environment":
					params := PreviewPushParams{OrgID: githubWebhookOrg, AppID: githubWebhookAppA, Branch: "crash-preview", CommitSHA: strings.Repeat("8", 40), IdempotencyKey: key, OrchestrationJobID: "5b000000-0000-0000-0000-000000000101", Identities: []PreviewIdentity{{EnvironmentID: "5b000000-0000-0000-0000-000000000101", Slug: "crash-preview", Namespace: "crash-preview"}}, Audit: &AuditEvent{ID: "5c000000-0000-0000-0000-000000000101"}}
					if _, err := dataStore.AcceptPreviewPush(t.Context(), params); err != nil {
						t.Fatal(err)
					}
					if _, err := New(pool).AcceptPreviewPush(t.Context(), params); err != nil {
						t.Fatal(err)
					}
				case "teardown job":
					if _, err := pool.Exec(t.Context(), `INSERT INTO environments(id,org_id,app_id,branch,kind,preview_slug,namespace,planned_hostname,lifecycle_status,desired_generation) VALUES('5b000000-0000-0000-0000-000000000102',$1,$2,'crash-teardown','preview','crash-teardown','crash-teardown','crash-teardown.example.test','ready',1)`, githubWebhookOrg, githubWebhookAppA); err != nil {
						t.Fatal(err)
					}
					params := PreviewTeardownParams{OrgID: githubWebhookOrg, AppID: githubWebhookAppA, Branch: "crash-teardown", IdempotencyKey: key, EventKind: "delete", Reason: "branch_deleted", Audit: &AuditEvent{ID: "5c000000-0000-0000-0000-000000000102"}}
					if _, err := dataStore.RequestPreviewTeardown(t.Context(), params); err != nil {
						t.Fatal(err)
					}
					if _, err := New(pool).RequestPreviewTeardown(t.Context(), params); err != nil {
						t.Fatal(err)
					}
				}
				assertCrashEffectCount(t, pool, effect, 1)
			})
		}
	})

	t.Run("B7 stale delivery fanout and intent terminal retry writes are fenced", func(t *testing.T) {
		dataStore, pool := newGitHubFanoutFixture(t)
		delivery := snapshotGitHubFanoutCount(t, dataStore, "5a000000-0000-0000-0000-000000000007", "refs/heads/crash-all-fences", strings.Repeat("0", 40), strings.Repeat("7", 40), 2, 1, 2)
		oldFanout := claimOneGitHubFanout(t, dataStore, "old-all-fanout", time.Minute)
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, delivery.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_fanout_manifests SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, oldFanout.ID); err != nil {
			t.Fatal(err)
		}
		freshDelivery := claimOne(t, dataStore, "fresh-all-delivery", time.Minute)
		freshFanout := claimOneGitHubFanout(t, dataStore, "fresh-all-fanout", time.Minute)
		for name, err := range map[string]error{
			"delivery retry":    dataStore.RetryGitHubWebhookDelivery(t.Context(), delivery.ID, *delivery.Lease, 0, time.Hour, "github_unavailable", "stale"),
			"delivery terminal": dataStore.CompleteGitHubWebhookDelivery(t.Context(), delivery.ID, *delivery.Lease, "processed", time.Hour),
			"fanout retry":      dataStore.RetryGitHubRoutingFanoutManifest(t.Context(), oldFanout.ID, *oldFanout.Lease, 0, "database_unavailable", "stale"),
			"fanout terminal":   dataStore.FailGitHubRoutingFanoutManifest(t.Context(), oldFanout.ID, *oldFanout.Lease, "internal", "stale"),
		} {
			if !errors.Is(err, ErrLeaseNotOwned) {
				t.Fatalf("%s error = %v", name, err)
			}
		}
		if err := dataStore.CompleteGitHubWebhookDelivery(t.Context(), freshDelivery.ID, *freshDelivery.Lease, "processed", time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, err := dataStore.ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: freshFanout.ID, ManifestLease: *freshFanout.Lease, BatchLimit: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := continueClaimedGitHubFanout(t, dataStore, delivery.ID, "fresh-all-fanout-final", 1, &AuditEvent{ID: "5c000000-0000-0000-0000-000000000007"}, nil); err != nil {
			t.Fatal(err)
		}
		intent := claimOneIntentForApp(t, dataStore, githubWebhookAppA, "old-all-intent")
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_intents SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, intent.ID); err != nil {
			t.Fatal(err)
		}
		_ = claimOneIntentForApp(t, dataStore, githubWebhookAppA, "fresh-all-intent")
		for name, err := range map[string]error{
			"intent retry":    dataStore.RetryGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, 0, "database_unavailable", "stale"),
			"intent terminal": dataStore.FailGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, "internal", "stale"),
		} {
			if !errors.Is(err, ErrLeaseNotOwned) {
				t.Fatalf("%s error = %v", name, err)
			}
		}
	})
}

func TestGitHubForcedDatabaseRetryMasterMatrixPostgres18(t *testing.T) {
	for index, code := range []string{"40001", "40P01"} {
		t.Run(code, func(t *testing.T) {
			dataStore, pool := newGitHubFanoutFixture(t)
			delivery := receiveDelivery(t, dataStore, webhookParams(fmt.Sprintf("5d000000-0000-0000-0000-%012d", index+1), fmt.Sprintf("%064x", 900+index), `{}`))
			delivery = claimOne(t, dataStore, "database-retry-worker-one", time.Minute)
			injected := New(pool)
			sha := strings.Repeat(fmt.Sprintf("%x", index+8), 40)
			params := SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: fmt.Sprintf("refs/heads/database-retry-%s", code), BeforeSHA: strings.Repeat("0", 40), CommitSHA: sha, AuthoritativeSHA: sha, EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2, Audit: &AuditEvent{ID: fmt.Sprintf("5e000000-0000-0000-0000-%012d", index+1)}}
			type mutationOutcome struct {
				result SnapshotGitHubPushResult
				err    error
			}
			var failed atomic.Bool
			params.Failpoint = func(point string) error {
				if point == "after_first_intent" && failed.CompareAndSwap(false, true) {
					return &pgconn.PgError{Code: code, Message: "forced database retry"}
				}
				return nil
			}
			outcome := mutationOutcome{}
			outcome.result, outcome.err = injected.SnapshotGitHubPush(t.Context(), params)
			if outcome.err != nil || outcome.result.TotalCount != 2 {
				t.Fatalf("retried mutation outcome = %+v, %v", outcome.result, outcome.err)
			}
			var attempts, manifests, effects int
			if err := pool.QueryRow(t.Context(), `SELECT attempts,(SELECT count(*) FROM github_routing_fanout_manifests WHERE delivery_id=$1),(SELECT count(*) FROM github_routing_intents WHERE delivery_id=$1) FROM github_webhook_deliveries WHERE id=$1`, delivery.ID).Scan(&attempts, &manifests, &effects); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 || manifests != 1 || effects != 0 {
				t.Fatalf("retry boundary attempts/manifests/effects = %d/%d/%d", attempts, manifests, effects)
			}
			completeGitHubFanout(t, New(pool), "database-retry-worker-two", time.Minute, 2, &AuditEvent{ID: fmt.Sprintf("5e000000-0000-0000-0001-%012d", index+1)})
			assertGitHubFanoutIntents(t, pool, delivery.ID, 2)
			if err := pool.QueryRow(t.Context(), `SELECT attempts FROM github_webhook_deliveries WHERE id=$1`, delivery.ID).Scan(&attempts); err != nil || attempts != 1 {
				t.Fatalf("delivery attempts after retry = %d, %v", attempts, err)
			}
		})
	}

	t.Run("real PostgreSQL serializable conflict", func(t *testing.T) {
		_, pool := newGitHubFanoutFixture(t)
		first, err := pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			t.Fatal(err)
		}
		defer first.Rollback(t.Context())
		second, err := pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			t.Fatal(err)
		}
		defer second.Rollback(t.Context())
		for _, tx := range []pgx.Tx{first, second} {
			var generation int
			if err := tx.QueryRow(t.Context(), `SELECT desired_generation FROM apps WHERE id=$1`, githubWebhookAppA).Scan(&generation); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := first.Exec(t.Context(), `UPDATE apps SET desired_generation=desired_generation+1 WHERE id=$1`, githubWebhookAppA); err != nil {
			t.Fatal(err)
		}
		if err := first.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, err = second.Exec(t.Context(), `UPDATE apps SET desired_generation=desired_generation+1 WHERE id=$1`, githubWebhookAppA)
		if !isSQLState(err, "40001") {
			t.Fatalf("real serializable conflict error = %v", err)
		}
	})
}

func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

func claimOneIntentForApp(t *testing.T, dataStore *Store, appID, owner string) GitHubRoutingIntent {
	t.Helper()
	claimed := claimAllGitHubRoutingIntents(t, dataStore, owner)
	for _, intent := range claimed {
		if intent.AppID == appID {
			for _, other := range claimed {
				if other.ID != intent.ID {
					if err := dataStore.CompleteGitHubRoutingIntent(t.Context(), other.ID, *other.Lease, "ignored"); err != nil {
						t.Fatal(err)
					}
				}
			}
			return intent
		}
	}
	t.Fatalf("intent for app %s was not claimed", appID)
	return GitHubRoutingIntent{}
}

func assertCrashEffectCount(t *testing.T, pool *pgxpool.Pool, effect string, want int) {
	t.Helper()
	var count int
	var err error
	switch effect {
	case "production orchestration":
		err = pool.QueryRow(t.Context(), `SELECT count(*) FROM orchestration_jobs WHERE environment_id=$1`, githubWebhookEnv).Scan(&count)
	case "preview environment":
		err = pool.QueryRow(t.Context(), `SELECT count(*) FROM environments WHERE app_id=$1 AND branch='crash-preview' AND torn_down_at IS NULL`, githubWebhookAppA).Scan(&count)
	case "teardown job":
		err = pool.QueryRow(t.Context(), `SELECT count(*) FROM environment_lifecycle_jobs WHERE environment_id='5b000000-0000-0000-0000-000000000102' AND kind='teardown'`).Scan(&count)
	default:
		t.Fatalf("unknown crash effect %q", effect)
	}
	if err != nil || count != want {
		t.Fatalf("%s effective count = %d, want %d, %v", effect, count, want, err)
	}
}

func newGitHubFanoutFixture(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	dataStore, pool := newGitHubWebhookIntegrationStore(t)
	seedGitHubWebhookFixture(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE apps SET deleted_at=NULL,desired_generation=1,github_workflow_path='.github/workflows/deploy.yml',production_branch='main',previews_enabled=true WHERE id IN ($1,$2)`, githubWebhookAppA, githubWebhookAppB); err != nil {
		t.Fatal(err)
	}
	return dataStore, pool
}

func seedGitHubFanoutApps(t *testing.T, pool *pgxpool.Pool, count, offset int) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch,previews_enabled)
		SELECT ('50000000-0000-0000-0000-'||lpad((n+$2)::text,12,'0'))::uuid,$1,'10000000-0000-0000-0000-000000000010','Fanout '||n,'fanout-'||(n+$2),'fanout-'||(n+$2),'ghcr.io/acme/fanout-'||(n+$2),'.github/workflows/deploy.yml','main',true
		FROM generate_series(1,$3) n ON CONFLICT(id) DO NOTHING`, githubWebhookOrg, offset, count); err != nil {
		t.Fatal(err)
	}
}

func snapshotGitHubFanout(t *testing.T, dataStore *Store, deliveryID, ref, before, after string, maxFanout, batch int) GitHubWebhookDelivery {
	t.Helper()
	return snapshotGitHubFanoutCount(t, dataStore, deliveryID, ref, before, after, maxFanout, batch, 251)
}

func snapshotGitHubFanoutCount(t *testing.T, dataStore *Store, deliveryID, ref, before, after string, maxFanout, batch, wantCount int) GitHubWebhookDelivery {
	t.Helper()
	delivery := receiveDelivery(t, dataStore, webhookParams(deliveryID, strings.Repeat(deliveryID[0:1], 64), `{}`))
	delivery = claimOne(t, dataStore, "fanout-"+deliveryID[len(deliveryID)-4:], time.Minute)
	result, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: ref, BeforeSHA: before, CommitSHA: after, AuthoritativeSHA: after, EventKind: "push", RefExists: true, MaxFanout: maxFanout, FanoutBatch: batch, Audit: &AuditEvent{ID: "53000000-0000-0000-0000-" + deliveryID[len(deliveryID)-12:]}})
	if err != nil || result.TotalCount != wantCount || result.InsertedCount != 0 || result.Terminal {
		t.Fatalf("snapshot %s = %+v, %v", ref, result, err)
	}
	return delivery
}

func assertFanoutState(t *testing.T, pool *pgxpool.Pool, deliveryID, wantStatus string, total, inserted, candidates, intents int) {
	t.Helper()
	var status string
	var gotTotal, gotInserted, candidateCount, intentCount int
	var cursor *string
	if err := pool.QueryRow(t.Context(), `SELECT manifest.status,manifest.total_count,manifest.inserted_count,manifest.cursor_app_id::text,
		(SELECT count(*) FROM github_routing_fanout_candidates WHERE manifest_id=manifest.id),
		(SELECT count(*) FROM github_routing_intents WHERE delivery_id=manifest.delivery_id)
		FROM github_routing_fanout_manifests manifest WHERE delivery_id=$1`, deliveryID).Scan(&status, &gotTotal, &gotInserted, &cursor, &candidateCount, &intentCount); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || gotTotal != total || gotInserted != inserted || candidateCount != candidates || intentCount != intents || (inserted == 0) != (cursor == nil) {
		t.Fatalf("fanout state = %s total=%d inserted=%d cursor=%v candidates=%d intents=%d", status, gotTotal, gotInserted, cursor, candidateCount, intentCount)
	}
}

func assertGitHubFanoutCandidates(t *testing.T, pool *pgxpool.Pool, deliveryID string, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_routing_fanout_candidates candidate JOIN github_routing_fanout_manifests manifest ON manifest.id=candidate.manifest_id WHERE manifest.delivery_id=$1`, deliveryID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("candidate rows for %s = %d, want %d", deliveryID, count, want)
	}
}

func assertGitHubFanoutIntents(t *testing.T, pool *pgxpool.Pool, deliveryID string, want int) {
	t.Helper()
	var total, unique int
	if err := pool.QueryRow(t.Context(), `SELECT count(*),count(DISTINCT app_id) FROM github_routing_intents WHERE delivery_id=$1`, deliveryID).Scan(&total, &unique); err != nil {
		t.Fatal(err)
	}
	if total != want || unique != want {
		t.Fatalf("intent rows for %s = %d/%d, want %d/%d", deliveryID, total, unique, want, want)
	}
}

func claimAllGitHubRoutingIntents(t *testing.T, dataStore *Store, owner string) []GitHubRoutingIntent {
	t.Helper()
	var all []GitHubRoutingIntent
	for {
		claimed, err := dataStore.ClaimGitHubRoutingIntents(t.Context(), owner, time.Minute, 100)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, claimed...)
		if len(claimed) < 100 {
			return all
		}
	}
}

func claimOneGitHubFanout(t *testing.T, dataStore *Store, owner string, duration time.Duration) GitHubRoutingFanoutManifest {
	t.Helper()
	claimed, err := dataStore.ClaimGitHubRoutingFanoutManifests(t.Context(), owner, duration, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim GitHub fanout = %+v, %v", claimed, err)
	}
	return claimed[0]
}

func continueClaimedGitHubFanout(t *testing.T, dataStore *Store, deliveryID, owner string, limit int, audit *AuditEvent, failpoint func(string) error) (ContinueGitHubPushFanoutResult, error) {
	t.Helper()
	manifest := claimOneGitHubFanout(t, dataStore, owner, time.Minute)
	if manifest.DeliveryID != deliveryID {
		t.Fatalf("claimed delivery = %q, want %q", manifest.DeliveryID, deliveryID)
	}
	return dataStore.ContinueGitHubPushFanout(t.Context(), ContinueGitHubPushFanoutParams{ManifestID: manifest.ID, ManifestLease: *manifest.Lease, BatchLimit: limit, Audit: audit, Failpoint: failpoint})
}

func githubFanoutManifestByDelivery(t *testing.T, dataStore *Store, deliveryID string) GitHubRoutingFanoutManifest {
	t.Helper()
	var manifest GitHubRoutingFanoutManifest
	if err := scanGitHubRoutingFanoutManifest(dataStore.db.QueryRow(t.Context(), `SELECT id::text,delivery_id::text,org_id::text,repository_id,ref,event_kind,routing_generation,total_count,inserted_count,attempts,lease_owner,lease_epoch,lease_expires_at FROM github_routing_fanout_manifests WHERE delivery_id=$1`, deliveryID), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}
