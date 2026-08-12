package uihandler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRedirectWithFlashUsesCookieAndCleanLocation(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/settings/user/profile", nil)

	redirectWithFlash(recorder, request, "/settings/user", "success", "Profile updated.")

	response := recorder.Result()
	if location := response.Header.Get("Location"); location != "/settings/user" {
		t.Fatalf("Location = %q", location)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 || cookies[0].Name != flashCookieName || !cookies[0].HttpOnly {
		t.Fatalf("flash cookies = %+v", cookies)
	}
}

func TestFlashFromCookieConsumesMessage(t *testing.T) {
	redirect := httptest.NewRecorder()
	redirectWithFlash(redirect, httptest.NewRequest(http.MethodPost, "/", nil), "/settings/user", "success", "Profile updated.")
	cookie := redirect.Result().Cookies()[0]

	request := httptest.NewRequest(http.MethodGet, "/settings/user", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	flash := flashFromCookie(recorder, request)

	if flash == nil || flash.Kind != "success" || flash.Message != "Profile updated." {
		t.Fatalf("flash = %+v", flash)
	}
	cleared := recorder.Result().Cookies()
	if len(cleared) != 1 || cleared[0].MaxAge != -1 {
		t.Fatalf("cleared cookies = %+v", cleared)
	}
}
