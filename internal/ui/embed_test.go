package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/avatar"
)

func TestStaticOutputCSSCacheHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/static/css/output.css", nil)
	recorder := httptest.NewRecorder()

	StaticHandler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "public, max-age=86400" {
		t.Fatalf("Cache-Control = %q, want %q", cacheControl, "public, max-age=86400")
	}
}

func TestStaticThemeJSCacheHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/static/theme.js", nil)
	recorder := httptest.NewRecorder()

	StaticHandler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "public, max-age=86400" {
		t.Fatalf("Cache-Control = %q, want %q", cacheControl, "public, max-age=86400")
	}
}

func TestAvatarHandlerFallsBackForMissingKey(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/avatars/ffffffffff.svg", nil)
	recorder := httptest.NewRecorder()

	AvatarHandler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "image/svg+xml" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	if recorder.Body.Len() == 0 {
		t.Fatal("fallback avatar body is empty")
	}
}

func TestAvatarHandlerServesDefault(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, avatar.URL(avatar.DefaultKey), nil)
	recorder := httptest.NewRecorder()

	AvatarHandler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
}
