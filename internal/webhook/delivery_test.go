package webhook

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/store"
)

type persistedSignatureCall struct {
	delivery  store.WebhookDelivery
	timestamp int64
	signature string
}

type fakeDeliveryStore struct {
	mu          sync.Mutex
	persisted   []persistedSignatureCall
	results     []store.WebhookAttemptResult
	persistErr  error
	completeErr error
	outcome     *store.WebhookDeliveryOutcome
}

func (f *fakeDeliveryStore) PersistWebhookSignature(_ context.Context, delivery store.WebhookDelivery, timestamp int64, signature string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.persistErr != nil {
		return f.persistErr
	}
	f.persisted = append(f.persisted, persistedSignatureCall{delivery: delivery, timestamp: timestamp, signature: signature})
	return nil
}

func (f *fakeDeliveryStore) CompleteWebhookAttempt(_ context.Context, _ store.WebhookDelivery, result store.WebhookAttemptResult) (store.WebhookDeliveryOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completeErr != nil {
		return store.WebhookDeliveryOutcome{}, f.completeErr
	}
	f.results = append(f.results, result)
	if f.outcome != nil {
		return *f.outcome, nil
	}
	if result.StatusCode >= 200 && result.StatusCode < 300 {
		return store.WebhookDeliveryOutcome{Delivered: true}, nil
	}
	return store.WebhookDeliveryOutcome{}, nil
}

func (f *fakeDeliveryStore) snapshot() ([]persistedSignatureCall, []store.WebhookAttemptResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]persistedSignatureCall(nil), f.persisted...), append([]store.WebhookAttemptResult(nil), f.results...)
}

type fakeSecretResolver struct {
	secret []byte
	err    error
	calls  int
}

type fakeSecretAuthorizer struct {
	calls []string
	err   error
}

type mutableReferenceAuthorizer struct {
	calls   int
	allowed bool
}

func (a *mutableReferenceAuthorizer) Authorize(context.Context, string, string) error { return nil }
func (a *mutableReferenceAuthorizer) AuthorizeReference(context.Context, string) error {
	a.calls++
	if !a.allowed {
		return errors.New("revoked")
	}
	return nil
}

func (f *fakeSecretAuthorizer) Authorize(_ context.Context, namespace, name string) error {
	f.calls = append(f.calls, namespace+"/"+name)
	return f.err
}

func (f *fakeSecretResolver) Resolve(context.Context, string) ([]byte, error) {
	f.calls++
	return f.secret, f.err
}

type capturedRequest struct {
	method      string
	contentType string
	event       string
	deliveryID  string
	timestamp   string
	signature   string
	body        []byte
}

func newReceiver(t *testing.T, status int) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		captured.method = r.Method
		captured.contentType = r.Header.Get("Content-Type")
		captured.event = r.Header.Get("X-Cyclopus-Event")
		captured.deliveryID = r.Header.Get("X-Cyclopus-Delivery")
		captured.timestamp = r.Header.Get("X-Cyclopus-Timestamp")
		captured.signature = r.Header.Get("X-Cyclopus-Signature")
		captured.body = body
		if status >= 300 && status < 400 {
			w.Header().Set("Location", "https://hooks.example.com/other")
		}
		w.WriteHeader(status)
	}))
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, captured
}

func newTestSender(dataStore Store, secrets SecretResolver, server *httptest.Server) *Sender {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
		},
	}
	return &Sender{store: dataStore, secrets: secrets,
		client: &http.Client{Timeout: RequestTimeout, Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		now: time.Now}
}

func testDelivery(target string, body []byte) store.WebhookDelivery {
	return store.WebhookDelivery{
		ID:             "00000000-0000-0000-0000-0000000000d1",
		OrgID:          "00000000-0000-0000-0000-000000000001",
		WebhookID:      "00000000-0000-0000-0000-0000000000e1",
		Event:          "deploy.succeeded",
		TargetURL:      target,
		SecretRef:      "k8s-secret://cyclopus/hooks#secret",
		RawBody:        body,
		Attempt:        1,
		Fence:          1,
		LeaseOwner:     "worker-one",
		LeaseExpiresAt: time.Now().Add(time.Minute),
	}
}

func TestWebhookSignatureFixedVector(t *testing.T) {
	body := []byte(`{"event":"deploy.succeeded","id":"00000000-0000-0000-0000-0000000000d1"}`)
	got := Sign([]byte("cyclopus-test-secret"), 1700000000, body)
	want := "sha256=f4423a564188a26071e385fb8182b4035a1a71bbce0170bc37e4a0fe5ddcf228"
	if got != want {
		t.Fatalf("Sign() = %q, want %q", got, want)
	}
}

func TestAuthorizedWebhookDeliveryAuthorizesBeforeSecretRead(t *testing.T) {
	server, _ := newReceiver(t, http.StatusOK)
	dataStore := &fakeDeliveryStore{}
	resolver := &fakeSecretResolver{secret: []byte("receiver-secret")}
	authorizer := &fakeSecretAuthorizer{}
	sender := newTestSender(dataStore, resolver, server)
	sender.authorizer = authorizer
	sender.secretNamespace = "cyclopus"

	if _, err := sender.Deliver(context.Background(), testDelivery("https://hooks.example.com/webhook", []byte(`{}`))); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if len(authorizer.calls) != 1 || authorizer.calls[0] != "cyclopus/hooks" || resolver.calls != 1 {
		t.Fatalf("authorization calls = %v, resolver calls = %d", authorizer.calls, resolver.calls)
	}
}

func TestAuthorizedWebhookDeliveryRejectsCrossNamespaceBeforeKubernetesCalls(t *testing.T) {
	server, _ := newReceiver(t, http.StatusOK)
	dataStore := &fakeDeliveryStore{}
	resolver := &fakeSecretResolver{secret: []byte("receiver-secret")}
	authorizer := &fakeSecretAuthorizer{}
	sender := newTestSender(dataStore, resolver, server)
	sender.authorizer = authorizer
	sender.secretNamespace = "cyclopus"
	delivery := testDelivery("https://hooks.example.com/webhook", []byte(`{}`))
	delivery.SecretRef = "k8s-secret://other/hooks#secret"

	if _, err := sender.Deliver(context.Background(), delivery); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if len(authorizer.calls) != 0 || resolver.calls != 0 {
		t.Fatalf("disallowed ref reached Kubernetes path: authorization=%v resolve=%d", authorizer.calls, resolver.calls)
	}
}

func TestWebhookSignatureFormat(t *testing.T) {
	signature := Sign([]byte("secret"), 1700000001, []byte(`{"a":1}`))
	if !strings.HasPrefix(signature, "sha256=") {
		t.Fatalf("Sign() = %q, want sha256= prefix", signature)
	}
	encoded := strings.TrimPrefix(signature, "sha256=")
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != 32 || len(encoded) != 64 {
		t.Fatalf("Sign() hex payload = %q (%d bytes), %v", encoded, len(decoded), err)
	}
	if Sign([]byte("secret"), 1700000001, []byte(`{"a":2}`)) == signature {
		t.Fatal("Sign() ignored body")
	}
	if Sign([]byte("secret"), 1700000002, []byte(`{"a":1}`)) == signature {
		t.Fatal("Sign() ignored timestamp")
	}
	if Sign([]byte("other"), 1700000001, []byte(`{"a":1}`)) == signature {
		t.Fatal("Sign() ignored secret")
	}
}

func TestWebhookSignatureDeliverSuccessPersistsSignatureAndSendsExactHeaders(t *testing.T) {
	server, captured := newReceiver(t, http.StatusNoContent)
	dataStore := &fakeDeliveryStore{}
	resolver := &fakeSecretResolver{secret: []byte("receiver-secret")}
	sender := newTestSender(dataStore, resolver, server)
	body := []byte(`{"event":"deploy.succeeded","id":"d1"}`)
	delivery := testDelivery("https://hooks.example.com/webhook", body)

	before := time.Now().Unix()
	if _, err := sender.Deliver(context.Background(), delivery); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	after := time.Now().Unix()

	persisted, results := dataStore.snapshot()
	if len(persisted) != 1 {
		t.Fatalf("PersistWebhookSignature calls = %d, want 1", len(persisted))
	}
	if persisted[0].delivery.ID != delivery.ID || persisted[0].delivery.Fence != delivery.Fence {
		t.Fatalf("persisted delivery = %+v", persisted[0].delivery)
	}
	if persisted[0].timestamp < before || persisted[0].timestamp > after {
		t.Fatalf("persisted timestamp = %d, want within [%d, %d]", persisted[0].timestamp, before, after)
	}
	wantSignature := Sign(resolver.secret, persisted[0].timestamp, body)
	if persisted[0].signature != wantSignature {
		t.Fatalf("persisted signature = %q, want %q", persisted[0].signature, wantSignature)
	}

	if captured.method != http.MethodPost || captured.contentType != "application/json" {
		t.Fatalf("request = %s %q", captured.method, captured.contentType)
	}
	if captured.event != "deploy.succeeded" || captured.deliveryID != delivery.ID {
		t.Fatalf("identity headers = %q/%q", captured.event, captured.deliveryID)
	}
	if captured.timestamp != strconv.FormatInt(persisted[0].timestamp, 10) {
		t.Fatalf("timestamp header = %q, want %d", captured.timestamp, persisted[0].timestamp)
	}
	if captured.signature != persisted[0].signature {
		t.Fatalf("signature header = %q, want %q", captured.signature, persisted[0].signature)
	}
	if string(captured.body) != string(body) {
		t.Fatalf("raw body = %q, want %q", captured.body, body)
	}

	if len(results) != 1 || results[0].StatusCode != http.StatusNoContent || results[0].Error != "" {
		t.Fatalf("CompleteWebhookAttempt results = %+v", results)
	}
}

func TestWebhookSignatureRetryReusesPersistedTimestampAndSignature(t *testing.T) {
	server, captured := newReceiver(t, http.StatusOK)
	dataStore := &fakeDeliveryStore{}
	resolver := &fakeSecretResolver{err: errors.New("secret backend down")}
	sender := newTestSender(dataStore, resolver, server)
	authorizer := &fakeSecretAuthorizer{}
	sender.authorizer = authorizer
	sender.secretNamespace = "cyclopus"
	body := []byte(`{"event":"deploy.failed"}`)
	delivery := testDelivery("https://hooks.example.com/webhook", body)
	timestamp := int64(1700000123)
	signature := Sign([]byte("first-attempt-secret"), timestamp, body)
	delivery.FirstSendTimestamp = &timestamp
	delivery.Signature = &signature
	delivery.Attempt = 3
	delivery.Fence = 3

	if _, err := sender.Deliver(context.Background(), delivery); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("Resolve calls = %d, want 0 for retry with persisted signature", resolver.calls)
	}
	if len(authorizer.calls) != 1 {
		t.Fatalf("Authorize calls = %d, want 1 for cached retry", len(authorizer.calls))
	}
	persisted, results := dataStore.snapshot()
	if len(persisted) != 0 {
		t.Fatalf("PersistWebhookSignature calls = %d, want 0", len(persisted))
	}
	if captured.timestamp != "1700000123" || captured.signature != signature {
		t.Fatalf("retry headers = %q/%q, want 1700000123/%q", captured.timestamp, captured.signature, signature)
	}
	if len(results) != 1 || results[0].StatusCode != http.StatusOK {
		t.Fatalf("CompleteWebhookAttempt results = %+v", results)
	}
}

func TestWebhookSignatureRetryRevokedBeforeCachedSignatureSend(t *testing.T) {
	server, captured := newReceiver(t, http.StatusOK)
	dataStore := &fakeDeliveryStore{}
	resolver := &fakeSecretResolver{secret: []byte("receiver-secret")}
	authorizer := &mutableReferenceAuthorizer{allowed: true}
	sender := newTestSender(dataStore, resolver, server)
	sender.authorizer = authorizer
	sender.secretNamespace = "cyclopus"
	delivery := testDelivery("https://hooks.example.com/webhook", []byte(`{}`))

	if _, err := sender.Deliver(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	persisted, _ := dataStore.snapshot()
	delivery.FirstSendTimestamp = &persisted[0].timestamp
	delivery.Signature = &persisted[0].signature
	authorizer.allowed = false
	if _, err := sender.Deliver(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	if authorizer.calls != 2 || resolver.calls != 1 {
		t.Fatalf("Authorize calls = %d, Resolve calls = %d, want 2/1", authorizer.calls, resolver.calls)
	}
	if captured.signature != persisted[0].signature {
		t.Fatalf("first request signature = %q", captured.signature)
	}
	_, results := dataStore.snapshot()
	if len(results) != 2 || !strings.Contains(results[1].Error, "authorize webhook secret") {
		t.Fatalf("retry results = %+v", results)
	}
}

func TestWebhookSignatureSecretResolutionFailureRecordsFailedAttempt(t *testing.T) {
	dataStore := &fakeDeliveryStore{}
	resolver := &fakeSecretResolver{err: errors.New("vault unavailable")}
	sender := &Sender{store: dataStore, secrets: resolver, client: &http.Client{}, now: time.Now}
	delivery := testDelivery("https://hooks.example.com/webhook", []byte(`{"event":"deploy.failed"}`))

	if _, err := sender.Deliver(context.Background(), delivery); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	persisted, results := dataStore.snapshot()
	if len(persisted) != 0 {
		t.Fatalf("PersistWebhookSignature calls = %d, want 0", len(persisted))
	}
	if len(results) != 1 || results[0].StatusCode != 0 || !strings.Contains(results[0].Error, "resolve webhook secret") {
		t.Fatalf("CompleteWebhookAttempt results = %+v", results)
	}
}

func TestWebhookSignatureRedirectRejectedAsFailure(t *testing.T) {
	server, _ := newReceiver(t, http.StatusFound)
	dataStore := &fakeDeliveryStore{}
	resolver := &fakeSecretResolver{secret: []byte("receiver-secret")}
	sender := newTestSender(dataStore, resolver, server)
	delivery := testDelivery("https://hooks.example.com/webhook", []byte(`{"event":"deploy.succeeded"}`))

	if _, err := sender.Deliver(context.Background(), delivery); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	_, results := dataStore.snapshot()
	if len(results) != 1 || results[0].StatusCode != http.StatusFound || !strings.Contains(results[0].Error, "redirect") {
		t.Fatalf("CompleteWebhookAttempt results = %+v", results)
	}
}

func TestWebhookSignatureServerErrorRecordsFailure(t *testing.T) {
	server, _ := newReceiver(t, http.StatusInternalServerError)
	dataStore := &fakeDeliveryStore{}
	resolver := &fakeSecretResolver{secret: []byte("receiver-secret")}
	sender := newTestSender(dataStore, resolver, server)
	delivery := testDelivery("https://hooks.example.com/webhook", []byte(`{"event":"deploy.succeeded"}`))

	if _, err := sender.Deliver(context.Background(), delivery); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	_, results := dataStore.snapshot()
	if len(results) != 1 || results[0].StatusCode != http.StatusInternalServerError ||
		!strings.Contains(results[0].Error, "HTTP 500") {
		t.Fatalf("CompleteWebhookAttempt results = %+v", results)
	}
}

func TestWebhookSignatureRejectsInvalidTargets(t *testing.T) {
	for _, target := range []string{
		"http://hooks.example.com/webhook",
		"not-a-url",
		"https://localhost/webhook",
		"https://api.localhost/webhook",
		"https://127.0.0.1/webhook",
		"https://10.1.2.3/webhook",
		"https://192.168.1.10/webhook",
		"https://169.254.169.254/latest/meta-data",
		"https://user:secret@hooks.example.com/webhook",
		"https://hooks.example.com/webhook#fragment",
	} {
		t.Run(target, func(t *testing.T) {
			if err := validateTarget(target); err == nil {
				t.Fatalf("validateTarget(%q) = nil, want error", target)
			}
			dataStore := &fakeDeliveryStore{}
			resolver := &fakeSecretResolver{secret: []byte("receiver-secret")}
			sender := &Sender{store: dataStore, secrets: resolver, client: &http.Client{}, now: time.Now}
			delivery := testDelivery(target, []byte(`{"event":"deploy.succeeded"}`))
			if _, err := sender.Deliver(context.Background(), delivery); err != nil {
				t.Fatalf("Deliver() error = %v", err)
			}
			if resolver.calls != 0 {
				t.Fatalf("Resolve calls = %d, want 0 for rejected target", resolver.calls)
			}
			persisted, results := dataStore.snapshot()
			if len(persisted) != 0 {
				t.Fatalf("PersistWebhookSignature calls = %d, want 0", len(persisted))
			}
			if len(results) != 1 || results[0].StatusCode != 0 || results[0].Error == "" {
				t.Fatalf("CompleteWebhookAttempt results = %+v", results)
			}
		})
	}
}

func TestWebhookSignatureAcceptsPublicTarget(t *testing.T) {
	for _, target := range []string{
		"https://hooks.example.com/webhook",
		"https://hooks.example.com:8443/webhook?token=abc",
		"https://8.8.8.8/webhook",
	} {
		if err := validateTarget(target); err != nil {
			t.Fatalf("validateTarget(%q) = %v, want nil", target, err)
		}
	}
}

func TestWebhookSignaturePublicIPClassification(t *testing.T) {
	for _, entry := range []struct {
		value string
		want  bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2606:4700:4700::1111", true},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"192.168.0.1", false},
		{"127.0.0.1", false},
		{"169.254.169.254", false},
		{"::1", false},
		{"fe80::1", false},
		{"0.0.0.0", false},
		// CGNAT shared address space (RFC 6598).
		{"100.64.0.1", false},
		{"100.127.255.254", false},
		// Benchmarking range (RFC 2544).
		{"198.18.0.1", false},
		{"198.19.255.254", false},
		// Reserved class E (includes broadcast).
		{"240.0.0.1", false},
		{"250.1.2.3", false},
		{"255.255.255.255", false},
		// NAT64 well-known prefix with embedded IPv4 (RFC 6052).
		{"64:ff9b::8.8.8.8", false},
		{"64:ff9b::10.0.0.1", false},
		// IPv4-mapped IPv6 forms classify like their embedded address.
		{"::ffff:8.8.8.8", true},
		{"::ffff:10.0.0.1", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:100.64.0.1", false},
		// Addresses just outside the rejected ranges remain allowed.
		{"100.63.255.255", true},
		{"100.128.0.0", true},
		{"198.17.255.255", true},
		{"198.20.0.0", true},
		{"239.255.255.255", false}, // multicast is not global unicast
		{"64:ff9a::8.8.8.8", true},
	} {
		ip := net.ParseIP(entry.value)
		if ip == nil {
			t.Fatalf("net.ParseIP(%q) = nil", entry.value)
		}
		if got := PublicIPAllowed(ip); got != entry.want {
			t.Fatalf("PublicIPAllowed(%s) = %v, want %v", entry.value, got, entry.want)
		}
	}
}

func TestWebhookSanitizeErrorStripsURLErrorTarget(t *testing.T) {
	cause := &url.Error{Op: "Post", URL: "https://hooks.example.com/webhook?token=secret-token", Err: errors.New("connection refused")}
	got := sanitizeError(cause)
	if strings.Contains(got, "hooks.example.com") || strings.Contains(got, "secret-token") {
		t.Fatalf("sanitizeError() = %q, leaked URL contents", got)
	}
	if !strings.Contains(got, "Post") || !strings.Contains(got, "connection refused") {
		t.Fatalf("sanitizeError() = %q, want op plus cause", got)
	}
	wrapped := fmt.Errorf("delivery attempt: %w", cause)
	got = sanitizeError(wrapped)
	if strings.Contains(got, "hooks.example.com") || strings.Contains(got, "secret-token") {
		t.Fatalf("sanitizeError(wrapped) = %q, leaked URL contents", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("sanitizeError(wrapped) = %q, want underlying cause", got)
	}
}

func TestWebhookSanitizeErrorCapsLength(t *testing.T) {
	long := errors.New(strings.Repeat("x", maxPersistedErrorBytes+100))
	if got := sanitizeError(long); len(got) != maxPersistedErrorBytes {
		t.Fatalf("sanitizeError() length = %d, want %d", len(got), maxPersistedErrorBytes)
	}
	longURL := &url.Error{Op: "Post", URL: "https://hooks.example.com/webhook", Err: errors.New(strings.Repeat("y", maxPersistedErrorBytes+100))}
	got := sanitizeError(longURL)
	if len(got) != maxPersistedErrorBytes || strings.Contains(got, "hooks.example.com") {
		t.Fatalf("sanitizeError() = %d bytes %q..., want capped and stripped", len(got), got[:32])
	}
	exact := errors.New(strings.Repeat("z", maxPersistedErrorBytes))
	if got := sanitizeError(exact); len(got) != maxPersistedErrorBytes {
		t.Fatalf("sanitizeError() length = %d, want %d", len(got), maxPersistedErrorBytes)
	}
	short := errors.New("receiver down")
	if got := sanitizeError(short); got != "receiver down" {
		t.Fatalf("sanitizeError() = %q, want unchanged", got)
	}
	if got := sanitizeError(nil); got != "" {
		t.Fatalf("sanitizeError(nil) = %q, want empty", got)
	}
}

func TestWebhookSignatureDeliverCompletesAttemptDespiteCancelledRequestContext(t *testing.T) {
	server, _ := newReceiver(t, http.StatusOK)
	dataStore := &fakeDeliveryStore{}
	resolver := &fakeSecretResolver{secret: []byte("receiver-secret")}
	sender := newTestSender(dataStore, resolver, server)
	delivery := testDelivery("https://hooks.example.com/webhook", []byte(`{"event":"deploy.succeeded"}`))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := sender.Deliver(ctx, delivery)
	if err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if outcome.Abandoned || outcome.Delivered {
		t.Fatalf("Deliver() outcome = %+v, want zero outcome from fake store", outcome)
	}
	_, results := dataStore.snapshot()
	if len(results) != 1 {
		t.Fatalf("CompleteWebhookAttempt calls = %d, want the attempt recorded despite cancellation", len(results))
	}
}

func TestWebhookSignatureDeliverPropagatesAbandonedOutcome(t *testing.T) {
	server, _ := newReceiver(t, http.StatusInternalServerError)
	dataStore := &fakeDeliveryStore{outcome: &store.WebhookDeliveryOutcome{Abandoned: true, Reason: "retry_exhausted", EndpointDisabled: true}}
	resolver := &fakeSecretResolver{secret: []byte("receiver-secret")}
	sender := newTestSender(dataStore, resolver, server)
	delivery := testDelivery("https://hooks.example.com/webhook", []byte(`{"event":"deploy.failed"}`))
	delivery.Attempt = 7
	delivery.Fence = 7

	outcome, err := sender.Deliver(context.Background(), delivery)
	if err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if !outcome.Abandoned || outcome.Reason != "retry_exhausted" || !outcome.EndpointDisabled {
		t.Fatalf("Deliver() outcome = %+v, want abandoned retry_exhausted disabled", outcome)
	}
}
