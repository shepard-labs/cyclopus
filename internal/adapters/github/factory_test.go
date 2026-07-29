package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v4"
)

func TestInstallationClientFactoryJWTTokenCachingAndHeaders(t *testing.T) {
	key := generatePrivateKey(t)
	privateKeyFile := writePrivateKey(t, key)
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

	factory, err := NewInstallationClientFactory(1234, privateKeyFile, server.URL, server.URL, "2026-03-10", http.DefaultTransport)
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
	privateKeyFile := writePrivateKey(t, key)
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

	factory, err := NewInstallationClientFactory(1234, privateKeyFile, server.URL, server.URL, "2026-03-10", http.DefaultTransport)
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

func writePrivateKey(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github.pem")
	contents := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFactoryJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatal(err)
	}
}
