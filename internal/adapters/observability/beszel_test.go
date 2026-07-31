package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBeszelGetOperatorMetricsAllowlistOnly(t *testing.T) {
	var requestPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		_, _ = w.Write([]byte(`{"items":[{"name":"node-1","updated":"2026-01-02 03:04:05.000Z","info":{"cpu":42.5,"mp":80,"secret_internal":"leak","container_id":"abc"}}]}`))
	}))
	t.Cleanup(server.Close)
	client := &BeszelClient{config: BeszelConfig{BaseURL: server.URL}, client: server.Client()}

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
	client := &BeszelClient{config: BeszelConfig{BaseURL: server.URL}, client: server.Client()}
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
	if _, err := NewBeszelClient(BeszelConfig{BaseURL: "http://insecure.example"}); err == nil {
		t.Fatal("expected non-HTTPS Beszel URL to be rejected")
	}
	if _, err := NewBeszelClient(BeszelConfig{BaseURL: "https://beszel.example"}); err != nil {
		t.Fatalf("valid HTTPS Beszel config rejected: %v", err)
	}
}

func TestBeszelNon2xxIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	client := &BeszelClient{config: BeszelConfig{BaseURL: server.URL}, client: server.Client()}
	if _, err := client.GetOperatorMetrics(context.Background()); err == nil {
		t.Fatal("expected an error on HTTP 502")
	}
}
