package observability

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeJWT builds an unsigned-but-well-formed JWT carrying only an exp claim,
// sufficient for jwtExpiry to decode. Signature verification is intentionally
// not part of the client (see authenticate's doc comment), so tests don't need
// a real PocketBase signing key.
func fakeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return header + "." + payload + ".sig"
}

// unauthedTestClient builds a BeszelClient with a pre-seeded, far-future
// cached token so GetOperatorMetrics skips authentication entirely — for
// tests that only care about the systems-fetch behavior.
func unauthedTestClient(serverURL string, httpClient *http.Client) *BeszelClient {
	return &BeszelClient{
		config:           BeszelConfig{BaseURL: serverURL},
		client:           httpClient,
		cachedToken:      "test-token",
		cachedTokenUntil: time.Now().Add(time.Hour),
	}
}

func TestBeszelGetOperatorMetricsAllowlistOnly(t *testing.T) {
	var requestPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		_, _ = w.Write([]byte(`{"items":[{"name":"node-1","updated":"2026-01-02 03:04:05.000Z","info":{"cpu":42.5,"mp":80,"secret_internal":"leak","container_id":"abc"}}]}`))
	}))
	t.Cleanup(server.Close)
	client := unauthedTestClient(server.URL, server.Client())

	metrics, err := client.GetOperatorMetrics(context.Background())
	if err != nil {
		t.Fatalf("GetOperatorMetrics: %v", err)
	}
	if !strings.HasPrefix(requestPath, "/api/collections/systems/records") {
		t.Fatalf("unexpected Beszel path: %s", requestPath)
	}
	names := map[string]bool{}
	for _, metric := range metrics {
		names[metric.Name] = true
		if strings.Contains(metric.Name, "secret") || strings.Contains(metric.Name, "container") {
			t.Fatalf("non-allowlisted metric leaked: %s", metric.Name)
		}
	}
	if !names["node-1/cpu.usage"] || !names["node-1/memory.usage"] {
		t.Fatalf("expected allowlisted metrics, got %v", names)
	}
}

func TestBeszelNoTenantParameters(t *testing.T) {
	var rawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(server.Close)
	client := unauthedTestClient(server.URL, server.Client())
	if _, err := client.GetOperatorMetrics(context.Background()); err != nil {
		t.Fatalf("GetOperatorMetrics: %v", err)
	}
	for _, forbidden := range []string{"org_id", "app_id", "environment_id", "tenant"} {
		if strings.Contains(rawQuery, forbidden) {
			t.Fatalf("Beszel request carried a tenant parameter %q: %s", forbidden, rawQuery)
		}
	}
}

func TestBeszelConstructorRequiresHTTPS(t *testing.T) {
	if _, err := NewBeszelClient(BeszelConfig{BaseURL: "http://insecure.example", Credential: "admin:password123"}); err == nil {
		t.Fatal("expected non-HTTPS Beszel URL to be rejected")
	}
	// §8 keeps Beszel in-cluster only; its ClusterIP Service is plaintext.
	if _, err := NewBeszelClient(BeszelConfig{BaseURL: "http://beszel.cyclopus.svc:8090", Credential: "admin:password123"}); err != nil {
		t.Fatalf("in-cluster .svc Beszel config rejected: %v", err)
	}
	if _, err := NewBeszelClient(BeszelConfig{BaseURL: "https://beszel.example", Credential: "admin:password123"}); err != nil {
		t.Fatalf("valid HTTPS Beszel config rejected: %v", err)
	}
}

func TestBeszelConstructorRequiresCredential(t *testing.T) {
	cases := []struct {
		name       string
		credential string
		wantErr    bool
	}{
		{"missing colon", "admin-no-password", true},
		{"empty identity", ":password123", true},
		{"empty password", "admin:", true},
		{"empty credential", "", true},
		{"valid", "admin@cyclopus.local:password123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewBeszelClient(BeszelConfig{BaseURL: "https://beszel.example", Credential: tc.credential})
			if tc.wantErr && err == nil {
				t.Fatalf("credential %q: expected error, got none", tc.credential)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("credential %q: unexpected error: %v", tc.credential, err)
			}
		})
	}
}

func TestBeszelNon2xxIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	client := unauthedTestClient(server.URL, server.Client())
	if _, err := client.GetOperatorMetrics(context.Background()); err == nil {
		t.Fatal("expected an error on HTTP 502")
	}
}

// beszelAuthServer builds a test hub that handles both the superuser
// auth-with-password login and the systems listing, so re-authentication
// behavior can be exercised end to end.
type beszelAuthServer struct {
	authCalls   int
	fetchCalls  int
	tokenExpiry time.Time
	// wantToken, if set, is checked against every systems request; a mismatch
	// responds 401 so re-auth-on-401 behavior can be tested.
	validToken string
}

func (s *beszelAuthServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/collections/_superusers/auth-with-password":
			s.authCalls++
			var body struct{ Identity, Password string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode auth request: %v", err)
			}
			if body.Identity != "admin@cyclopus.local" || body.Password != "s3cret-pw" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			token := fakeJWT(t, s.tokenExpiry)
			s.validToken = token
			_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/collections/systems/records"):
			s.fetchCalls++
			if r.Header.Get("Authorization") != s.validToken || s.validToken == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// httptestBeszelClient builds a BeszelClient talking to an httptest server
// (plain HTTP) with the given credential, bypassing NewBeszelClient's HTTPS
// requirement — HTTPS enforcement is covered separately by
// TestBeszelConstructorRequiresHTTPS.
func httptestBeszelClient(serverURL string, httpClient *http.Client, identity, password string) *BeszelClient {
	return &BeszelClient{
		config:   BeszelConfig{BaseURL: serverURL, Credential: identity + ":" + password},
		identity: identity,
		password: password,
		client:   httpClient,
	}
}

func TestBeszelAuthenticatesAndCachesToken(t *testing.T) {
	auth := &beszelAuthServer{tokenExpiry: time.Now().Add(time.Hour)}
	server := httptest.NewServer(auth.handler(t))
	t.Cleanup(server.Close)

	client := httptestBeszelClient(server.URL, server.Client(), "admin@cyclopus.local", "s3cret-pw")

	for range 2 {
		if _, err := client.GetOperatorMetrics(context.Background()); err != nil {
			t.Fatalf("GetOperatorMetrics: %v", err)
		}
	}
	if auth.authCalls != 1 {
		t.Fatalf("expected 1 auth call (cached token reused), got %d", auth.authCalls)
	}
	if auth.fetchCalls != 2 {
		t.Fatalf("expected 2 systems fetches, got %d", auth.fetchCalls)
	}
}

func TestBeszelReauthenticatesBeforeExpiry(t *testing.T) {
	auth := &beszelAuthServer{tokenExpiry: time.Now().Add(30 * time.Second)} // inside tokenRefreshMargin
	server := httptest.NewServer(auth.handler(t))
	t.Cleanup(server.Close)

	client := httptestBeszelClient(server.URL, server.Client(), "admin@cyclopus.local", "s3cret-pw")

	for range 2 {
		if _, err := client.GetOperatorMetrics(context.Background()); err != nil {
			t.Fatalf("GetOperatorMetrics: %v", err)
		}
	}
	if auth.authCalls != 2 {
		t.Fatalf("expected 2 auth calls (near-expiry token not reused), got %d", auth.authCalls)
	}
}

func TestBeszelReauthenticatesOn401(t *testing.T) {
	auth := &beszelAuthServer{tokenExpiry: time.Now().Add(time.Hour)}
	server := httptest.NewServer(auth.handler(t))
	t.Cleanup(server.Close)

	client := httptestBeszelClient(server.URL, server.Client(), "admin@cyclopus.local", "s3cret-pw")

	// Prime a cached token, then invalidate it server-side (simulating a
	// revoked/stale session) so the next call must observe a 401 and recover.
	if _, err := client.GetOperatorMetrics(context.Background()); err != nil {
		t.Fatalf("GetOperatorMetrics (priming): %v", err)
	}
	auth.validToken = "revoked"

	if _, err := client.GetOperatorMetrics(context.Background()); err != nil {
		t.Fatalf("GetOperatorMetrics (after revocation): %v", err)
	}
	if auth.authCalls != 2 {
		t.Fatalf("expected re-auth after a 401, got %d auth calls", auth.authCalls)
	}
}

func TestBeszelAuthFailureIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	client := httptestBeszelClient(server.URL, server.Client(), "admin@cyclopus.local", "wrong-pw")

	if _, err := client.GetOperatorMetrics(context.Background()); err == nil {
		t.Fatal("expected an error when Beszel rejects the superuser credential")
	}
}
