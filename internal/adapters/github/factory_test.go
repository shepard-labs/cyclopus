package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v4"
)

func TestInstallationClientFactoryJWTTokenCachingAndHeaders(t *testing.T) {
	key := generatePrivateKey(t)
	credentials := &fakeCredentialProvider{credential: AppCredential{AppID: 1234, PrivateKey: privateKey(key), ResourceVersion: "1"}}
	var tokenRequests int
	var apiRequests int

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
			t.Fatalf("API version header = %q", request.Header.Get("X-GitHub-Api-Version"))
		}
		switch request.URL.Path {
		case "/app/installations/42/access_tokens":
			tokenRequests++
			if request.Method != http.MethodPost {
				t.Fatalf("token method = %s", request.Method)
			}
			authorization := request.Header.Get("Authorization")
			if !strings.HasPrefix(authorization, "Bearer ") {
				t.Fatalf("token authorization = %q", authorization)
			}
			parsed, err := jwt.ParseWithClaims(strings.TrimPrefix(authorization, "Bearer "), &jwt.RegisteredClaims{}, func(token *jwt.Token) (any, error) {
				return &key.PublicKey, nil
			})
			if err != nil || !parsed.Valid {
				t.Fatalf("parse JWT: %v", err)
			}
			claims := parsed.Claims.(*jwt.RegisteredClaims)
			if claims.Issuer != "1234" || claims.IssuedAt == nil || claims.ExpiresAt == nil || claims.ExpiresAt.Sub(claims.IssuedAt.Time) != 2*time.Minute {
				t.Fatalf("claims = %+v", claims)
			}
			writeFactoryJSON(t, writer, map[string]any{
				"token":      "installation-token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		case "/rate_limit":
			apiRequests++
			if request.Header.Get("Authorization") != "token installation-token" {
				t.Fatalf("API authorization = %q", request.Header.Get("Authorization"))
			}
			if request.Header.Get("Accept") != "application/vnd.github+json" {
				t.Fatalf("accept = %q", request.Header.Get("Accept"))
			}
			writeFactoryJSON(t, writer, map[string]any{"resources": map[string]any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	factory, err := NewInstallationClientFactory(credentials, server.URL, server.URL, "2026-03-10", http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	clientOne, err := factory.Client(42)
	if err != nil {
		t.Fatal(err)
	}
	clientTwo, err := factory.Client(42)
	if err != nil {
		t.Fatal(err)
	}
	if clientOne != clientTwo {
		t.Fatal("factory did not cache installation client")
	}
	for range 2 {
		if _, _, err := clientOne.RateLimit.Get(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if tokenRequests != 1 || apiRequests != 2 {
		t.Fatalf("requests = token:%d api:%d", tokenRequests, apiRequests)
	}
}

func TestInstallationClientFactoryRefreshesExpiringToken(t *testing.T) {
	key := generatePrivateKey(t)
	credentials := &fakeCredentialProvider{credential: AppCredential{AppID: 1234, PrivateKey: privateKey(key), ResourceVersion: "1"}}
	var tokenRequests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/app/installations/42/access_tokens":
			tokenRequests++
			writeFactoryJSON(t, writer, map[string]any{
				"token":      "installation-token",
				"expires_at": time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339),
			})
		case "/rate_limit":
			writeFactoryJSON(t, writer, map[string]any{"resources": map[string]any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	factory, err := NewInstallationClientFactory(credentials, server.URL, server.URL, "2026-03-10", http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	client, err := factory.Client(42)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, err := client.RateLimit.Get(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if tokenRequests != 2 {
		t.Fatalf("token requests = %d, want refresh for near-expiry token", tokenRequests)
	}
}

func generatePrivateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func privateKey(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestInstallationClientFactoryRotatesWithCredentialResourceVersion(t *testing.T) {
	provider := &fakeCredentialProvider{credential: AppCredential{AppID: 1234, PrivateKey: privateKey(generatePrivateKey(t)), ResourceVersion: "1"}}
	factory, err := NewInstallationClientFactory(provider, "https://api.github.test", "https://uploads.github.test", "2026-03-10", http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	clientOne, err := factory.Client(42)
	if err != nil {
		t.Fatal(err)
	}
	provider.set(AppCredential{AppID: 1234, PrivateKey: privateKey(generatePrivateKey(t)), ResourceVersion: "2"}, nil)
	clientTwo, err := factory.Client(42)
	if err != nil {
		t.Fatal(err)
	}
	if clientOne == clientTwo {
		t.Fatal("credential rotation reused the stale installation client")
	}
}

func TestInstallationClientFactoryRejectsInvalidInputsAndCredentialFailures(t *testing.T) {
	want := errors.New("Secret key missing")
	provider := &fakeCredentialProvider{err: want}
	factory, err := NewInstallationClientFactory(provider, "https://api.github.test", "https://uploads.github.test", "2026-03-10", http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.Client(0); err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("Client(0) error = %v", err)
	}
	if _, err := factory.Client(42); !errors.Is(err, want) || !strings.Contains(err.Error(), "load GitHub App credential") {
		t.Fatalf("Client() error = %v, want wrapped credential error", err)
	}
	provider.set(AppCredential{AppID: 1234, PrivateKey: []byte("not a PEM"), ResourceVersion: "3"}, nil)
	if _, err := factory.Client(42); err == nil || !strings.Contains(err.Error(), "create GitHub installation transport") {
		t.Fatalf("Client() malformed key error = %v", err)
	}
}

func TestBoundedTransportRejectsRedirectAndOversizedResponses(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "redirect", status: http.StatusFound},
		{name: "oversized", status: http.StatusOK, body: strings.Repeat("x", githubResponseLimit+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := boundedTransport{next: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})}
			response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, "https://api.github.test/app", nil))
			if test.status == http.StatusFound {
				if err == nil {
					t.Fatal("redirect succeeded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(response.Body)
			if err == nil {
				t.Fatal("oversized response succeeded")
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type fakeCredentialProvider struct {
	mu         sync.Mutex
	credential AppCredential
	err        error
}

func (p *fakeCredentialProvider) AppCredential(context.Context) (AppCredential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.credential, p.err
}

func (p *fakeCredentialProvider) set(credential AppCredential, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.credential, p.err = credential, err
}

func writeFactoryJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatal(err)
	}
}
