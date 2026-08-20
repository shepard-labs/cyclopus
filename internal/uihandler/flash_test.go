package uihandler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/shepard-labs/cyclopus/internal/ui/pages"
)

func TestFlashCookieSetOnRedirect(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/settings/user/profile", nil)

	redirectWithFlash(recorder, request, "/settings/user", "success", "Profile updated.")

	response := recorder.Result()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusSeeOther)
	}
	if location := response.Header.Get("Location"); location != "/settings/user" {
		t.Fatalf("Location = %q, want clean redirect path", location)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %+v, want exactly one", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != flashCookieName || cookie.Value == "" || !cookie.HttpOnly {
		t.Fatalf("flash cookie = %+v", cookie)
	}
	if cookie.Path != "/" || cookie.MaxAge <= 0 || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("flash cookie attributes = %+v", cookie)
	}
}

func TestDisconnectConflictShowsDispatchPinWaitAndRetryGuidance(t *testing.T) {
	for _, typedErr := range []error{
		fmt.Errorf("mapped conflict: %w", service.ErrConflict),
		fmt.Errorf("mapped in progress: %w", service.ErrInProgress),
	} {
		h := &Handler{disconnectGitConnection: func(context.Context, string, string, string) (store.GitConnection, error) {
			return store.GitConnection{}, typedErr
		}}
		route := chi.NewRouteContext()
		route.URLParams.Add("id", "connection-1")
		request := httptest.NewRequest(http.MethodPost, "/settings/git-connections/connection-1/disconnect", nil)
		request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
		response := httptest.NewRecorder()
		h.postGitConnectionDisconnect(response, request)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusSeeOther)
		}
		flashRequest := httptest.NewRequest(http.MethodGet, "/settings/git-connections", nil)
		flashRequest.AddCookie(response.Result().Cookies()[0])
		flash := flashFromCookie(httptest.NewRecorder(), flashRequest)
		if flash == nil || flash.Kind != "error" || !strings.Contains(flash.Message, "active GitHub request") ||
			!strings.Contains(flash.Message, "dispatch pin to expire") || !strings.Contains(flash.Message, "retry disconnect") {
			t.Fatalf("disconnect conflict flash = %+v", flash)
		}
	}
}

func TestFlashCookieRenderedOnceAndCleared(t *testing.T) {
	redirect := httptest.NewRecorder()
	redirectWithFlash(redirect, httptest.NewRequest(http.MethodPost, "/projects", nil), "/projects", "success", "Project created.")
	cookie := redirect.Result().Cookies()[0]

	firstRequest := httptest.NewRequest(http.MethodGet, "/projects", nil)
	firstRequest.AddCookie(cookie)
	firstResponse := httptest.NewRecorder()
	flash := flashFromCookie(firstResponse, firstRequest)
	render(firstResponse, firstRequest, pages.Projects(nil, store.Organization{}, store.User{}, flash))

	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first GET status = %d, want %d", firstResponse.Code, http.StatusOK)
	}
	if count := strings.Count(firstResponse.Body.String(), "Project created."); count != 1 {
		t.Fatalf("first GET flash count = %d, want 1", count)
	}
	cleared := firstResponse.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != flashCookieName || cleared[0].MaxAge != -1 {
		t.Fatalf("cleared cookies = %+v", cleared)
	}

	secondRequest := httptest.NewRequest(http.MethodGet, "/projects", nil)
	secondResponse := httptest.NewRecorder()
	secondFlash := flashFromCookie(secondResponse, secondRequest)
	render(secondResponse, secondRequest, pages.Projects(nil, store.Organization{}, store.User{}, secondFlash))

	if secondFlash != nil {
		t.Fatalf("second GET flash = %+v, want nil", secondFlash)
	}
	if strings.Contains(secondResponse.Body.String(), "Project created.") {
		t.Fatal("second GET rendered consumed flash")
	}
	if cookies := secondResponse.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("second GET cookies = %+v, want none", cookies)
	}
}

func TestFlashFromCookieRejectsMalformedValueAndClearsIt(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/projects", nil)
	request.AddCookie(&http.Cookie{Name: flashCookieName, Value: "not-base64"})
	recorder := httptest.NewRecorder()

	if flash := flashFromCookie(recorder, request); flash != nil {
		t.Fatalf("flash = %+v, want nil", flash)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge != -1 {
		t.Fatalf("cleared cookies = %+v", cookies)
	}
}

func TestFlashFromCookieIgnoresEmptyValue(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/projects", nil)
	request.AddCookie(&http.Cookie{Name: flashCookieName})
	recorder := httptest.NewRecorder()

	if flash := flashFromCookie(recorder, request); flash != nil {
		t.Fatalf("flash = %+v, want nil", flash)
	}
	if cookies := recorder.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("cookies = %+v, want none", cookies)
	}
}
