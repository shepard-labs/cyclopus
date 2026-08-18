//go:build integration

package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGitHubLifecyclePushReversedOrderMatrixPostgres18(t *testing.T) {
	for index, test := range []struct {
		name             string
		kind             string
		repositoryScoped bool
		wantInstallation string
		wantRepository   string
		wantConnection   string
	}{
		{name: "installation suspend", kind: "suspend", wantInstallation: "suspended", wantRepository: "active", wantConnection: "suspended"},
		{name: "installation delete", kind: "delete", wantInstallation: "deleted", wantRepository: "active", wantConnection: "deleted"},
		{name: "repository remove", kind: "remove", repositoryScoped: true, wantInstallation: "active", wantRepository: "inaccessible", wantConnection: "inaccessible"},
	} {
		for _, lifecycleFirst := range []bool{false, true} {
			order := "push_then_lifecycle"
			if lifecycleFirst {
				order = "lifecycle_then_push"
			}
			t.Run(test.name+"/"+order, func(t *testing.T) {
				dataStore, pool := newGitHubFanoutFixture(t)
				ref := fmt.Sprintf("refs/heads/matrix-%d-%t", index, lifecycleFirst)
				sha := strings.Repeat(fmt.Sprintf("%x", index+1), 40)
				sequence := index * 10
				push := func() (GitHubWebhookDelivery, error) {
					delivery := receiveDelivery(t, dataStore, webhookParams(fmt.Sprintf("6%07d-0000-0000-0000-%012d", sequence+1, sequence+1), fmt.Sprintf("%064x", sequence+1), `{}`))
					delivery = claimOne(t, dataStore, "matrix-push", time.Minute)
					_, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: ref, BeforeSHA: strings.Repeat("0", 40), CommitSHA: sha, AuthoritativeSHA: sha, EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2, Audit: matrixAudit(sequence + 1)})
					return delivery, err
				}
				revoke := func() {
					installationID := int64(90001)
					params := webhookParams(fmt.Sprintf("7%07d-0000-0000-0000-%012d", sequence+2, sequence+2), fmt.Sprintf("%064x", sequence+2), `{}`)
					params.Event, params.InstallationID, params.RepositoryID, params.RepositoryNodeID = "installation", &installationID, nil, nil
					if test.repositoryScoped {
						params.Event, params.RepositoryID, params.RepositoryNodeID = "installation_repositories", ptr64(githubWebhookRepo), ptrString("R_node")
					}
					receiveDelivery(t, dataStore, params)
					delivery := claimOne(t, dataStore, "matrix-lifecycle", time.Minute)
					revokeParams := RevokeGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, InstallationID: 90001, Kind: test.kind, Audit: matrixAudit(sequence + 2)}
					if test.repositoryScoped {
						revokeParams.RepositoryID, revokeParams.RepositoryNodeID = githubWebhookRepo, "R_node"
					}
					if err := dataStore.RevokeGitHubEvent(t.Context(), revokeParams); err != nil {
						t.Fatal(err)
					}
				}

				if lifecycleFirst {
					revoke()
					if _, err := push(); !errors.Is(err, ErrFailedPrecondition) {
						t.Fatalf("push after authoritative revocation error = %v", err)
					}
				} else {
					delivery, err := push()
					if err != nil {
						t.Fatal(err)
					}
					completeGitHubFanout(t, dataStore, "matrix-fanout", time.Minute, 2, matrixAudit(sequence+3))
					revoke()
					assertMatrixOldPushIntentsSuperseded(t, dataStore, delivery.ID)
				}
				assertGitHubAccessMatrixState(t, pool, test.wantInstallation, test.wantRepository, test.wantConnection)
			})
		}
	}
}

func TestGitHubLifecyclePushRecreationAndDelayedOldDeliveryPostgres18(t *testing.T) {
	dataStore, pool := newGitHubFanoutFixture(t)
	installationID := int64(90001)
	old := receiveDelivery(t, dataStore, webhookParams("68000000-0000-0000-0000-000000000001", strings.Repeat("1", 64), `{}`))
	old = claimOne(t, dataStore, "old-active-worker", time.Minute)

	lifecycleParams := webhookParams("68000000-0000-0000-0000-000000000002", strings.Repeat("2", 64), `{}`)
	lifecycleParams.Event, lifecycleParams.InstallationID, lifecycleParams.RepositoryID, lifecycleParams.RepositoryNodeID = "installation", &installationID, nil, nil
	receiveDelivery(t, dataStore, lifecycleParams)
	revocation := claimOne(t, dataStore, "delete-worker", time.Minute)
	if err := dataStore.RevokeGitHubEvent(t.Context(), RevokeGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: revocation.ID, DeliveryLease: *revocation.Lease, InstallationID: installationID, Kind: "delete", Audit: matrixAudit(101)}); err != nil {
		t.Fatal(err)
	}
	var deletedGeneration int64
	if err := pool.QueryRow(t.Context(), `SELECT reconciliation_generation FROM github_installations WHERE org_id=$1 AND github_installation_id=$2`, githubWebhookOrg, installationID).Scan(&deletedGeneration); err != nil {
		t.Fatal(err)
	}

	recreateParams := lifecycleParams
	recreateParams.DeliveryID, recreateParams.PayloadDigest = "68000000-0000-0000-0000-000000000003", strings.Repeat("3", 64)
	receiveDelivery(t, dataStore, recreateParams)
	recreation := claimOne(t, dataStore, "recreate-worker", time.Minute)
	if _, err := dataStore.ReconcileGitHubEvent(t.Context(), ReconcileGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: recreation.ID, DeliveryLease: *recreation.Lease, ExpectedGeneration: deletedGeneration,
		Installation: GitHubAuthoritativeInstallation{InstallationID: installationID, AccountID: 90002, AccountLogin: "acme", AccountType: "Organization", Selection: "selected"},
		Repositories: []GitHubRepository{{GitHubRepositoryID: githubWebhookRepo, GitHubInstallationID: installationID, NodeID: "R_node", OwnerLogin: "acme", Name: "hooks", FullName: "acme/hooks", Visibility: "private", DefaultBranch: "main"}}, Audit: matrixAudit(102)}); err != nil {
		t.Fatal(err)
	}
	assertGitHubAccessMatrixState(t, pool, "active", "active", "active")

	if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed := claimOne(t, dataStore, "replacement-worker", time.Minute)
	if reclaimed.ID != old.ID || reclaimed.Lease.Epoch != old.Lease.Epoch+1 {
		t.Fatalf("reclaimed old delivery = %+v, old = %+v", reclaimed, old)
	}
	if _, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: old.ID, DeliveryLease: *old.Lease, OrgID: githubWebhookOrg, InstallationID: installationID, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: "refs/heads/delayed-old", BeforeSHA: strings.Repeat("0", 40), CommitSHA: strings.Repeat("a", 40), AuthoritativeSHA: strings.Repeat("a", 40), EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2}); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("old reclaimed worker snapshot error = %v", err)
	}
	var staleManifests, staleIntents int
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM github_routing_fanout_manifests WHERE delivery_id=$1),
		(SELECT count(*) FROM github_routing_intents WHERE delivery_id=$1)`, old.ID).Scan(&staleManifests, &staleIntents); err != nil {
		t.Fatal(err)
	}
	if staleManifests != 0 || staleIntents != 0 {
		t.Fatalf("stale delivery side effects = manifests:%d intents:%d", staleManifests, staleIntents)
	}
}

func TestGitHubLifecyclePushAuthoritativeActivationMatrixPostgres18(t *testing.T) {
	for index, test := range []struct {
		name             string
		destructiveKind  string
		repositoryScoped bool
	}{
		{name: "installation unsuspend", destructiveKind: "suspend"},
		{name: "installation authoritative recreate", destructiveKind: "delete"},
		{name: "repository authoritative add", destructiveKind: "remove", repositoryScoped: true},
	} {
		for _, activationFirst := range []bool{false, true} {
			order := "push_then_activation"
			if activationFirst {
				order = "activation_then_push"
			}
			t.Run(test.name+"/"+order, func(t *testing.T) {
				dataStore, pool := newGitHubFanoutFixture(t)
				installationID := int64(90001)
				revokeParams := webhookParams(matrixUUID('a', index*10+1), fmt.Sprintf("%064x", index*10+1), `{}`)
				revokeParams.Event, revokeParams.InstallationID, revokeParams.RepositoryID, revokeParams.RepositoryNodeID = "installation", &installationID, nil, nil
				if test.repositoryScoped {
					revokeParams.Event, revokeParams.RepositoryID, revokeParams.RepositoryNodeID = "installation_repositories", ptr64(githubWebhookRepo), ptrString("R_node")
				}
				receiveDelivery(t, dataStore, revokeParams)
				revocation := claimOne(t, dataStore, "activation-revoke", time.Minute)
				revoke := RevokeGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: revocation.ID, DeliveryLease: *revocation.Lease, InstallationID: installationID, Kind: test.destructiveKind, Audit: matrixAudit(200 + index*10)}
				if test.repositoryScoped {
					revoke.RepositoryID, revoke.RepositoryNodeID = githubWebhookRepo, "R_node"
				}
				if err := dataStore.RevokeGitHubEvent(t.Context(), revoke); err != nil {
					t.Fatal(err)
				}
				var generation int64
				if err := pool.QueryRow(t.Context(), `SELECT reconciliation_generation FROM github_installations WHERE org_id=$1 AND github_installation_id=$2`, githubWebhookOrg, installationID).Scan(&generation); err != nil {
					t.Fatal(err)
				}

				activate := func() {
					params := webhookParams(matrixUUID('b', index*10+2), fmt.Sprintf("%064x", index*10+2), `{}`)
					params.Event, params.InstallationID, params.RepositoryID, params.RepositoryNodeID = "installation", &installationID, nil, nil
					receiveDelivery(t, dataStore, params)
					delivery := claimOne(t, dataStore, "activation-authority", time.Minute)
					if _, err := dataStore.ReconcileGitHubEvent(t.Context(), ReconcileGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, ExpectedGeneration: generation,
						Installation: GitHubAuthoritativeInstallation{InstallationID: installationID, AccountID: 90002, AccountLogin: "acme", AccountType: "Organization", Selection: "selected"},
						Repositories: []GitHubRepository{{GitHubRepositoryID: githubWebhookRepo, GitHubInstallationID: installationID, NodeID: "R_node", OwnerLogin: "acme", Name: "hooks", FullName: "acme/hooks", Visibility: "private", DefaultBranch: "main"}}, Audit: matrixAudit(201 + index*10)}); err != nil {
						t.Fatal(err)
					}
				}
				push := func(wantBlocked bool) {
					delivery := receiveDelivery(t, dataStore, webhookParams(matrixUUID('c', index*10+3), fmt.Sprintf("%064x", index*10+3), `{}`))
					delivery = claimOne(t, dataStore, "activation-push", time.Minute)
					sha := strings.Repeat(fmt.Sprintf("%x", index+4), 40)
					_, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: installationID, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: fmt.Sprintf("refs/heads/activation-%d-%t", index, activationFirst), BeforeSHA: strings.Repeat("0", 40), CommitSHA: sha, AuthoritativeSHA: sha, EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2, Audit: matrixAudit(202 + index*10)})
					if wantBlocked && !errors.Is(err, ErrFailedPrecondition) {
						t.Fatalf("push before activation error = %v", err)
					}
					if !wantBlocked && err != nil {
						t.Fatal(err)
					}
				}
				if activationFirst {
					activate()
					push(false)
				} else {
					push(true)
					activate()
				}
				assertGitHubAccessMatrixState(t, pool, "active", "active", "active")
			})
		}
	}
}

func TestGitHubBranchDeleteRecreateAndForcePushMatrixPostgres18(t *testing.T) {
	for index, test := range []struct {
		name   string
		events []struct {
			kind, before, after string
			exists              bool
		}
		want string
	}{
		{name: "push then branch delete", events: []struct {
			kind, before, after string
			exists              bool
		}{{"push", strings.Repeat("0", 40), strings.Repeat("a", 40), true}, {"delete", strings.Repeat("a", 40), "", false}}},
		{name: "branch delete then recreate", events: []struct {
			kind, before, after string
			exists              bool
		}{{"delete", strings.Repeat("0", 40), "", false}, {"push", strings.Repeat("0", 40), strings.Repeat("a", 40), true}}, want: strings.Repeat("a", 40)},
		{name: "force A to B", events: []struct {
			kind, before, after string
			exists              bool
		}{{"push", strings.Repeat("0", 40), strings.Repeat("a", 40), true}, {"push", strings.Repeat("a", 40), strings.Repeat("b", 40), true}}, want: strings.Repeat("b", 40)},
		{name: "force B to A", events: []struct {
			kind, before, after string
			exists              bool
		}{{"push", strings.Repeat("0", 40), strings.Repeat("b", 40), true}, {"push", strings.Repeat("b", 40), strings.Repeat("a", 40), true}}, want: strings.Repeat("a", 40)},
		{name: "recurring A to B", events: []struct {
			kind, before, after string
			exists              bool
		}{{"push", strings.Repeat("0", 40), strings.Repeat("a", 40), true}, {"push", strings.Repeat("a", 40), strings.Repeat("b", 40), true}, {"push", strings.Repeat("b", 40), strings.Repeat("a", 40), true}, {"push", strings.Repeat("a", 40), strings.Repeat("b", 40), true}}, want: strings.Repeat("b", 40)},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataStore, pool := newGitHubFanoutFixture(t)
			ref := fmt.Sprintf("refs/heads/master-force-%d", index)
			var last GitHubWebhookDelivery
			for eventIndex, event := range test.events {
				delivery := receiveDelivery(t, dataStore, webhookParams(matrixUUID('d', index*10+eventIndex+1), fmt.Sprintf("%064x", 500+index*10+eventIndex), `{}`))
				delivery = claimOne(t, dataStore, "force-matrix", time.Minute)
				result, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: 90001, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: ref, BeforeSHA: event.before, CommitSHA: event.after, AuthoritativeSHA: event.after, EventKind: event.kind, RefExists: event.exists, MaxFanout: 2, FanoutBatch: 2, Audit: matrixAudit(300 + index*10 + eventIndex)})
				if err != nil || result.RoutingGeneration != int64(eventIndex+1) {
					t.Fatalf("event %d result = %+v, %v", eventIndex, result, err)
				}
				last = delivery
			}
			var exists bool
			var sha *string
			var lastDelivery string
			if err := pool.QueryRow(t.Context(), `SELECT ref_exists,validated_sha,last_delivery_id::text FROM github_ref_routing_state WHERE repository_id=$1 AND ref=$2`, githubWebhookRepo, ref).Scan(&exists, &sha, &lastDelivery); err != nil {
				t.Fatal(err)
			}
			if test.want == "" {
				if exists || sha != nil {
					t.Fatalf("deleted ref = exists:%t sha:%v", exists, sha)
				}
			} else if !exists || sha == nil || *sha != test.want || lastDelivery != last.ID {
				t.Fatalf("final ref = exists:%t sha:%v delivery:%s, want %s/%s", exists, sha, lastDelivery, test.want, last.ID)
			}
		})
	}
}

func TestGitHubDelayedOldDestructiveEventsAreFencedAfterActivationPostgres18(t *testing.T) {
	for index, test := range []struct {
		name             string
		kind             string
		repositoryScoped bool
	}{
		{name: "suspend after unsuspend", kind: "suspend"},
		{name: "delete after recreate", kind: "delete"},
		{name: "remove after add", kind: "remove", repositoryScoped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataStore, pool := newGitHubFanoutFixture(t)
			installationID := int64(90001)
			params := webhookParams(matrixUUID('e', index+1), fmt.Sprintf("%064x", 700+index), `{}`)
			params.Event, params.InstallationID, params.RepositoryID, params.RepositoryNodeID = "installation", &installationID, nil, nil
			if test.repositoryScoped {
				params.Event, params.RepositoryID, params.RepositoryNodeID = "installation_repositories", ptr64(githubWebhookRepo), ptrString("R_node")
			}
			receiveDelivery(t, dataStore, params)
			old := claimOne(t, dataStore, "old-destructive", time.Minute)
			if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
				t.Fatal(err)
			}
			fresh := claimOne(t, dataStore, "replacement-destructive", time.Minute)
			if fresh.ID != old.ID || fresh.Lease.Epoch != old.Lease.Epoch+1 {
				t.Fatalf("replacement destructive delivery = %+v, old = %+v", fresh, old)
			}
			revoke := RevokeGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: old.ID, DeliveryLease: *old.Lease, InstallationID: installationID, Kind: test.kind, Audit: matrixAudit(400 + index)}
			if test.repositoryScoped {
				revoke.RepositoryID, revoke.RepositoryNodeID = githubWebhookRepo, "R_node"
			}
			if err := dataStore.RevokeGitHubEvent(t.Context(), revoke); !errors.Is(err, ErrLeaseNotOwned) {
				t.Fatalf("delayed destructive error = %v", err)
			}
			assertGitHubAccessMatrixState(t, pool, "active", "active", "active")
		})
	}
}

func TestGitHubPushTransferAuthorityMatrixPostgres18(t *testing.T) {
	for _, test := range []struct {
		name       string
		pushFirst  bool
		crossOrg   bool
		wantTarget int64
	}{
		{name: "push then same-org transfer", pushFirst: true, wantTarget: 90101},
		{name: "same-org transfer then push", wantTarget: 90101},
		{name: "push then rejected cross-org transfer", pushFirst: true, crossOrg: true, wantTarget: 90001},
		{name: "rejected cross-org transfer then push", crossOrg: true, wantTarget: 90001},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataStore, pool := newGitHubFanoutFixture(t)
			const destination = int64(90101)
			destinationOrg := githubWebhookOrg
			if test.crossOrg {
				if err := pool.QueryRow(t.Context(), `INSERT INTO orgs(name,slug) VALUES('Cross Org','cross-org-'||substr(uuidv7()::text,1,8)) RETURNING id::text`).Scan(&destinationOrg); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation,repository_reconciliation_generation)
				VALUES($1,$2,90102,'acme-destination','Organization','selected','active',1,1,1)`, destination, destinationOrg); err != nil {
				t.Fatal(err)
			}
			push := func() {
				installationID := int64(90001)
				if !test.crossOrg && !test.pushFirst {
					installationID = destination
				}
				delivery := receiveDelivery(t, dataStore, webhookParams(matrixUUID('f', matrixBoolInt(test.pushFirst)+10*matrixBoolInt(test.crossOrg)+1), fmt.Sprintf("%064x", 810+matrixBoolInt(test.pushFirst)+10*matrixBoolInt(test.crossOrg)), `{}`))
				delivery = claimOne(t, dataStore, "transfer-push", time.Minute)
				sha := strings.Repeat("9", 40)
				if _, err := dataStore.SnapshotGitHubPush(t.Context(), SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: githubWebhookOrg, InstallationID: installationID, RepositoryID: githubWebhookRepo, RepositoryNodeID: "R_node", Ref: fmt.Sprintf("refs/heads/transfer-%t-%t", test.pushFirst, test.crossOrg), BeforeSHA: strings.Repeat("0", 40), CommitSHA: sha, AuthoritativeSHA: sha, EventKind: "push", RefExists: true, MaxFanout: 2, FanoutBatch: 2, Audit: matrixAudit(500 + matrixBoolInt(test.pushFirst) + 10*matrixBoolInt(test.crossOrg))}); err != nil {
					t.Fatal(err)
				}
			}
			transfer := func() {
				if !test.crossOrg {
					if _, err := pool.Exec(t.Context(), `UPDATE github_installations SET access_state='suspended',suspended_at=clock_timestamp() WHERE github_installation_id=90001`); err != nil {
						t.Fatal(err)
					}
				}
				installationID := int64(90001)
				params := webhookParams(matrixUUID('9', matrixBoolInt(test.pushFirst)+10*matrixBoolInt(test.crossOrg)+1), fmt.Sprintf("%064x", 820+matrixBoolInt(test.pushFirst)+10*matrixBoolInt(test.crossOrg)), `{}`)
				params.Event, params.InstallationID, params.RepositoryID, params.RepositoryNodeID = "installation", &installationID, nil, nil
				receiveDelivery(t, dataStore, params)
				delivery := claimOne(t, dataStore, "transfer-authority", time.Minute)
				_, err := dataStore.ReconcileGitHubEvent(t.Context(), ReconcileGitHubEventParams{OrgID: githubWebhookOrg, DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease,
					Installation: GitHubAuthoritativeInstallation{InstallationID: destination, AccountID: 90102, AccountLogin: "acme-destination", AccountType: "Organization", Selection: "selected"},
					Repositories: []GitHubRepository{{GitHubRepositoryID: githubWebhookRepo, GitHubInstallationID: destination, NodeID: "R_node", OwnerLogin: "acme", Name: "hooks", FullName: "acme/hooks", Visibility: "private", DefaultBranch: "main"}}, Audit: matrixAudit(510 + matrixBoolInt(test.pushFirst) + 10*matrixBoolInt(test.crossOrg))})
				if test.crossOrg && err == nil {
					t.Fatalf("cross-org transfer error = %v", err)
				}
				if !test.crossOrg && err != nil {
					t.Fatal(err)
				}
			}
			if test.pushFirst {
				push()
				transfer()
			} else {
				transfer()
				push()
			}
			var installationID int64
			if err := pool.QueryRow(t.Context(), `SELECT github_installation_id FROM github_repositories WHERE org_id=$1 AND github_repository_id=$2`, githubWebhookOrg, githubWebhookRepo).Scan(&installationID); err != nil || installationID != test.wantTarget {
				t.Fatalf("repository installation = %d, want %d, %v", installationID, test.wantTarget, err)
			}
		})
	}
}

func assertMatrixOldPushIntentsSuperseded(t *testing.T, dataStore *Store, deliveryID string) {
	t.Helper()
	claimed := claimAllGitHubRoutingIntents(t, dataStore, "matrix-intent")
	matched := 0
	for _, intent := range claimed {
		if intent.DeliveryID != deliveryID {
			continue
		}
		result, err := dataStore.ExecuteGitHubRoutingIntent(t.Context(), intent.ID, *intent.Lease, []PreviewIdentity{{EnvironmentID: fmt.Sprintf("69000000-0000-0000-0000-%012d", matched+1), Slug: fmt.Sprintf("matrix-%d", matched+1), Namespace: fmt.Sprintf("matrix-%d", matched+1)}})
		if err != nil || result != "superseded" {
			t.Fatalf("old push intent result = %q, %v", result, err)
		}
		matched++
	}
	if matched != 2 {
		t.Fatalf("old push intent count = %d, want 2", matched)
	}
}

func assertGitHubAccessMatrixState(t *testing.T, pool *pgxpool.Pool, wantInstallation, wantRepository, wantConnection string) {
	t.Helper()
	var installation, repository, connection string
	if err := pool.QueryRow(t.Context(), `SELECT installation.access_state,repository.access_state,connection.access_state
		FROM github_installations installation JOIN github_repositories repository ON repository.org_id=installation.org_id AND repository.github_installation_id=installation.github_installation_id
		JOIN git_connections connection ON connection.org_id=repository.org_id AND connection.github_repository_id=repository.github_repository_id
		WHERE installation.org_id=$1 AND installation.github_installation_id=90001 AND repository.github_repository_id=$2`, githubWebhookOrg, githubWebhookRepo).Scan(&installation, &repository, &connection); err != nil {
		t.Fatal(err)
	}
	if installation != wantInstallation || repository != wantRepository || connection != wantConnection {
		t.Fatalf("access state = %s/%s/%s, want %s/%s/%s", installation, repository, connection, wantInstallation, wantRepository, wantConnection)
	}
}

func ptr64(value int64) *int64       { return &value }
func ptrString(value string) *string { return &value }
func matrixAudit(value int) *AuditEvent {
	return &AuditEvent{ID: fmt.Sprintf("6f000000-0000-0000-0000-%012d", value)}
}

func matrixUUID(prefix byte, value int) string {
	return fmt.Sprintf("%c0000000-0000-0000-0000-%012d", prefix, value)
}

func matrixBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
