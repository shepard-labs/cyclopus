package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	internalauth "github.com/shepard-labs/cyclopus/internal/auth"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/shepard-labs/cyclopus/internal/ui"
)

func TestGitHubBrowserRoutesRejectWrongMethodsAndSetSecurityHeaders(t *testing.T) {
	handler := NewGitHubBrowser(nil, nil, "https://cyclopus.example", []byte(strings.Repeat("k", 32)))
	for _, path := range []string{"/app-manifest/start", "/app-installation/start", "/app-manifest/callback/continue", "/app/setup/continue"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		want := http.StatusUnauthorized
		if strings.HasSuffix(path, "/continue") {
			want = http.StatusSeeOther
		}
		if response.Code != want {
			t.Fatalf("GET %s status = %d, want %d", path, response.Code, want)
		}
		if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" || !strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action https://github.com") {
			t.Fatalf("GET %s security headers = %v", path, response.Header())
		}
		post := httptest.NewRequest(http.MethodPost, path, nil)
		postResponse := httptest.NewRecorder()
		handler.ServeHTTP(postResponse, post)
		if postResponse.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status = %d, want 405", path, postResponse.Code)
		}
	}
}

func TestGitHubBrowserAuthenticatedManifestFlowAndCleanCallback(t *testing.T) {
	state, code := strings.Repeat("s", 64), "manifest.code.with.dots"
	fake := &fakeGitHubBrowserService{manifest: []byte(`{"public":true}`)}
	handler := newGitHubBrowser(fake, browserAuthenticator(true), "https://cyclopus.example", []byte(strings.Repeat("k", 32)))
	for name, cookie := range map[string]string{"unauthenticated": "", "invalid": "invalid"} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/app-manifest/start?state="+state, nil)
			if cookie != "" {
				request.AddCookie(&http.Cookie{Name: "cyclopus_session", Value: cookie})
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d", response.Code)
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "/app-manifest/start?state="+url.QueryEscape(state), nil)
	ui.SetSession(httptest.NewRecorder(), "admin", serviceTime())
	request.AddCookie(&http.Cookie{Name: "cyclopus_session", Value: "admin"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || fake.startState != state || !strings.Contains(body, `action="https://github.com/settings/apps/new"`) || !strings.Contains(body, `name="state"`) {
		t.Fatalf("start = %d %s", response.Code, body)
	}
	callback := httptest.NewRequest(http.MethodGet, "/app-manifest/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	callback.Host = "spoof.example"
	callback.Header.Set("X-Forwarded-Host", "spoof-forwarded.example")
	checkpoint := httptest.NewRecorder()
	handler.ServeHTTP(checkpoint, callback)
	if checkpoint.Code != http.StatusSeeOther || checkpoint.Header().Get("Location") != "https://cyclopus.example/github/app-manifest/callback/continue" || strings.Contains(checkpoint.Header().Get("Location"), code) || strings.Contains(checkpoint.Header().Get("Location"), state) {
		t.Fatalf("checkpoint = %d %s", checkpoint.Code, checkpoint.Header().Get("Location"))
	}
	cookies := checkpoint.Result().Cookies()
	if len(cookies) == 0 || strings.Contains(cookies[0].Value, code) || strings.Contains(cookies[0].Value, state) {
		t.Fatalf("callback cookie was not encoded: %+v", cookies)
	}
	tampered := *cookies[0]
	tampered.Value = tampered.Value[:len(tampered.Value)/2] + "A" + tampered.Value[len(tampered.Value)/2+1:]
	tamperedRequest := httptest.NewRequest(http.MethodGet, "/app-manifest/callback/continue", nil)
	tamperedRequest.AddCookie(&http.Cookie{Name: "cyclopus_session", Value: "admin"})
	tamperedRequest.AddCookie(&tampered)
	tamperedResponse := httptest.NewRecorder()
	handler.ServeHTTP(tamperedResponse, tamperedRequest)
	if tamperedResponse.Header().Get("Location") != "https://cyclopus.example/settings/git-connections?github=setup_failed" {
		t.Fatalf("tampered checkpoint redirect = %q", tamperedResponse.Header().Get("Location"))
	}
	clean := httptest.NewRequest(http.MethodGet, "/app-manifest/callback/continue", nil)
	clean.AddCookie(&http.Cookie{Name: "cyclopus_session", Value: "admin"})
	clean.AddCookie(cookies[0])
	final := httptest.NewRecorder()
	handler.ServeHTTP(final, clean)
	if final.Code != http.StatusSeeOther || final.Header().Get("Location") != "https://cyclopus.example/settings/git-connections" || fake.completeCode != code || fake.completeState != state {
		t.Fatalf("final=%d %s calls=%q/%q", final.Code, final.Header().Get("Location"), fake.completeCode, fake.completeState)
	}
}

func TestGitHubBrowserAuthorizationAndInstallationCallbackValidation(t *testing.T) {
	fake := &fakeGitHubBrowserService{installationURL: "https://github.com/apps/cyclopus/installations/new"}
	handler := newGitHubBrowser(fake, browserAuthenticator(false), "https://cyclopus.example", []byte(strings.Repeat("k", 32)))
	request := httptest.NewRequest(http.MethodGet, "/app-installation/start?state="+strings.Repeat("x", 64), nil)
	request.AddCookie(&http.Cookie{Name: "cyclopus_session", Value: "member"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-admin status=%d", response.Code)
	}
	handler = newGitHubBrowser(fake, browserAuthenticator(true), "https://cyclopus.example", []byte(strings.Repeat("k", 32)))
	for _, query := range []string{"installation_id=0&setup_action=install&state=x", "installation_id=42&setup_action=delete&state=x", "installation_id=42&setup_action=install"} {
		callback := httptest.NewRequest(http.MethodGet, "/app/setup?"+query, nil)
		callback.AddCookie(&http.Cookie{Name: "cyclopus_session", Value: "admin"})
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, callback)
		if result.Header().Get("Location") != "https://cyclopus.example/settings/git-connections?github=installation_failed" {
			t.Fatalf("query %q redirect=%q", query, result.Header().Get("Location"))
		}
	}
	missing := httptest.NewRequest(http.MethodGet, "/app/setup/continue", nil)
	missing.AddCookie(&http.Cookie{Name: "cyclopus_session", Value: "admin"})
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, missing)
	if result.Code != http.StatusSeeOther {
		t.Fatalf("missing cookie status=%d", result.Code)
	}
	initial := httptest.NewRequest(http.MethodGet, "/app/setup?installation_id=42&setup_action=install&state=state.value", nil)
	checkpoint := httptest.NewRecorder()
	handler.ServeHTTP(checkpoint, initial)
	clean := httptest.NewRequest(http.MethodGet, "/app/setup/continue", nil)
	clean.AddCookie(&http.Cookie{Name: "cyclopus_session", Value: "admin"})
	clean.AddCookie(checkpoint.Result().Cookies()[0])
	final := httptest.NewRecorder()
	handler.ServeHTTP(final, clean)
	if final.Header().Get("Location") != "https://cyclopus.example/settings/git-connections" || fake.installationID != 42 || fake.installationState != "state.value" {
		t.Fatalf("installation final=%q call=%d/%q", final.Header().Get("Location"), fake.installationID, fake.installationState)
	}
}

func TestGitHubContinuationRedirectsThroughLoginAndRestoresCleanPath(t *testing.T) {
	fake := &fakeGitHubBrowserService{}
	handler := newGitHubBrowser(fake, browserAuthenticator(true), "https://cyclopus.example", []byte(strings.Repeat("k", 32)))
	request := httptest.NewRequest(http.MethodGet, "/app-manifest/callback/continue?code=secret", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	location := response.Header().Get("Location")
	if response.Code != http.StatusSeeOther || strings.Contains(location, "secret") || !strings.Contains(location, "/login?return_to=") {
		t.Fatalf("login redirect = %d %q", response.Code, location)
	}
	cookies := response.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Value != "/app-manifest/callback/continue" {
		t.Fatalf("continuation cookie = %+v", cookies)
	}
	login := LoginContinuationMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/projects")
		w.WriteHeader(http.StatusSeeOther)
	}))
	loginRequest := httptest.NewRequest(http.MethodPost, "/login", nil)
	loginRequest.AddCookie(&http.Cookie{Name: "cyclopus_login_continuation", Value: "/github/app-manifest/callback/continue"})
	loginResponse := httptest.NewRecorder()
	login.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Header().Get("Location") != "/github/app-manifest/callback/continue" {
		t.Fatalf("post-login redirect = %q", loginResponse.Header().Get("Location"))
	}
}

type fakeGitHubBrowserService struct {
	manifest                                                                    []byte
	installationURL, startState, completeCode, completeState, installationState string
	installationID                                                              int64
}

func (f *fakeGitHubBrowserService) ValidateGitHubStart(_ context.Context, _ service.ActorContext, state, _ string) error {
	f.startState = state
	if state == "" {
		return errors.New("invalid")
	}
	return nil
}
func (f *fakeGitHubBrowserService) GitHubManifest() ([]byte, error) { return f.manifest, nil }
func (f *fakeGitHubBrowserService) GitHubInstallationURL() (string, error) {
	return f.installationURL, nil
}
func (f *fakeGitHubBrowserService) CompleteGitHubManifest(_ context.Context, _ service.ActorContext, state, code string) error {
	f.completeState, f.completeCode = state, code
	return nil
}
func (f *fakeGitHubBrowserService) CompleteGitHubInstallation(_ context.Context, _ service.ActorContext, state string, id int64) (store.GitHubInstallation, error) {
	f.installationState, f.installationID = state, id
	return store.GitHubInstallation{GitHubInstallationID: id}, nil
}
func (f *fakeGitHubBrowserService) CheckpointGitHubCallback(_ context.Context, state, _ string) (service.GitHubCallbackIdentity, error) {
	if state == "" {
		return service.GitHubCallbackIdentity{}, errors.New("invalid")
	}
	return service.GitHubCallbackIdentity{UserID: "user", SessionHash: digestBrowserSession("session")}, nil
}
func browserAuthenticator(admin bool) func(context.Context, string) (context.Context, error) {
	return func(ctx context.Context, authorization string) (context.Context, error) {
		if authorization != "Bearer admin" && authorization != "Bearer member" {
			return nil, errors.New("unauthenticated")
		}
		actor := service.ActorContext{UserID: "user", OrgID: "org", SessionID: "session", Operator: admin}
		ctx = context.WithValue(ctx, principalContextKey, internalauth.Principal{Kind: internalauth.CredentialKindSession, Operator: admin})
		return context.WithValue(ctx, actorContextKey, actor), nil
	}
}
func serviceTime() time.Time { return time.Now().Add(time.Hour) }

func TestRenderAutoSubmitEscapesSecretsAndTargetsGitHub(t *testing.T) {
	response := httptest.NewRecorder()
	renderAutoSubmit(response, "https://github.com/settings/apps/new", map[string]string{"state": `state"><script>evil()</script>`, "manifest": `{"public":true}`})
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `action="https://github.com/settings/apps/new"`) || !strings.Contains(body, "document.forms[0].submit()") {
		t.Fatalf("auto-submit response = %d %s", response.Code, body)
	}
	if strings.Contains(body, "<script>evil()") || !strings.Contains(body, "&lt;script&gt;evil()") {
		t.Fatalf("auto-submit did not HTML-escape state: %s", body)
	}
}
