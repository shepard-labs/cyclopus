//go:build integration

package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/config"
	"github.com/shepard-labs/cyclopus/internal/store"
)

func TestGitHubWebhookHandlerCommitsBefore202AndRejectsWithoutInsertPostgres18(t *testing.T) {
	dataStore, pool := newGitHubServerStore(t)
	actor := seedGitHubServerActor(t, pool)
	codec, err := store.NewAESGCMGitHubWebhookPayloadCodec("payload-k1", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	dataStore.WithGitHubWebhookPayloadCodec(codec)
	if _, err := dataStore.UpsertGitHubInstallation(t.Context(), actor.OrgID, store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"}, store.GitHubAuditContext{OrgID: actor.OrgID, Event: &store.AuditEvent{ID: "00000000-0000-7000-8000-000000000201"}, Source: "test"}, actor.OrgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(91,$1,42,'R_91','acme','api','acme/api','private','main','active',1,1)`, actor.OrgID); err != nil {
		t.Fatal(err)
	}
	secret := "integration-hmac-secret"
	handler := NewGitHubWebhookHandler(config.Webhook{GitHubActiveSecretRef: "active-ref", GitHubActiveKeyVersion: "hmac-v1", GitHubBodyLimit: 1024, GitHubMaxConcurrent: 1, GitHubBodyReadTimeout: time.Second}, integrationWebhookSecret{value: []byte(secret)}, dataStore)
	body := []byte(`{"installation":{"id":42},"repository":{"id":91,"node_id":"R_91"}}`)
	request := webhookRequest(body, "push", secret)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	requireEmptyWebhookResponse(t, response, http.StatusAccepted)
	var count int
	var deliveryID, keyVersion string
	if err := pool.QueryRow(t.Context(), `SELECT count(*),min(delivery_id::text),min(key_version) FROM github_webhook_deliveries`).Scan(&count, &deliveryID, &keyVersion); err != nil {
		t.Fatal(err)
	}
	if count != 1 || deliveryID != webhookDelivery || keyVersion != "payload-k1" {
		t.Fatalf("committed rows = %d delivery=%q key=%q", count, deliveryID, keyVersion)
	}
	rejected := webhookRequest([]byte(`{"installation":{"id":42},"repository":{"id":91,"node_id":"R_91"}}`), "push", secret)
	rejected.Header.Set("X-GitHub-Delivery", "20000000-0000-0000-0000-000000000002")
	rejected.Header.Set("X-GitHub-Signature-256", "sha256="+strings.Repeat("0", 64))
	rejected.Body = io.NopCloser(bytes.NewReader(body))
	rejectedResponse := httptest.NewRecorder()
	handler.ServeHTTP(rejectedResponse, rejected)
	requireEmptyWebhookResponse(t, rejectedResponse, http.StatusUnauthorized)
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_webhook_deliveries`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows after rejected request = %d, %v", count, err)
	}
}

func TestGitHubWebhookHandlerInstallationRepositoriesAndPingPostgres18(t *testing.T) {
	dataStore, pool := newGitHubServerStore(t)
	actor := seedGitHubServerActor(t, pool)
	codec, err := store.NewAESGCMGitHubWebhookPayloadCodec("payload-k1", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	dataStore.WithGitHubWebhookPayloadCodec(codec)
	if _, err := dataStore.UpsertGitHubInstallation(t.Context(), actor.OrgID, store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"}, store.GitHubAuditContext{OrgID: actor.OrgID, Event: &store.AuditEvent{ID: "00000000-0000-7000-8000-000000000202"}, Source: "test"}, actor.OrgID); err != nil {
		t.Fatal(err)
	}
	secret := "integration-hmac-secret"
	handler := NewGitHubWebhookHandler(config.Webhook{GitHubActiveSecretRef: "active-ref", GitHubActiveKeyVersion: "hmac-v1", GitHubBodyLimit: 2048, GitHubMaxConcurrent: 1, GitHubBodyReadTimeout: time.Second}, integrationWebhookSecret{value: []byte(secret)}, dataStore)
	body := []byte(`{"action":"added","installation":{"id":42},"repositories_added":[{"id":91,"node_id":"R_91"},{"id":92,"node_id":"R_92"}],"repositories_removed":[]}`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, webhookRequest(body, "installation_repositories", secret))
	requireEmptyWebhookResponse(t, response, http.StatusAccepted)
	var installationID int64
	var repositoryID *int64
	var repositoryNodeID *string
	if err := pool.QueryRow(t.Context(), `SELECT installation_id,repository_id,repository_node_id FROM github_webhook_deliveries`).Scan(&installationID, &repositoryID, &repositoryNodeID); err != nil || installationID != 42 || repositoryID != nil || repositoryNodeID != nil {
		t.Fatalf("installation-scope row = %d/%v/%v, %v", installationID, repositoryID, repositoryNodeID, err)
	}
	claimed, err := dataStore.ClaimGitHubWebhookDeliveries(t.Context(), "integration", time.Minute, 1)
	if err != nil || len(claimed) != 1 || !bytes.Equal(claimed[0].Payload, body) {
		t.Fatalf("claimed exact payload = %+v, %v", claimed, err)
	}

	ping := []byte(`{"zen":"stateless"}`)
	request := webhookRequest(ping, "ping", secret)
	request.Header.Set("X-GitHub-Delivery", "20000000-0000-0000-0000-000000000099")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	requireEmptyWebhookResponse(t, response, http.StatusAccepted)
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_webhook_deliveries`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows after ping = %d, %v", count, err)
	}
}

func TestGitHubWebhookHandlerReplaysAfterIdentityBecomesInactivePostgres18(t *testing.T) {
	for _, tc := range []struct {
		name, event, body, deactivate string
	}{
		{"deleted installation", "installation", `{"action":"deleted","installation":{"id":42}}`, `UPDATE github_installations SET access_state='deleted',deleted_at=clock_timestamp() WHERE github_installation_id=42`},
		{"suspended installation", "installation", `{"action":"suspend","installation":{"id":42}}`, `UPDATE github_installations SET access_state='suspended',suspended_at=clock_timestamp() WHERE github_installation_id=42`},
		{"inaccessible repository", "push", `{"installation":{"id":42},"repository":{"id":91,"node_id":"R_91"}}`, `UPDATE github_repositories SET access_state='inaccessible' WHERE github_repository_id=91`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataStore, pool := newGitHubServerStore(t)
			actor := seedGitHubServerActor(t, pool)
			codec, err := store.NewAESGCMGitHubWebhookPayloadCodec("payload-k1", []byte("0123456789abcdef0123456789abcdef"))
			if err != nil {
				t.Fatal(err)
			}
			dataStore.WithGitHubWebhookPayloadCodec(codec)
			if _, err := dataStore.UpsertGitHubInstallation(t.Context(), actor.OrgID, store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"}, store.GitHubAuditContext{OrgID: actor.OrgID, Event: &store.AuditEvent{ID: "00000000-0000-7000-8000-000000000203"}, Source: "test"}, actor.OrgID); err != nil {
				t.Fatal(err)
			}
			if tc.event == "push" {
				if _, err := pool.Exec(t.Context(), `INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(91,$1,42,'R_91','acme','api','acme/api','private','main','active',1,1)`, actor.OrgID); err != nil {
					t.Fatal(err)
				}
			}
			secret := "integration-hmac-secret"
			handler := NewGitHubWebhookHandler(config.Webhook{GitHubActiveSecretRef: "active-ref", GitHubActiveKeyVersion: "hmac-v1", GitHubBodyLimit: 1024, GitHubMaxConcurrent: 1, GitHubBodyReadTimeout: time.Second}, integrationWebhookSecret{value: []byte(secret)}, dataStore)
			body := []byte(tc.body)
			requireEmptyWebhookResponse(t, serveWebhook(handler, webhookRequest(body, tc.event, secret)), http.StatusAccepted)
			if _, err := pool.Exec(t.Context(), tc.deactivate); err != nil {
				t.Fatal(err)
			}
			requireEmptyWebhookResponse(t, serveWebhook(handler, webhookRequest(body, tc.event, secret)), http.StatusAccepted)
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_webhook_deliveries`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("delivery rows after inactive replay = %d, %v", count, err)
			}
		})
	}
}

func TestGitHubWebhookHandlerInactiveIdentityConflictAndUnknownPostgres18(t *testing.T) {
	dataStore, pool := newGitHubServerStore(t)
	actor := seedGitHubServerActor(t, pool)
	codec, err := store.NewAESGCMGitHubWebhookPayloadCodec("payload-k1", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	dataStore.WithGitHubWebhookPayloadCodec(codec)
	if _, err := dataStore.UpsertGitHubInstallation(t.Context(), actor.OrgID, store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"}, store.GitHubAuditContext{OrgID: actor.OrgID, Event: &store.AuditEvent{ID: "00000000-0000-7000-8000-000000000204"}, Source: "test"}, actor.OrgID); err != nil {
		t.Fatal(err)
	}
	secret := "integration-hmac-secret"
	handler := NewGitHubWebhookHandler(config.Webhook{GitHubActiveSecretRef: "active-ref", GitHubActiveKeyVersion: "hmac-v1", GitHubBodyLimit: 1024, GitHubMaxConcurrent: 1, GitHubBodyReadTimeout: time.Second}, integrationWebhookSecret{value: []byte(secret)}, dataStore)
	body := []byte(`{"action":"deleted","installation":{"id":42}}`)
	requireEmptyWebhookResponse(t, serveWebhook(handler, webhookRequest(body, "installation", secret)), http.StatusAccepted)
	if _, err := pool.Exec(t.Context(), `UPDATE github_installations SET access_state='deleted',deleted_at=clock_timestamp() WHERE github_installation_id=42`); err != nil {
		t.Fatal(err)
	}
	changed := []byte(`{"action":"deleted","installation":{"id":43}}`)
	requireEmptyWebhookResponse(t, serveWebhook(handler, webhookRequest(changed, "installation", secret)), http.StatusConflict)
	unknown := webhookRequest(body, "installation", secret)
	unknown.Header.Set("X-GitHub-Delivery", "20000000-0000-0000-0000-000000000002")
	requireEmptyWebhookResponse(t, serveWebhook(handler, unknown), http.StatusServiceUnavailable)
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_webhook_deliveries`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows after conflict and unknown inactive identity = %d, %v", count, err)
	}
}

type integrationWebhookSecret struct{ value []byte }

func (s integrationWebhookSecret) Resolve(context.Context, string) ([]byte, error) {
	return append([]byte(nil), s.value...), nil
}
