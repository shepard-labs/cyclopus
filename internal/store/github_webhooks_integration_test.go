//go:build integration

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	githubWebhookOrg  = "10000000-0000-0000-0000-000000000001"
	githubWebhookAppA = "10000000-0000-0000-0000-000000000002"
	githubWebhookAppB = "10000000-0000-0000-0000-000000000003"
	githubWebhookEnv  = "10000000-0000-0000-0000-000000000004"
	githubWebhookRepo = int64(91001)
)

func TestGitHubWebhookPersistencePostgres18(t *testing.T) {
	t.Run("migration up down re-up and guarded rollback", testGitHubWebhookMigrationLifecycle)

	store, pool := newGitHubWebhookIntegrationStore(t)
	seedGitHubWebhookFixture(t, pool)

	t.Run("concurrent receipt replay and conflict preserve original", func(t *testing.T) {
		p := webhookParams("20000000-0000-0000-0000-000000000001", strings.Repeat("a", 64), `{"ref":"main"}`)
		var wg sync.WaitGroup
		dispositions := make(chan GitHubWebhookReceiveDisposition, 8)
		errs := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				disposition, _, err := store.ReceiveGitHubWebhook(t.Context(), p)
				dispositions <- disposition
				errs <- err
			}()
		}
		wg.Wait()
		close(dispositions)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		accepted, replayed := 0, 0
		for disposition := range dispositions {
			switch disposition {
			case GitHubWebhookAccepted:
				accepted++
			case GitHubWebhookReplayed:
				replayed++
			default:
				t.Fatalf("concurrent disposition = %q", disposition)
			}
		}
		if accepted != 1 || replayed != 7 {
			t.Fatalf("accepted/replayed = %d/%d, want 1/7", accepted, replayed)
		}
		rotated := p
		rotated.SignatureKeyVersion = "hmac-v2"
		disposition, replay, err := store.ReceiveGitHubWebhook(t.Context(), rotated)
		if err != nil || disposition != GitHubWebhookReplayed || replay.SignatureKeyVersion != p.SignatureKeyVersion {
			t.Fatalf("rotated signature replay = %q %+v, %v", disposition, replay, err)
		}
		var deliveryCount int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_webhook_deliveries WHERE delivery_id=$1`, p.DeliveryID).Scan(&deliveryCount); err != nil || deliveryCount != 1 {
			t.Fatalf("replayed delivery rows = %d, %v", deliveryCount, err)
		}
		conflict := p
		conflict.PayloadDigest = strings.Repeat("b", 64)
		conflict.Payload = []byte(`{"ref":"evil"}`)
		disposition, got, err := store.ReceiveGitHubWebhook(t.Context(), conflict)
		if err != nil || disposition != GitHubWebhookConflict {
			t.Fatalf("conflict receive = %q, %v", disposition, err)
		}
		if got.PayloadDigest != p.PayloadDigest || got.Payload != nil {
			t.Fatalf("conflict replaced original: digest=%q payload=%q", got.PayloadDigest, got.Payload)
		}
	})

	t.Run("replay and conflict do not encrypt or decrypt", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		p := webhookParams("20000000-0000-0000-0000-000000000089", strings.Repeat("0", 64), `{"value":1}`)
		receiveDelivery(t, store, p)
		codec := &countingFailingCodec{}
		store.WithGitHubWebhookPayloadCodec(codec)
		disposition, replay, err := store.ReceiveGitHubWebhook(t.Context(), p)
		if err != nil || disposition != GitHubWebhookReplayed || replay.Payload != nil || codec.encrypts != 0 || codec.decrypts != 0 {
			t.Fatalf("replay = %q %+v, %v; codec=%+v", disposition, replay, err, codec)
		}
		conflict := p
		conflict.PayloadDigest = strings.Repeat("1", 64)
		disposition, _, err = store.ReceiveGitHubWebhook(t.Context(), conflict)
		if err != nil || disposition != GitHubWebhookConflict || codec.encrypts != 0 || codec.decrypts != 0 {
			t.Fatalf("conflict = %q, %v; codec=%+v", disposition, err, codec)
		}
		fresh := webhookParams("20000000-0000-0000-0000-000000000088", strings.Repeat("2", 64), `{}`)
		if _, _, err := store.ReceiveGitHubWebhook(t.Context(), fresh); !errors.Is(err, ErrFailedPrecondition) || codec.encrypts != 1 {
			t.Fatalf("fresh receive error = %v; codec=%+v", err, codec)
		}
		workingCodec, err := NewAESGCMGitHubWebhookPayloadCodec("k1", []byte("0123456789abcdef0123456789abcdef"))
		if err != nil {
			t.Fatal(err)
		}
		store.WithGitHubWebhookPayloadCodec(workingCodec)
	})

	t.Run("pre identity lookup replay conflict and unknown", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		p := webhookParams("20000000-0000-0000-0000-000000000087", strings.Repeat("3", 64), `{}`)
		receiveDelivery(t, store, p)
		lookup := LookupGitHubWebhookParams{DeliveryID: p.DeliveryID, Event: p.Event, Action: p.Action, PayloadDigest: p.PayloadDigest, InstallationID: p.InstallationID, RepositoryID: p.RepositoryID, RepositoryNodeID: p.RepositoryNodeID}
		disposition, identity, err := store.LookupGitHubWebhook(t.Context(), lookup)
		if err != nil || disposition != GitHubWebhookReplay || identity.OrgID != p.OrgID {
			t.Fatalf("lookup replay = %q %+v, %v", disposition, identity, err)
		}
		for _, mutate := range []func(*LookupGitHubWebhookParams){
			func(v *LookupGitHubWebhookParams) { v.PayloadDigest = strings.Repeat("4", 64) },
			func(v *LookupGitHubWebhookParams) { v.Event = "issues" },
			func(v *LookupGitHubWebhookParams) { id := int64(99999); v.InstallationID = &id },
			func(v *LookupGitHubWebhookParams) { id := int64(99998); v.RepositoryID = &id },
			func(v *LookupGitHubWebhookParams) { node := "other-tenant-node"; v.RepositoryNodeID = &node },
		} {
			mismatch := lookup
			mutate(&mismatch)
			if disposition, _, err := store.LookupGitHubWebhook(t.Context(), mismatch); err != nil || disposition != GitHubWebhookMismatch {
				t.Fatalf("lookup mismatch = %q, %v; params=%+v", disposition, err, mismatch)
			}
		}
		missing := lookup
		missing.DeliveryID = "20000000-0000-0000-0000-000000000086"
		if disposition, identity, err := store.LookupGitHubWebhook(t.Context(), missing); err != nil || disposition != GitHubWebhookNotFound || identity != (GitHubWebhookIdentity{}) {
			t.Fatalf("lookup missing = %q %+v, %v", disposition, identity, err)
		}
	})

	t.Run("payload is encrypted and claimed body is exact", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		body := []byte(`{"ref":"refs/heads/main","secret":"not plaintext"}`)
		p := webhookParams("20000000-0000-0000-0000-000000000090", strings.Repeat("9", 64), string(body))
		d := receiveDelivery(t, store, p)
		var ciphertext, nonce []byte
		if err := pool.QueryRow(t.Context(), `SELECT verified_payload,payload_nonce FROM github_webhook_deliveries WHERE id=$1`, d.ID).Scan(&ciphertext, &nonce); err != nil {
			t.Fatal(err)
		}
		if string(ciphertext) == string(body) || strings.Contains(string(ciphertext), "not plaintext") || len(nonce) != 12 {
			t.Fatalf("payload not safely encrypted: ciphertext=%q nonce=%d", ciphertext, len(nonce))
		}
		claimed := claimOne(t, store, "decrypt-worker", time.Minute)
		if string(claimed.Payload) != string(body) {
			t.Fatalf("claimed payload=%q want=%q", claimed.Payload, body)
		}
		wrong, err := NewAESGCMGitHubWebhookPayloadCodec("k1", []byte("abcdef0123456789abcdef0123456789"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wrong.Decrypt(ciphertext, nonce, "k1", p.payloadContext()); err == nil {
			t.Fatal("wrong key decrypted payload")
		}
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, d.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.WithGitHubWebhookPayloadCodec(wrong).ClaimGitHubWebhookDeliveries(t.Context(), "wrong-key-worker", time.Minute, 1); !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("claim with wrong key error = %v, want ErrFailedPrecondition", err)
		}
		wrongVersion, err := NewAESGCMGitHubWebhookPayloadCodec("k2", []byte("0123456789abcdef0123456789abcdef"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.WithGitHubWebhookPayloadCodec(wrongVersion).ClaimGitHubWebhookDeliveries(t.Context(), "wrong-codec-worker", time.Minute, 1); !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("claim with wrong codec version error = %v, want ErrFailedPrecondition", err)
		}
		codec, err := NewAESGCMGitHubWebhookPayloadCodec("k1", []byte("0123456789abcdef0123456789abcdef"))
		if err != nil {
			t.Fatal(err)
		}
		store.WithGitHubWebhookPayloadCodec(codec)
	})

	t.Run("ciphertext tuple cannot be substituted between deliveries", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		first := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000095", strings.Repeat("3", 64), `{"row":1}`))
		second := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000096", strings.Repeat("4", 64), `{"row":2}`))
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries a SET verified_payload=b.verified_payload,payload_nonce=b.payload_nonce,key_version=b.key_version FROM github_webhook_deliveries b WHERE a.id=$1 AND b.id=$2`, first.ID, second.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ClaimGitHubWebhookDeliveries(t.Context(), "tuple-swap", time.Minute, 1); !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("tuple substitution claim error = %v, want ErrFailedPrecondition", err)
		}
	})

	t.Run("stored immutable metadata authenticates payload", func(t *testing.T) {
		mutations := []struct {
			name, update string
			args         []any
		}{
			{name: "delivery UUID", update: `delivery_id=$2`, args: []any{"20000000-0000-0000-0000-000000000197"}},
			{name: "event", update: `event=$2`, args: []any{"issues"}},
			{name: "action", update: `action=$2`, args: []any{"opened"}},
			{name: "payload digest", update: `payload_digest=$2`, args: []any{strings.Repeat("e", 64)}},
			{name: "nonce", update: `payload_nonce=set_byte(payload_nonce,0,get_byte(payload_nonce,0)#1)`},
			{name: "key version", update: `key_version=$2`, args: []any{"substituted"}},
		}
		for i, mutation := range mutations {
			t.Run(mutation.name, func(t *testing.T) {
				clearGitHubWebhookWork(t, pool)
				p := webhookParams(fmt.Sprintf("20000000-0000-0000-0001-%012d", i), fmt.Sprintf("%064x", i+1), `{}`)
				d := receiveDelivery(t, store, p)
				args := append([]any{d.ID}, mutation.args...)
				if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET `+mutation.update+` WHERE id=$1`, args...); err != nil {
					t.Fatal(err)
				}
				if _, err := store.ClaimGitHubWebhookDeliveries(t.Context(), "metadata-mutation", time.Minute, 1); !errors.Is(err, ErrFailedPrecondition) {
					t.Fatalf("claim error = %v, want ErrFailedPrecondition", err)
				}
			})
		}

		t.Run("installation and nullable repository context", func(t *testing.T) {
			clearGitHubWebhookWork(t, pool)
			p := webhookParams("20000000-0000-0000-0000-000000000198", strings.Repeat("d", 64), `{}`)
			p.Event, p.RepositoryID, p.RepositoryNodeID = "ping", nil, nil
			d := receiveDelivery(t, store, p)
			if _, err := pool.Exec(t.Context(), `INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(90003,$1,90004,'acme-2','Organization','selected','active',1,1)`, githubWebhookOrg); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET installation_id=90003 WHERE id=$1`, d.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ClaimGitHubWebhookDeliveries(t.Context(), "installation-mutation", time.Minute, 1); !errors.Is(err, ErrFailedPrecondition) {
				t.Fatalf("installation mutation claim error = %v", err)
			}

			clearGitHubWebhookWork(t, pool)
			d = receiveDelivery(t, store, p)
			if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET repository_id=$2,repository_node_id='R_node' WHERE id=$1`, d.ID, githubWebhookRepo); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ClaimGitHubWebhookDeliveries(t.Context(), "repository-mutation", time.Minute, 1); !errors.Is(err, ErrFailedPrecondition) {
				t.Fatalf("repository mutation claim error = %v", err)
			}
		})
	})

	t.Run("replay survives active key rotation and retained keys decrypt both versions", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		old := webhookParams("20000000-0000-0000-0000-000000000091", strings.Repeat("8", 64), `{"key":"old"}`)
		oldDelivery := receiveDelivery(t, store, old)
		rotated, err := NewAESGCMGitHubWebhookPayloadKeyring("k2", map[string][]byte{
			"k1": []byte("0123456789abcdef0123456789abcdef"),
			"k2": []byte("abcdef0123456789abcdef0123456789"),
		})
		if err != nil {
			t.Fatal(err)
		}
		store.WithGitHubWebhookPayloadCodec(rotated)
		disposition, replay, err := store.ReceiveGitHubWebhook(t.Context(), old)
		if err != nil || disposition != GitHubWebhookReplayed || replay.ID != oldDelivery.ID || replay.Payload != nil || replay.PayloadKeyVersion != "k1" {
			t.Fatalf("rotated replay = %q %+v, %v", disposition, replay, err)
		}
		newDelivery := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000092", strings.Repeat("7", 64), `{"key":"new"}`))
		if newDelivery.PayloadKeyVersion != "k2" {
			t.Fatalf("new delivery key version = %q", newDelivery.PayloadKeyVersion)
		}
		withoutOld, err := NewAESGCMGitHubWebhookPayloadKeyring("k2", map[string][]byte{"k2": []byte("abcdef0123456789abcdef0123456789")})
		if err != nil {
			t.Fatal(err)
		}
		store.WithGitHubWebhookPayloadCodec(withoutOld)
		if disposition, replay, err := store.ReceiveGitHubWebhook(t.Context(), old); err != nil || disposition != GitHubWebhookReplayed || replay.Payload != nil {
			t.Fatalf("replay without old key = %q %+v, %v", disposition, replay, err)
		}
		store.WithGitHubWebhookPayloadCodec(rotated)
	})

	t.Run("tenant scoped repository receipt integrity", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		repositoryless := webhookParams("20000000-0000-0000-0000-000000000093", strings.Repeat("6", 64), `{}`)
		repositoryless.Event = "ping"
		repositoryless.InstallationID, repositoryless.RepositoryID, repositoryless.RepositoryNodeID = nil, nil, nil
		if delivery := receiveDelivery(t, store, repositoryless); delivery.InstallationID != nil || delivery.RepositoryID != nil || delivery.RepositoryNodeID != nil {
			t.Fatalf("repository-less delivery = %+v", delivery)
		}
		if claimed := claimOne(t, store, "ping-worker", time.Minute); claimed.InstallationID != nil || string(claimed.Payload) != `{}` {
			t.Fatalf("claimed ping delivery = %+v", claimed)
		}
		clearGitHubWebhookWork(t, pool)
		installationID := int64(90001)
		lifecycle := webhookParams("20000000-0000-0000-0000-000000000097", strings.Repeat("7", 64), `{}`)
		lifecycle.Event, lifecycle.InstallationID, lifecycle.RepositoryID, lifecycle.RepositoryNodeID = "installation", &installationID, nil, nil
		receiveDelivery(t, store, lifecycle)
		nonexistent := lifecycle
		nonexistent.DeliveryID, nonexistent.PayloadDigest = "20000000-0000-0000-0000-000000000098", strings.Repeat("8", 64)
		missingInstallationID := int64(99999)
		nonexistent.InstallationID = &missingInstallationID
		if _, _, err := store.ReceiveGitHubWebhook(t.Context(), nonexistent); err == nil {
			t.Fatal("nonexistent installation receipt was accepted")
		}
		otherOrg := "10000000-0000-0000-0000-000000000099"
		if _, err := pool.Exec(t.Context(), `INSERT INTO orgs(id,name,slug) VALUES($1,'Other','other')`, otherOrg); err != nil {
			t.Fatal(err)
		}
		mismatch := webhookParams("20000000-0000-0000-0000-000000000094", strings.Repeat("5", 64), `{}`)
		mismatch.OrgID = otherOrg
		if _, _, err := store.ReceiveGitHubWebhook(t.Context(), mismatch); err == nil {
			t.Fatal("cross-org repository receipt was accepted")
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(90005,$1,90006,'other','Organization','selected','active',1,1)`, otherOrg); err != nil {
			t.Fatal(err)
		}
		crossOrgInstallation := lifecycle
		crossOrgInstallation.DeliveryID, crossOrgInstallation.PayloadDigest = "20000000-0000-0000-0000-000000000099", strings.Repeat("9", 64)
		crossOrgInstallationID := int64(90005)
		crossOrgInstallation.InstallationID = &crossOrgInstallationID
		if _, _, err := store.ReceiveGitHubWebhook(t.Context(), crossOrgInstallation); err == nil {
			t.Fatal("cross-org installation receipt was accepted")
		}
		mismatchedInstallation := webhookParams("20000000-0000-0000-0000-000000000100", strings.Repeat("a", 64), `{}`)
		mismatchedInstallation.InstallationID = &crossOrgInstallationID
		if _, _, err := store.ReceiveGitHubWebhook(t.Context(), mismatchedInstallation); err == nil {
			t.Fatal("repository receipt with mismatched installation was accepted")
		}
	})

	t.Run("bounded skip locked claim exclusivity", func(t *testing.T) {
		for i := range 6 {
			receiveDelivery(t, store, webhookParams(fmt.Sprintf("20000000-0000-0000-0000-%012d", i+100), fmt.Sprintf("%064x", i+100), `{}`))
		}
		start := make(chan struct{})
		claims := make(chan []GitHubWebhookDelivery, 2)
		for _, owner := range []string{"worker-a", "worker-b"} {
			go func(owner string) {
				<-start
				got, err := store.ClaimGitHubWebhookDeliveries(t.Context(), owner, time.Minute, 3)
				if err != nil {
					t.Errorf("claim %s: %v", owner, err)
				}
				claims <- got
			}(owner)
		}
		close(start)
		seen := map[string]string{}
		for range 2 {
			for _, delivery := range <-claims {
				if previous := seen[delivery.ID]; previous != "" {
					t.Fatalf("delivery %s claimed by %s and %s", delivery.ID, previous, delivery.Lease.Owner)
				}
				seen[delivery.ID] = delivery.Lease.Owner
			}
		}
		if len(seen) != 6 {
			t.Fatalf("unique claims = %d, want 6", len(seen))
		}
	})

	t.Run("routing intent lease matrix", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		sha := strings.Repeat("a", 40)
		generation, err := store.AdvanceGitHubRoutingGeneration(t.Context(), githubWebhookOrg, githubWebhookRepo, "R_node", "refs/heads/main", 3, 4, &sha, true)
		if err != nil {
			t.Fatal(err)
		}
		newIntent := func(deliverySuffix int, appID, environmentID string) (string, GitHubRoutingIntentParams) {
			delivery := receiveDelivery(t, store, webhookParams(fmt.Sprintf("21000000-0000-0000-0000-%012d", deliverySuffix), fmt.Sprintf("%064x", deliverySuffix), `{}`))
			params := GitHubRoutingIntentParams{DeliveryID: delivery.ID, OrgID: githubWebhookOrg, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", AppID: appID, EnvironmentID: environmentID, Ref: "refs/heads/main", CommitSHA: sha, EventKind: "push", RoutingGeneration: generation + int64(deliverySuffix), RepositoryGeneration: 3, RefLifecycleGeneration: 4, ConfigGeneration: 7, WorkflowPath: ".github/workflows/deploy.yml", ProductionRef: true}
			id, created, err := store.PersistGitHubRoutingIntent(t.Context(), params)
			if err != nil || !created {
				t.Fatalf("persist intent = %s/%t, %v", id, created, err)
			}
			return id, params
		}
		claim := func(owner string, limit int) []GitHubRoutingIntent {
			claimed, err := store.ClaimGitHubRoutingIntents(t.Context(), owner, time.Minute, limit)
			if err != nil {
				t.Fatalf("claim intents: %v", err)
			}
			return claimed
		}

		id, snapshot := newIntent(1, githubWebhookAppA, githubWebhookEnv)
		claimed := claim("snapshot-owner", 1)
		if len(claimed) != 1 {
			t.Fatalf("snapshot claims = %d", len(claimed))
		}
		got := claimed[0]
		if got.ID != id || got.DeliveryID != snapshot.DeliveryID || got.OrgID != snapshot.OrgID || got.RepositoryID != snapshot.RepositoryID || got.RepositoryNodeID != snapshot.RepositoryNodeID || got.AppID != snapshot.AppID || got.EnvironmentID != snapshot.EnvironmentID || got.Ref != snapshot.Ref || got.CommitSHA != snapshot.CommitSHA || got.EventKind != snapshot.EventKind || got.RoutingGeneration != snapshot.RoutingGeneration || got.RepositoryGeneration != snapshot.RepositoryGeneration || got.RefLifecycleGeneration != snapshot.RefLifecycleGeneration || got.ConfigGeneration != snapshot.ConfigGeneration || got.WorkflowPath != snapshot.WorkflowPath || got.ProductionRef != snapshot.ProductionRef || got.Attempts != 1 || got.Status != "leased" || got.Lease == nil || got.Lease.Owner != "snapshot-owner" {
			t.Fatalf("claimed immutable snapshot = %+v, want %+v", got, snapshot)
		}
		renewed, err := store.RenewGitHubRoutingIntent(t.Context(), id, *got.Lease, 2*time.Minute)
		if err != nil || renewed.Epoch != got.Lease.Epoch || !renewed.ExpiresAt.After(got.Lease.ExpiresAt) {
			t.Fatalf("renew intent = %+v, %v", renewed, err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_intents SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		reclaimed := claim("replacement-owner", 1)[0]
		if reclaimed.ID != id || reclaimed.Attempts != 2 || reclaimed.Lease.Epoch != got.Lease.Epoch+1 {
			t.Fatalf("reclaimed intent = %+v, old = %+v", reclaimed, got)
		}
		stale := *got.Lease
		staleErrors := []error{
			func() error {
				_, err := store.RenewGitHubRoutingIntent(t.Context(), id, stale, time.Minute)
				return err
			}(),
			store.RetryGitHubRoutingIntent(t.Context(), id, stale, 0, "internal", "stale"),
			store.CompleteGitHubRoutingIntent(t.Context(), id, stale, "processed"),
			store.FailGitHubRoutingIntent(t.Context(), id, stale, "internal", "stale"),
		}
		for operation, err := range staleErrors {
			if !errors.Is(err, ErrLeaseNotOwned) {
				t.Fatalf("stale operation %d error = %v", operation, err)
			}
		}
		if err := store.CompleteGitHubRoutingIntent(t.Context(), id, *reclaimed.Lease, "processed"); err != nil {
			t.Fatal(err)
		}
		var status, result string
		var terminal time.Time
		if err := pool.QueryRow(t.Context(), `SELECT status,result_code,terminal_at FROM github_routing_intents WHERE id=$1`, id).Scan(&status, &result, &terminal); err != nil || status != "completed" || result != "processed" || terminal.IsZero() {
			t.Fatalf("completed intent = %s/%s/%s, %v", status, result, terminal, err)
		}

		for i := 10; i < 16; i++ {
			newIntent(i, githubWebhookAppA, githubWebhookEnv)
		}
		start := make(chan struct{})
		claims := make(chan []GitHubRoutingIntent, 2)
		for _, owner := range []string{"intent-a", "intent-b"} {
			go func(owner string) { <-start; claims <- claim(owner, 3) }(owner)
		}
		close(start)
		seen := map[string]string{}
		for range 2 {
			for _, intent := range <-claims {
				if previous := seen[intent.ID]; previous != "" {
					t.Fatalf("intent %s claimed by %s and %s", intent.ID, previous, intent.Lease.Owner)
				}
				seen[intent.ID] = intent.Lease.Owner
			}
		}
		if len(seen) != 6 {
			t.Fatalf("exclusive intent claims = %d, want 6", len(seen))
		}
		if _, err := pool.Exec(t.Context(), `DELETE FROM github_routing_intents WHERE status='leased'`); err != nil {
			t.Fatal(err)
		}

		capID, _ := newIntent(20, githubWebhookAppA, githubWebhookEnv)
		for attempt := 1; attempt <= 12; attempt++ {
			one := claim("attempt-owner", 1)
			if len(one) != 1 || one[0].ID != capID || one[0].Attempts != attempt {
				t.Fatalf("intent attempt %d = %+v", attempt, one)
			}
			if err := store.RetryGitHubRoutingIntent(t.Context(), capID, *one[0].Lease, 0, "github_unavailable", "retry"); err != nil {
				t.Fatal(err)
			}
		}
		if extra := claim("attempt-13", 1); len(extra) != 0 {
			t.Fatalf("13th intent claim = %+v", extra)
		}
		var attempts int
		var errorCode string
		if err := pool.QueryRow(t.Context(), `SELECT status,attempts,error_code FROM github_routing_intents WHERE id=$1`, capID).Scan(&status, &attempts, &errorCode); err != nil || status != "failed" || attempts != 12 || errorCode != "attempts_exhausted" {
			t.Fatalf("exhausted intent = %s/%d/%s, %v", status, attempts, errorCode, err)
		}

		failedID, _ := newIntent(21, githubWebhookAppA, githubWebhookEnv)
		failure := claim("failure-owner", 1)[0]
		if err := store.FailGitHubRoutingIntent(t.Context(), failedID, *failure.Lease, "invalid_snapshot", "permanent"); err != nil {
			t.Fatal(err)
		}
		var detail string
		if err := pool.QueryRow(t.Context(), `SELECT status,error_code,error_detail,terminal_at FROM github_routing_intents WHERE id=$1`, failedID).Scan(&status, &errorCode, &detail, &terminal); err != nil || status != "failed" || errorCode != "invalid_snapshot" || detail != "permanent" || terminal.IsZero() {
			t.Fatalf("failed intent = %s/%s/%s/%s, %v", status, errorCode, detail, terminal, err)
		}

		siblingA, _ := newIntent(30, githubWebhookAppA, githubWebhookEnv)
		siblingB, _ := newIntent(31, githubWebhookAppB, "")
		first := claim("sibling-owner", 1)[0]
		if first.ID != siblingA || store.CompleteGitHubRoutingIntent(t.Context(), siblingA, *first.Lease, "ignored") != nil {
			t.Fatalf("complete first sibling = %+v", first)
		}
		if err := pool.QueryRow(t.Context(), `SELECT status FROM github_routing_intents WHERE id=$1`, siblingB).Scan(&status); err != nil || status != "pending" {
			t.Fatalf("second sibling status = %s, %v", status, err)
		}
		bad := snapshot
		bad.DeliveryID = receiveDelivery(t, store, webhookParams("21000000-0000-0000-0000-000000000099", strings.Repeat("f", 64), `{}`)).ID
		bad.RoutingGeneration += 100
		bad.RepositoryNodeID = "wrong-node"
		if _, _, err := store.PersistGitHubRoutingIntent(t.Context(), bad); err == nil {
			t.Fatal("mismatched authoritative repository node was accepted")
		}
	})

	t.Run("unverified revocation blocks routing and authoritative reconciliation recovers", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		installationID := int64(90001)
		lifecycleParams := webhookParams("20000000-0000-0000-0000-000000000590", strings.Repeat("9", 64), `{}`)
		lifecycleParams.Event, lifecycleParams.InstallationID, lifecycleParams.RepositoryID, lifecycleParams.RepositoryNodeID = "installation", &installationID, nil, nil
		receiveDelivery(t, store, lifecycleParams)
		lifecycle := claimOne(t, store, "lifecycle-revoke", time.Minute)
		if err := store.RevokeGitHubEvent(t.Context(), RevokeGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: lifecycle.ID, DeliveryLease: *lifecycle.Lease, InstallationID: 90001, Kind: "unverified", Audit: &AuditEvent{ID: "10000000-0000-0000-0000-000000000091"}}); err != nil {
			t.Fatal(err)
		}
		var installationState, connectionState string
		if err := pool.QueryRow(t.Context(), `SELECT installation.access_state,connection.access_state FROM github_installations installation
			JOIN github_repositories repository ON repository.org_id=installation.org_id AND repository.github_installation_id=installation.github_installation_id
			JOIN git_connections connection ON connection.org_id=repository.org_id AND connection.github_repository_id=repository.github_repository_id
			WHERE installation.org_id=$1 AND installation.github_installation_id=90001`, githubWebhookOrg).Scan(&installationState, &connectionState); err != nil {
			t.Fatal(err)
		}
		if installationState != "unverified_revocation" || connectionState != "unverified_revocation" {
			t.Fatalf("revocation states = %q/%q", installationState, connectionState)
		}
		push := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000591", strings.Repeat("8", 64), `{}`))
		push = claimOne(t, store, "blocked-push", time.Minute)
		if _, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: push.ID, DeliveryLease: *push.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/main", CommitSHA: strings.Repeat("a", 40), EventKind: "push", RefExists: true, MaxIntents: 10}); !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("snapshot while unverified error = %v", err)
		}
		_, err := store.ReconcileGitHubEvent(t.Context(), ReconcileGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: lifecycle.ID, DeliveryLease: *lifecycle.Lease,
			Installation: GitHubAuthoritativeInstallation{InstallationID: 90001, AccountID: 90002, AccountLogin: "acme", AccountType: "Organization", Selection: "selected"},
			Repositories: []GitHubRepository{{GitHubRepositoryID: githubWebhookRepo, GitHubInstallationID: 90001, NodeID: "R_node", OwnerLogin: "acme", Name: "hooks", FullName: "acme/hooks", Visibility: "private", DefaultBranch: "main"}},
			Audit:        &AuditEvent{ID: "10000000-0000-0000-0000-000000000092"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT installation.access_state,connection.access_state FROM github_installations installation
			JOIN github_repositories repository ON repository.org_id=installation.org_id AND repository.github_installation_id=installation.github_installation_id
			JOIN git_connections connection ON connection.org_id=repository.org_id AND connection.github_repository_id=repository.github_repository_id
			WHERE installation.org_id=$1 AND installation.github_installation_id=90001`, githubWebhookOrg).Scan(&installationState, &connectionState); err != nil || installationState != "active" || connectionState != "active" {
			t.Fatalf("recovered states = %q/%q, %v", installationState, connectionState, err)
		}
	})

	t.Run("unverified repository removal preserves metadata and recovers", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		delivery := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000592", strings.Repeat("7", 64), `{}`))
		delivery = claimOne(t, store, "repository-revoke", time.Minute)
		if err := store.RevokeGitHubEvent(t.Context(), RevokeGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Kind: "unverified_remove", Audit: &AuditEvent{ID: "10000000-0000-0000-0000-000000000093"}}); err != nil {
			t.Fatal(err)
		}
		var state, connectionState, fullName string
		if err := pool.QueryRow(t.Context(), `SELECT repository.access_state,connection.access_state,repository.full_name FROM github_repositories repository JOIN git_connections connection ON connection.org_id=repository.org_id AND connection.github_repository_id=repository.github_repository_id WHERE repository.org_id=$1 AND repository.github_repository_id=$2`, githubWebhookOrg, githubWebhookRepo).Scan(&state, &connectionState, &fullName); err != nil || state != "unverified_revocation" || connectionState != "unverified_revocation" || fullName != "acme/hooks" {
			t.Fatalf("repository/connection state and metadata = %q/%q/%q, %v", state, connectionState, fullName, err)
		}
		blocked := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000593", strings.Repeat("6", 64), `{}`))
		blocked = claimOne(t, store, "repository-blocked-push", time.Minute)
		if _, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: blocked.ID, DeliveryLease: *blocked.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/main", CommitSHA: strings.Repeat("a", 40), AuthoritativeSHA: strings.Repeat("a", 40), EventKind: "push", RefExists: true, MaxIntents: 10}); !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("snapshot while repository unverified error = %v", err)
		}
		_, err := store.ReconcileGitHubEvent(t.Context(), ReconcileGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease,
			Installation: GitHubAuthoritativeInstallation{InstallationID: 90001, AccountID: 90002, AccountLogin: "acme", AccountType: "Organization", Selection: "selected"},
			Repositories: []GitHubRepository{{GitHubRepositoryID: githubWebhookRepo, GitHubInstallationID: 90001, NodeID: "R_node", OwnerLogin: "acme", Name: "hooks", FullName: "acme/hooks", Visibility: "private", DefaultBranch: "main"}},
			Audit:        &AuditEvent{ID: "10000000-0000-0000-0000-000000000094"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT repository.access_state,connection.access_state,repository.full_name FROM github_repositories repository JOIN git_connections connection ON connection.org_id=repository.org_id AND connection.github_repository_id=repository.github_repository_id WHERE repository.org_id=$1 AND repository.github_repository_id=$2`, githubWebhookOrg, githubWebhookRepo).Scan(&state, &connectionState, &fullName); err != nil || state != "active" || connectionState != "active" || fullName != "acme/hooks" {
			t.Fatalf("recovered repository/connection = %q/%q/%q, %v", state, connectionState, fullName, err)
		}
	})

	t.Run("terminal revocations resist later unverified events until authoritative recreation", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		installationID := int64(90001)
		installationDelivery := func(id, digest, owner string) GitHubWebhookDelivery {
			t.Helper()
			p := webhookParams(id, digest, `{}`)
			p.Event, p.InstallationID, p.RepositoryID, p.RepositoryNodeID = "installation", &installationID, nil, nil
			receiveDelivery(t, store, p)
			return claimOne(t, store, owner, time.Minute)
		}
		repositoryDelivery := func(id, digest, owner string) GitHubWebhookDelivery {
			t.Helper()
			receiveDelivery(t, store, webhookParams(id, digest, `{}`))
			return claimOne(t, store, owner, time.Minute)
		}
		states := func() (string, string, string, int64, int64) {
			t.Helper()
			var installationState, repositoryState, connectionState string
			var installationGeneration, repositoryGeneration int64
			if err := pool.QueryRow(t.Context(), `SELECT installation.access_state,repository.access_state,connection.access_state,
				installation.reconciliation_generation,repository.reconciliation_generation
				FROM github_installations installation
				JOIN github_repositories repository ON repository.org_id=installation.org_id AND repository.github_installation_id=installation.github_installation_id
				JOIN git_connections connection ON connection.org_id=repository.org_id AND connection.github_repository_id=repository.github_repository_id
				WHERE installation.org_id=$1 AND installation.github_installation_id=$2 AND repository.github_repository_id=$3`, githubWebhookOrg, installationID, githubWebhookRepo).
				Scan(&installationState, &repositoryState, &connectionState, &installationGeneration, &repositoryGeneration); err != nil {
				t.Fatal(err)
			}
			return installationState, repositoryState, connectionState, installationGeneration, repositoryGeneration
		}
		reconcile := func(delivery GitHubWebhookDelivery, expectedGeneration int64, auditID string) {
			t.Helper()
			if _, err := store.ReconcileGitHubEvent(t.Context(), ReconcileGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, ExpectedGeneration: expectedGeneration,
				Installation: GitHubAuthoritativeInstallation{InstallationID: installationID, AccountID: 90002, AccountLogin: "acme", AccountType: "Organization", Selection: "selected"},
				Repositories: []GitHubRepository{{GitHubRepositoryID: githubWebhookRepo, GitHubInstallationID: installationID, NodeID: "R_node", OwnerLogin: "acme", Name: "hooks", FullName: "acme/hooks", Visibility: "private", DefaultBranch: "main"}},
				Audit:        &AuditEvent{ID: auditID}}); err != nil {
				t.Fatal(err)
			}
		}

		deleted := installationDelivery("20000000-0000-0000-0000-000000000594", strings.Repeat("a", 64), "installation-delete")
		if err := store.RevokeGitHubEvent(t.Context(), RevokeGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: deleted.ID, DeliveryLease: *deleted.Lease, InstallationID: installationID, Kind: "delete", Audit: &AuditEvent{ID: "10000000-0000-0000-0000-000000000095"}}); err != nil {
			t.Fatal(err)
		}
		_, _, _, deletedGeneration, repositoryGeneration := states()
		for i := range 2 {
			unverified := installationDelivery(fmt.Sprintf("20000000-0000-0000-0000-%012d", 595+i), strings.Repeat(fmt.Sprintf("%x", 11+i), 64), fmt.Sprintf("installation-unverified-%d", i))
			if err := store.RevokeGitHubEvent(t.Context(), RevokeGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: unverified.ID, DeliveryLease: *unverified.Lease, InstallationID: installationID, Kind: "unverified", Audit: &AuditEvent{ID: fmt.Sprintf("10000000-0000-0000-0000-%012d", 96+i)}}); err != nil {
				t.Fatal(err)
			}
		}
		installationState, repositoryState, connectionState, afterUnverifiedGeneration, afterUnverifiedRepositoryGeneration := states()
		if installationState != "deleted" || repositoryState != "active" || connectionState != "deleted" || afterUnverifiedGeneration != deletedGeneration || afterUnverifiedRepositoryGeneration != repositoryGeneration {
			t.Fatalf("installation terminal precedence = states:%q/%q/%q generations:%d/%d, want deleted/active/deleted %d/%d", installationState, repositoryState, connectionState, afterUnverifiedGeneration, afterUnverifiedRepositoryGeneration, deletedGeneration, repositoryGeneration)
		}

		recreatedInstallation := installationDelivery("20000000-0000-0000-0000-000000000597", strings.Repeat("d", 64), "installation-recreate")
		reconcile(recreatedInstallation, deletedGeneration, "10000000-0000-0000-0000-000000000098")
		installationState, repositoryState, connectionState, _, _ = states()
		if installationState != "active" || repositoryState != "active" || connectionState != "active" {
			t.Fatalf("authoritative installation recreation states = %q/%q/%q", installationState, repositoryState, connectionState)
		}

		if _, err := pool.Exec(t.Context(), `UPDATE github_repositories SET access_state='deleted',reconciliation_generation=reconciliation_generation+1,updated_at=clock_timestamp() WHERE org_id=$1 AND github_repository_id=$2`, githubWebhookOrg, githubWebhookRepo); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE git_connections SET access_state='deleted',updated_at=clock_timestamp() WHERE org_id=$1 AND github_repository_id=$2`, githubWebhookOrg, githubWebhookRepo); err != nil {
			t.Fatal(err)
		}
		_, _, _, activeInstallationGeneration, deletedRepositoryGeneration := states()
		for i := range 2 {
			unverifiedRemove := repositoryDelivery(fmt.Sprintf("20000000-0000-0000-0000-%012d", 598+i), strings.Repeat(fmt.Sprintf("%x", 14+i), 64), fmt.Sprintf("repository-unverified-%d", i))
			if err := store.RevokeGitHubEvent(t.Context(), RevokeGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: unverifiedRemove.ID, DeliveryLease: *unverifiedRemove.Lease, InstallationID: installationID, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Kind: "unverified_remove", Audit: &AuditEvent{ID: fmt.Sprintf("10000000-0000-0000-0000-%012d", 99+i)}}); err != nil {
				t.Fatal(err)
			}
		}
		installationState, repositoryState, connectionState, afterRemoveInstallationGeneration, afterRemoveRepositoryGeneration := states()
		if installationState != "active" || repositoryState != "deleted" || connectionState != "deleted" || afterRemoveInstallationGeneration != activeInstallationGeneration || afterRemoveRepositoryGeneration != deletedRepositoryGeneration {
			t.Fatalf("repository terminal precedence = states:%q/%q/%q generations:%d/%d, want active/deleted/deleted %d/%d", installationState, repositoryState, connectionState, afterRemoveInstallationGeneration, afterRemoveRepositoryGeneration, activeInstallationGeneration, deletedRepositoryGeneration)
		}

		recreatedRepository := installationDelivery("20000000-0000-0000-0000-000000000600", strings.Repeat("1", 64), "repository-recreate")
		reconcile(recreatedRepository, activeInstallationGeneration, "10000000-0000-0000-0000-000000000101")
		installationState, repositoryState, connectionState, _, _ = states()
		if installationState != "active" || repositoryState != "active" || connectionState != "active" {
			t.Fatalf("authoritative repository recreation states = %q/%q/%q", installationState, repositoryState, connectionState)
		}
		var audits, deliveries int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE id BETWEEN '10000000-0000-0000-0000-000000000095'::uuid AND '10000000-0000-0000-0000-000000000101'::uuid`).Scan(&audits); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_webhook_deliveries WHERE delivery_id BETWEEN '20000000-0000-0000-0000-000000000594'::uuid AND '20000000-0000-0000-0000-000000000600'::uuid`).Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		if audits != 7 || deliveries != 7 {
			t.Fatalf("repeated event audit/delivery rows = %d/%d, want 7/7", audits, deliveries)
		}
	})

	t.Run("push snapshot predecessor fence and failpoints are atomic", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		shaA, shaB := strings.Repeat("a", 40), strings.Repeat("b", 40)
		audit := func(suffix int) *AuditEvent {
			return &AuditEvent{ID: fmt.Sprintf("10000000-0000-0000-0000-%012d", 700+suffix)}
		}
		first := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000610", strings.Repeat("a", 64), `{}`))
		first = claimOne(t, store, "predecessor-1", time.Minute)
		result, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: first.ID, DeliveryLease: *first.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/main", BeforeSHA: strings.Repeat("0", 40), CommitSHA: shaA, AuthoritativeSHA: shaA, EventKind: "push", RefExists: true, MaxIntents: 10, Audit: audit(0)})
		if err != nil || result.IntentCount != 1 {
			t.Fatalf("first snapshot = %+v, %v", result, err)
		}
		delayed := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000612", strings.Repeat("c", 64), `{}`))
		delayed = claimOne(t, store, "predecessor-delayed", time.Minute)
		second := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000611", strings.Repeat("b", 64), `{}`))
		second = claimOne(t, store, "predecessor-2", time.Minute)
		if _, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: second.ID, DeliveryLease: *second.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/main", BeforeSHA: shaA, CommitSHA: shaB, AuthoritativeSHA: shaB, EventKind: "push", RefExists: true, MaxIntents: 10, Audit: audit(1)}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delayed.ID, DeliveryLease: *delayed.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/main", BeforeSHA: strings.Repeat("0", 40), CommitSHA: shaA, AuthoritativeSHA: shaA, EventKind: "push", RefExists: true, MaxIntents: 10, Audit: audit(2)}); !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("older ingestion sequence with authoritative A error = %v", err)
		}
		shaC := strings.Repeat("c", 40)
		gap := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000613", strings.Repeat("e", 64), `{}`))
		gap = claimOne(t, store, "predecessor-gap", time.Minute)
		if _, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: gap.ID, DeliveryLease: *gap.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/main", BeforeSHA: shaA, CommitSHA: shaC, AuthoritativeSHA: shaC, EventKind: "push", RefExists: true, MaxIntents: 10, Audit: audit(6)}); err != nil {
			t.Fatalf("newer authoritative gap: %v", err)
		}
		var observationKind string
		if err := pool.QueryRow(t.Context(), `SELECT observation_kind FROM github_ref_transition_observations WHERE delivery_id=$1`, gap.ID).Scan(&observationKind); err != nil || observationKind != "resynchronized_gap" {
			t.Fatalf("gap observation = %q, %v", observationKind, err)
		}
		back := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000614", strings.Repeat("f", 64), `{}`))
		back = claimOne(t, store, "predecessor-back", time.Minute)
		if _, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: back.ID, DeliveryLease: *back.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/main", BeforeSHA: shaC, CommitSHA: shaB, AuthoritativeSHA: shaB, EventKind: "push", RefExists: true, MaxIntents: 10, Audit: audit(7)}); err != nil {
			t.Fatalf("legitimate C to B transition: %v", err)
		}

		for i, point := range []string{"after_eligibility", "after_first_intent"} {
			delivery := receiveDelivery(t, store, webhookParams(fmt.Sprintf("20000000-0000-0000-0000-%012d", 620+i), strings.Repeat("d", 64), `{}`))
			delivery = claimOne(t, store, fmt.Sprintf("failpoint-%d", i), time.Minute)
			fail := errors.New("snapshot failpoint")
			ref := "refs/heads/failpoint"
			before := strings.Repeat("0", 40)
			if point == "after_first_intent" {
				ref = "refs/heads/main"
				before = shaB
			}
			_, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: ref, BeforeSHA: before, CommitSHA: shaA, AuthoritativeSHA: shaA, EventKind: "push", RefExists: true, MaxIntents: 10, Audit: audit(3 + i), Failpoint: func(got string) error {
				if got == point {
					return fail
				}
				return nil
			}})
			if !errors.Is(err, fail) {
				t.Fatalf("%s error = %v", point, err)
			}
			var intents, refs int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_routing_intents WHERE delivery_id=$1`, delivery.ID).Scan(&intents); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_ref_routing_state WHERE repository_id=$1 AND ref=$2`, githubWebhookRepo, ref).Scan(&refs); err != nil {
				t.Fatal(err)
			}
			wantRefs := 0
			if ref == "refs/heads/main" {
				wantRefs = 1
			}
			if intents != 0 || refs != wantRefs {
				t.Fatalf("%s partial state = intents:%d refs:%d", point, intents, refs)
			}
		}
	})

	t.Run("delete snapshot excludes production apps before fanout limit", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		const deletedRef = "refs/heads/deleted"
		if _, err := pool.Exec(t.Context(), `UPDATE apps SET production_branch=CASE id WHEN $1 THEN 'deleted' ELSE 'other' END WHERE id IN ($1,$2)`, githubWebhookAppA, githubWebhookAppB); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE environments SET branch='deleted' WHERE id=$1`, githubWebhookEnv); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := pool.Exec(context.Background(), `UPDATE apps SET production_branch='main' WHERE id IN ($1,$2)`, githubWebhookAppA, githubWebhookAppB); err != nil {
				t.Errorf("restore delete fanout fixture: %v", err)
			}
			if _, err := pool.Exec(context.Background(), `UPDATE environments SET branch='main' WHERE id=$1`, githubWebhookEnv); err != nil {
				t.Errorf("restore delete fanout production environment: %v", err)
			}
		})

		snapshotDelete := func(deliveryID, digest, owner, auditID string, maxIntents int) (SnapshotGitHubPushResult, error) {
			t.Helper()
			delivery := receiveDelivery(t, store, webhookParams(deliveryID, digest, `{}`))
			delivery = claimOne(t, store, owner, time.Minute)
			return store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease,
				OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: deletedRef,
				BeforeSHA: strings.Repeat("a", 40), EventKind: "delete", RefExists: false, MaxIntents: maxIntents, Audit: &AuditEvent{ID: auditID}})
		}

		result, err := snapshotDelete("20000000-0000-0000-0000-000000000680", strings.Repeat("1", 64), "delete-ordered", "10000000-0000-0000-0000-000000000780", 1)
		if err != nil || result.IntentCount != 1 {
			t.Fatalf("ordered production/preview delete snapshot = %+v, %v", result, err)
		}
		var appID string
		var production bool
		if err := pool.QueryRow(t.Context(), `SELECT app_id::text,production_ref FROM github_routing_intents WHERE delivery_id=(SELECT id FROM github_webhook_deliveries WHERE delivery_id='20000000-0000-0000-0000-000000000680')`).Scan(&appID, &production); err != nil {
			t.Fatal(err)
		}
		if appID != githubWebhookAppB || production {
			t.Fatalf("ordered delete intent = app %s production:%t, want preview app %s", appID, production, githubWebhookAppB)
		}
		var productionDeletes int
		var productionOutcome string
		if err := pool.QueryRow(t.Context(), `SELECT (after->>'production_branch_deleted_count')::int,after->>'production_branch_delete_outcome' FROM audit_events WHERE id='10000000-0000-0000-0000-000000000780'`).Scan(&productionDeletes, &productionOutcome); err != nil {
			t.Fatal(err)
		}
		if productionDeletes != 1 || productionOutcome != "recorded_no_teardown" {
			t.Fatalf("production deletion audit = count %d outcome %q", productionDeletes, productionOutcome)
		}

		for i := range 5 {
			appID := fmt.Sprintf("10000000-0000-0000-0000-%012d", 800+i)
			environmentID := fmt.Sprintf("10000000-0000-0000-0000-%012d", 900+i)
			if _, err := pool.Exec(t.Context(), `INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch)
				VALUES($1,$2,'10000000-0000-0000-0000-000000000010',$3,$4,$4,$5,'.github/workflows/deploy.yml','deleted')`,
				appID, githubWebhookOrg, fmt.Sprintf("Production %d", i), fmt.Sprintf("production-%d", i), fmt.Sprintf("ghcr.io/acme/production-%d", i),
			); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO environments(id,org_id,app_id,branch,kind,namespace,planned_hostname) VALUES($1,$2,$3,'deleted','production',$4,$5)`,
				environmentID, githubWebhookOrg, appID, fmt.Sprintf("production-%d", i), fmt.Sprintf("production-%d.example.test", i)); err != nil {
				t.Fatal(err)
			}
		}
		result, err = snapshotDelete("20000000-0000-0000-0000-000000000681", strings.Repeat("2", 64), "delete-many-production", "10000000-0000-0000-0000-000000000781", 1)
		if err != nil || result.IntentCount != 1 {
			t.Fatalf("many production delete snapshot = %+v, %v", result, err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT (after->>'production_branch_deleted_count')::int FROM audit_events WHERE id='10000000-0000-0000-0000-000000000781'`).Scan(&productionDeletes); err != nil || productionDeletes != 6 {
			t.Fatalf("many production deletion audit count = %d, %v", productionDeletes, err)
		}

		if _, err := pool.Exec(t.Context(), `UPDATE apps SET production_branch='other' WHERE id='10000000-0000-0000-0000-000000000804'`); err != nil {
			t.Fatal(err)
		}
		if _, err := snapshotDelete("20000000-0000-0000-0000-000000000682", strings.Repeat("3", 64), "delete-preview-overflow", "10000000-0000-0000-0000-000000000782", 1); !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("actual preview overflow error = %v", err)
		}
		var intents, observations, audits int
		if err := pool.QueryRow(t.Context(), `SELECT
			(SELECT count(*) FROM github_routing_intents intent JOIN github_webhook_deliveries delivery ON delivery.id=intent.delivery_id WHERE delivery.delivery_id='20000000-0000-0000-0000-000000000682'),
			(SELECT count(*) FROM github_ref_transition_observations observation JOIN github_webhook_deliveries delivery ON delivery.id=observation.delivery_id WHERE delivery.delivery_id='20000000-0000-0000-0000-000000000682'),
			(SELECT count(*) FROM audit_events WHERE id='10000000-0000-0000-0000-000000000782')`).Scan(&intents, &observations, &audits); err != nil {
			t.Fatal(err)
		}
		if intents != 0 || observations != 0 || audits != 0 {
			t.Fatalf("preview overflow partial state = intents:%d observations:%d audits:%d", intents, observations, audits)
		}
	})

	t.Run("delete intent authorization follows stable preview ownership and ref lifecycle", func(t *testing.T) {
		const projectID = "10000000-0000-0000-0000-000000000010"
		zero := strings.Repeat("0", 40)
		caseNumber := 0
		prepare := func(t *testing.T) (GitHubRoutingIntent, string) {
			t.Helper()
			caseNumber++
			clearGitHubWebhookWork(t, pool)
			resets := []struct {
				query string
				args  []any
			}{
				{`UPDATE github_installations SET access_state='active' WHERE org_id=$1 AND github_installation_id=90001`, []any{githubWebhookOrg}},
				{`UPDATE github_repositories SET github_installation_id=90001,node_id='R_node',name='hooks',full_name='acme/hooks',access_state='active',archived=false WHERE org_id=$1 AND github_repository_id=$2`, []any{githubWebhookOrg, githubWebhookRepo}},
				{`UPDATE git_connections SET github_repository_id=$2,state='active',access_state='active' WHERE org_id=$1 AND id='10000000-0000-0000-0000-000000000009'`, []any{githubWebhookOrg, githubWebhookRepo}},
				{`UPDATE projects SET deleted_at=NULL WHERE org_id=$1 AND id=$2`, []any{githubWebhookOrg, projectID}},
				{`UPDATE apps SET deleted_at=NULL,desired_generation=1,github_workflow_path='.github/workflows/deploy.yml',production_branch='main',previews_enabled=true WHERE org_id=$1 AND id IN ($2,$3)`, []any{githubWebhookOrg, githubWebhookAppA, githubWebhookAppB}},
			}
			for _, reset := range resets {
				if _, err := pool.Exec(t.Context(), reset.query, reset.args...); err != nil {
					t.Fatal(err)
				}
			}
			branch := fmt.Sprintf("delete-auth-%d", caseNumber)
			environmentID := fmt.Sprintf("30000000-0000-0000-0000-%012d", caseNumber)
			if _, err := pool.Exec(t.Context(), `INSERT INTO environments(id,org_id,app_id,branch,kind,preview_slug,namespace,planned_hostname,lifecycle_status)
				VALUES($1,$2,$3,$4,'preview',$4,$5,$6,'ready')`, environmentID, githubWebhookOrg, githubWebhookAppB, branch, "preview-"+branch, branch+".example.test"); err != nil {
				t.Fatal(err)
			}
			deliveryID := fmt.Sprintf("20000000-0000-0000-0000-%012d", 700+caseNumber)
			delivery := receiveDelivery(t, store, webhookParams(deliveryID, strings.Repeat(fmt.Sprintf("%x", caseNumber), 64), `{}`))
			delivery = claimOne(t, store, "delete-authorize-snapshot", time.Minute)
			result, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease,
				OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/" + branch,
				BeforeSHA: zero, EventKind: "delete", RefExists: false, MaxIntents: 10, Audit: &AuditEvent{ID: fmt.Sprintf("40000000-0000-0000-0000-%012d", caseNumber)}})
			if err != nil || result.IntentCount < 1 {
				t.Fatalf("delete snapshot = %+v, %v", result, err)
			}
			claimed, err := store.ClaimGitHubRoutingIntents(t.Context(), "delete-authorize-execute", time.Minute, 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, intent := range claimed {
				if intent.AppID == githubWebhookAppB {
					for _, other := range claimed {
						if other.ID != intent.ID {
							if err := store.CompleteGitHubRoutingIntent(t.Context(), other.ID, *other.Lease, "ignored"); err != nil {
								t.Fatal(err)
							}
						}
					}
					return intent, environmentID
				}
			}
			t.Fatal("preview delete intent was not claimed")
			return GitHubRoutingIntent{}, ""
		}
		assertTeardown := func(t *testing.T, environmentID string) {
			t.Helper()
			var status string
			var jobs int
			if err := pool.QueryRow(t.Context(), `SELECT lifecycle_status FROM environments WHERE id=$1`, environmentID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM environment_lifecycle_jobs WHERE environment_id=$1 AND kind='teardown'`, environmentID).Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			if status != "teardown_requested" || jobs != 1 {
				t.Fatalf("teardown state = %q jobs=%d", status, jobs)
			}
		}

		t.Run("repository reconciliation metadata change", func(t *testing.T) {
			intent, environmentID := prepare(t)
			if _, err := pool.Exec(t.Context(), `UPDATE github_repositories SET name='hooks-renamed',full_name='acme/hooks-renamed',reconciliation_generation=reconciliation_generation+1 WHERE org_id=$1 AND github_repository_id=$2`, githubWebhookOrg, githubWebhookRepo); err != nil {
				t.Fatal(err)
			}
			if result, err := store.ExecuteGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, nil); err != nil || result != "processed" {
				t.Fatalf("execute after repository metadata change = %q, %v", result, err)
			}
			assertTeardown(t, environmentID)
		})

		t.Run("app config and workflow change", func(t *testing.T) {
			intent, environmentID := prepare(t)
			if _, err := pool.Exec(t.Context(), `UPDATE apps SET desired_generation=desired_generation+1,github_workflow_path='.github/workflows/renamed.yml',production_branch='renamed' WHERE id=$1`, githubWebhookAppB); err != nil {
				t.Fatal(err)
			}
			if result, err := store.ExecuteGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, nil); err != nil || result != "processed" {
				t.Fatalf("execute after app config change = %q, %v", result, err)
			}
			assertTeardown(t, environmentID)
		})

		t.Run("repository transfer supersedes old delete and fresh owner delete tears down", func(t *testing.T) {
			intent, environmentID := prepare(t)
			const newInstallationID = int64(90105)
			if _, err := pool.Exec(t.Context(), `INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation,repository_reconciliation_generation)
				VALUES($1,$2,90106,'acme-new-owner','Organization','selected','active',1,1,1) ON CONFLICT(github_installation_id) DO UPDATE SET access_state='active'`, newInstallationID, githubWebhookOrg); err != nil {
				t.Fatal(err)
			}
			installationID := newInstallationID
			transferParams := webhookParams("20000000-0000-0000-0000-000000000790", strings.Repeat("d", 64), `{}`)
			transferParams.Event, transferParams.InstallationID, transferParams.RepositoryID, transferParams.RepositoryNodeID = "installation", &installationID, nil, nil
			receiveDelivery(t, store, transferParams)
			transfer := claimOne(t, store, "delete-transfer", time.Minute)
			if _, err := store.ReconcileGitHubEvent(t.Context(), ReconcileGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: transfer.ID, DeliveryLease: *transfer.Lease,
				Installation: GitHubAuthoritativeInstallation{InstallationID: newInstallationID, AccountID: 90106, AccountLogin: "acme-new-owner", AccountType: "Organization", Selection: "selected"},
				Repositories: []GitHubRepository{{GitHubRepositoryID: githubWebhookRepo, GitHubInstallationID: newInstallationID, NodeID: "R_node", OwnerLogin: "acme", Name: "hooks", FullName: "acme/hooks", Visibility: "private", DefaultBranch: "main"}},
				Audit:        &AuditEvent{ID: "40000000-0000-0000-0000-000000000790"}}); err != nil {
				t.Fatal(err)
			}
			if result, err := store.ExecuteGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, nil); err != nil || result != "superseded" {
				t.Fatalf("execute old owner delete after transfer = %q, %v", result, err)
			}
			var intentStatus, intentResult string
			if err := pool.QueryRow(t.Context(), `SELECT status,result_code FROM github_routing_intents WHERE id=$1`, intent.ID).Scan(&intentStatus, &intentResult); err != nil || intentStatus != "dropped" || intentResult != "superseded" {
				t.Fatalf("old owner delete terminal state = %q/%q, %v", intentStatus, intentResult, err)
			}
			var status string
			if err := pool.QueryRow(t.Context(), `SELECT lifecycle_status FROM environments WHERE id=$1`, environmentID).Scan(&status); err != nil || status == "teardown_requested" {
				t.Fatalf("transferred preview status = %q, %v", status, err)
			}

			freshParams := webhookParams("20000000-0000-0000-0000-000000000791", strings.Repeat("e", 64), `{}`)
			freshParams.InstallationID = &installationID
			fresh := receiveDelivery(t, store, freshParams)
			fresh = claimOne(t, store, "delete-new-owner-snapshot", time.Minute)
			snapshot, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: fresh.ID, DeliveryLease: *fresh.Lease,
				OrgID: githubWebhookOrg, InstallationID: newInstallationID, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: intent.Ref,
				BeforeSHA: zero, EventKind: "delete", RefExists: false, MaxIntents: 10, Audit: &AuditEvent{ID: "40000000-0000-0000-0000-000000000791"}})
			if err != nil || snapshot.IntentCount < 1 {
				t.Fatalf("new owner delete snapshot = %+v, %v", snapshot, err)
			}
			claimed, err := store.ClaimGitHubRoutingIntents(t.Context(), "delete-new-owner-execute", time.Minute, 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, freshIntent := range claimed {
				if freshIntent.AppID != githubWebhookAppB {
					if err := store.CompleteGitHubRoutingIntent(t.Context(), freshIntent.ID, *freshIntent.Lease, "ignored"); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if result, err := store.ExecuteGitHubRoutingIntent(t.Context(), freshIntent.ID, *freshIntent.Lease, nil); err != nil || result != "processed" {
					t.Fatalf("execute new owner delete = %q, %v", result, err)
				}
				assertTeardown(t, environmentID)
				return
			}
			t.Fatal("new owner delete intent was not claimed")
		})

		t.Run("deleted app and project with live preview", func(t *testing.T) {
			intent, environmentID := prepare(t)
			if _, err := pool.Exec(t.Context(), `ALTER TABLE apps DISABLE TRIGGER apps_tombstone_ready`); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = pool.Exec(context.Background(), `ALTER TABLE apps ENABLE TRIGGER apps_tombstone_ready`) }()
			if _, err := pool.Exec(t.Context(), `ALTER TABLE projects DISABLE TRIGGER projects_tombstone_ready`); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_, _ = pool.Exec(context.Background(), `ALTER TABLE projects ENABLE TRIGGER projects_tombstone_ready`)
			}()
			if _, err := pool.Exec(t.Context(), `UPDATE apps SET external_credentials_revoked_at=clock_timestamp(),deleted_at=clock_timestamp() WHERE id=$1`, githubWebhookAppB); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE projects SET external_credentials_revoked_at=clock_timestamp(),deleted_at=clock_timestamp() WHERE id=$1`, projectID); err != nil {
				t.Fatal(err)
			}
			if result, err := store.ExecuteGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, nil); err != nil || result != "processed" {
				t.Fatalf("execute after app/project deletion = %q, %v", result, err)
			}
			assertTeardown(t, environmentID)
		})

		t.Run("newer ref recreation supersedes delete", func(t *testing.T) {
			intent, environmentID := prepare(t)
			if _, err := pool.Exec(t.Context(), `UPDATE github_ref_routing_state SET routing_generation=routing_generation+1,lifecycle_generation=lifecycle_generation+1,validated_sha=$3,ref_exists=true WHERE repository_id=$1 AND ref=$2`, githubWebhookRepo, intent.Ref, strings.Repeat("a", 40)); err != nil {
				t.Fatal(err)
			}
			if result, err := store.ExecuteGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, nil); err != nil || result != "superseded" {
				t.Fatalf("execute after ref recreation = %q, %v", result, err)
			}
			var status string
			if err := pool.QueryRow(t.Context(), `SELECT lifecycle_status FROM environments WHERE id=$1`, environmentID).Scan(&status); err != nil || status == "teardown_requested" {
				t.Fatalf("recreated preview status = %q, %v", status, err)
			}
		})

		t.Run("repository node identity change drops delete", func(t *testing.T) {
			intent, environmentID := prepare(t)
			if _, err := pool.Exec(t.Context(), `INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation)
				VALUES(91002,$1,90001,'R_replaced','acme','replacement','acme/replacement','private','main','active',1,1)`, githubWebhookOrg); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE git_connections SET github_repository_id=91002 WHERE org_id=$1 AND id='10000000-0000-0000-0000-000000000009'`, githubWebhookOrg); err != nil {
				t.Fatal(err)
			}
			if result, err := store.ExecuteGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, nil); err != nil || result != "superseded" {
				t.Fatalf("execute after repository identity change = %q, %v", result, err)
			}
			var status string
			if err := pool.QueryRow(t.Context(), `SELECT lifecycle_status FROM environments WHERE id=$1`, environmentID).Scan(&status); err != nil || status == "teardown_requested" {
				t.Fatalf("reassociated preview status = %q, %v", status, err)
			}
		})

		t.Run("concurrent retry executes teardown exactly once", func(t *testing.T) {
			intent, environmentID := prepare(t)
			type execution struct {
				result string
				err    error
			}
			executions := make(chan execution, 2)
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					result, err := store.ExecuteGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, nil)
					executions <- execution{result: result, err: err}
				}()
			}
			wg.Wait()
			close(executions)
			processed, leaseLost := 0, 0
			for execution := range executions {
				if execution.err == nil && execution.result == "processed" {
					processed++
				} else if errors.Is(execution.err, ErrLeaseNotOwned) {
					leaseLost++
				} else if execution.err != nil {
					t.Fatal(execution.err)
				}
			}
			if processed != 1 || leaseLost != 1 {
				t.Fatalf("concurrent execution processed=%d lease_lost=%d", processed, leaseLost)
			}
			assertTeardown(t, environmentID)
		})
	})

	t.Run("recurring transitions route newest observations", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		zero, shaA, shaB := strings.Repeat("0", 40), strings.Repeat("a", 40), strings.Repeat("b", 40)
		transitions := []struct {
			id, before, after string
		}{
			{"20000000-0000-0000-0000-000000000650", zero, shaA},
			{"20000000-0000-0000-0000-000000000651", shaA, shaB},
			{"20000000-0000-0000-0000-000000000652", shaB, shaA},
			{"20000000-0000-0000-0000-000000000653", shaA, shaB},
		}
		var newest GitHubWebhookDelivery
		for i, transition := range transitions {
			delivery := receiveDelivery(t, store, webhookParams(transition.id, strings.Repeat(fmt.Sprintf("%x", i+1), 64), `{}`))
			delivery = claimOne(t, store, fmt.Sprintf("recurring-%d", i), time.Minute)
			result, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/main", BeforeSHA: transition.before, CommitSHA: transition.after, AuthoritativeSHA: transition.after, EventKind: "push", RefExists: true, MaxIntents: 10, Audit: &AuditEvent{ID: fmt.Sprintf("10000000-0000-0000-0000-%012d", 750+i)}})
			if err != nil || result.RoutingGeneration != int64(i+1) || result.IntentCount != 1 {
				t.Fatalf("recurring transition %d snapshot = %+v, %v", i, result, err)
			}
			newest = delivery
		}
		var currentSHA, lastDelivery string
		var sequence int64
		if err := pool.QueryRow(t.Context(), `SELECT validated_sha,last_delivery_id::text,last_observation_sequence FROM github_ref_routing_state WHERE repository_id=$1 AND ref='refs/heads/main'`, githubWebhookRepo).Scan(&currentSHA, &lastDelivery, &sequence); err != nil {
			t.Fatal(err)
		}
		if currentSHA != shaB || lastDelivery != newest.ID || sequence != newest.IngestionSequence {
			t.Fatalf("newest recurring route = sha:%q delivery:%q sequence:%d, want %q/%q/%d", currentSHA, lastDelivery, sequence, shaB, newest.ID, newest.IngestionSequence)
		}
		var newestIntents int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_routing_intents WHERE delivery_id=$1`, newest.ID).Scan(&newestIntents); err != nil || newestIntents != 1 {
			t.Fatalf("newest recurring intents = %d, %v", newestIntents, err)
		}
	})

	t.Run("delete and recreate may repeat identical SHA transitions", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		zero, shaA := strings.Repeat("0", 40), strings.Repeat("a", 40)
		events := []struct {
			id, kind, before, commit string
			exists                   bool
		}{
			{"20000000-0000-0000-0000-000000000660", "push", zero, shaA, true},
			{"20000000-0000-0000-0000-000000000661", "delete", shaA, "", false},
			{"20000000-0000-0000-0000-000000000662", "push", zero, shaA, true},
			{"20000000-0000-0000-0000-000000000663", "delete", shaA, "", false},
			{"20000000-0000-0000-0000-000000000664", "push", zero, shaA, true},
		}
		for i, event := range events {
			delivery := receiveDelivery(t, store, webhookParams(event.id, strings.Repeat(fmt.Sprintf("%x", i+5), 64), `{}`))
			delivery = claimOne(t, store, fmt.Sprintf("lifecycle-repeat-%d", i), time.Minute)
			authoritative := event.commit
			result, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/recreated", BeforeSHA: event.before, CommitSHA: event.commit, AuthoritativeSHA: authoritative, EventKind: event.kind, RefExists: event.exists, MaxIntents: 10, Audit: &AuditEvent{ID: fmt.Sprintf("10000000-0000-0000-0000-%012d", 760+i)}})
			if err != nil || result.RoutingGeneration != int64(i+1) {
				t.Fatalf("repeated lifecycle transition %d snapshot = %+v, %v", i, result, err)
			}
		}
		var exists bool
		var sha string
		var lifecycle, observations, fingerprints int
		if err := pool.QueryRow(t.Context(), `SELECT ref_exists,validated_sha,lifecycle_generation FROM github_ref_routing_state WHERE repository_id=$1 AND ref='refs/heads/recreated'`, githubWebhookRepo).Scan(&exists, &sha, &lifecycle); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_ref_transition_observations WHERE repository_id=$1 AND ref='refs/heads/recreated'`, githubWebhookRepo).Scan(&observations); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT count(DISTINCT transition_fingerprint) FROM github_ref_transition_observations WHERE repository_id=$1 AND ref='refs/heads/recreated'`, githubWebhookRepo).Scan(&fingerprints); err != nil {
			t.Fatal(err)
		}
		if !exists || sha != shaA || lifecycle != 4 || observations != 5 || fingerprints != 2 {
			t.Fatalf("recreated ref = exists:%t sha:%q lifecycle:%d observations:%d diagnostic fingerprints:%d", exists, sha, lifecycle, observations, fingerprints)
		}
	})

	t.Run("concurrent newer observations serialize on routing state", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		zero, shaA, shaB, shaC := strings.Repeat("0", 40), strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
		initial := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000670", strings.Repeat("a", 64), `{}`))
		initial = claimOne(t, store, "concurrent-initial", time.Minute)
		if _, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: initial.ID, DeliveryLease: *initial.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/concurrent", BeforeSHA: zero, CommitSHA: shaA, AuthoritativeSHA: shaA, EventKind: "push", RefExists: true, MaxIntents: 10, Audit: &AuditEvent{ID: "10000000-0000-0000-0000-000000000770"}}); err != nil {
			t.Fatal(err)
		}
		lower := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000671", strings.Repeat("b", 64), `{}`))
		lower = claimOne(t, store, "concurrent-lower", time.Minute)
		higher := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000672", strings.Repeat("c", 64), `{}`))
		higher = claimOne(t, store, "concurrent-higher", time.Minute)
		locked, release := make(chan struct{}), make(chan struct{})
		lowerErr := make(chan error, 1)
		go func() {
			_, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: lower.ID, DeliveryLease: *lower.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/concurrent", BeforeSHA: shaA, CommitSHA: shaB, AuthoritativeSHA: shaB, EventKind: "push", RefExists: true, MaxIntents: 10, Audit: &AuditEvent{ID: "10000000-0000-0000-0000-000000000771"}, Failpoint: func(point string) error {
				if point == "after_eligibility" {
					close(locked)
					<-release
				}
				return nil
			}})
			lowerErr <- err
		}()
		<-locked
		higherErr := make(chan error, 1)
		go func() {
			_, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: higher.ID, DeliveryLease: *higher.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/concurrent", BeforeSHA: shaA, CommitSHA: shaC, AuthoritativeSHA: shaC, EventKind: "push", RefExists: true, MaxIntents: 10, Audit: &AuditEvent{ID: "10000000-0000-0000-0000-000000000772"}})
			higherErr <- err
		}()
		close(release)
		if err := <-lowerErr; err != nil {
			t.Fatalf("lower concurrent observation: %v", err)
		}
		if err := <-higherErr; err != nil {
			t.Fatalf("higher concurrent observation: %v", err)
		}
		var sha, lastDelivery string
		var generation int64
		if err := pool.QueryRow(t.Context(), `SELECT validated_sha,last_delivery_id::text,routing_generation FROM github_ref_routing_state WHERE repository_id=$1 AND ref='refs/heads/concurrent'`, githubWebhookRepo).Scan(&sha, &lastDelivery, &generation); err != nil {
			t.Fatal(err)
		}
		if sha != shaC || lastDelivery != higher.ID || generation != 3 {
			t.Fatalf("serialized current route = sha:%q delivery:%q generation:%d", sha, lastDelivery, generation)
		}
	})

	t.Run("event mutations require current delivery lease", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		installationID := int64(90001)
		lifecycleParams := webhookParams("20000000-0000-0000-0000-000000000630", strings.Repeat("1", 64), `{}`)
		lifecycleParams.Event, lifecycleParams.InstallationID, lifecycleParams.RepositoryID, lifecycleParams.RepositoryNodeID = "installation", &installationID, nil, nil
		receiveDelivery(t, store, lifecycleParams)
		oldLifecycle := claimOne(t, store, "old-lifecycle", time.Second)
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, oldLifecycle.ID); err != nil {
			t.Fatal(err)
		}
		_ = claimOne(t, store, "new-lifecycle", time.Minute)
		if err := store.RevokeGitHubEvent(t.Context(), RevokeGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: oldLifecycle.ID, DeliveryLease: *oldLifecycle.Lease, InstallationID: 90001, Kind: "unverified", Audit: &AuditEvent{ID: "10000000-0000-0000-0000-000000000730"}}); !errors.Is(err, ErrLeaseNotOwned) {
			t.Fatalf("stale lifecycle mutation error = %v", err)
		}
		var state string
		if err := pool.QueryRow(t.Context(), `SELECT access_state FROM github_installations WHERE org_id=$1 AND github_installation_id=90001`, githubWebhookOrg).Scan(&state); err != nil || state != "active" {
			t.Fatalf("installation state = %q, %v", state, err)
		}

		push := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000631", strings.Repeat("2", 64), `{}`))
		oldPush := claimOne(t, store, "old-push", time.Second)
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, push.ID); err != nil {
			t.Fatal(err)
		}
		_ = claimOne(t, store, "new-push", time.Minute)
		if _, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: oldPush.ID, DeliveryLease: *oldPush.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/fenced", BeforeSHA: strings.Repeat("0", 40), CommitSHA: strings.Repeat("a", 40), AuthoritativeSHA: strings.Repeat("a", 40), EventKind: "push", RefExists: true, MaxIntents: 10, Audit: &AuditEvent{ID: "10000000-0000-0000-0000-000000000731"}}); !errors.Is(err, ErrLeaseNotOwned) {
			t.Fatalf("stale push mutation error = %v", err)
		}
	})

	t.Run("repository transfer accepts lower new installation generation", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		if _, err := pool.Exec(t.Context(), `INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation,repository_reconciliation_generation) VALUES(90103,$1,90104,'acme-new','Organization','selected','active',1,1,1)`, githubWebhookOrg); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO github_repository_routing_state(repository_id,org_id,repository_node_id,installation_id,repository_generation,lifecycle_generation) VALUES($1,$2,'R_node',90001,1,99)`, githubWebhookRepo, githubWebhookOrg); err != nil {
			t.Fatal(err)
		}
		installationID := int64(90103)
		p := webhookParams("20000000-0000-0000-0000-000000000632", strings.Repeat("3", 64), `{}`)
		p.Event, p.InstallationID, p.RepositoryID, p.RepositoryNodeID = "installation", &installationID, nil, nil
		receiveDelivery(t, store, p)
		delivery := claimOne(t, store, "transfer", time.Minute)
		_, err := store.ReconcileGitHubEvent(t.Context(), ReconcileGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, ExpectedGeneration: 1,
			Installation: GitHubAuthoritativeInstallation{InstallationID: 90103, AccountID: 90104, AccountLogin: "acme-new", AccountType: "Organization", Selection: "selected"},
			Repositories: []GitHubRepository{{GitHubRepositoryID: githubWebhookRepo, GitHubInstallationID: 90103, NodeID: "R_node", OwnerLogin: "acme", Name: "hooks", FullName: "acme/hooks", Visibility: "private", DefaultBranch: "main"}},
			Audit:        &AuditEvent{ID: "10000000-0000-0000-0000-000000000732"}})
		if err != nil {
			t.Fatal(err)
		}
		var routedInstallation, lifecycle int64
		if err := pool.QueryRow(t.Context(), `SELECT installation_id,lifecycle_generation FROM github_repository_routing_state WHERE repository_id=$1`, githubWebhookRepo).Scan(&routedInstallation, &lifecycle); err != nil || routedInstallation != 90103 || lifecycle >= 99 {
			t.Fatalf("routing identity = %d/%d, %v", routedInstallation, lifecycle, err)
		}
	})

	t.Run("int64 installation reconciliation revocation and crossed transfer do not overflow or deadlock", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		const (
			lifecycleInstallation = int64(1<<31 + 101)
			lifecycleAccount      = int64(1<<31 + 102)
			lifecycleRepository   = int64(1<<31 + 103)
			firstInstallation     = int64(1<<31 + 201)
			firstAccount          = int64(1<<31 + 202)
			firstRepository       = int64(1<<31 + 203)
			secondInstallation    = int64(1<<31 + 301)
			secondAccount         = int64(1<<31 + 302)
			secondRepository      = int64(1<<31 + 303)
		)
		if _, err := pool.Exec(t.Context(), `INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation,repository_reconciliation_generation) VALUES
			($1,$3,$2,'large-lifecycle','Organization','selected','active',1,1,1),
			($4,$3,$5,'large-first','Organization','selected','active',1,1,1),
			($6,$3,$7,'large-second','Organization','selected','active',1,1,1)`, lifecycleInstallation, lifecycleAccount, githubWebhookOrg, firstInstallation, firstAccount, secondInstallation, secondAccount); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES
			($1,$3,$2,'R_large_lifecycle','acme','large-lifecycle','acme/large-lifecycle','private','main','active',1,1),
			($4,$3,$5,'R_large_first','acme','large-first','acme/large-first','private','main','active',1,1),
			($6,$3,$7,'R_large_second','acme','large-second','acme/large-second','private','main','active',1,1)`, lifecycleRepository, lifecycleInstallation, githubWebhookOrg, firstRepository, firstInstallation, secondRepository, secondInstallation); err != nil {
			t.Fatal(err)
		}

		lifecycleInstallationID := lifecycleInstallation
		lifecycleParams := webhookParams("20000000-0000-0000-0000-000000000740", strings.Repeat("4", 64), `{}`)
		lifecycleParams.Event, lifecycleParams.InstallationID, lifecycleParams.RepositoryID, lifecycleParams.RepositoryNodeID = "installation", &lifecycleInstallationID, nil, nil
		receiveDelivery(t, store, lifecycleParams)
		lifecycle := claimOne(t, store, "large-reconcile", time.Minute)
		if _, err := store.ReconcileGitHubEvent(t.Context(), ReconcileGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: lifecycle.ID, DeliveryLease: *lifecycle.Lease,
			Installation: GitHubAuthoritativeInstallation{InstallationID: lifecycleInstallation, AccountID: lifecycleAccount, AccountLogin: "large-lifecycle", AccountType: "Organization", Selection: "selected"},
			Repositories: []GitHubRepository{{GitHubRepositoryID: lifecycleRepository, GitHubInstallationID: lifecycleInstallation, NodeID: "R_large_lifecycle", OwnerLogin: "acme", Name: "large-lifecycle", FullName: "acme/large-lifecycle", Visibility: "private", DefaultBranch: "main"}},
			Audit:        &AuditEvent{ID: "10000000-0000-0000-0000-000000000740"}}); err != nil {
			t.Fatalf("large installation reconciliation: %v", err)
		}
		revokeParams := webhookParams("20000000-0000-0000-0000-000000000741", strings.Repeat("5", 64), `{}`)
		revokeParams.Event, revokeParams.InstallationID, revokeParams.RepositoryID, revokeParams.RepositoryNodeID = "installation", &lifecycleInstallationID, nil, nil
		receiveDelivery(t, store, revokeParams)
		revocation := claimOne(t, store, "large-revoke", time.Minute)
		if err := store.RevokeGitHubEvent(t.Context(), RevokeGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: revocation.ID, DeliveryLease: *revocation.Lease, InstallationID: lifecycleInstallation, Kind: "unverified", Audit: &AuditEvent{ID: "10000000-0000-0000-0000-000000000741"}}); err != nil {
			t.Fatalf("large installation revocation: %v", err)
		}

		for i, installationID := range []int64{firstInstallation, secondInstallation} {
			params := webhookParams(fmt.Sprintf("20000000-0000-0000-0000-%012d", 742+i), strings.Repeat(fmt.Sprint(6+i), 64), `{}`)
			params.Event, params.InstallationID, params.RepositoryID, params.RepositoryNodeID = "installation", new(int64), nil, nil
			*params.InstallationID = installationID
			receiveDelivery(t, store, params)
		}
		deliveries, err := store.ClaimGitHubWebhookDeliveries(t.Context(), "large-transfer", time.Minute, 2)
		if err != nil || len(deliveries) != 2 {
			t.Fatalf("claim large transfer deliveries = %+v, %v", deliveries, err)
		}
		byInstallation := make(map[int64]GitHubWebhookDelivery, 2)
		for _, delivery := range deliveries {
			byInstallation[*delivery.InstallationID] = delivery
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := make(chan struct{})
		results := make(chan error, 2)
		attempts := []struct {
			installation GitHubAuthoritativeInstallation
			repository   GitHubRepository
			delivery     GitHubWebhookDelivery
			auditID      string
		}{
			{GitHubAuthoritativeInstallation{InstallationID: firstInstallation, AccountID: firstAccount, AccountLogin: "large-first", AccountType: "Organization", Selection: "selected"}, GitHubRepository{GitHubRepositoryID: secondRepository, GitHubInstallationID: firstInstallation, NodeID: "R_large_second", OwnerLogin: "acme", Name: "large-second", FullName: "acme/large-second", Visibility: "private", DefaultBranch: "main"}, byInstallation[firstInstallation], "10000000-0000-0000-0000-000000000742"},
			{GitHubAuthoritativeInstallation{InstallationID: secondInstallation, AccountID: secondAccount, AccountLogin: "large-second", AccountType: "Organization", Selection: "selected"}, GitHubRepository{GitHubRepositoryID: firstRepository, GitHubInstallationID: secondInstallation, NodeID: "R_large_first", OwnerLogin: "acme", Name: "large-first", FullName: "acme/large-first", Visibility: "private", DefaultBranch: "main"}, byInstallation[secondInstallation], "10000000-0000-0000-0000-000000000743"},
		}
		for _, attempt := range attempts {
			go func(attempt struct {
				installation GitHubAuthoritativeInstallation
				repository   GitHubRepository
				delivery     GitHubWebhookDelivery
				auditID      string
			}) {
				<-start
				_, err := store.ReconcileGitHubEvent(ctx, ReconcileGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: attempt.delivery.ID, DeliveryLease: *attempt.delivery.Lease, Installation: attempt.installation, Repositories: []GitHubRepository{attempt.repository}, Audit: &AuditEvent{ID: attempt.auditID}})
				results <- err
			}(attempt)
		}
		close(start)
		for range attempts {
			if err := <-results; err != nil {
				t.Fatalf("crossed large installation transfer: %v", err)
			}
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("crossed large installation transfer timed out: %v", err)
		}
		var firstDestination, secondDestination int64
		if err := pool.QueryRow(t.Context(), `SELECT github_installation_id FROM github_repositories WHERE github_repository_id=$1`, firstRepository).Scan(&firstDestination); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT github_installation_id FROM github_repositories WHERE github_repository_id=$1`, secondRepository).Scan(&secondDestination); err != nil {
			t.Fatal(err)
		}
		if firstDestination != secondInstallation || secondDestination != firstInstallation {
			t.Fatalf("crossed large installation destinations = %d,%d", firstDestination, secondDestination)
		}
	})

	t.Run("production cardinality planner uses routing indexes", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO github_webhook_deliveries(delivery_id,event,installation_id,repository_id,repository_node_id,org_id,payload_digest,verified_payload,payload_nonce,key_version,signature_key_version,status,attempts,next_attempt_at,lease_owner,lease_epoch,lease_expires_at,result_code,terminal_at,retain_until)
			SELECT uuidv7(),'push',90001,$1,'R_node',$2,encode(sha256(g::text::bytea),'hex'),decode(repeat('00',18),'hex'),decode(repeat('00',12),'hex'),'k1','hmac-v1',
				CASE g%3 WHEN 0 THEN 'pending' WHEN 1 THEN 'leased' ELSE 'completed' END,1,
				CASE WHEN g%3=0 THEN clock_timestamp()-interval '1 second' ELSE clock_timestamp()+interval '1 day' END,
				CASE WHEN g%3=1 THEN 'seed' END,g,CASE WHEN g%3=1 THEN clock_timestamp()-interval '1 second' END,CASE WHEN g%3=2 THEN 'processed' END,
				CASE WHEN g%3=2 THEN clock_timestamp()-interval '2 days' END,CASE WHEN g%3=2 THEN clock_timestamp()-interval '1 day' END
			FROM generate_series(1,60000) g`, githubWebhookRepo, githubWebhookOrg); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation)
			SELECT 100000+g,$1,90001,'R_plan_'||g,'acme','plan-'||g,'acme/plan-'||g,'private','main','active',1,1 FROM generate_series(1,30000) g`, githubWebhookOrg); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO github_repository_routing_state(repository_id,org_id,repository_node_id,installation_id,repository_generation,lifecycle_generation) VALUES($1,$2,'R_node',90001,3,4) ON CONFLICT DO NOTHING`, githubWebhookRepo, githubWebhookOrg); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO github_ref_routing_state(repository_id,ref,routing_generation,lifecycle_generation,validated_sha)
			SELECT $1,'refs/heads/plan-'||g,g,4,repeat('a',40) FROM generate_series(1,60000) g`, githubWebhookRepo); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO github_routing_intents(delivery_id,org_id,repository_id,repository_node_id,installation_id,app_id,environment_id,ref,commit_sha,event_kind,routing_generation,repository_generation,ref_lifecycle_generation,config_generation,workflow_path,production_ref,status,attempts,next_attempt_at,lease_owner,lease_epoch,lease_expires_at)
			SELECT d.id,$1,$2,'R_node',90001,$3,NULL,'refs/heads/plan-'||n.n,repeat('a',40),'push',n.n,3,4,7,'.github/workflows/deploy.yml',false,
				CASE n.n%2 WHEN 0 THEN 'pending' ELSE 'leased' END,1,
				CASE WHEN n.n%2=0 THEN clock_timestamp()-interval '1 second' ELSE clock_timestamp()+interval '1 day' END,
				CASE WHEN n.n%2=1 THEN 'seed' END,n.n,CASE WHEN n.n%2=1 THEN clock_timestamp()-interval '1 second' END
			FROM generate_series(1,60000) n JOIN (SELECT id,row_number() OVER (ORDER BY id) n FROM github_webhook_deliveries LIMIT 60000) d ON d.n=n.n`, githubWebhookOrg, githubWebhookRepo, githubWebhookAppA); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"github_webhook_deliveries", "github_routing_intents", "github_repositories", "github_ref_routing_state"} {
			if _, err := pool.Exec(t.Context(), "ANALYZE "+table); err != nil {
				t.Fatal(err)
			}
		}
		assertPlanUsesIndex(t, pool, "delivery pending due", "github_webhook_deliveries_due", `SELECT id,next_attempt_at,received_at FROM github_webhook_deliveries WHERE status='pending' AND attempts<12 AND next_attempt_at<=clock_timestamp() ORDER BY next_attempt_at,received_at,id LIMIT 100`)
		assertPlanUsesIndex(t, pool, "delivery expired lease", "github_webhook_deliveries_expired_lease", `SELECT id,lease_expires_at,received_at FROM github_webhook_deliveries WHERE status='leased' AND attempts<12 AND lease_expires_at<=clock_timestamp() ORDER BY lease_expires_at,received_at,id LIMIT 100`)
		assertPlanUsesIndex(t, pool, "intent pending due", "github_routing_intents_due", `SELECT id,next_attempt_at,created_at FROM github_routing_intents WHERE status='pending' AND attempts<12 AND next_attempt_at<=clock_timestamp() ORDER BY next_attempt_at,created_at,id LIMIT 100`)
		assertPlanUsesIndex(t, pool, "intent expired lease", "github_routing_intents_expired_lease", `SELECT id,lease_expires_at,created_at FROM github_routing_intents WHERE status='leased' AND attempts<12 AND lease_expires_at<=clock_timestamp() ORDER BY lease_expires_at,created_at,id LIMIT 100`)
		assertPlanUsesIndex(t, pool, "terminal payload cleanup", "github_webhook_deliveries_terminal_retention", `SELECT id FROM github_webhook_deliveries WHERE status IN ('completed','failed') AND verified_payload IS NOT NULL AND retain_until<=clock_timestamp() ORDER BY retain_until,id LIMIT 100`)
		assertPlanUsesIndex(t, pool, "repository generation lookup", "github_repositories_org_id_node_unique", fmt.Sprintf(`SELECT node_id FROM github_repositories WHERE org_id='%s' AND github_repository_id=%d`, githubWebhookOrg, githubWebhookRepo))
		assertPlanUsesIndex(t, pool, "ref generation lookup", "github_ref_routing_state_pkey", fmt.Sprintf(`SELECT routing_generation,lifecycle_generation FROM github_ref_routing_state WHERE repository_id=%d AND ref='refs/heads/plan-40000'`, githubWebhookRepo))
	})

	t.Run("attempt cap and retry schedule", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		d := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000300", strings.Repeat("c", 64), `{}`))
		for attempt := 1; attempt <= 12; attempt++ {
			claimed, err := store.ClaimGitHubWebhookDeliveries(t.Context(), "cap-worker", time.Minute, 1)
			if err != nil || len(claimed) != 1 || claimed[0].ID != d.ID || claimed[0].Attempts != attempt {
				t.Fatalf("claim attempt %d = %+v, %v", attempt, claimed, err)
			}
			delay := time.Duration(attempt) * time.Minute
			if err := store.RetryGitHubWebhookDelivery(t.Context(), d.ID, *claimed[0].Lease, delay, 24*time.Hour, "github_unavailable", "retry"); err != nil {
				t.Fatal(err)
			}
			var status string
			var attempts int
			var persisted time.Time
			if err := pool.QueryRow(t.Context(), `SELECT status,attempts,next_attempt_at FROM github_webhook_deliveries WHERE id=$1`, d.ID).Scan(&status, &attempts, &persisted); err != nil {
				t.Fatal(err)
			}
			wantStatus := "pending"
			if attempt == 12 {
				wantStatus = "failed"
			} else if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, d.ID); err != nil {
				t.Fatal(err)
			}
			if status != wantStatus || attempts != attempt || persisted.Before(time.Now().Add(delay-time.Minute)) {
				t.Fatalf("attempt %d state = %s/%d/%s", attempt, status, attempts, persisted)
			}
		}
		claimed, err := store.ClaimGitHubWebhookDeliveries(t.Context(), "attempt-13", time.Minute, 1)
		if err != nil || len(claimed) != 0 {
			t.Fatalf("13th claim = %+v, %v", claimed, err)
		}
	})

	t.Run("attempt twelve uses configured payload retention and database clock", func(t *testing.T) {
		for index, retention := range []time.Duration{0, 7 * time.Hour, 30 * 24 * time.Hour} {
			clearGitHubWebhookWork(t, pool)
			d := receiveDelivery(t, store, webhookParams(fmt.Sprintf("20000000-0000-0000-0002-%012d", index), fmt.Sprintf("%064x", index+700), `{}`))
			if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET attempts=11 WHERE id=$1`, d.ID); err != nil {
				t.Fatal(err)
			}
			claimed := claimOne(t, store, "retention-worker", time.Minute)
			var before time.Time
			if err := pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := store.RetryGitHubWebhookDelivery(t.Context(), d.ID, *claimed.Lease, 0, retention, "github_unavailable", "retry"); err != nil {
				t.Fatal(err)
			}
			var terminal, retainUntil, after time.Time
			if err := pool.QueryRow(t.Context(), `SELECT terminal_at,retain_until,clock_timestamp() FROM github_webhook_deliveries WHERE id=$1`, d.ID).Scan(&terminal, &retainUntil, &after); err != nil {
				t.Fatal(err)
			}
			if terminal.Before(before) || terminal.After(after) || retainUntil.Before(terminal.Add(retention)) || retainUntil.After(after.Add(retention)) {
				t.Fatalf("retention %s produced terminal/retain %s/%s outside database clock bounds %s/%s", retention, terminal, retainUntil, before, after)
			}
		}
		if err := store.RetryGitHubWebhookDelivery(t.Context(), "unused", LeaseToken{}, 0, 30*24*time.Hour+time.Nanosecond, "internal", "invalid"); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("over-bound retention error = %v", err)
		}
	})

	t.Run("expired exhausted delivery and intent leases are swept exactly once", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		const retention = 7 * time.Hour
		delivery := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0003-000000000001", strings.Repeat("7", 64), `{"secret":"sweep"}`))
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET attempts=11 WHERE id=$1`, delivery.ID); err != nil {
			t.Fatal(err)
		}
		exhaustedDelivery := claimOne(t, store, "crashed-delivery", time.Minute)
		if exhaustedDelivery.Attempts != 12 {
			t.Fatalf("exhausted delivery attempt = %d", exhaustedDelivery.Attempts)
		}

		sha := strings.Repeat("a", 40)
		generation, err := store.AdvanceGitHubRoutingGeneration(t.Context(), githubWebhookOrg, githubWebhookRepo, "R_node", "refs/heads/main", 3, 4, &sha, true)
		if err != nil {
			t.Fatal(err)
		}
		intentDelivery := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0003-000000000002", strings.Repeat("8", 64), `{}`))
		intentID, created, err := store.PersistGitHubRoutingIntent(t.Context(), GitHubRoutingIntentParams{DeliveryID: intentDelivery.ID, OrgID: githubWebhookOrg, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", AppID: githubWebhookAppA, EnvironmentID: githubWebhookEnv, Ref: "refs/heads/main", CommitSHA: sha, EventKind: "push", RoutingGeneration: generation, RepositoryGeneration: 3, RefLifecycleGeneration: 4, ConfigGeneration: 7, WorkflowPath: ".github/workflows/deploy.yml", ProductionRef: true})
		if err != nil || !created {
			t.Fatalf("persist exhausted intent = %s/%t, %v", intentID, created, err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_intents SET attempts=11 WHERE id=$1`, intentID); err != nil {
			t.Fatal(err)
		}
		intents, err := store.ClaimGitHubRoutingIntents(t.Context(), "crashed-intent", time.Minute, 1)
		if err != nil || len(intents) != 1 || intents[0].ID != intentID || intents[0].Attempts != 12 {
			t.Fatalf("exhausted intent claim = %+v, %v", intents, err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET status='completed',result_code='processed',terminal_at=clock_timestamp(),retain_until=clock_timestamp()+interval '1 day' WHERE id=$1`, intentDelivery.ID); err != nil {
			t.Fatal(err)
		}

		unexpired := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0003-000000000003", strings.Repeat("9", 64), `{}`))
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET attempts=11 WHERE id=$1`, unexpired.ID); err != nil {
			t.Fatal(err)
		}
		unexpired = claimOne(t, store, "active-attempt-twelve", time.Hour)
		reclaimable := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0003-000000000004", strings.Repeat("a", 64), `{}`))
		reclaimable = claimOne(t, store, "expired-attempt-eleven", time.Minute)
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id IN ($1,$2)`, exhaustedDelivery.ID, reclaimable.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_intents SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, intentID); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		results := make(chan GitHubEventLeaseStatus, 2)
		errs := make(chan error, 2)
		for range 2 {
			go func() {
				<-start
				result, err := store.FailExpiredExhaustedGitHubEventLeases(t.Context(), retention, 10)
				results <- result
				errs <- err
			}()
		}
		close(start)
		var swept GitHubEventLeaseStatus
		for range 2 {
			sweptResult := <-results
			swept.Deliveries += sweptResult.Deliveries
			swept.Intents += sweptResult.Intents
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		if swept != (GitHubEventLeaseStatus{Deliveries: 1, Intents: 1}) {
			t.Fatalf("concurrent sweep total = %+v", swept)
		}
		if again, err := store.FailExpiredExhaustedGitHubEventLeases(t.Context(), retention, 10); err != nil || again != (GitHubEventLeaseStatus{}) {
			t.Fatalf("idempotent sweep = %+v, %v", again, err)
		}

		var status, code, detail string
		var attempts int
		var terminal, retainUntil, databaseNow time.Time
		var owner *string
		var expires *time.Time
		if err := pool.QueryRow(t.Context(), `SELECT status,attempts,error_code,error_detail,terminal_at,retain_until,lease_owner,lease_expires_at,clock_timestamp() FROM github_webhook_deliveries WHERE id=$1`, exhaustedDelivery.ID).Scan(&status, &attempts, &code, &detail, &terminal, &retainUntil, &owner, &expires, &databaseNow); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || attempts != 12 || code != "attempts_exhausted" || detail != "attempts_exhausted" || owner != nil || expires != nil || retainUntil.Before(terminal.Add(retention)) || retainUntil.After(databaseNow.Add(retention)) {
			t.Fatalf("swept delivery = %s/%d/%s/%s terminal=%s retain=%s owner=%v expires=%v", status, attempts, code, detail, terminal, retainUntil, owner, expires)
		}
		if err := pool.QueryRow(t.Context(), `SELECT status,attempts,error_code,error_detail,terminal_at,lease_owner,lease_expires_at FROM github_routing_intents WHERE id=$1`, intentID).Scan(&status, &attempts, &code, &detail, &terminal, &owner, &expires); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || attempts != 12 || code != "attempts_exhausted" || detail != "attempts_exhausted" || terminal.IsZero() || owner != nil || expires != nil {
			t.Fatalf("swept intent = %s/%d/%s/%s terminal=%s owner=%v expires=%v", status, attempts, code, detail, terminal, owner, expires)
		}
		if claimed, err := store.ClaimGitHubWebhookDeliveries(t.Context(), "attempt-13", time.Minute, 10); err != nil || len(claimed) != 1 || claimed[0].ID != reclaimable.ID || claimed[0].Attempts != 2 {
			t.Fatalf("post-sweep delivery claims = %+v, %v", claimed, err)
		}
		if claimed, err := store.ClaimGitHubRoutingIntents(t.Context(), "intent-attempt-13", time.Minute, 10); err != nil || len(claimed) != 0 {
			t.Fatalf("post-sweep intent claims = %+v, %v", claimed, err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT status,attempts FROM github_webhook_deliveries WHERE id=$1`, unexpired.ID).Scan(&status, &attempts); err != nil || status != "leased" || attempts != 12 {
			t.Fatalf("unexpired attempt twelve = %s/%d, %v", status, attempts, err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET retain_until=clock_timestamp()-interval '1 second' WHERE id=$1`, exhaustedDelivery.ID); err != nil {
			t.Fatal(err)
		}
		if redacted, err := store.RedactTerminalGitHubWebhookPayloads(t.Context(), 10); err != nil || redacted != 1 {
			t.Fatalf("swept payload redaction = %d, %v", redacted, err)
		}
		var payload []byte
		if err := pool.QueryRow(t.Context(), `SELECT verified_payload FROM github_webhook_deliveries WHERE id=$1`, exhaustedDelivery.ID).Scan(&payload); err != nil || payload != nil {
			t.Fatalf("redacted swept payload = %x, %v", payload, err)
		}
		if _, err := store.FailExpiredExhaustedGitHubEventLeases(t.Context(), 30*24*time.Hour+time.Nanosecond, 10); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid sweep retention error = %v", err)
		}
		if _, err := store.FailExpiredExhaustedGitHubEventLeases(t.Context(), time.Hour, 0); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid sweep batch error = %v", err)
		}
	})

	t.Run("periodic delivery renewal preserves epoch", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		d := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000400", strings.Repeat("d", 64), `{}`))
		old := claimOne(t, store, "old-worker", time.Second)
		renewed, err := store.RenewGitHubWebhookDelivery(t.Context(), d.ID, *old.Lease, 2*time.Second)
		if err != nil || renewed.Epoch != old.Lease.Epoch || !renewed.ExpiresAt.After(old.Lease.ExpiresAt) {
			t.Fatalf("renew = %+v, %v", renewed, err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, d.ID); err != nil {
			t.Fatal(err)
		}
		fresh := claimOne(t, store, "new-worker", time.Minute)
		if fresh.ID != d.ID || fresh.Lease.Epoch <= old.Lease.Epoch {
			t.Fatalf("reclaim = %+v; old = %+v", fresh, old)
		}
		oldToken := *old.Lease
		checks := []error{
			func() error {
				_, err := store.RenewGitHubWebhookDelivery(t.Context(), d.ID, oldToken, time.Minute)
				return err
			}(),
			store.RetryGitHubWebhookDelivery(t.Context(), d.ID, oldToken, 0, time.Hour, "internal", "old"),
			store.CompleteGitHubWebhookDelivery(t.Context(), d.ID, oldToken, "processed", time.Hour),
			store.FailGitHubWebhookDelivery(t.Context(), d.ID, oldToken, "internal", "old", time.Hour),
		}
		for i, err := range checks {
			if !errors.Is(err, ErrLeaseNotOwned) {
				t.Fatalf("old operation %d error = %v", i, err)
			}
		}
		if err := store.CompleteGitHubWebhookDelivery(t.Context(), d.ID, *fresh.Lease, "processed", time.Hour); err != nil {
			t.Fatalf("new owner complete: %v", err)
		}
	})

	t.Run("final delivery rotation immediately fences old token", func(t *testing.T) {
		for iteration := range 20 {
			clearGitHubWebhookWork(t, pool)
			delivery := receiveDelivery(t, store, webhookParams(fmt.Sprintf("20000000-0000-0000-0004-%012d", iteration), fmt.Sprintf("%064x", iteration+900), `{}`))
			claimed := claimOne(t, store, "final-delivery", time.Second)
			original := *claimed.Lease
			if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()+interval '20 milliseconds' WHERE id=$1`, delivery.ID); err != nil {
				t.Fatal(err)
			}
			original.ExpiresAt = databaseTime(t, pool, `SELECT lease_expires_at FROM github_webhook_deliveries WHERE id=$1`, delivery.ID)
			rotated, err := store.RotateGitHubWebhookDeliveryLease(t.Context(), delivery.ID, original, time.Second)
			if err != nil || rotated.Epoch != original.Epoch+1 || !rotated.ExpiresAt.After(original.ExpiresAt) {
				t.Fatalf("iteration %d final delivery rotation = %+v, %v", iteration, rotated, err)
			}
			if err := store.CompleteGitHubWebhookDelivery(t.Context(), delivery.ID, original, "processed", time.Hour); !errors.Is(err, ErrLeaseNotOwned) {
				t.Fatalf("iteration %d original delivery token error = %v", iteration, err)
			}
			if err := store.CompleteGitHubWebhookDelivery(t.Context(), delivery.ID, rotated, "processed", time.Hour); err != nil {
				t.Fatalf("iteration %d rotated delivery token: %v", iteration, err)
			}
		}
	})

	t.Run("final intent rotation immediately fences old token", func(t *testing.T) {
		for iteration := range 20 {
			clearGitHubWebhookWork(t, pool)
			delivery := receiveDelivery(t, store, webhookParams(fmt.Sprintf("20000000-0000-0000-0005-%012d", iteration), fmt.Sprintf("%064x", iteration+1000), `{}`))
			sha := strings.Repeat("a", 40)
			generation, err := store.AdvanceGitHubRoutingGeneration(t.Context(), githubWebhookOrg, githubWebhookRepo, "R_node", "refs/heads/main", 3, 4, &sha, true)
			if err != nil {
				t.Fatal(err)
			}
			intentID, created, err := store.PersistGitHubRoutingIntent(t.Context(), GitHubRoutingIntentParams{DeliveryID: delivery.ID, OrgID: githubWebhookOrg, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", AppID: githubWebhookAppA, EnvironmentID: githubWebhookEnv, Ref: "refs/heads/main", CommitSHA: sha, EventKind: "push", RoutingGeneration: generation, RepositoryGeneration: 3, RefLifecycleGeneration: 4, ConfigGeneration: 7, WorkflowPath: ".github/workflows/deploy.yml", ProductionRef: true})
			if err != nil || !created {
				t.Fatalf("iteration %d intent = %s/%t, %v", iteration, intentID, created, err)
			}
			intents, err := store.ClaimGitHubRoutingIntents(t.Context(), "final-intent", time.Second, 1)
			if err != nil || len(intents) != 1 || intents[0].ID != intentID {
				t.Fatalf("iteration %d intent claim = %+v, %v", iteration, intents, err)
			}
			original := *intents[0].Lease
			if _, err := pool.Exec(t.Context(), `UPDATE github_routing_intents SET lease_expires_at=clock_timestamp()+interval '20 milliseconds' WHERE id=$1`, intentID); err != nil {
				t.Fatal(err)
			}
			original.ExpiresAt = databaseTime(t, pool, `SELECT lease_expires_at FROM github_routing_intents WHERE id=$1`, intentID)
			rotated, err := store.RotateGitHubRoutingIntentLease(t.Context(), intentID, original, time.Second)
			if err != nil || rotated.Epoch != original.Epoch+1 || !rotated.ExpiresAt.After(original.ExpiresAt) {
				t.Fatalf("iteration %d final intent rotation = %+v, %v", iteration, rotated, err)
			}
			if err := store.RetryGitHubRoutingIntent(t.Context(), intentID, original, 0, "github_unavailable", "retry"); !errors.Is(err, ErrLeaseNotOwned) {
				t.Fatalf("iteration %d original intent token error = %v", iteration, err)
			}
			if err := store.RetryGitHubRoutingIntent(t.Context(), intentID, rotated, 0, "github_unavailable", "retry"); err != nil {
				t.Fatalf("iteration %d rotated intent token: %v", iteration, err)
			}
		}
	})

	t.Run("terminal redaction retains replay digest", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		p := webhookParams("20000000-0000-0000-0000-000000000500", strings.Repeat("e", 64), `{"secret":"payload"}`)
		d := receiveDelivery(t, store, p)
		claimed := claimOne(t, store, "redactor", time.Minute)
		if claimed.ID != d.ID {
			t.Fatalf("claimed %s, want %s", claimed.ID, d.ID)
		}
		if err := store.CompleteGitHubWebhookDelivery(t.Context(), d.ID, *claimed.Lease, "processed", 0); err != nil {
			t.Fatal(err)
		}
		redacted, err := store.RedactTerminalGitHubWebhookPayloads(t.Context(), 10)
		if err != nil || redacted != 1 {
			t.Fatalf("redact = %d, %v", redacted, err)
		}
		disposition, replay, err := store.ReceiveGitHubWebhook(t.Context(), p)
		if err != nil || disposition != GitHubWebhookReplayed || replay.Payload != nil || replay.PayloadDigest != p.PayloadDigest {
			t.Fatalf("redacted replay = %q %+v, %v", disposition, replay, err)
		}
		conflict := p
		conflict.PayloadDigest = strings.Repeat("f", 64)
		disposition, _, err = store.ReceiveGitHubWebhook(t.Context(), conflict)
		if err != nil || disposition != GitHubWebhookConflict {
			t.Fatalf("redacted conflict = %q, %v", disposition, err)
		}
	})

	t.Run("routing generations deterministic snapshots and sibling isolation", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		d := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000600", strings.Repeat("1", 64), `{}`))
		sha := strings.Repeat("a", 40)
		g1, err := store.AdvanceGitHubRoutingGeneration(t.Context(), githubWebhookOrg, githubWebhookRepo, "R_node", "refs/heads/main", 3, 4, &sha, true)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.AdvanceGitHubRoutingGeneration(t.Context(), githubWebhookOrg, githubWebhookRepo, "R_node", "refs/heads/main", 2, 3, &sha, true)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("stale generation error = %v", err)
		}
		g2, err := store.AdvanceGitHubRoutingGeneration(t.Context(), githubWebhookOrg, githubWebhookRepo, "R_node", "refs/heads/main", 3, 4, &sha, true)
		if err != nil || g2 != g1+1 {
			t.Fatalf("fresh generation=%d,%v", g2, err)
		}
		var repoGeneration, lifecycle int64
		if err := pool.QueryRow(t.Context(), `SELECT repository_generation,lifecycle_generation FROM github_repository_routing_state WHERE repository_id=$1`, githubWebhookRepo).Scan(&repoGeneration, &lifecycle); err != nil || repoGeneration != 3 || lifecycle != 4 {
			t.Fatalf("repository state = %d/%d, %v", repoGeneration, lifecycle, err)
		}
		base := GitHubRoutingIntentParams{DeliveryID: d.ID, OrgID: githubWebhookOrg, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", EnvironmentID: githubWebhookEnv, Ref: "refs/heads/main", CommitSHA: sha, EventKind: "push", RoutingGeneration: g2, RepositoryGeneration: 3, RefLifecycleGeneration: 4, ConfigGeneration: 7, WorkflowPath: ".github/workflows/deploy.yml", ProductionRef: true}
		base.AppID = githubWebhookAppA
		idA, created, err := store.PersistGitHubRoutingIntent(t.Context(), base)
		if err != nil || !created {
			t.Fatalf("first intent = %s/%t, %v", idA, created, err)
		}
		replayID, created, err := store.PersistGitHubRoutingIntent(t.Context(), base)
		if err != nil || created || replayID != idA {
			t.Fatalf("replayed intent = %s/%t, %v", replayID, created, err)
		}
		base.AppID, base.EnvironmentID = githubWebhookAppB, ""
		idB, created, err := store.PersistGitHubRoutingIntent(t.Context(), base)
		if err != nil || !created || idB == idA {
			t.Fatalf("sibling intent = %s/%t, %v", idB, created, err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE github_routing_intents SET status='failed',terminal_at=clock_timestamp(),error_code='internal' WHERE id=$1`, idA); err != nil {
			t.Fatal(err)
		}
		var status, node, workflow, commit string
		var generation, config int64
		if err := pool.QueryRow(t.Context(), `SELECT status,repository_node_id,workflow_path,commit_sha,repository_generation,config_generation FROM github_routing_intents WHERE id=$1`, idB).Scan(&status, &node, &workflow, &commit, &generation, &config); err != nil {
			t.Fatal(err)
		}
		if status != "pending" || node != "R_node" || workflow != base.WorkflowPath || commit != sha || generation != 3 || config != 7 {
			t.Fatalf("sibling snapshot changed: %s/%s/%s/%s/%d/%d", status, node, workflow, commit, generation, config)
		}
	})

	t.Run("transaction boundary rollback", func(t *testing.T) {
		p := webhookParams("20000000-0000-0000-0000-000000000700", strings.Repeat("2", 64), `{}`)
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `INSERT INTO github_webhook_deliveries(delivery_id,event,installation_id,repository_id,repository_node_id,org_id,payload_digest,verified_payload,payload_nonce,key_version,signature_key_version) VALUES($1,'push',90001,$2,'R_node',$3,$4,$5,decode(repeat('00',12),'hex'),'k1','hmac-v1')`, p.DeliveryID, githubWebhookRepo, githubWebhookOrg, p.PayloadDigest, append(p.Payload, make([]byte, 16)...)); err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback(t.Context())
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_webhook_deliveries WHERE delivery_id=$1`, p.DeliveryID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rolled-back receipt count = %d, %v", count, err)
		}
		bad := p
		bad.DeliveryID = "20000000-0000-0000-0000-000000000701"
		bad.PayloadDigest = "invalid"
		if _, _, err := store.ReceiveGitHubWebhook(t.Context(), bad); err == nil {
			t.Fatal("constraint failpoint unexpectedly committed")
		}
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_webhook_deliveries WHERE delivery_id=$1`, bad.DeliveryID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed transaction receipt count = %d, %v", count, err)
		}
	})
}

func databaseTime(t *testing.T, pool *pgxpool.Pool, query string, args ...any) time.Time {
	t.Helper()
	var value time.Time
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

type countingFailingCodec struct{ encrypts, decrypts int }

func (c *countingFailingCodec) Encrypt([]byte, GitHubWebhookPayloadContext) ([]byte, []byte, string, error) {
	c.encrypts++
	return nil, nil, "", ErrFailedPrecondition
}

func (c *countingFailingCodec) Decrypt([]byte, []byte, string, GitHubWebhookPayloadContext) ([]byte, error) {
	c.decrypts++
	return nil, ErrFailedPrecondition
}

func testGitHubWebhookMigrationLifecycle(t *testing.T) {
	url, schema, admin := newGitHubWebhookSchema(t)
	defer admin.Close(context.Background())
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	m := githubWebhookMigrator(t, url)
	if err := m.Migrate(9); err != nil {
		t.Fatalf("migrate baseline: %v", err)
	}
	if sourceErr, databaseErr := m.Close(); sourceErr != nil || databaseErr != nil {
		t.Fatalf("close baseline migrator: source=%v database=%v", sourceErr, databaseErr)
	}
	db, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	committedV10, err := os.ReadFile("testdata/000010_github_webhook_routing.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), string(committedV10)); err != nil {
		t.Fatalf("apply literal committed 000010: %v", err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE schema_migrations SET version=10,dirty=false WHERE version=9 AND dirty=false`); err != nil {
		t.Fatalf("record literal committed 000010: %v", err)
	}
	seedGitHubWebhookFixtureConn(t, db)
	var deliveryID string
	if err := db.QueryRow(t.Context(), `INSERT INTO github_webhook_deliveries(delivery_id,event,installation_id,repository_id,repository_node_id,org_id,payload_digest,verified_payload,payload_nonce,key_version,status,result_code,terminal_at,payload_redacted_at) VALUES('30000000-0000-0000-0000-000000000001','push',90001,$1,'R_node',$2,repeat('a',64),NULL,NULL,'k1','completed','processed',clock_timestamp(),clock_timestamp()) RETURNING id::text`, githubWebhookRepo, githubWebhookOrg).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `INSERT INTO github_repository_routing_state(repository_id,org_id,repository_node_id,repository_generation,lifecycle_generation) VALUES($1,$2,'R_node',1,1)`, githubWebhookRepo, githubWebhookOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `INSERT INTO github_ref_routing_state(repository_id,ref,routing_generation,lifecycle_generation,validated_sha) VALUES($1,'refs/heads/main',1,1,repeat('a',40))`, githubWebhookRepo); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `INSERT INTO github_routing_intents(delivery_id,org_id,repository_id,repository_node_id,app_id,environment_id,ref,commit_sha,event_kind,routing_generation,repository_generation,ref_lifecycle_generation,config_generation,workflow_path,production_ref,status,result_code,terminal_at) VALUES($1,$2,$3,'R_node',$4,$5,'refs/heads/main',repeat('a',40),'push',1,1,1,1,'.github/workflows/deploy.yml',true,'completed','processed',clock_timestamp())`, deliveryID, githubWebhookOrg, githubWebhookRepo, githubWebhookAppA, githubWebhookEnv); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), integrationConfig(url)); err != nil {
		t.Fatalf("original 000010 to current 000012 upgrade: %v", err)
	}
	m = githubWebhookMigrator(t, url)
	defer m.Close()
	var signatureKeyVersion string
	if err := db.QueryRow(t.Context(), `SELECT signature_key_version FROM github_webhook_deliveries WHERE id=$1`, deliveryID).Scan(&signatureKeyVersion); err != nil || signatureKeyVersion != "legacy" {
		t.Fatalf("000012 signature key version backfill = %q, %v", signatureKeyVersion, err)
	}
	for _, invalid := range []string{"", " ", "\t", strings.Repeat("v", 129)} {
		if _, err := db.Exec(t.Context(), `UPDATE github_webhook_deliveries SET signature_key_version=$2 WHERE id=$1`, deliveryID, invalid); err == nil {
			t.Fatalf("invalid signature key version %q succeeded", invalid)
		}
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("000012 down: %v", err)
	}
	var signatureColumn int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='github_webhook_deliveries' AND column_name='signature_key_version'`).Scan(&signatureColumn); err != nil || signatureColumn != 0 {
		t.Fatalf("000011 signature column count = %d, %v", signatureColumn, err)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("000012 re-up: %v", err)
	}
	if err := db.QueryRow(t.Context(), `SELECT signature_key_version FROM github_webhook_deliveries WHERE id=$1`, deliveryID).Scan(&signatureKeyVersion); err != nil || signatureKeyVersion != "legacy" {
		t.Fatalf("000012 re-up backfill = %q, %v", signatureKeyVersion, err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE github_webhook_deliveries SET signature_key_version='hmac-v1' WHERE id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	if err := m.Steps(-1); err == nil {
		t.Fatal("000012 down succeeded with non-legacy signature key metadata")
	}
	var guardedVersion uint
	var guardedDirty bool
	if err := db.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&guardedVersion, &guardedDirty); err != nil || guardedVersion != 12 || guardedDirty {
		t.Fatalf("000012 guarded down state = %d/%t, %v", guardedVersion, guardedDirty, err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE github_webhook_deliveries SET signature_key_version='legacy' WHERE id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	var stateInstallation, intentInstallation, ingestionSequence int64
	if err := db.QueryRow(t.Context(), `SELECT state.installation_id,intent.installation_id,delivery.ingestion_sequence FROM github_repository_routing_state state JOIN github_routing_intents intent ON intent.repository_id=state.repository_id JOIN github_webhook_deliveries delivery ON delivery.id=intent.delivery_id WHERE intent.id IS NOT NULL`).Scan(&stateInstallation, &intentInstallation, &ingestionSequence); err != nil {
		t.Fatal(err)
	}
	if stateInstallation != 90001 || intentInstallation != 90001 || ingestionSequence < 1 {
		t.Fatalf("000011 backfill = state installation %d, intent installation %d, sequence %d", stateInstallation, intentInstallation, ingestionSequence)
	}
	var refColumns, observationTable, installationFK, publicPayloadSelect int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='github_ref_routing_state' AND column_name IN ('last_delivery_id','last_observed_received_at','last_observation_sequence')`).Scan(&refColumns); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='github_ref_transition_observations'`).Scan(&observationTable); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.table_constraints WHERE constraint_schema=current_schema() AND constraint_name IN ('github_repository_routing_state_installation_fk','github_routing_intents_installation_fk')`).Scan(&installationFK); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(t.Context(), `SELECT has_column_privilege('public','github_webhook_deliveries','verified_payload','SELECT')::int`).Scan(&publicPayloadSelect); err != nil {
		t.Fatal(err)
	}
	if refColumns != 3 || observationTable != 1 || installationFK != 2 || publicPayloadSelect != 0 {
		t.Fatalf("000011 shape = ref columns %d, observations %d, installation FKs %d, public payload SELECT %d", refColumns, observationTable, installationFK, publicPayloadSelect)
	}
	for table, state := range map[string]string{"github_installations": "unverified_revocation", "github_repositories": "unverified_revocation", "git_connections": "unverified_revocation"} {
		if _, err := db.Exec(t.Context(), `UPDATE `+table+` SET access_state=$1`, state); err != nil {
			t.Fatalf("%s current access state rejected: %v", table, err)
		}
	}
	if _, err := db.Exec(t.Context(), `UPDATE github_installations SET access_state='active'; UPDATE github_repositories SET access_state='active'; UPDATE git_connections SET access_state='active'`); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	codec, err := NewAESGCMGitHubWebhookPayloadCodec("k1", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	store := New(pool).WithGitHubWebhookPayloadCodec(codec)
	installationID, repositoryID, nodeID := int64(90001), githubWebhookRepo, "R_node"
	identity, err := store.ResolveGitHubWebhookIdentity(t.Context(), &installationID, &repositoryID, &nodeID)
	if err != nil || identity.OrgID != githubWebhookOrg {
		t.Fatalf("current identity query after committed 000010 upgrade = %+v, %v", identity, err)
	}
	if _, _, err := store.PersistGitHubRoutingIntent(t.Context(), GitHubRoutingIntentParams{DeliveryID: deliveryID, OrgID: githubWebhookOrg, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", AppID: githubWebhookAppB, Ref: "refs/heads/main", CommitSHA: strings.Repeat("a", 40), EventKind: "push", RoutingGeneration: 1, RepositoryGeneration: 1, RefLifecycleGeneration: 1, ConfigGeneration: 1, WorkflowPath: ".github/workflows/deploy.yml", ProductionRef: true}); err != nil {
		t.Fatalf("current routing-intent query after original 000010 upgrade: %v", err)
	}
	current := receiveDelivery(t, store, webhookParams("30000000-0000-0000-0000-000000000002", strings.Repeat("b", 64), `{}`))
	claimed := claimOne(t, store, "migration-worker", time.Minute)
	if claimed.ID != current.ID || claimed.IngestionSequence <= ingestionSequence {
		t.Fatalf("current delivery claim after committed 000010 upgrade = %+v", claimed)
	}
	snapshot, err := store.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: current.ID, DeliveryLease: *claimed.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/main", BeforeSHA: strings.Repeat("a", 40), CommitSHA: strings.Repeat("b", 40), AuthoritativeSHA: strings.Repeat("b", 40), EventKind: "push", RefExists: true, ObservationSequence: claimed.IngestionSequence, MaxIntents: 10, Audit: &AuditEvent{ID: "10000000-0000-0000-0000-000000000099"}})
	if err != nil || snapshot.RoutingGeneration != 2 || snapshot.IntentCount != 1 {
		t.Fatalf("current snapshot after committed 000010 upgrade = %+v, %v", snapshot, err)
	}
	claimedIntents, err := store.ClaimGitHubRoutingIntents(t.Context(), "migration-router", time.Minute, 10)
	if err != nil || len(claimedIntents) != 2 {
		t.Fatalf("current routing claims after committed 000010 upgrade = %+v, %v", claimedIntents, err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE github_webhook_deliveries SET signature_key_version='legacy'`); err != nil {
		t.Fatal(err)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("safe 000012 down before 000011 lifecycle: %v", err)
	}
	assertDownRejected := func(name string) {
		t.Helper()
		if err := m.Steps(-1); err == nil {
			t.Fatalf("unsafe 000011 down succeeded with %s", name)
		}
		var version uint
		var dirty bool
		if err := db.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 11 || dirty {
			t.Fatalf("migration state after rejected %s down = %d/%t, %v", name, version, dirty, err)
		}
	}
	assertDownRejected("new ref observation")
	if _, err := db.Exec(t.Context(), `DELETE FROM github_ref_transition_observations; UPDATE github_ref_routing_state SET last_delivery_id=NULL,last_observed_received_at=NULL,last_observation_sequence=NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE github_installations SET access_state='unverified_revocation'`); err != nil {
		t.Fatal(err)
	}
	assertDownRejected("unverified revocation state")
	if _, err := db.Exec(t.Context(), `UPDATE github_installations SET access_state='active'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE github_routing_intents SET status='completed',result_code='processed',terminal_at=COALESCE(terminal_at,clock_timestamp()),lease_owner=NULL,lease_expires_at=NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE github_webhook_deliveries SET status='pending',result_code=NULL,terminal_at=NULL,retain_until=NULL,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, current.ID); err != nil {
		t.Fatal(err)
	}
	assertDownRejected("pending delivery")
	if _, err := db.Exec(t.Context(), `UPDATE github_webhook_deliveries SET status='leased',lease_owner='down-test',lease_epoch=lease_epoch+1,lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, current.ID); err != nil {
		t.Fatal(err)
	}
	assertDownRejected("leased delivery")
	if _, err := db.Exec(t.Context(), `UPDATE github_webhook_deliveries SET status='completed',result_code='processed',terminal_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, current.ID); err != nil {
		t.Fatal(err)
	}
	assertDownRejected("retained terminal payload")
	if _, err := db.Exec(t.Context(), `UPDATE github_webhook_deliveries SET verified_payload=NULL,payload_nonce=NULL,payload_redacted_at=clock_timestamp(),retain_until=clock_timestamp(); UPDATE github_routing_intents SET status='pending',result_code=NULL,terminal_at=NULL`); err != nil {
		t.Fatal(err)
	}
	assertDownRejected("pending routing intent")
	if _, err := db.Exec(t.Context(), `UPDATE github_routing_intents SET status='leased',lease_owner='down-router',lease_epoch=lease_epoch+1,lease_expires_at=clock_timestamp()+interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	assertDownRejected("leased routing intent")
	if _, err := db.Exec(t.Context(), `UPDATE github_routing_intents SET status='completed',result_code='processed',terminal_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL`); err != nil {
		t.Fatal(err)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("safe 000011 down: %v", err)
	}
	var v10RefColumns, v10ObservationTable, v10InstallationColumns int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='github_ref_routing_state' AND column_name IN ('last_delivery_id','last_observed_received_at','last_observation_sequence')`).Scan(&v10RefColumns); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='github_ref_transition_observations'`).Scan(&v10ObservationTable); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND ((table_name='github_repository_routing_state' OR table_name='github_routing_intents') AND column_name='installation_id' OR table_name='github_webhook_deliveries' AND column_name='ingestion_sequence')`).Scan(&v10InstallationColumns); err != nil {
		t.Fatal(err)
	}
	if v10RefColumns != 0 || v10ObservationTable != 0 || v10InstallationColumns != 0 {
		t.Fatalf("000010 restored shape = ref columns %d, observations %d, v11 identity columns %d", v10RefColumns, v10ObservationTable, v10InstallationColumns)
	}
	if _, err := db.Exec(t.Context(), `UPDATE github_installations SET access_state='unverified_revocation'`); err == nil {
		t.Fatal("000010 access-state constraint accepted unverified_revocation")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("000011 re-up: %v", err)
	}
	var version uint
	var dirty bool
	if err := db.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 11 || dirty {
		t.Fatalf("re-up state = %d/%t, %v", version, dirty, err)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("000012 final re-up: %v", err)
	}
	if err := db.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 12 || dirty {
		t.Fatalf("final re-up state = %d/%t, %v", version, dirty, err)
	}
}

func newGitHubWebhookIntegrationStore(t *testing.T) (*Store, *pgxpool.Pool) {
	url, schema, admin := newGitHubWebhookSchema(t)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close(context.Background())
	})
	if err := Migrate(t.Context(), integrationConfig(url)); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	codec, err := NewAESGCMGitHubWebhookPayloadCodec("k1", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return New(pool).WithGitHubWebhookPayloadCodec(codec), pool
}

func newGitHubWebhookSchema(t *testing.T) (string, string, *pgx.Conn) {
	t.Helper()
	url := os.Getenv("CYCLOPUS_TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("POSTGRES_TEST_URL")
	}
	if url == "" {
		t.Fatal("CYCLOPUS_TEST_DATABASE_URL or POSTGRES_TEST_URL must point to PostgreSQL 18")
	}
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := admin.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::integer`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 180000 || version >= 190000 {
		t.Fatalf("PostgreSQL server_version_num=%d, want 18.x", version)
	}
	schema := fmt.Sprintf("github_webhooks_%d", time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	return withSearchPath(url, schema), schema, admin
}

func githubWebhookMigrator(t *testing.T, url string) *migrate.Migrate {
	t.Helper()
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatal(err)
	}
	source, err := iofs.New(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func seedGitHubWebhookFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedGitHubWebhookFixtureExec(t, func(query string, args ...any) error {
		_, err := pool.Exec(t.Context(), query, args...)
		return err
	})
}

func seedGitHubWebhookFixtureConn(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	seedGitHubWebhookFixtureExec(t, func(query string, args ...any) error {
		_, err := conn.Exec(t.Context(), query, args...)
		return err
	})
}

func seedGitHubWebhookFixtureExec(t *testing.T, exec func(string, ...any) error) {
	t.Helper()
	queries := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO orgs(id,name,slug) VALUES($1,'Webhook Org','webhook-org')`, []any{githubWebhookOrg}},
		{`INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(90001,$1,90002,'acme','Organization','selected','active',1,1)`, []any{githubWebhookOrg}},
		{`INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES($1,$2,90001,'R_node','acme','hooks','acme/hooks','private','main','active',1,1)`, []any{githubWebhookRepo, githubWebhookOrg}},
		{`INSERT INTO git_connections(id,org_id,github_repository_id,state,access_state) VALUES('10000000-0000-0000-0000-000000000009',$1,$2,'active','active')`, []any{githubWebhookOrg, githubWebhookRepo}},
		{`INSERT INTO projects(id,org_id,name,slug,git_connection_id) VALUES('10000000-0000-0000-0000-000000000010',$1,'Hooks','hooks','10000000-0000-0000-0000-000000000009')`, []any{githubWebhookOrg}},
		{`INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch) VALUES($1,$3,'10000000-0000-0000-0000-000000000010','A','a','a','ghcr.io/acme/a','.github/workflows/deploy.yml','main'),($2,$3,'10000000-0000-0000-0000-000000000010','B','b','b','ghcr.io/acme/b','.github/workflows/deploy.yml','main')`, []any{githubWebhookAppA, githubWebhookAppB, githubWebhookOrg}},
		{`INSERT INTO environments(id,org_id,app_id,branch,kind,namespace,planned_hostname) VALUES($1,$2,$3,'main','production','hooks-production','hooks.example.test')`, []any{githubWebhookEnv, githubWebhookOrg, githubWebhookAppA}},
	}
	for _, item := range queries {
		if err := exec(item.query, item.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func webhookParams(id, digest, payload string) ReceiveGitHubWebhookParams {
	repositoryID, repositoryNodeID := githubWebhookRepo, "R_node"
	installationID := int64(90001)
	return ReceiveGitHubWebhookParams{DeliveryID: id, Event: "push", Action: "", InstallationID: &installationID, RepositoryID: &repositoryID, RepositoryNodeID: &repositoryNodeID, OrgID: githubWebhookOrg, PayloadDigest: digest, Payload: []byte(payload), SignatureKeyVersion: "hmac-v1"}
}

func receiveDelivery(t *testing.T, store *Store, p ReceiveGitHubWebhookParams) GitHubWebhookDelivery {
	t.Helper()
	disposition, delivery, err := store.ReceiveGitHubWebhook(t.Context(), p)
	if err != nil || disposition != GitHubWebhookAccepted {
		t.Fatalf("receive %s = %q, %v", p.DeliveryID, disposition, err)
	}
	return delivery
}

func claimOne(t *testing.T, store *Store, owner string, duration time.Duration) GitHubWebhookDelivery {
	t.Helper()
	claimed, err := store.ClaimGitHubWebhookDeliveries(t.Context(), owner, duration, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim one for %s = %+v, %v", owner, claimed, err)
	}
	return claimed[0]
}

func clearGitHubWebhookWork(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `DELETE FROM github_routing_intents; DELETE FROM github_ref_transition_observations; DELETE FROM github_ref_routing_state; DELETE FROM github_repository_routing_state; DELETE FROM github_webhook_deliveries`); err != nil {
		t.Fatal(err)
	}
}

func assertPlanUsesIndex(t *testing.T, pool *pgxpool.Pool, name, index, query string) {
	t.Helper()
	rows, err := pool.Query(t.Context(), "EXPLAIN (COSTS OFF) "+query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(lines, "\n")
	if !strings.Contains(plan, index) || strings.Contains(plan, "Seq Scan") {
		t.Fatalf("%s plan did not exclusively use %s:\n%s", name, index, plan)
	}
}
