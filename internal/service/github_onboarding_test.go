package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v88/github"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	githubadapter "github.com/shepard-labs/cyclopus/internal/adapters/github"
	"github.com/shepard-labs/cyclopus/internal/secretref"
	"github.com/shepard-labs/cyclopus/internal/store"
)

func TestGitHubManifestUsesOnlyCanonicalExternalURLAndExactPrivileges(t *testing.T) {
	onboarding := &GitHubOnboarding{config: GitHubOnboardingConfig{ExternalURL: "https://cyclopus.example"}}
	encoded, err := onboarding.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["public"] != true || manifest["url"] != "https://cyclopus.example" || manifest["redirect_url"] != "https://cyclopus.example/github/app-manifest/callback" || manifest["setup_url"] != "https://cyclopus.example/github/app/setup" {
		t.Fatalf("manifest URLs/public = %+v", manifest)
	}
	hook := manifest["hook_attributes"].(map[string]any)
	if hook["url"] != "https://cyclopus.example/webhooks/github" || hook["active"] != true {
		t.Fatalf("hook attributes = %+v", hook)
	}
	wantPermissions := map[string]any{"actions": "write", "contents": "read", "metadata": "read"}
	permissions := manifest["default_permissions"].(map[string]any)
	if len(permissions) != len(wantPermissions) {
		t.Fatalf("permissions = %+v", permissions)
	}
	for key, want := range wantPermissions {
		if permissions[key] != want {
			t.Fatalf("permission %s = %v, want %v", key, permissions[key], want)
		}
	}
	wantEvents := []string{"push", "installation", "installation_repositories"}
	events := manifest["default_events"].([]any)
	if len(events) != len(wantEvents) {
		t.Fatalf("events = %+v", events)
	}
	for index, want := range wantEvents {
		if events[index] != want {
			t.Fatalf("event %d = %v, want %s", index, events[index], want)
		}
	}
	for _, forbidden := range []string{"callback_urls", "request_oauth_on_install", "oauth", "email"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("manifest unexpectedly contains OAuth identity scope %q: %s", forbidden, encoded)
		}
	}
}

func TestGitHubManifestExchangeSagaIntegration(t *testing.T) {
	dataStore, pool := newGitHubIntegrationStore(t)
	actor := seedGitHubActor(t, pool)
	private := privateKeyPEM(t)
	webhook := "01234567890123456789012345678901"
	secrets := &fakeManagedSecrets{}
	exchanges := 0
	var exchangeMu sync.Mutex
	onboarding := newTestOnboarding(t, dataStore, secrets,
		WithGitHubStateGenerator(func() (string, string, error) { state := strings.Repeat("a", 64); return state, digest(state), nil }),
		WithGitHubManifestExchanger(func(_ context.Context, code string) (*gh.AppConfig, error) {
			exchangeMu.Lock()
			defer exchangeMu.Unlock()
			exchanges++
			if code != "one-time-code" {
				t.Fatalf("exchange code = %q", code)
			}
			return manifestConfig(private, webhook), nil
		}))
	flow, err := onboarding.BeginAppSetup(t.Context(), actor, "setup-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := onboarding.ValidateStart(t.Context(), actor, flow.State, "app_setup"); err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string]ActorContext{
		"non admin":     {UserID: actor.UserID, OrgID: actor.OrgID, SessionID: actor.SessionID},
		"wrong user":    {UserID: "00000000-0000-0000-0000-000000000092", OrgID: actor.OrgID, SessionID: actor.SessionID, Operator: true},
		"wrong session": {UserID: actor.UserID, OrgID: actor.OrgID, SessionID: "other", Operator: true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := onboarding.HandleManifestCallback(t.Context(), invalid, flow.State, "one-time-code"); err == nil {
				t.Fatal("invalid callback succeeded")
			}
			if exchanges != 0 {
				t.Fatal("state failure reached exchange")
			}
		})
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- onboarding.HandleManifestCallback(context.Background(), actor, flow.State, "one-time-code")
		}()
	}
	close(start)
	succeeded, rejected := 0, 0
	for range 2 {
		if err := <-results; err == nil {
			succeeded++
		} else if errors.Is(err, ErrGitHubSetupState) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent callbacks = succeeded:%d rejected:%d", succeeded, rejected)
	}
	app, err := dataStore.GetGitHubApp(t.Context())
	if err != nil || app.State != "ready" || app.AppID == nil || *app.AppID != 42 || !app.HasCredential {
		t.Fatalf("app = %+v, %v", app, err)
	}
	if exchanges != 1 || secrets.writes != 1 {
		t.Fatalf("calls = exchange:%d writes:%d", exchanges, secrets.writes)
	}
	if err := onboarding.HandleManifestCallback(t.Context(), actor, flow.State, "one-time-code"); !errors.Is(err, ErrGitHubSetupState) {
		t.Fatalf("replay = %v", err)
	}
	if exchanges != 1 {
		t.Fatal("replay exchanged twice")
	}
	assertNoGitHubSecretsPersisted(t, pool, private, webhook)
}

func TestGitHubBeginSetupIdempotencyReplaysStableStateIntegration(t *testing.T) {
	dataStore, pool := newGitHubIntegrationStore(t)
	actor := seedGitHubActor(t, pool)
	onboarding := newTestOnboarding(t, dataStore, &fakeManagedSecrets{})
	first, err := onboarding.BeginAppSetup(t.Context(), actor, "stable-key")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := onboarding.BeginAppSetup(t.Context(), actor, "stable-key")
	if err != nil {
		t.Fatal(err)
	}
	if replay.StartURL != first.StartURL || !replay.ExpiresAt.Equal(first.ExpiresAt) || replay.State != first.State {
		t.Fatalf("replay = %+v, first = %+v", replay, first)
	}
	otherSession := actor
	otherSession.SessionID = "renewed-session"
	if _, err := onboarding.BeginAppSetup(t.Context(), otherSession, "stable-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("renewed session replay error = %v", err)
	}
	otherUser := actor
	otherUser.UserID = "00000000-0000-0000-0000-000000000092"
	if _, err := onboarding.BeginAppSetup(t.Context(), otherUser, "stable-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different admin replay error = %v", err)
	}
	var active int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_setup_sessions WHERE flow_kind='app_setup' AND status='active'`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active setup sessions = %d, %v", active, err)
	}
	var stateHash, generation, userID, sessionHash string
	if err := pool.QueryRow(t.Context(), `SELECT state_hash,state_generation,initiating_user_id::text,initiating_session_hash FROM github_setup_sessions WHERE flow_kind='app_setup' AND status='active'`).Scan(&stateHash, &generation, &userID, &sessionHash); err != nil {
		t.Fatal(err)
	}
	if stateHash != digest(first.State) || generation == "" || userID != actor.UserID || sessionHash != digest(actor.SessionID) {
		t.Fatalf("persisted state binding = hash:%q generation:%q user:%q session:%q", stateHash, generation, userID, sessionHash)
	}
	if first.State != onboarding.deriveState("app_setup", generation, actor.UserID, digest(actor.SessionID)) {
		t.Fatal("persisted state generation does not reproduce the returned HMAC state")
	}
	var requestBody string
	if err := pool.QueryRow(t.Context(), `SELECT response_body::text FROM api_idempotency_records WHERE org_id=$1 AND scope='github.app_setup.begin' AND idempotency_key='stable-key'`, actor.OrgID).Scan(&requestBody); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(requestBody, first.State) || strings.Contains(requestBody, stateHash) || !strings.Contains(requestBody, generation) {
		t.Fatalf("idempotency replay record = %s", requestBody)
	}
}

func TestGitHubBeginSetupAtomicFailureRetriesImmediatelyIntegration(t *testing.T) {
	dataStore, pool := newGitHubIntegrationStore(t)
	actor := seedGitHubActor(t, pool)
	onboarding := newTestOnboarding(t, dataStore, &fakeManagedSecrets{})
	onboarding.beforeStoreCommit = func() error { return errors.New("injected before commit") }
	if _, err := onboarding.BeginAppSetup(t.Context(), actor, "atomic-setup"); err == nil {
		t.Fatal("injected setup failure succeeded")
	}
	var sessions, audits, completed int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_setup_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='github_app.setup_started'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM api_idempotency_records WHERE scope='github.app_setup.begin' AND idempotency_key='atomic-setup' AND status='completed'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || audits != 0 || completed != 0 {
		t.Fatalf("rolled-back setup = sessions:%d audits:%d completed:%d", sessions, audits, completed)
	}
	onboarding.beforeStoreCommit = nil
	if _, err := onboarding.BeginAppSetup(t.Context(), actor, "atomic-setup"); err != nil {
		t.Fatalf("immediate retry: %v", err)
	}
	var owner string
	if err := pool.QueryRow(t.Context(), `SELECT COALESCE(lease_owner,'') FROM api_idempotency_records WHERE scope='github.app_setup.begin' AND idempotency_key='atomic-setup'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "" || owner == actor.UserID {
		t.Fatalf("completed lease owner = %q", owner)
	}
}

func TestGitHubBeginSetupConcurrentStartAndExpiredReplayIntegration(t *testing.T) {
	dataStore, pool := newGitHubIntegrationStore(t)
	actor := seedGitHubActor(t, pool)
	now := time.Now().UTC()
	onboarding := newTestOnboarding(t, dataStore, &fakeManagedSecrets{}, WithGitHubOnboardingClock(func() time.Time { return now }))
	start := make(chan struct{})
	type result struct {
		flow GitHubFlow
		err  error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			flow, err := onboarding.BeginAppSetup(context.Background(), actor, "concurrent-start")
			results <- result{flow: flow, err: err}
		}()
	}
	close(start)
	var successful []GitHubFlow
	inProgress := 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			successful = append(successful, result.flow)
		case errors.Is(result.err, ErrInProgress):
			inProgress++
		default:
			t.Fatal(result.err)
		}
	}
	if len(successful) == 0 || len(successful)+inProgress != 2 {
		t.Fatalf("concurrent starts = succeeded:%d in-progress:%d", len(successful), inProgress)
	}
	if len(successful) == 2 && (successful[0].State != successful[1].State || successful[0].StartURL != successful[1].StartURL || !successful[0].ExpiresAt.Equal(successful[1].ExpiresAt)) {
		t.Fatalf("concurrent successful starts differ: %+v %+v", successful[0], successful[1])
	}
	var active int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_setup_sessions WHERE flow_kind='app_setup' AND status='active'`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active setup sessions = %d, %v", active, err)
	}
	now = now.Add(setupTTL + time.Second)
	if _, err := onboarding.BeginAppSetup(t.Context(), actor, "concurrent-start"); !errors.Is(err, store.ErrFailedPrecondition) {
		t.Fatalf("expired idempotent replay error = %v", err)
	}
}

func TestGitHubBeginSetupReplayCheckpointSemanticsIntegration(t *testing.T) {
	for _, test := range []struct {
		checkpoint, status string
		wantReplay         bool
	}{
		{"created", "active", true},
		{"redirected", "active", true},
		{"installation_verified", "active", true},
		{"completed", "active", true},
		{"callback_validated", "consumed", false},
		{"completed", "failed", false},
		{"completed", "expired", false},
	} {
		t.Run(test.status+"_"+test.checkpoint, func(t *testing.T) {
			dataStore, pool := newGitHubIntegrationStore(t)
			actor := seedGitHubActor(t, pool)
			onboarding := newTestOnboarding(t, dataStore, &fakeManagedSecrets{})
			first, err := onboarding.BeginAppSetup(t.Context(), actor, "checkpoint-replay")
			if err != nil {
				t.Fatal(err)
			}
			consumedAt := "NULL"
			if test.status == "consumed" {
				consumedAt = "now()"
			}
			failure := "NULL"
			if test.status == "failed" {
				failure = "'test_failure'"
			}
			query := fmt.Sprintf(`UPDATE github_setup_sessions SET status=$1,checkpoint=$2,consumed_at=%s,failure_code=%s WHERE state_hash=$3`, consumedAt, failure)
			if _, err := pool.Exec(t.Context(), query, test.status, test.checkpoint, digest(first.State)); err != nil {
				t.Fatal(err)
			}
			replay, err := onboarding.BeginAppSetup(t.Context(), actor, "checkpoint-replay")
			if test.wantReplay {
				if err != nil || replay != first {
					t.Fatalf("replay = %+v, %v; want %+v", replay, err, first)
				}
			} else if !errors.Is(err, store.ErrFailedPrecondition) {
				t.Fatalf("terminal replay error = %v", err)
			}
		})
	}
}

func TestGitHubManifestExchangeHTTPContractAndFailuresIntegration(t *testing.T) {
	private := privateKeyPEM(t)
	webhook := "01234567890123456789012345678901"
	for name, response := range map[string]string{
		"malformed":  `{`,
		"error":      `{"message":"bad code"}`,
		"incomplete": `{"id":42,"slug":"cyclopus"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dataStore, pool := newGitHubIntegrationStore(t)
			actor := seedGitHubActor(t, pool)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPost || r.URL.Path != "/app-manifests/secret-code/conversions" {
					t.Fatalf("request = %s %s host=%s", r.Method, r.URL.Path, r.Host)
				}
				w.Header().Set("Content-Type", "application/json")
				if name == "error" {
					w.WriteHeader(http.StatusUnprocessableEntity)
				}
				_, _ = w.Write([]byte(response))
			}))
			defer server.Close()
			onboarding := newTestOnboardingAt(t, dataStore, &fakeManagedSecrets{}, server.URL+"/", server.Client())
			flow, err := onboarding.BeginAppSetup(t.Context(), actor, "setup-"+name)
			if err != nil {
				t.Fatal(err)
			}
			if err := onboarding.ValidateStart(t.Context(), actor, flow.State, "app_setup"); err != nil {
				t.Fatal(err)
			}
			err = onboarding.HandleManifestCallback(t.Context(), actor, flow.State, "secret-code")
			if err == nil || requests != 1 {
				t.Fatalf("callback = %v requests=%d", err, requests)
			}
			app, getErr := dataStore.GetGitHubApp(t.Context())
			if getErr != nil || (app.State != "failed" && app.State != "orphaned") {
				t.Fatalf("app = %+v, %v", app, getErr)
			}
			assertNoGitHubSecretsPersisted(t, pool, private, webhook, "secret-code")
		})
	}
}

func TestGitHubManifestExchangeRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", githubResponseLimit+1)))
	}))
	defer server.Close()
	onboarding := &GitHubOnboarding{config: GitHubOnboardingConfig{APIBaseURL: server.URL + "/", APIVersion: "2022-11-28"}, client: server.Client()}
	if _, err := onboarding.exchangeManifestCode(t.Context(), "code"); err == nil || strings.Contains(err.Error(), strings.Repeat("x", 32)) {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestGitHubAppIdentityVerificationMatchesPersistedMetadata(t *testing.T) {
	private := privateKeyPEM(t)
	appID, ownerID, ownerLogin, ownerType := int64(42), int64(7), "acme", "Organization"
	want := store.GitHubApp{AppID: &appID, OwnerID: &ownerID, Slug: "cyclopus", OwnerLogin: &ownerLogin, OwnerType: &ownerType}
	for _, test := range []struct {
		name                        string
		id, ownerID                 int64
		slug, ownerLogin, ownerType string
		status                      int
		wantErr                     bool
	}{
		{"success", 42, 7, "cyclopus", "acme", "Organization", http.StatusOK, false},
		{"wrong app", 43, 7, "cyclopus", "acme", "Organization", http.StatusOK, true},
		{"wrong slug", 42, 7, "other", "acme", "Organization", http.StatusOK, true},
		{"wrong owner", 42, 8, "cyclopus", "other", "User", http.StatusOK, true},
		{"unavailable", 0, 0, "", "", "", http.StatusServiceUnavailable, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/app" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
					t.Fatalf("request = %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
				}
				w.WriteHeader(test.status)
				if test.status == http.StatusOK {
					_ = json.NewEncoder(w).Encode(map[string]any{"id": test.id, "slug": test.slug, "owner": map[string]any{"id": test.ownerID, "login": test.ownerLogin, "type": test.ownerType}})
				}
			}))
			defer server.Close()
			onboarding := &GitHubOnboarding{config: GitHubOnboardingConfig{APIBaseURL: server.URL + "/", UploadURL: server.URL + "/", APIVersion: "2022-11-28", RequestTimeout: time.Second}, client: server.Client()}
			err := onboarding.verifyPersistedApp(t.Context(), githubadapter.AppCredential{AppID: 42, PrivateKey: private}, want)
			if (err != nil) != test.wantErr {
				t.Fatalf("verify error = %v", err)
			}
		})
	}
}

func TestGitHubManifestSecretAndActivationFailuresAreDurableIntegration(t *testing.T) {
	private := privateKeyPEM(t)
	webhook := "01234567890123456789012345678901"
	for name, prepare := range map[string]func(*fakeManagedSecrets, *pgxpool.Pool){
		"secret write": func(secrets *fakeManagedSecrets, _ *pgxpool.Pool) {
			secrets.writeErr = errors.New("secret backend unavailable")
		},
		"activation": func(_ *fakeManagedSecrets, pool *pgxpool.Pool) {
			_, _ = pool.Exec(t.Context(), `CREATE OR REPLACE FUNCTION block_github_ready() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='ready' THEN RAISE EXCEPTION 'blocked'; END IF; RETURN NEW; END $$; CREATE TRIGGER block_github_ready BEFORE UPDATE ON github_app FOR EACH ROW EXECUTE FUNCTION block_github_ready()`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dataStore, pool := newGitHubIntegrationStore(t)
			actor := seedGitHubActor(t, pool)
			secrets := &fakeManagedSecrets{}
			prepare(secrets, pool)
			onboarding := newTestOnboarding(t, dataStore, secrets, WithGitHubManifestExchanger(func(context.Context, string) (*gh.AppConfig, error) { return manifestConfig(private, webhook), nil }))
			flow, _ := onboarding.BeginAppSetup(t.Context(), actor, "setup-"+name)
			if err := onboarding.ValidateStart(t.Context(), actor, flow.State, "app_setup"); err != nil {
				t.Fatal(err)
			}
			if err := onboarding.HandleManifestCallback(t.Context(), actor, flow.State, "code"); err == nil {
				t.Fatal("failure path succeeded")
			}
			app, err := dataStore.GetGitHubApp(t.Context())
			if err != nil || app.State != "orphaned" || app.HasCredential || app.FailureCode == nil {
				t.Fatalf("app = %+v, %v", app, err)
			}
			assertNoGitHubSecretsPersisted(t, pool, private, webhook)
		})
	}
}

func TestGitHubRestartReconciliationMatrixIntegration(t *testing.T) {
	private := privateKeyPEM(t)
	webhook := "01234567890123456789012345678901"
	t.Run("no row and empty chart secret is unconfigured", func(t *testing.T) {
		dataStore, pool := newGitHubIntegrationStore(t)
		_ = seedGitHubActor(t, pool)
		onboarding := newTestOnboarding(t, dataStore, &fakeManagedSecrets{secret: secretref.ManagedSecret{Data: map[string][]byte{}}})
		if err := onboarding.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := dataStore.GetGitHubApp(t.Context()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("app error = %v", err)
		}
	})
	tests := []struct {
		name, state, checkpoint string
		secret                  secretref.ManagedSecret
		readErr                 error
		wantState               string
		verifyErr               error
	}{
		{"exchanging absent", "exchanging", "exchange_started", secretref.ManagedSecret{}, errors.New("not found"), "failed", nil},
		{"pending absent", "secret_pending", "metadata_received", secretref.ManagedSecret{}, errors.New("not found"), "orphaned", nil},
		{"pending empty", "secret_pending", "secret_write_started", secretref.ManagedSecret{Data: map[string][]byte{}}, nil, "orphaned", nil},
		{"pending valid", "secret_pending", "secret_write_started", secretref.ManagedSecret{Data: map[string][]byte{secretref.GitHubPrivateKeyKey: private, secretref.GitHubWebhookKey: []byte(webhook)}}, nil, "ready", nil},
		{"pending verifier fails", "secret_pending", "secret_written", secretref.ManagedSecret{Data: map[string][]byte{secretref.GitHubPrivateKeyKey: private, secretref.GitHubWebhookKey: []byte(webhook)}}, nil, "orphaned", errors.New("wrong app")},
		{"ready missing", "ready", "ready_committed", secretref.ManagedSecret{}, errors.New("not found"), "failed", nil},
		{"ready malformed", "ready", "ready_committed", secretref.ManagedSecret{Data: map[string][]byte{secretref.GitHubPrivateKeyKey: []byte("bad"), secretref.GitHubWebhookKey: []byte(webhook)}}, nil, "failed", errors.New("bad key")},
		{"orphan leftover", "orphaned", "orphan_recorded", secretref.ManagedSecret{Data: map[string][]byte{secretref.GitHubPrivateKeyKey: private, secretref.GitHubWebhookKey: []byte(webhook)}}, nil, "orphaned", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataStore, pool := newGitHubIntegrationStore(t)
			actor := seedGitHubActor(t, pool)
			session := seedGitHubAppState(t, dataStore, actor, test.state, test.checkpoint)
			secrets := &fakeManagedSecrets{secret: test.secret, readErr: test.readErr}
			onboarding := newTestOnboarding(t, dataStore, secrets, WithGitHubAppVerifier(func(context.Context, githubadapter.AppCredential) error { return test.verifyErr }))
			if err := onboarding.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			app, err := dataStore.GetGitHubApp(t.Context())
			if err != nil || app.State != test.wantState {
				t.Fatalf("app = %+v, %v session=%s", app, err, session)
			}
			if err := onboarding.Reconcile(t.Context()); err != nil {
				t.Fatalf("idempotent reconcile: %v", err)
			}
			if secrets.writes != 0 {
				t.Fatalf("reconcile unexpectedly wrote secret %d times", secrets.writes)
			}
			if test.wantState != test.state {
				var count int
				if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE after->>'source'='startup_reconciliation'`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count == 0 {
					t.Fatal("startup reconciliation transition was not audited")
				}
				if err := onboarding.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
				var repeated int
				if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE after->>'source'='startup_reconciliation'`).Scan(&repeated); err != nil {
					t.Fatal(err)
				}
				if repeated != count {
					t.Fatalf("reconcile audit count = %d, want %d", repeated, count)
				}
			}
		})
	}
}

func TestGitHubInstallationRefreshLifecycleIntegration(t *testing.T) {
	dataStore, pool := newGitHubIntegrationStore(t)
	actor := seedGitHubActor(t, pool)
	seedReadyGitHubApp(t, dataStore, actor)
	claimed, err := dataStore.UpsertGitHubInstallation(t.Context(), actor.OrgID, store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "all", AccessState: "active"}, testGitHubAudit(actor), actor.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		value     store.GitHubInstallation
		err       error
		wantState string
	}{
		{"active", store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"}, nil, "active"},
		{"suspended", store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "suspended", SuspendedAt: timePtr(time.Now())}, nil, "suspended"},
		{"restored", store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"}, nil, "active"},
		{"deleted", store.GitHubInstallation{GitHubInstallationID: 42, AccessState: "deleted"}, nil, "deleted"},
		{"wrong app", store.GitHubInstallation{}, ErrGitHubSetupState, "deleted"},
		{"malformed account", store.GitHubInstallation{}, ErrGitHubSetupState, "deleted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			onboarding := newTestOnboarding(t, dataStore, &fakeManagedSecrets{secret: validManagedSecret(t)}, WithGitHubInstallationVerifier(func(context.Context, githubadapter.AppCredential, int64) (store.GitHubInstallation, error) {
				return test.value, test.err
			}))
			got, err := onboarding.RefreshInstallation(t.Context(), actor, "refresh-"+test.name, 42)
			if test.err != nil {
				if !errors.Is(err, test.err) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || got.AccessState != test.wantState {
				t.Fatalf("installation = %+v, %v", got, err)
			}
		})
	}
	audits, err := dataStore.ListAuditEvents(t.Context(), actor.OrgID, store.AuditEventFilter{TargetType: "github_installation", TargetID: claimed.ID}, 20)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, audit := range audits {
		actions[audit.Action] = true
	}
	for _, action := range []string{"github_installation.claimed", "github_installation.refreshed", "github_installation.suspended", "github_installation.restored", "github_installation.deleted"} {
		if !actions[action] {
			t.Fatalf("installation audit lifecycle missing %q: %+v", action, audits)
		}
	}
}

func TestGitHubInstallationRefreshClaimsBeforeExternalWorkIntegration(t *testing.T) {
	dataStore, pool := newGitHubIntegrationStore(t)
	actor := seedGitHubActor(t, pool)
	seedReadyGitHubApp(t, dataStore, actor)
	if _, err := dataStore.UpsertGitHubInstallation(t.Context(), actor.OrgID, store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "all", AccessState: "active"}, testGitHubAudit(actor), actor.OrgID); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	verify := func(context.Context, githubadapter.AppCredential, int64) (store.GitHubInstallation, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"}, nil
	}
	onboarding := newTestOnboarding(t, dataStore, &fakeManagedSecrets{secret: validManagedSecret(t)}, WithGitHubInstallationVerifier(verify))
	first, err := onboarding.RefreshInstallation(t.Context(), actor, "refresh-fenced", 42)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := onboarding.RefreshInstallation(t.Context(), actor, "refresh-fenced", 42)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	if _, err := onboarding.RefreshInstallation(t.Context(), actor, "refresh-fenced", 43); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	mu.Lock()
	if calls != 1 {
		t.Fatalf("external verification calls = %d, want 1", calls)
	}
	mu.Unlock()
	entered, release := make(chan struct{}), make(chan struct{})
	concurrentCalls := 0
	concurrent := newTestOnboarding(t, dataStore, &fakeManagedSecrets{secret: validManagedSecret(t)}, WithGitHubInstallationVerifier(func(context.Context, githubadapter.AppCredential, int64) (store.GitHubInstallation, error) {
		concurrentCalls++
		close(entered)
		<-release
		return store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"}, nil
	}))
	firstDone := make(chan error, 1)
	go func() {
		_, err := concurrent.RefreshInstallation(context.Background(), actor, "refresh-concurrent", 42)
		firstDone <- err
	}()
	<-entered
	if _, err := concurrent.RefreshInstallation(t.Context(), actor, "refresh-concurrent", 42); !errors.Is(err, ErrInProgress) {
		t.Fatalf("concurrent refresh error = %v", err)
	}
	if concurrentCalls != 1 {
		t.Fatalf("concurrent external calls = %d, want 1", concurrentCalls)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}

	networkCalls := 0
	failing := newTestOnboarding(t, dataStore, &fakeManagedSecrets{secret: validManagedSecret(t)}, WithGitHubInstallationVerifier(func(context.Context, githubadapter.AppCredential, int64) (store.GitHubInstallation, error) {
		networkCalls++
		return store.GitHubInstallation{}, errors.New("network unavailable")
	}))
	if _, err := failing.RefreshInstallation(t.Context(), actor, "refresh-network", 42); err == nil {
		t.Fatal("network failure succeeded")
	}
	if _, err := failing.RefreshInstallation(t.Context(), actor, "refresh-network", 42); !errors.Is(err, store.ErrFailedPrecondition) {
		t.Fatalf("failed replay error = %v", err)
	}
	if networkCalls != 1 {
		t.Fatalf("failed replay external calls = %d, want 1", networkCalls)
	}

	request := struct {
		InstallationID int64 `json:"installation_id"`
	}{42}
	if _, err := dataStore.ClaimIdempotency(t.Context(), store.IdempotencyClaimParams{OrgID: actor.OrgID, Scope: "github.installation.refresh", Key: "refresh-expired-lease", RequestHash: requestHash(request), Owner: "stale-owner", LeaseDuration: time.Millisecond, Retention: time.Hour}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := onboarding.RefreshInstallation(t.Context(), actor, "refresh-expired-lease", 42); err != nil {
		t.Fatalf("expired lease takeover: %v", err)
	}
	mu.Lock()
	if calls != 2 {
		t.Fatalf("lease takeover external calls = %d, want 2", calls)
	}
	mu.Unlock()
}

func TestGitHubInstallationRefreshAtomicFailureIntegration(t *testing.T) {
	dataStore, pool := newGitHubIntegrationStore(t)
	actor := seedGitHubActor(t, pool)
	seedReadyGitHubApp(t, dataStore, actor)
	if _, err := dataStore.UpsertGitHubInstallation(t.Context(), actor.OrgID, store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "all", AccessState: "active"}, testGitHubAudit(actor), actor.OrgID); err != nil {
		t.Fatal(err)
	}
	onboarding := newTestOnboarding(t, dataStore, &fakeManagedSecrets{secret: validManagedSecret(t)}, WithGitHubInstallationVerifier(func(context.Context, githubadapter.AppCredential, int64) (store.GitHubInstallation, error) {
		return store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "suspended", SuspendedAt: timePtr(time.Now())}, nil
	}))
	onboarding.beforeStoreCommit = func() error { return errors.New("injected before commit") }
	if _, err := onboarding.RefreshInstallation(t.Context(), actor, "atomic-refresh", 42); err == nil {
		t.Fatal("injected refresh failure succeeded")
	}
	var state string
	if err := pool.QueryRow(t.Context(), `SELECT access_state FROM github_installations WHERE github_installation_id=42`).Scan(&state); err != nil || state != "active" {
		t.Fatalf("state = %q, %v", state, err)
	}
	var completed, audits int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM api_idempotency_records WHERE scope='github.installation.refresh' AND idempotency_key='atomic-refresh' AND status='completed'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='github_installation.suspended'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if completed != 0 || audits != 0 {
		t.Fatalf("rolled-back refresh = completed:%d audits:%d", completed, audits)
	}
	onboarding.beforeStoreCommit = nil
	if _, err := onboarding.RefreshInstallation(t.Context(), actor, "atomic-refresh", 42); err != nil {
		t.Fatalf("immediate retry: %v", err)
	}
}

type fakeManagedSecrets struct {
	mu                sync.Mutex
	secret            secretref.ManagedSecret
	readErr, writeErr error
	writes            int
}

func (f *fakeManagedSecrets) ReadGitHubApp(context.Context) (secretref.ManagedSecret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.secret, f.readErr
}
func (f *fakeManagedSecrets) WriteGitHubApp(_ context.Context, value secretref.ManagedSecret) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.writeErr != nil {
		return "", f.writeErr
	}
	value.ResourceVersion = "1"
	f.secret = value
	return "1", nil
}

func newTestOnboarding(t *testing.T, dataStore *store.Store, secrets secretref.ManagedWriter, options ...GitHubOnboardingOption) *GitHubOnboarding {
	return newTestOnboardingAt(t, dataStore, secrets, "https://api.github.test/", &http.Client{}, options...)
}
func newTestOnboardingAt(t *testing.T, dataStore *store.Store, secrets secretref.ManagedWriter, api string, client *http.Client, options ...GitHubOnboardingOption) *GitHubOnboarding {
	t.Helper()
	value, err := NewGitHubOnboarding(dataStore, secrets, GitHubOnboardingConfig{ExternalURL: "https://cyclopus.example", APIBaseURL: api, UploadURL: api, APIVersion: "2022-11-28", CredentialSecretRef: "k8s-secret://cyclopus/cyclopus-github-app#private-key.pem", RequestTimeout: time.Second, SealingKey: []byte(strings.Repeat("k", 32))}, client, options...)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func manifestConfig(private []byte, webhook string) *gh.AppConfig {
	return &gh.AppConfig{ID: gh.Ptr(int64(42)), Slug: gh.Ptr("cyclopus"), Name: gh.Ptr("Cyclopus"), HTMLURL: gh.Ptr("https://github.com/apps/cyclopus"), PEM: gh.Ptr(string(private)), WebhookSecret: gh.Ptr(webhook), Owner: &gh.User{ID: gh.Ptr(int64(7)), Login: gh.Ptr("acme"), Type: gh.Ptr("Organization")}}
}
func validManagedSecret(t *testing.T) secretref.ManagedSecret {
	return secretref.ManagedSecret{ResourceVersion: "1", Data: map[string][]byte{secretref.GitHubPrivateKeyKey: privateKeyPEM(t), secretref.GitHubWebhookKey: []byte("01234567890123456789012345678901")}}
}
func timePtr(value time.Time) *time.Time { return &value }

func newGitHubIntegrationStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("CYCLOPUS_TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("POSTGRES_TEST_URL")
	}
	if databaseURL == "" {
		t.Skip("PostgreSQL 18 integration URL is not set")
	}
	admin, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := admin.QueryRow(t.Context(), "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 180000 || version >= 190000 {
		t.Skipf("PostgreSQL 18 required, got %d", version)
	}
	schema := fmt.Sprintf("github_service_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_ = admin.Close(context.Background())
	})
	separator := "?"
	if strings.Contains(databaseURL, "?") {
		separator = "&"
	}
	testURL := databaseURL + separator + "search_path=" + url.QueryEscape(schema)
	if err := store.Migrate(t.Context(), store.MigrationConfig{URL: testURL, LockTimeout: 5 * time.Second, Migrations: store.Migrations}); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool), pool
}

func seedGitHubActor(t *testing.T, pool *pgxpool.Pool) ActorContext {
	t.Helper()
	orgID, userID := "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000091"
	if _, err := pool.Exec(t.Context(), `INSERT INTO orgs(id,name,slug) VALUES($1,'Acme','acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($2,$1,'admin@example.test','hash','Admin',true)`, orgID, userID); err != nil {
		t.Fatal(err)
	}
	return ActorContext{UserID: userID, OrgID: orgID, SessionID: "session-one", Operator: true}
}

func seedGitHubAppState(t *testing.T, dataStore *store.Store, actor ActorContext, state, checkpoint string) string {
	t.Helper()
	raw := strings.Repeat("b", 64)
	session, err := dataStore.BeginGitHubSetupSession(t.Context(), digest(raw), strings.Repeat("b", 64), "app_setup", actor.UserID, digest(actor.SessionID), nil, nil, time.Now().Add(10*time.Minute), testGitHubAudit(actor))
	if err != nil {
		t.Fatal(err)
	}
	audit := testGitHubAudit(actor)
	if err := dataStore.StartGitHubAppExchange(t.Context(), session.ID, audit); err != nil {
		t.Fatal(err)
	}
	if state != "exchanging" {
		if err := dataStore.RecordGitHubAppMetadata(t.Context(), session.ID, store.GitHubAppMetadata{AppID: 42, OwnerID: 7, Slug: "cyclopus", Name: "Cyclopus", HTMLURL: "https://github.com/apps/cyclopus", OwnerLogin: "acme", OwnerType: "Organization"}, testGitHubAudit(actor)); err != nil {
			t.Fatal(err)
		}
	}
	switch checkpoint {
	case "secret_write_started":
		if err := dataStore.MarkGitHubAppSecretWrite(t.Context(), session.ID, false, testGitHubAudit(actor)); err != nil {
			t.Fatal(err)
		}
	case "secret_written":
		if err := dataStore.MarkGitHubAppSecretWrite(t.Context(), session.ID, true, testGitHubAudit(actor)); err != nil {
			t.Fatal(err)
		}
	case "ready_committed":
		if err := dataStore.MarkGitHubAppSecretWrite(t.Context(), session.ID, true, testGitHubAudit(actor)); err != nil {
			t.Fatal(err)
		}
		if err := dataStore.ActivateGitHubApp(t.Context(), session.ID, "k8s-secret://cyclopus/cyclopus-github-app#private-key.pem", testGitHubAudit(actor)); err != nil {
			t.Fatal(err)
		}
	case "orphan_recorded":
		if err := dataStore.FailGitHubAppSetup(t.Context(), session.ID, "github_app_secret_write_failed", "cleanup", true, testGitHubAudit(actor)); err != nil {
			t.Fatal(err)
		}
	}
	return session.ID
}

func testGitHubAudit(actor ActorContext) store.GitHubAuditContext {
	return store.GitHubAuditContext{OrgID: actor.OrgID, Event: &store.AuditEvent{ID: newID(), ActorUserID: &actor.UserID}, Source: "test"}
}

func seedReadyGitHubApp(t *testing.T, dataStore *store.Store, actor ActorContext) {
	seedGitHubAppState(t, dataStore, actor, "ready", "ready_committed")
}

func privateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func assertNoGitHubSecretsPersisted(t *testing.T, pool *pgxpool.Pool, forbidden ...any) {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT row_to_json(github_app)::text FROM github_app UNION ALL SELECT row_to_json(github_setup_sessions)::text FROM github_setup_sessions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var dump strings.Builder
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		dump.WriteString(value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, value := range forbidden {
		var raw string
		switch v := value.(type) {
		case []byte:
			raw = string(v)
		case string:
			raw = v
		}
		if raw != "" && strings.Contains(dump.String(), raw) {
			t.Fatalf("database contains secret material")
		}
	}
}

func TestRandomStateHas256BitsAndPersistsOnlyDigestShape(t *testing.T) {
	stateOne, err := randomGeneration()
	if err != nil {
		t.Fatal(err)
	}
	stateTwo, err := randomGeneration()
	if err != nil {
		t.Fatal(err)
	}
	if len(stateOne) != 64 {
		t.Fatalf("generation shape = %d value=%q", len(stateOne), stateOne)
	}
	if stateOne == stateTwo {
		t.Fatal("independent setup states collided")
	}
	if setupTTL != 15*time.Minute {
		t.Fatalf("setup TTL = %s, want 15m", setupTTL)
	}
}

func TestGitHubStateRequiresDeploymentKey(t *testing.T) {
	actor := ActorContext{UserID: "user", SessionID: "session"}
	generation := strings.Repeat("a", 64)
	one := &GitHubOnboarding{config: GitHubOnboardingConfig{SealingKey: []byte(strings.Repeat("k", 32))}}
	two := &GitHubOnboarding{config: GitHubOnboardingConfig{SealingKey: []byte(strings.Repeat("z", 32))}}
	state := one.deriveState("app_setup", generation, actor.UserID, digest(actor.SessionID))
	if state == two.deriveState("app_setup", generation, actor.UserID, digest(actor.SessionID)) {
		t.Fatal("deployment key rotation did not invalidate GitHub state")
	}
	if state == digest(generation) || state == generation {
		t.Fatal("database generation alone derived GitHub state")
	}
}

func TestManifestMetadataValidatesIdentityAndExcludesSecrets(t *testing.T) {
	valid := &gh.AppConfig{ID: gh.Ptr(int64(42)), Slug: gh.Ptr("cyclopus"), Name: gh.Ptr("Cyclopus"), HTMLURL: gh.Ptr("https://github.com/apps/cyclopus"),
		PEM: gh.Ptr("PRIVATE"), WebhookSecret: gh.Ptr("WEBHOOK"), Owner: &gh.User{ID: gh.Ptr(int64(7)), Login: gh.Ptr("acme"), Type: gh.Ptr("Organization")}}
	metadata, err := manifestMetadata(valid)
	if err != nil || metadata.AppID != 42 || metadata.OwnerID != 7 || metadata.OwnerType != "Organization" {
		t.Fatalf("manifestMetadata() = %+v, %v", metadata, err)
	}
	encoded, _ := json.Marshal(metadata)
	if strings.Contains(string(encoded), "PRIVATE") || strings.Contains(string(encoded), "WEBHOOK") {
		t.Fatalf("metadata exposed secret bytes: %s", encoded)
	}
	for _, invalid := range []*gh.AppConfig{nil, {}, {ID: gh.Ptr(int64(42)), Slug: gh.Ptr("x"), PEM: gh.Ptr("p"), WebhookSecret: gh.Ptr("w"), Owner: &gh.User{ID: gh.Ptr(int64(7)), Type: gh.Ptr("Bot")}}} {
		if _, err := manifestMetadata(invalid); err == nil {
			t.Fatalf("manifestMetadata(%+v) succeeded", invalid)
		}
	}
}
