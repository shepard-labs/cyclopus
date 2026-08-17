package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/config"
	"github.com/shepard-labs/cyclopus/internal/store"
)

const webhookDelivery = "20000000-0000-0000-0000-000000000001"

type fakeGitHubWebhookSecrets struct {
	values map[string][]byte
	err    error
	errors map[string]error
	calls  atomic.Int32
}

func (f *fakeGitHubWebhookSecrets) Resolve(_ context.Context, ref string) ([]byte, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	if err := f.errors[ref]; err != nil {
		return nil, err
	}
	return append([]byte(nil), f.values[ref]...), nil
}

type fakeGitHubWebhookStore struct {
	identity      store.GitHubWebhookIdentity
	identityErr   error
	receiveErr    error
	disposition   store.GitHubWebhookReceiveDisposition
	lookup        store.GitHubWebhookLookupDisposition
	lookupErr     error
	lookupCalls   atomic.Int32
	identityCalls atomic.Int32
	receiveCalls  atomic.Int32
	lookupParams  store.LookupGitHubWebhookParams
	params        store.ReceiveGitHubWebhookParams
	blockIdentity chan struct{}
}

func (f *fakeGitHubWebhookStore) LookupGitHubWebhook(_ context.Context, p store.LookupGitHubWebhookParams) (store.GitHubWebhookLookupDisposition, store.GitHubWebhookIdentity, error) {
	f.lookupCalls.Add(1)
	f.lookupParams = p
	return f.lookup, f.identity, f.lookupErr
}

func (f *fakeGitHubWebhookStore) ResolveGitHubWebhookIdentity(_ context.Context, installationID, repositoryID *int64, nodeID *string) (store.GitHubWebhookIdentity, error) {
	f.identityCalls.Add(1)
	if f.blockIdentity != nil {
		<-f.blockIdentity
	}
	if f.identityErr != nil {
		return store.GitHubWebhookIdentity{}, f.identityErr
	}
	value := f.identity
	value.InstallationID, value.RepositoryID, value.RepositoryNodeID = installationID, repositoryID, nodeID
	return value, nil
}

func (f *fakeGitHubWebhookStore) ReceiveGitHubWebhook(_ context.Context, p store.ReceiveGitHubWebhookParams) (store.GitHubWebhookReceiveDisposition, store.GitHubWebhookDelivery, error) {
	f.receiveCalls.Add(1)
	f.params = p
	return f.disposition, store.GitHubWebhookDelivery{}, f.receiveErr
}

func webhookFixture() (*githubWebhookHandler, *fakeGitHubWebhookSecrets, *fakeGitHubWebhookStore) {
	secrets := &fakeGitHubWebhookSecrets{values: map[string][]byte{"active-ref": []byte("active-secret"), "previous-ref": []byte("previous-secret")}}
	inbox := &fakeGitHubWebhookStore{identity: store.GitHubWebhookIdentity{OrgID: "10000000-0000-0000-0000-000000000001"}, disposition: store.GitHubWebhookAccepted, lookup: store.GitHubWebhookNotFound}
	handler := NewGitHubWebhookHandler(config.Webhook{GitHubActiveSecretRef: "active-ref", GitHubActiveKeyVersion: "active-v2", GitHubPreviousSecretRef: "previous-ref", GitHubPreviousKeyVersion: "previous-v1", GitHubSecretOverlapUntil: time.Now().Add(time.Hour), GitHubBodyLimit: 1024, GitHubMaxConcurrent: 1, GitHubBodyReadTimeout: time.Second}, secrets, inbox).(*githubWebhookHandler)
	return handler, secrets, inbox
}

func webhookRequest(body []byte, event, secret string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-GitHub-Delivery", webhookDelivery)
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-GitHub-Signature-256", webhookSignature(body, secret))
	return r
}

func webhookSignature(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func serveWebhook(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func requireEmptyWebhookResponse(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status || response.Body.Len() != 0 || response.Header().Get("Content-Length") != "0" {
		t.Fatalf("response = %d body=%q length=%q, want empty %d", response.Code, response.Body.String(), response.Header().Get("Content-Length"), status)
	}
}

func TestGitHubWebhookAcceptsActiveAndPreviousSignaturesAndPersistsExactBytes(t *testing.T) {
	body := []byte("{\n  \"installation\": {\"id\": 42}, \"repository\": {\"id\": 91, \"node_id\": \"R_91\"}\n}")
	for name, values := range map[string][2]string{"active": {"active-secret", "active-v2"}, "previous": {"previous-secret", "previous-v1"}} {
		t.Run(name, func(t *testing.T) {
			handler, _, inbox := webhookFixture()
			response := serveWebhook(handler, webhookRequest(body, "push", values[0]))
			requireEmptyWebhookResponse(t, response, http.StatusAccepted)
			digest := sha256.Sum256(body)
			if inbox.receiveCalls.Load() != 1 || !bytes.Equal(inbox.params.Payload, body) || inbox.params.PayloadDigest != hex.EncodeToString(digest[:]) || inbox.params.KeyVersion != values[1] || inbox.params.InstallationID == nil || *inbox.params.InstallationID != 42 || inbox.params.RepositoryID == nil || *inbox.params.RepositoryID != 91 || inbox.params.RepositoryNodeID == nil || *inbox.params.RepositoryNodeID != "R_91" {
				t.Fatalf("persisted params = %+v", inbox.params)
			}
			if strings.Contains(inbox.params.KeyVersion, "secret") || strings.Contains(inbox.params.KeyVersion, "ref") {
				t.Fatalf("persisted secret material as key version %q", inbox.params.KeyVersion)
			}
		})
	}
}

func TestGitHubWebhookResolvesPreviousSecretOnlyWhenRequired(t *testing.T) {
	body := []byte(`{"installation":{"id":42},"repository":{"id":91,"node_id":"R_91"}}`)

	t.Run("active valid ignores previous failure", func(t *testing.T) {
		handler, secrets, _ := webhookFixture()
		secrets.errors = map[string]error{"previous-ref": errors.New("previous secret unavailable")}
		requireEmptyWebhookResponse(t, serveWebhook(handler, webhookRequest(body, "push", "active-secret")), http.StatusAccepted)
		if secrets.calls.Load() != 1 {
			t.Fatalf("secret resolutions = %d, want active only", secrets.calls.Load())
		}
	})

	t.Run("active invalid requires previous during overlap", func(t *testing.T) {
		handler, secrets, inbox := webhookFixture()
		secrets.errors = map[string]error{"previous-ref": errors.New("previous secret unavailable")}
		requireEmptyWebhookResponse(t, serveWebhook(handler, webhookRequest(body, "push", "invalid-secret")), http.StatusServiceUnavailable)
		if secrets.calls.Load() != 2 || inbox.lookupCalls.Load() != 0 {
			t.Fatalf("secret/store calls = %d/%d, want 2/0", secrets.calls.Load(), inbox.lookupCalls.Load())
		}
	})

	t.Run("outside overlap does not resolve previous", func(t *testing.T) {
		handler, secrets, inbox := webhookFixture()
		handler.cfg.GitHubSecretOverlapUntil = handler.now().Add(-time.Second)
		secrets.errors = map[string]error{"previous-ref": errors.New("previous secret unavailable")}
		requireEmptyWebhookResponse(t, serveWebhook(handler, webhookRequest(body, "push", "invalid-secret")), http.StatusUnauthorized)
		if secrets.calls.Load() != 1 || inbox.lookupCalls.Load() != 0 {
			t.Fatalf("secret/store calls = %d/%d, want 1/0", secrets.calls.Load(), inbox.lookupCalls.Load())
		}
	})
}

func TestGitHubWebhookRejectsAmbiguousAndInvalidAuthenticationUniformly(t *testing.T) {
	body := []byte(`{}`)
	tests := map[string]func(*http.Request){
		"missing delivery":    func(r *http.Request) { r.Header.Del("X-GitHub-Delivery") },
		"duplicate delivery":  func(r *http.Request) { r.Header.Add("X-GitHub-Delivery", webhookDelivery) },
		"folded delivery":     func(r *http.Request) { r.Header.Set("X-GitHub-Delivery", webhookDelivery+","+webhookDelivery) },
		"missing event":       func(r *http.Request) { r.Header.Del("X-GitHub-Event") },
		"duplicate event":     func(r *http.Request) { r.Header.Add("X-GitHub-Event", "push") },
		"folded event":        func(r *http.Request) { r.Header.Set("X-GitHub-Event", "push,ping") },
		"missing signature":   func(r *http.Request) { r.Header.Del("X-GitHub-Signature-256") },
		"duplicate signature": func(r *http.Request) { r.Header.Add("X-GitHub-Signature-256", webhookSignature(body, "active-secret")) },
		"folded signature": func(r *http.Request) {
			r.Header.Set("X-GitHub-Signature-256", webhookSignature(body, "active-secret")+","+webhookSignature(body, "active-secret"))
		},
		"malformed scheme": func(r *http.Request) { r.Header.Set("X-GitHub-Signature-256", "sha1="+strings.Repeat("0", 64)) },
		"uppercase scheme": func(r *http.Request) { r.Header.Set("X-GitHub-Signature-256", "SHA256="+strings.Repeat("0", 64)) },
		"short hex":        func(r *http.Request) { r.Header.Set("X-GitHub-Signature-256", "sha256="+strings.Repeat("0", 62)) },
		"invalid hex":      func(r *http.Request) { r.Header.Set("X-GitHub-Signature-256", "sha256="+strings.Repeat("z", 64)) },
		"uppercase hex":    func(r *http.Request) { r.Header.Set("X-GitHub-Signature-256", "sha256="+strings.Repeat("A", 64)) },
		"mismatch":         func(r *http.Request) { r.Header.Set("X-GitHub-Signature-256", "sha256="+strings.Repeat("0", 64)) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			handler, secrets, inbox := webhookFixture()
			request := webhookRequest(body, "push", "active-secret")
			mutate(request)
			requireEmptyWebhookResponse(t, serveWebhook(handler, request), http.StatusUnauthorized)
			if inbox.identityCalls.Load() != 0 || inbox.receiveCalls.Load() != 0 || (name != "mismatch" && secrets.calls.Load() != 0) {
				t.Fatalf("calls secrets/identity/receive = %d/%d/%d", secrets.calls.Load(), inbox.identityCalls.Load(), inbox.receiveCalls.Load())
			}
		})
	}
}

func TestGitHubWebhookTransportAndMediaTypeBoundary(t *testing.T) {
	body := []byte(`{}`)
	handler, _, _ := webhookFixture()
	wrongMethod := webhookRequest(body, "ping", "active-secret")
	wrongMethod.Method = http.MethodGet
	response := serveWebhook(handler, wrongMethod)
	requireEmptyWebhookResponse(t, response, http.StatusMethodNotAllowed)
	if response.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("Allow = %q", response.Header().Get("Allow"))
	}
	for _, tc := range []struct {
		name, contentType, encoding string
		want                        int
	}{
		{"exact", "application/json", "", 202}, {"charset", "application/json; charset=UTF-8", "", 202},
		{"missing", "", "", 415}, {"wrong", "text/json", "", 415}, {"wrong charset", "application/json; charset=latin1", "", 415},
		{"extra parameter", "application/json; profile=x", "", 415}, {"gzip", "application/json", "gzip", 415},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := webhookFixture()
			r := webhookRequest(body, "ping", "active-secret")
			r.Header.Set("Content-Type", tc.contentType)
			if tc.encoding != "" {
				r.Header.Set("Content-Encoding", tc.encoding)
			}
			requireEmptyWebhookResponse(t, serveWebhook(h, r), tc.want)
		})
	}
	for _, name := range []string{"duplicate", "comma folded"} {
		t.Run(name+" content type", func(t *testing.T) {
			h, secrets, inbox := webhookFixture()
			r := webhookRequest(body, "ping", "active-secret")
			if name == "duplicate" {
				r.Header.Add("Content-Type", "application/json")
			} else {
				r.Header.Set("Content-Type", "application/json, application/json")
			}
			requireEmptyWebhookResponse(t, serveWebhook(h, r), http.StatusUnsupportedMediaType)
			if secrets.calls.Load() != 0 || inbox.identityCalls.Load() != 0 || inbox.receiveCalls.Load() != 0 {
				t.Fatal("ambiguous content type reached secret or store")
			}
		})
	}
}

func TestGitHubWebhookBodyLimitsLengthMismatchTimeoutAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		size, want int
	}{{"exact", 128, 202}, {"one over", 129, 413}} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := webhookFixture()
			h.cfg.GitHubBodyLimit = 128
			body := []byte(`{"padding":"` + strings.Repeat("x", tc.size-len(`{"padding":""}`)) + `"}`)
			r := webhookRequest(body, "ping", "active-secret")
			r.TransferEncoding = []string{"chunked"}
			r.ContentLength = -1
			requireEmptyWebhookResponse(t, serveWebhook(h, r), tc.want)
		})
	}
	t.Run("server framing rejects short content length", func(t *testing.T) { testWebhookContentLengthFraming(t, 100, `{}`) })
	t.Run("server framing bounds long content length", func(t *testing.T) { testWebhookContentLengthFraming(t, 1, `{}`) })

	t.Run("network read deadline releases concurrency slot", testWebhookSlowNetworkBody)

	h, _, inbox := webhookFixture()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	r := webhookRequest([]byte(`{}`), "ping", "active-secret").WithContext(canceled)
	requireEmptyWebhookResponse(t, serveWebhook(h, r), http.StatusServiceUnavailable)
	if inbox.identityCalls.Load() != 0 {
		t.Fatal("canceled request reached identity lookup")
	}
}

func testWebhookSlowNetworkBody(t *testing.T) {
	t.Helper()
	h, _, inbox := webhookFixture()
	h.cfg.GitHubBodyReadTimeout = 30 * time.Millisecond
	server := httptest.NewUnstartedServer(h)
	server.EnableHTTP2 = false
	server.Config.ReadTimeout = time.Second
	server.Start()
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, err = fmt.Fprintf(conn, "POST /webhooks/github HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nX-GitHub-Delivery: %s\r\nX-GitHub-Event: ping\r\nX-GitHub-Signature-256: sha256=%s\r\nContent-Length: 2\r\n\r\n{", webhookDelivery, strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	_, _ = conn.Write([]byte("}"))
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("slow body status = %d, want 503", response.StatusCode)
	}
	if inbox.identityCalls.Load() != 0 || inbox.receiveCalls.Load() != 0 {
		t.Fatal("slow body reached store")
	}
	requireEmptyWebhookResponse(t, serveWebhook(h, webhookRequest([]byte(`{}`), "ping", "active-secret")), http.StatusAccepted)
}

func testWebhookContentLengthFraming(t *testing.T, length int, body string) {
	t.Helper()
	h, _, _ := webhookFixture()
	server := httptest.NewUnstartedServer(h)
	server.EnableHTTP2 = false
	server.Start()
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	signature := webhookSignature([]byte(body), "active-secret")
	_, err = fmt.Fprintf(conn, "POST /webhooks/github HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nX-GitHub-Delivery: %s\r\nX-GitHub-Event: ping\r\nX-GitHub-Signature-256: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", webhookDelivery, signature, length, body)
	if err != nil {
		t.Fatal(err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusAccepted {
		t.Fatalf("mismatched Content-Length accepted: %d", response.StatusCode)
	}
}

func TestGitHubWebhookRejectsMalformedAndAmbiguousJSONBeforeDatabase(t *testing.T) {
	tests := map[string]string{
		"malformed": `{`, "array": `[]`, "scalar": `1`, "null": `null`, "trailing object": `{} {}`, "trailing token": `{} true`,
		"duplicate action":                            `{"action":"a","action":"b"}`,
		"duplicate nested id":                         `{"installation":{"id":1,"id":2},"action":"created"}`,
		"duplicate nested node":                       `{"installation":{"id":1},"repository":{"id":2,"node_id":"R1","node_id":"R2"}}`,
		"invalid action":                              `{"installation":{"id":1},"action":"UPPER"}`,
		"missing repository node":                     `{"installation":{"id":1},"repository":{"id":2}}`,
		"ping installation":                           `{"installation":{"id":1}}`,
		"ping repository":                             `{"repository":{"id":2,"node_id":"R2"}}`,
		"installation missing action":                 `{"installation":{"id":1}}`,
		"installation repository":                     `{"action":"created","installation":{"id":1},"repository":{"id":2,"node_id":"R2"}}`,
		"installation repositories wrong action":      `{"action":"created","installation":{"id":1},"repositories_added":[]}`,
		"push action":                                 `{"action":"created","installation":{"id":1},"repository":{"id":2,"node_id":"R2"}}`,
		"unsupported repository without installation": `{"repository":{"id":2,"node_id":"R2"}}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			h, _, inbox := webhookFixture()
			event := "push"
			switch name {
			case "ping installation", "ping repository":
				event = "ping"
			case "installation missing action", "installation repository":
				event = "installation"
			case "installation repositories wrong action":
				event = "installation_repositories"
			case "unsupported repository without installation":
				event = "issues"
			}
			requireEmptyWebhookResponse(t, serveWebhook(h, webhookRequest([]byte(raw), event, "active-secret")), http.StatusBadRequest)
			if inbox.lookupCalls.Load() != 0 || inbox.identityCalls.Load() != 0 || inbox.receiveCalls.Load() != 0 {
				t.Fatalf("database calls = %d/%d/%d", inbox.lookupCalls.Load(), inbox.identityCalls.Load(), inbox.receiveCalls.Load())
			}
		})
	}
}

func TestGitHubWebhookPreIdentityReplayLookup(t *testing.T) {
	body := []byte(`{"installation":{"id":42},"repository":{"id":91,"node_id":"R_91"}}`)
	for _, tc := range []struct {
		name   string
		lookup store.GitHubWebhookLookupDisposition
		err    error
		want   int
	}{
		{"inactive identity replay", store.GitHubWebhookReplay, nil, http.StatusAccepted},
		{"canonical metadata mismatch", store.GitHubWebhookMismatch, nil, http.StatusConflict},
		{"cross tenant metadata mismatch", store.GitHubWebhookMismatch, nil, http.StatusConflict},
		{"lookup failure", "", errors.New("database unavailable"), http.StatusServiceUnavailable},
		{"unknown lookup disposition", "unknown", nil, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, inbox := webhookFixture()
			inbox.lookup, inbox.lookupErr = tc.lookup, tc.err
			response := serveWebhook(h, webhookRequest(body, "push", "active-secret"))
			requireEmptyWebhookResponse(t, response, tc.want)
			if inbox.lookupCalls.Load() != 1 || inbox.identityCalls.Load() != 0 || inbox.receiveCalls.Load() != 0 {
				t.Fatalf("lookup/identity/receive calls = %d/%d/%d", inbox.lookupCalls.Load(), inbox.identityCalls.Load(), inbox.receiveCalls.Load())
			}
			digest := sha256.Sum256(body)
			if inbox.lookupParams.DeliveryID != webhookDelivery || inbox.lookupParams.Event != "push" || inbox.lookupParams.Action != "" || inbox.lookupParams.PayloadDigest != hex.EncodeToString(digest[:]) || inbox.lookupParams.InstallationID == nil || *inbox.lookupParams.InstallationID != 42 || inbox.lookupParams.RepositoryID == nil || *inbox.lookupParams.RepositoryID != 91 || inbox.lookupParams.RepositoryNodeID == nil || *inbox.lookupParams.RepositoryNodeID != "R_91" {
				t.Fatalf("lookup params = %+v", inbox.lookupParams)
			}
		})
	}
}

func TestGitHubWebhookDoesNotLookupBeforeAuthenticationAndJSONValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		edit func(*http.Request)
		want int
	}{
		{"body read", []byte(strings.Repeat("x", 1025)), nil, http.StatusRequestEntityTooLarge},
		{"headers", []byte(`{}`), func(r *http.Request) { r.Header.Del("X-GitHub-Signature-256") }, http.StatusUnauthorized},
		{"signature", []byte(`{}`), func(r *http.Request) { r.Header.Set("X-GitHub-Signature-256", "sha256="+strings.Repeat("0", 64)) }, http.StatusUnauthorized},
		{"json", []byte(`{"installation":`), nil, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, inbox := webhookFixture()
			r := webhookRequest(tc.body, "push", "active-secret")
			if tc.edit != nil {
				tc.edit(r)
			}
			requireEmptyWebhookResponse(t, serveWebhook(h, r), tc.want)
			if inbox.lookupCalls.Load() != 0 || inbox.identityCalls.Load() != 0 || inbox.receiveCalls.Load() != 0 {
				t.Fatalf("store calls = %d/%d/%d", inbox.lookupCalls.Load(), inbox.identityCalls.Load(), inbox.receiveCalls.Load())
			}
		})
	}
}

func TestGitHubWebhookCanonicalIdentityExtraction(t *testing.T) {
	for _, tc := range []struct {
		name, event, raw         string
		installation, repository int64
	}{
		{"installation", "installation", `{"action":"created","installation":{"id":42}}`, 42, 0},
		{"installation repositories", "installation_repositories", `{"action":"added","installation":{"id":42},"repositories_added":[{"id":91,"node_id":"R_91"},{"id":92,"node_id":"R_92"}],"repositories_removed":[]}`, 42, 0},
		{"push", "push", `{"installation":{"id":42},"repository":{"id":91,"node_id":"R_91"}}`, 42, 91},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, inbox := webhookFixture()
			requireEmptyWebhookResponse(t, serveWebhook(h, webhookRequest([]byte(tc.raw), tc.event, "active-secret")), http.StatusAccepted)
			if (tc.installation == 0) != (inbox.params.InstallationID == nil) || tc.installation != 0 && *inbox.params.InstallationID != tc.installation || (tc.repository == 0) != (inbox.params.RepositoryID == nil) || tc.repository != 0 && *inbox.params.RepositoryID != tc.repository {
				t.Fatalf("identity params = %+v", inbox.params)
			}
		})
	}
}

func TestGitHubWebhookInstallationRepositoriesValidation(t *testing.T) {
	tests := []struct {
		name, raw string
		want      int
	}{
		{"zero", `{"action":"added","installation":{"id":42},"repositories_added":[],"repositories_removed":[]}`, 202},
		{"one", `{"action":"added","installation":{"id":42},"repositories_added":[{"id":91,"node_id":"R_91","name":"octo-repo","full_name":"octo-org/octo-repo","private":false}],"repositories_removed":[]}`, 202},
		{"multiple removed", `{"action":"removed","installation":{"id":42},"repositories_added":[],"repositories_removed":[{"id":91,"node_id":"R_91","name":"old-repo","full_name":"octo-org/old-repo","private":true},{"id":92,"node_id":"R_92","name":"older-repo","full_name":"octo-org/older-repo","private":false}]}`, 202},
		{"missing selected", `{"action":"added","installation":{"id":42}}`, 400},
		{"missing opposite", `{"action":"added","installation":{"id":42},"repositories_added":[]}`, 400},
		{"selected non-array", `{"action":"added","installation":{"id":42},"repositories_added":{},"repositories_removed":[]}`, 400},
		{"opposite non-array", `{"action":"added","installation":{"id":42},"repositories_added":[],"repositories_removed":{}}`, 400},
		{"opposite non-empty", `{"action":"added","installation":{"id":42},"repositories_added":[],"repositories_removed":[{"id":92,"node_id":"R_92"}]}`, 400},
		{"bad id", `{"action":"added","installation":{"id":42},"repositories_added":[{"id":0,"node_id":"R_91"}],"repositories_removed":[]}`, 400},
		{"blank node", `{"action":"added","installation":{"id":42},"repositories_added":[{"id":91,"node_id":" "}],"repositories_removed":[]}`, 400},
		{"duplicate id", `{"action":"added","installation":{"id":42},"repositories_added":[{"id":91,"node_id":"R_91"},{"id":91,"node_id":"R_92"}],"repositories_removed":[]}`, 400},
		{"duplicate node", `{"action":"added","installation":{"id":42},"repositories_added":[{"id":91,"node_id":"R_91"},{"id":92,"node_id":"R_91"}],"repositories_removed":[]}`, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _, inbox := webhookFixture()
			body := []byte(tc.raw)
			requireEmptyWebhookResponse(t, serveWebhook(h, webhookRequest(body, "installation_repositories", "active-secret")), tc.want)
			if tc.want == 202 {
				if inbox.receiveCalls.Load() != 1 || inbox.params.InstallationID == nil || *inbox.params.InstallationID != 42 || inbox.params.RepositoryID != nil || inbox.params.RepositoryNodeID != nil || !bytes.Equal(inbox.params.Payload, body) {
					t.Fatalf("installation-scope params = %+v", inbox.params)
				}
			} else if inbox.identityCalls.Load() != 0 || inbox.receiveCalls.Load() != 0 {
				t.Fatal("malformed repository list reached store")
			}
		})
	}
}

func TestGitHubWebhookPingIsAuthenticatedAndStateless(t *testing.T) {
	h, secrets, inbox := webhookFixture()
	payload := []byte(`{"zen":"payload-secret-marker"}`)
	response := serveWebhook(h, webhookRequest(payload, "ping", "active-secret"))
	requireEmptyWebhookResponse(t, response, http.StatusAccepted)
	if secrets.calls.Load() == 0 || inbox.identityCalls.Load() != 0 || inbox.receiveCalls.Load() != 0 {
		t.Fatalf("ping calls secret/identity/receive = %d/%d/%d", secrets.calls.Load(), inbox.identityCalls.Load(), inbox.receiveCalls.Load())
	}
	if strings.Contains(response.Body.String(), "payload-secret-marker") {
		t.Fatal("ping response leaked payload")
	}
}

func TestGitHubWebhookOverloadReplayConflictAndFailures(t *testing.T) {
	body := []byte(`{"installation":{"id":42},"repository":{"id":91,"node_id":"R_91"}}`)
	h, _, inbox := webhookFixture()
	inbox.blockIdentity = make(chan struct{})
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- serveWebhook(h, webhookRequest(body, "push", "active-secret")) }()
	for inbox.identityCalls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	requireEmptyWebhookResponse(t, serveWebhook(h, webhookRequest(body, "push", "active-secret")), http.StatusTooManyRequests)
	close(inbox.blockIdentity)
	requireEmptyWebhookResponse(t, <-firstDone, http.StatusAccepted)

	for _, tc := range []struct {
		name        string
		disposition store.GitHubWebhookReceiveDisposition
		receiveErr  error
		want        int
	}{
		{"accepted", store.GitHubWebhookAccepted, nil, 202}, {"replay", store.GitHubWebhookReplayed, nil, 202}, {"conflict", store.GitHubWebhookConflict, nil, 409}, {"unknown", "unknown", nil, 503}, {"receive failure", "", errors.New("internal database detail"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, inbox := webhookFixture()
			inbox.disposition, inbox.receiveErr = tc.disposition, tc.receiveErr
			requireEmptyWebhookResponse(t, serveWebhook(h, webhookRequest(body, "push", "active-secret")), tc.want)
		})
	}
	h, secrets, inbox := webhookFixture()
	secrets.err = errors.New("secret ref active-ref failed")
	requireEmptyWebhookResponse(t, serveWebhook(h, webhookRequest(body, "push", "active-secret")), http.StatusServiceUnavailable)
	if inbox.identityCalls.Load() != 0 {
		t.Fatal("secret failure reached store")
	}
	h, _, inbox = webhookFixture()
	inbox.identityErr = errors.New("internal database detail")
	requireEmptyWebhookResponse(t, serveWebhook(h, webhookRequest(body, "push", "active-secret")), http.StatusServiceUnavailable)
}

func TestGitHubWebhookResponsesDoNotLeakSensitiveInputOrErrors(t *testing.T) {
	payload := []byte(`{"secret":"payload-marker"}`)
	h, secrets, inbox := webhookFixture()
	secrets.values["active-ref"] = []byte("hmac-secret-marker")
	inbox.receiveErr = errors.New("internal-db-marker")
	r := webhookRequest(payload, "issues", "hmac-secret-marker")
	signature := r.Header.Get("X-GitHub-Signature-256")
	response := serveWebhook(h, r)
	combined := response.Body.String()
	for _, forbidden := range []string{"payload-marker", signature, "hmac-secret-marker", "active-ref", "internal-db-marker"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("response leaked %q: %q", forbidden, combined)
		}
	}
}

func TestGitHubWebhookCanceledDuringIdentityDoesNotReceive(t *testing.T) {
	h, _, inbox := webhookFixture()
	inbox.identityErr = context.Canceled
	response := serveWebhook(h, webhookRequest([]byte(`{"installation":{"id":42},"repository":{"id":91,"node_id":"R_91"}}`), "push", "active-secret"))
	requireEmptyWebhookResponse(t, response, http.StatusServiceUnavailable)
	if inbox.receiveCalls.Load() != 0 {
		t.Fatal("identity failure reached receive")
	}
}
