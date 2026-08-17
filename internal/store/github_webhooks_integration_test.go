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
		if err != nil || disposition != GitHubWebhookReplayed || replay.ID != oldDelivery.ID || replay.Payload != nil || replay.KeyVersion != "k1" {
			t.Fatalf("rotated replay = %q %+v, %v", disposition, replay, err)
		}
		newDelivery := receiveDelivery(t, store, webhookParams("20000000-0000-0000-0000-000000000092", strings.Repeat("7", 64), `{"key":"new"}`))
		if newDelivery.KeyVersion != "k2" {
			t.Fatalf("new delivery key version = %q", newDelivery.KeyVersion)
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

	t.Run("production cardinality planner uses routing indexes", func(t *testing.T) {
		clearGitHubWebhookWork(t, pool)
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO github_webhook_deliveries(delivery_id,event,installation_id,repository_id,repository_node_id,org_id,payload_digest,verified_payload,payload_nonce,key_version,status,attempts,next_attempt_at,lease_owner,lease_epoch,lease_expires_at,result_code,terminal_at,retain_until)
			SELECT uuidv7(),'push',90001,$1,'R_node',$2,encode(sha256(g::text::bytea),'hex'),decode(repeat('00',18),'hex'),decode(repeat('00',12),'hex'),'k1',
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
		if _, err := pool.Exec(t.Context(), `INSERT INTO github_ref_routing_state(repository_id,ref,routing_generation,lifecycle_generation,validated_sha)
			SELECT $1,'refs/heads/plan-'||g,g,4,repeat('a',40) FROM generate_series(1,60000) g`, githubWebhookRepo); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO github_routing_intents(delivery_id,org_id,repository_id,repository_node_id,app_id,environment_id,ref,commit_sha,event_kind,routing_generation,repository_generation,ref_lifecycle_generation,config_generation,workflow_path,production_ref,status,attempts,next_attempt_at,lease_owner,lease_epoch,lease_expires_at)
			SELECT d.id,$1,$2,'R_node',$3,NULL,'refs/heads/plan-'||n.n,repeat('a',40),'push',n.n,3,4,7,'.github/workflows/deploy.yml',false,
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
		assertPlanUsesIndex(t, pool, "repository generation lookup", "github_repositories_id_node_unique", fmt.Sprintf(`SELECT node_id FROM github_repositories WHERE org_id='%s' AND github_repository_id=%d`, githubWebhookOrg, githubWebhookRepo))
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
			if err := store.RetryGitHubWebhookDelivery(t.Context(), d.ID, *claimed[0].Lease, delay, "github_unavailable", "retry"); err != nil {
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

	t.Run("lease fencing reclaim and renewal", func(t *testing.T) {
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
			store.RetryGitHubWebhookDelivery(t.Context(), d.ID, oldToken, 0, "internal", "old"),
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
		if _, err := tx.Exec(t.Context(), `INSERT INTO github_webhook_deliveries(delivery_id,event,installation_id,repository_id,repository_node_id,org_id,payload_digest,verified_payload,payload_nonce,key_version) VALUES($1,'push',90001,$2,'R_node',$3,$4,$5,decode(repeat('00',12),'hex'),'k1')`, p.DeliveryID, githubWebhookRepo, githubWebhookOrg, p.PayloadDigest, append(p.Payload, make([]byte, 16)...)); err != nil {
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
	defer m.Close()
	if err := m.Migrate(9); err != nil {
		t.Fatalf("migrate baseline: %v", err)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("000010 up: %v", err)
	}
	db, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	seedGitHubWebhookFixtureConn(t, db)
	if _, err := db.Exec(t.Context(), `INSERT INTO github_webhook_deliveries(delivery_id,event,installation_id,repository_id,repository_node_id,org_id,payload_digest,verified_payload,payload_nonce,key_version) VALUES('30000000-0000-0000-0000-000000000001','push',90001,$1,'R_node',$2,repeat('a',64),decode(repeat('00',18),'hex'),decode(repeat('00',12),'hex'),'k1')`, githubWebhookRepo, githubWebhookOrg); err != nil {
		t.Fatal(err)
	}
	if err := m.Steps(-1); err == nil {
		t.Fatal("unsafe 000010 down succeeded with nonterminal retained payload")
	}
	if _, err := db.Exec(t.Context(), `UPDATE github_webhook_deliveries SET status='completed',result_code='processed',terminal_at=clock_timestamp(),retain_until=clock_timestamp(),verified_payload=NULL,payload_nonce=NULL,payload_redacted_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("safe 000010 down: %v", err)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("000010 re-up: %v", err)
	}
	var version uint
	var dirty bool
	if err := db.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 10 || dirty {
		t.Fatalf("re-up state = %d/%t, %v", version, dirty, err)
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
		{`INSERT INTO projects(id,org_id,name,slug) VALUES('10000000-0000-0000-0000-000000000010',$1,'Hooks','hooks')`, []any{githubWebhookOrg}},
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
	return ReceiveGitHubWebhookParams{DeliveryID: id, Event: "push", Action: "", InstallationID: &installationID, RepositoryID: &repositoryID, RepositoryNodeID: &repositoryNodeID, OrgID: githubWebhookOrg, PayloadDigest: digest, Payload: []byte(payload), KeyVersion: "k1"}
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
	if _, err := pool.Exec(t.Context(), `DELETE FROM github_routing_intents; DELETE FROM github_webhook_deliveries`); err != nil {
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
