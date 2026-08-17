package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/shepard-labs/cyclopus/internal/ui"
)

type GitHubBrowser struct {
	service      githubBrowserService
	authenticate func(context.Context, string) (context.Context, error)
	externalURL  string
	callbackAEAD cipher.AEAD
}

type githubBrowserService interface {
	ValidateGitHubStart(context.Context, service.ActorContext, string, string) error
	GitHubManifest() ([]byte, error)
	GitHubInstallationURL() (string, error)
	CompleteGitHubManifest(context.Context, service.ActorContext, string, string) error
	CompleteGitHubInstallation(context.Context, service.ActorContext, string, int64) (store.GitHubInstallation, error)
	CheckpointGitHubCallback(context.Context, string, string) (service.GitHubCallbackIdentity, error)
}

func NewGitHubBrowser(domain *service.Service, authenticator *Authenticator, externalURL string, sealingKey []byte) http.Handler {
	authenticate := func(ctx context.Context, authorization string) (context.Context, error) {
		return authenticator.AuthenticateAuthorization(ctx, authorization)
	}
	return newGitHubBrowser(domain, authenticate, externalURL, sealingKey)
}

func newGitHubBrowser(domain githubBrowserService, authenticate func(context.Context, string) (context.Context, error), externalURL string, sealingKey []byte) http.Handler {
	h := &GitHubBrowser{service: domain, authenticate: authenticate, externalURL: strings.TrimRight(externalURL, "/"), callbackAEAD: newCallbackAEAD(sealingKey)}
	r := chi.NewRouter()
	r.Get("/app-manifest/start", h.authenticated(h.manifestStart))
	r.Get("/app-installation/start", h.authenticated(h.installationStart))
	r.Get("/app-manifest/callback", h.manifestCallbackCheckpoint)
	r.Get("/app-manifest/callback/continue", h.continuation(h.manifestCallbackContinue))
	r.Get("/app/setup", h.installationCallbackCheckpoint)
	r.Get("/app/setup/continue", h.continuation(h.installationCallbackContinue))
	return r
}

func (h *GitHubBrowser) continuation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setGitHubBrowserHeaders(w)
		token, ok := ui.GetSession(r)
		if !ok {
			h.redirectToLogin(w, r)
			return
		}
		ctx, err := h.authenticate(r.Context(), "Bearer "+token)
		if err != nil {
			h.redirectToLogin(w, r)
			return
		}
		actor, err := administratorActor(ctx)
		if err != nil {
			http.Error(w, "administrator permission required", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(contextWithGitHubActor(ctx, actor)))
	}
}

func (h *GitHubBrowser) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	returnTo := h.externalURL + r.URL.Path
	http.SetCookie(w, &http.Cookie{Name: "cyclopus_login_continuation", Value: r.URL.Path, Path: "/", MaxAge: 60, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, h.externalURL+"/login?return_to="+url.QueryEscape(returnTo), http.StatusSeeOther)
}

// LoginContinuationMiddleware restores only the two fixed, query-free GitHub
// continuation paths after the existing UI login handler issues its normal
// successful redirect. The cookie is a short-lived routing hint, not authority.
func LoginContinuationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("cyclopus_login_continuation")
		if err != nil || r.Method != http.MethodPost || r.URL.Path != "/login" || !validContinuationPath(cookie.Value) {
			next.ServeHTTP(w, r)
			return
		}
		wrapped := &continuationResponseWriter{ResponseWriter: w, path: cookie.Value}
		next.ServeHTTP(wrapped, r)
	})
}

type continuationResponseWriter struct {
	http.ResponseWriter
	path string
}

func (w *continuationResponseWriter) WriteHeader(status int) {
	if status == http.StatusSeeOther && w.Header().Get("Location") == "/projects" {
		w.Header().Set("Location", w.path)
		http.SetCookie(w, &http.Cookie{Name: "cyclopus_login_continuation", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}
	w.ResponseWriter.WriteHeader(status)
}
func validContinuationPath(path string) bool {
	return path == "/github/app-manifest/callback/continue" || path == "/github/app/setup/continue"
}

func (h *GitHubBrowser) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setGitHubBrowserHeaders(w)
		token, ok := ui.GetSession(r)
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		ctx, err := h.authenticate(r.Context(), "Bearer "+token)
		if err != nil {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		actor, err := administratorActor(ctx)
		if err != nil {
			http.Error(w, "administrator permission required", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(contextWithGitHubActor(ctx, actor)))
	}
}

type githubActorKey struct{}

func contextWithGitHubActor(ctx context.Context, actor service.ActorContext) context.Context {
	return context.WithValue(ctx, githubActorKey{}, actor)
}
func githubActor(ctx context.Context) service.ActorContext {
	return ctx.Value(githubActorKey{}).(service.ActorContext)
}

func setGitHubBrowserHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; form-action https://github.com; base-uri 'none'; frame-ancestors 'none'")
}
func (h *GitHubBrowser) manifestStart(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" || h.service.ValidateGitHubStart(r.Context(), githubActor(r.Context()), state, "app_setup") != nil {
		http.Error(w, "invalid state", 400)
		return
	}
	manifest, err := h.service.GitHubManifest()
	if err != nil {
		http.Error(w, "GitHub setup unavailable", 409)
		return
	}
	renderAutoSubmit(w, "https://github.com/settings/apps/new", map[string]string{"state": state, "manifest": string(manifest)})
}
func (h *GitHubBrowser) installationStart(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	target, err := h.service.GitHubInstallationURL()
	if err != nil || state == "" || h.service.ValidateGitHubStart(r.Context(), githubActor(r.Context()), state, "installation") != nil {
		http.Error(w, "GitHub installation unavailable", 409)
		return
	}
	u, _ := url.Parse(target)
	q := u.Query()
	q.Set("state", state)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}
func (h *GitHubBrowser) manifestCallbackCheckpoint(w http.ResponseWriter, r *http.Request) {
	setGitHubBrowserHeaders(w)
	code, state := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	if !boundedCallbackValue(code, 1024) || !boundedCallbackValue(state, 512) {
		http.Redirect(w, r, h.externalURL+"/settings/git-connections?github=setup_failed", http.StatusSeeOther)
		return
	}
	identity, err := h.service.CheckpointGitHubCallback(r.Context(), state, "app_setup")
	if err != nil || !h.setCallbackCookie(w, "cyclopus_github_manifest", "/github/app-manifest/callback/continue", map[string]string{"code": code, "state": state, "user_id": identity.UserID, "session_hash": identity.SessionHash}) {
		return
	}
	http.Redirect(w, r, h.externalURL+"/github/app-manifest/callback/continue", http.StatusSeeOther)
}
func (h *GitHubBrowser) manifestCallbackContinue(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("cyclopus_github_manifest")
	code, state := "", ""
	if err == nil {
		values, decodeErr := h.decodeCallbackCookie(cookie.Value, "cyclopus_github_manifest", "/github/app-manifest/callback/continue")
		if decodeErr == nil {
			actor := githubActor(r.Context())
			if values["user_id"] != actor.UserID || values["session_hash"] != digestBrowserSession(actor.SessionID) {
				decodeErr = errors.New("callback identity mismatch")
			} else {
				code, state = values["code"], values["state"]
			}
		} else {
			err = decodeErr
		}
	}
	deleteCallbackCookie(w, "cyclopus_github_manifest", "/github/app-manifest/callback/continue")
	if err == nil {
		err = h.service.CompleteGitHubManifest(r.Context(), githubActor(r.Context()), state, code)
	}
	clean := h.externalURL + "/settings/git-connections"
	if err != nil {
		clean += "?github=setup_failed"
	}
	http.Redirect(w, r, clean, http.StatusSeeOther)
}
func (h *GitHubBrowser) installationCallbackCheckpoint(w http.ResponseWriter, r *http.Request) {
	setGitHubBrowserHeaders(w)
	id, action, state := r.URL.Query().Get("installation_id"), r.URL.Query().Get("setup_action"), r.URL.Query().Get("state")
	parsedID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || parsedID <= 0 || (action != "install" && action != "update") || !boundedCallbackValue(state, 512) {
		http.Redirect(w, r, h.externalURL+"/settings/git-connections?github=installation_failed", http.StatusSeeOther)
		return
	}
	identity, err := h.service.CheckpointGitHubCallback(r.Context(), state, "installation")
	if err != nil || !h.setCallbackCookie(w, "cyclopus_github_installation", "/github/app/setup/continue", map[string]string{"installation_id": id, "setup_action": action, "state": state, "user_id": identity.UserID, "session_hash": identity.SessionHash}) {
		return
	}
	http.Redirect(w, r, h.externalURL+"/github/app/setup/continue", http.StatusSeeOther)
}
func (h *GitHubBrowser) installationCallbackContinue(w http.ResponseWriter, r *http.Request) {
	cookie, cookieErr := r.Cookie("cyclopus_github_installation")
	payload := map[string]string{}
	if cookieErr == nil {
		payload, cookieErr = h.decodeCallbackCookie(cookie.Value, "cyclopus_github_installation", "/github/app/setup/continue")
	}
	actor := githubActor(r.Context())
	if cookieErr == nil && (payload["user_id"] != actor.UserID || payload["session_hash"] != digestBrowserSession(actor.SessionID)) {
		cookieErr = errors.New("callback identity mismatch")
	}
	idRaw, action, state := payload["installation_id"], payload["setup_action"], payload["state"]
	deleteCallbackCookie(w, "cyclopus_github_installation", "/github/app/setup/continue")
	if cookieErr != nil || (action != "install" && action != "update") || state == "" {
		http.Redirect(w, r, h.externalURL+"/settings/git-connections?github=installation_failed", http.StatusSeeOther)
		return
	}
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err == nil {
		_, err = h.service.CompleteGitHubInstallation(r.Context(), githubActor(r.Context()), state, id)
	}
	clean := h.externalURL + "/settings/git-connections"
	if err != nil {
		clean += "?github=installation_failed"
	}
	http.Redirect(w, r, clean, http.StatusSeeOther)
}

func (h *GitHubBrowser) setCallbackCookie(w http.ResponseWriter, name, path string, values map[string]string) bool {
	aead := h.callbackAEAD
	if aead == nil {
		return false
	}
	encoded, _ := json.Marshal(values)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		http.Error(w, "GitHub callback unavailable", http.StatusServiceUnavailable)
		return false
	}
	sealed := aead.Seal(nonce, nonce, encoded, []byte(name+"\x00"+path))
	http.SetCookie(w, &http.Cookie{Name: name, Value: base64.RawURLEncoding.EncodeToString(sealed), Path: path, MaxAge: 60, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	return true
}

func (h *GitHubBrowser) decodeCallbackCookie(value, name, path string) (map[string]string, error) {
	aead := h.callbackAEAD
	if aead == nil {
		return nil, errors.New("callback sealing unavailable")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	if len(raw) < aead.NonceSize() {
		return nil, errors.New("invalid callback checkpoint")
	}
	opened, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(name+"\x00"+path))
	if err != nil {
		return nil, errors.New("invalid callback checkpoint")
	}
	var values map[string]string
	if err := json.Unmarshal(opened, &values); err != nil {
		return nil, err
	}
	return values, nil
}
func newCallbackAEAD(sealingKey []byte) cipher.AEAD {
	if len(sealingKey) < 32 {
		return nil
	}
	mac := hmac.New(sha256.New, sealingKey)
	_, _ = mac.Write([]byte("cyclopus-github-callback-v1"))
	key := mac.Sum(nil)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil
	}
	aead, _ := cipher.NewGCM(block)
	return aead
}
func digestBrowserSession(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func boundedCallbackValue(value string, max int) bool { return value != "" && len(value) <= max }
func deleteCallbackCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: path, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}
func renderAutoSubmit(w http.ResponseWriter, action string, fields map[string]string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	const page = `<!doctype html><meta charset="utf-8"><title>Continue to GitHub</title><form method="post" action="{{.Action}}">{{range $k,$v:=.Fields}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}<noscript><button type="submit">Continue</button></noscript></form><script>document.forms[0].submit()</script>`
	_ = template.Must(template.New("github").Parse(page)).Execute(w, map[string]any{"Action": action, "Fields": fields})
}
