package ui

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionCookiesAreSecure(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	created := httptest.NewRecorder()
	SetSession(created, "token", expires)
	createdCookies := created.Result().Cookies()
	if len(createdCookies) != 1 || !createdCookies[0].Secure || !createdCookies[0].HttpOnly || createdCookies[0].Path != sessionCookiePath || createdCookies[0].Expires.Unix() != expires.Unix() {
		t.Fatalf("created session cookie = %+v", createdCookies)
	}

	cleared := httptest.NewRecorder()
	ClearSession(cleared)
	clearedCookies := cleared.Result().Cookies()
	if len(clearedCookies) != 1 || !clearedCookies[0].Secure || !clearedCookies[0].HttpOnly || clearedCookies[0].Path != sessionCookiePath || clearedCookies[0].MaxAge != -1 {
		t.Fatalf("cleared session cookie = %+v", clearedCookies)
	}
}
