package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIDocsRoutesArePublic(t *testing.T) {
	handler, err := Handler(nil, Services{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/openapi.json", "/docs"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
	// The gateway surface itself is untouched: unknown paths still 404 as JSON.
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/not-a-route", nil))
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code"`) {
		t.Fatalf("gateway 404 = %d %q", response.Code, response.Body.String())
	}
}
