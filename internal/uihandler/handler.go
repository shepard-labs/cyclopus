package uihandler

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
	internalauth "github.com/shepard-labs/cyclopus/internal/auth"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/shepard-labs/cyclopus/internal/ui"
	"github.com/shepard-labs/cyclopus/internal/ui/pages"
	adminpages "github.com/shepard-labs/cyclopus/internal/ui/pages/admin"
	settingspages "github.com/shepard-labs/cyclopus/internal/ui/pages/settings"
)

// Handler is the chi router serving all Cyclopus UI routes.
type Handler struct {
	client   *ui.Backend
	loadLogs logsPageLoader
}

type logsPageData struct {
	app        store.App
	project    store.Project
	entries    []observability.LogEntry
	nextCursor string
	envID      string
	query      string
	user       store.User
	org        store.Organization
}

type logsPageLoader func(context.Context, string, string, string, string, string) (logsPageData, error)

// New constructs the UI http.Handler. The backend resolves UI reads and writes
// in-process against the service/auth layers.
func New(client *ui.Backend) http.Handler {
	return newHandler(client, nil)
}

func newHandler(client *ui.Backend, loadLogs logsPageLoader) http.Handler {
	h := &Handler{client: client, loadLogs: loadLogs}
	if h.loadLogs == nil {
		h.loadLogs = h.loadLogsPage
	}
	r := chi.NewRouter()
	r.Use(chimiddleware.Recoverer)

	// Static assets (htmx, app.css) — embed.FS served at /static/*
	r.Handle("/static/*", ui.StaticHandler())
	r.Handle("/avatars/*", ui.AvatarHandler())

	// Public routes — no session required
	r.Get("/login", h.getLogin)
	r.Post("/login", h.postLogin)
	r.Get("/bootstrap", h.getBootstrap)
	r.Post("/bootstrap", h.postBootstrap)
	r.Post("/logout", h.postLogout)

	// Protected routes — authMiddleware redirects to /login if no session
	r.Group(func(r chi.Router) {
		r.Use(ui.AuthMiddleware)
		r.Get("/", h.getRoot)

		// Projects
		r.Get("/projects", h.getProjects)
		r.Post("/projects", h.postProject)
		r.Get("/projects/{projectID}", h.getProject)
		r.Post("/projects/{projectID}/apps", h.postApp)

		// Apps
		r.Get("/apps/{appID}", h.getApp)
		r.Get("/apps/{appID}/settings", h.getAppSettings)
		r.Post("/apps/{appID}/settings", h.postAppSettings)
		r.Get("/apps/{appID}/logs", h.getLogs)

		// Environments
		r.Get("/environments/{envID}", h.getEnvironment)
		r.Post("/environments/{envID}/rollback", h.postRollback)
		r.Get("/environments/{envID}/status-fragment", h.getEnvStatusFragment)

		// Settings
		r.Get("/settings/user", h.getUserSettings)
		r.Post("/settings/user/profile", h.postUserProfile)
		r.Post("/settings/user/password", h.postUserPassword)
		r.Get("/settings/theme", h.getTheme)

		r.Get("/settings/git-connections", h.getGitConnections)

		r.Get("/settings/registries", h.getRegistries)

		r.Get("/settings/webhooks", h.getWebhooks)
		r.Post("/settings/webhooks", h.postWebhook)
		r.Post("/settings/webhooks/{id}/delete", h.deleteWebhook)

		r.Get("/settings/tokens", h.getTokens)
		r.Post("/settings/tokens", h.postToken)
		r.Post("/settings/tokens/{id}/revoke", h.revokeToken)

		r.Get("/settings/audit", h.getAudit)

		// Admin
		r.Get("/admin/reconcile", h.getReconcile)
		r.Post("/admin/reconcile", h.postReconcile)
		r.Get("/admin/metrics", h.getMetrics)
	})

	return r
}

func (h *Handler) getUserSettings(w http.ResponseWriter, r *http.Request) {
	token, _ := ui.GetSession(r)
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	render(w, r, settingspages.User(user, org, flashFromCookie(w, r), "", "", ""))
}

func (h *Handler) getTheme(w http.ResponseWriter, r *http.Request) {
	token, _ := ui.GetSession(r)
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	render(w, r, settingspages.Theme(user, org, flashFromCookie(w, r)))
}

func (h *Handler) postUserProfile(w http.ResponseWriter, r *http.Request) {
	if !trustedFormOrigin(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	token, _ := ui.GetSession(r)
	current, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	submitted := current
	action := r.FormValue("action")
	switch action {
	case "avatar":
		submitted.AvatarKey = r.FormValue("avatar_key")
	case "email":
		submitted.Email = r.FormValue("email")
	default:
		submitted.FirstName = r.FormValue("first_name")
		submitted.LastName = r.FormValue("last_name")
		submitted.DisplayName = r.FormValue("display_name")
	}
	_, err = h.client.UpdateCurrentUser(r.Context(), token, internalauth.UpdateProfileParams{
		Email: submitted.Email, FirstName: submitted.FirstName, LastName: submitted.LastName,
		DisplayName: submitted.DisplayName, AvatarKey: submitted.AvatarKey, CurrentPassword: r.FormValue("current_password"),
	})
	if err != nil {
		openDialog := ""
		if action == "avatar" || action == "email" {
			openDialog = action
		}
		render(w, r, settingspages.User(submitted, org, nil, profileErrorMessage(err), "", openDialog))
		return
	}
	message := "Profile updated."
	if action == "avatar" {
		message = "Avatar updated."
	} else if action == "email" {
		message = "Email updated."
	}
	redirectWithFlash(w, r, "/settings/user", "success", message)
}

func (h *Handler) postUserPassword(w http.ResponseWriter, r *http.Request) {
	if !trustedFormOrigin(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	token, _ := ui.GetSession(r)
	newPassword := r.FormValue("new_password")
	if newPassword != r.FormValue("confirm_password") {
		user, org, err := h.pageCtx(r.Context(), token)
		if err != nil {
			h.handleErr(w, r, err)
			return
		}
		render(w, r, settingspages.User(user, org, nil, "", "New passwords do not match.", "password"))
		return
	}
	if err := h.client.ChangePassword(r.Context(), token, r.FormValue("current_password"), newPassword); err != nil {
		user, org, pageErr := h.pageCtx(r.Context(), token)
		if pageErr != nil {
			h.handleErr(w, r, pageErr)
			return
		}
		render(w, r, settingspages.User(user, org, nil, "", passwordErrorMessage(err), "password"))
		return
	}
	redirectWithFlash(w, r, "/settings/user", "success", "Password changed. Other sessions were signed out.")
}

func profileErrorMessage(err error) string {
	if errors.Is(err, internalauth.ErrUnauthenticated) {
		return "Current password is incorrect."
	}
	if errors.Is(err, store.ErrAlreadyExists) || errors.Is(err, store.ErrConflict) {
		return "That email address is already in use."
	}
	if errors.Is(err, internalauth.ErrInvalidCredential) {
		return "Check the profile fields and selected avatar."
	}
	if errors.Is(err, internalauth.ErrRateLimited) {
		return "Too many attempts. Wait a minute and try again."
	}
	return "Profile update failed. Try again."
}

func passwordErrorMessage(err error) string {
	if errors.Is(err, internalauth.ErrUnauthenticated) {
		return "Current password is incorrect."
	}
	if errors.Is(err, internalauth.ErrInvalidCredential) {
		return "New password must contain at least 8 characters."
	}
	if errors.Is(err, internalauth.ErrRateLimited) {
		return "Too many attempts. Wait a minute and try again."
	}
	if errors.Is(err, store.ErrConflict) {
		return "Your password changed in another session. Reload and try again."
	}
	return "Password change failed. Try again."
}

func trustedFormOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// pageCtx fetches the current user and their organization. Every protected
// page calls this to populate the layout.
func (h *Handler) pageCtx(ctx context.Context, token string) (store.User, store.Organization, error) {
	user, err := h.client.GetCurrentUser(ctx, token)
	if err != nil {
		return store.User{}, store.Organization{}, err
	}
	org, err := h.client.GetOrganization(ctx, token, user.OrgID)
	if err != nil {
		return store.User{}, store.Organization{}, err
	}
	return user, org, nil
}

// handleErr handles API errors: 401 clears the cookie and redirects to /login,
// everything else returns a plain 500.
func (h *Handler) handleErr(w http.ResponseWriter, r *http.Request, err error) {
	if ui.IsUnauthorized(err) {
		ui.ClearSession(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Error(w, "Error: "+err.Error(), http.StatusInternalServerError)
}

// newIdemKey generates a random idempotency key for each mutating request.
func newIdemKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const flashCookieName = "cyclopus_flash"

// flashFromCookie consumes the one-time notification set by redirectWithFlash.
func flashFromCookie(w http.ResponseWriter, r *http.Request) *ui.FlashMessage {
	cookie, err := r.Cookie(flashCookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	payload, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return nil
	}
	var flash ui.FlashMessage
	if json.Unmarshal(payload, &flash) != nil || flash.Message == "" {
		return nil
	}
	return &flash
}

// redirectWithFlash stores a one-time notification without exposing it in the URL.
func redirectWithFlash(w http.ResponseWriter, r *http.Request, path, kind, msg string) {
	payload, _ := json.Marshal(ui.FlashMessage{Kind: kind, Message: msg})
	http.SetCookie(w, &http.Cookie{
		Name: flashCookieName, Value: base64.RawURLEncoding.EncodeToString(payload), Path: "/",
		MaxAge: 60, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// render writes Content-Type and renders a templ component.
// http.ResponseWriter satisfies io.Writer so component.Render accepts it.
func render(w http.ResponseWriter, r *http.Request, component templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := component.Render(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ── Auth handlers ─────────────────────────────────────────────────────────────

func (h *Handler) getLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := ui.GetSession(r); ok {
		http.Redirect(w, r, "/projects", http.StatusSeeOther)
		return
	}
	render(w, r, pages.Login(""))
}

func (h *Handler) postLogin(w http.ResponseWriter, r *http.Request) {
	email := r.FormValue("email")
	password := r.FormValue("password")
	sess, err := h.client.Login(r.Context(), email, password)
	if err != nil {
		render(w, r, pages.Login(loginErrorMessage(err)))
		return
	}
	ui.SetSession(w, sess.AccessToken, sess.ExpiresAt)
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// loginErrorMessage maps an API error to a message the operator can act on.
// A 401 is a genuine bad credential; anything else (e.g. 503 while the control
// plane is still starting) should say so rather than blame the credentials.
func loginErrorMessage(err error) string {
	if ui.IsUnauthorized(err) {
		return "Invalid email or password."
	}
	return "Sign in failed: " + err.Error()
}

func (h *Handler) getBootstrap(w http.ResponseWriter, r *http.Request) {
	if _, ok := ui.GetSession(r); ok {
		http.Redirect(w, r, "/projects", http.StatusSeeOther)
		return
	}
	render(w, r, pages.Bootstrap(""))
}

func (h *Handler) postBootstrap(w http.ResponseWriter, r *http.Request) {
	email := r.FormValue("email")
	password := r.FormValue("password")
	bootstrapToken := r.FormValue("bootstrap_token")
	sess, err := h.client.Bootstrap(r.Context(), email, password, bootstrapToken)
	if err != nil {
		render(w, r, pages.Bootstrap("Setup failed: "+err.Error()))
		return
	}
	ui.SetSession(w, sess.AccessToken, sess.ExpiresAt)
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

func (h *Handler) postLogout(w http.ResponseWriter, r *http.Request) {
	if token, ok := ui.GetSession(r); ok {
		_ = h.client.Logout(r.Context(), token)
	}
	ui.ClearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) getRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// ── Projects & Apps ───────────────────────────────────────────────────────────

func (h *Handler) getProjects(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	projects, _, err := h.client.ListProjects(r.Context(), token, org.ID, "")
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	render(w, r, pages.Projects(projects, org, user, flashFromCookie(w, r), ""))
}

func (h *Handler) postProject(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	name := r.FormValue("name")
	slug := r.FormValue("slug")
	proj, err := h.client.CreateProject(r.Context(), token, org.ID, newIdemKey(), name, slug)
	if err != nil {
		projects, _, _ := h.client.ListProjects(r.Context(), token, org.ID, "")
		render(w, r, pages.Projects(projects, org, user, nil, err.Error()))
		return
	}
	redirectWithFlash(w, r, "/projects/"+proj.ID, "success", "Project created.")
}

func (h *Handler) getProject(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	projectID := chi.URLParam(r, "projectID")
	proj, err := h.client.GetProject(r.Context(), token, projectID)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	apps, _, err := h.client.ListApplications(r.Context(), token, projectID, "")
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	render(w, r, pages.ProjectDetail(*proj, apps, user, org, flashFromCookie(w, r), ""))
}

func (h *Handler) postApp(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	projectID := chi.URLParam(r, "projectID")
	name := r.FormValue("name")
	slug := r.FormValue("slug")
	hostnameLabel := r.FormValue("hostname_label")
	port := 8080
	if v := r.FormValue("container_port"); v != "" {
		if p, e := fmt.Sscanf(v, "%d", &port); e != nil || p != 1 {
			port = 8080
		}
	}
	input := service.AppInput{Name: name, Slug: slug, HostnameLabel: hostnameLabel, ContainerPort: port, HealthCheckPath: "/", ProjectID: projectID}
	created, err := h.client.CreateApplication(r.Context(), token, projectID, newIdemKey(), input)
	if err != nil {
		proj, _ := h.client.GetProject(r.Context(), token, projectID)
		apps, _, _ := h.client.ListApplications(r.Context(), token, projectID, "")
		if proj == nil {
			proj = &store.Project{ID: projectID}
		}
		render(w, r, pages.ProjectDetail(*proj, apps, user, org, nil, err.Error()))
		return
	}
	redirectWithFlash(w, r, "/apps/"+created.ID, "success", "Application created.")
}

func (h *Handler) getApp(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	appID := chi.URLParam(r, "appID")
	app, err := h.client.GetApplication(r.Context(), token, appID)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	proj, _ := h.client.GetProject(r.Context(), token, app.ProjectID)
	if proj == nil {
		proj = &store.Project{ID: app.ProjectID}
	}
	envs, err := h.client.ListEnvironments(r.Context(), token, appID, false)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	var prodEnv *store.Environment
	var previews []store.Environment
	for i := range envs {
		if envs[i].Kind == "production" {
			e := envs[i]
			prodEnv = &e
		} else {
			previews = append(previews, envs[i])
		}
	}
	var prodAlias *store.Alias
	var prodRelease *store.Release
	if prodEnv != nil {
		if aliases, e := h.client.ListAliases(r.Context(), token, prodEnv.ID); e == nil && len(aliases) > 0 {
			prodAlias = &aliases[0]
		}
		if prodEnv.CurrentReleaseID != nil {
			prodRelease, _ = h.client.GetRelease(r.Context(), token, *prodEnv.CurrentReleaseID)
		}
	}
	render(w, r, pages.AppOverview(*app, *proj, prodEnv, prodAlias, prodRelease, previews, user, org, flashFromCookie(w, r)))
}

func (h *Handler) getAppSettings(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	appID := chi.URLParam(r, "appID")
	app, err := h.client.GetApplication(r.Context(), token, appID)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	proj, _ := h.client.GetProject(r.Context(), token, app.ProjectID)
	if proj == nil {
		proj = &store.Project{ID: app.ProjectID}
	}
	render(w, r, pages.AppSettings(*app, *proj, user, org, flashFromCookie(w, r), ""))
}

func (h *Handler) postAppSettings(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	appID := chi.URLParam(r, "appID")
	app, err := h.client.GetApplication(r.Context(), token, appID)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	proj, _ := h.client.GetProject(r.Context(), token, app.ProjectID)
	if proj == nil {
		proj = &store.Project{ID: app.ProjectID}
	}

	fields := map[string]any{}
	paths := []string{}
	setField := func(key, val string) {
		if val != "" {
			fields[key] = val
			paths = append(paths, key)
		}
	}
	setField("name", r.FormValue("name"))
	setField("hostname_label", r.FormValue("hostname_label"))
	setField("health_check_path", r.FormValue("health_check_path"))
	setField("github_workflow_path", r.FormValue("github_workflow_path"))
	setField("image_repository", r.FormValue("image_repository"))
	setField("scaling_mode", r.FormValue("scaling_mode"))
	for _, key := range []string{"container_port", "min_replicas", "max_replicas", "hpa_target_cpu_percent",
		"cpu_request_millicores", "cpu_limit_millicores", "memory_request_mib", "memory_limit_mib",
		"max_preview_environments", "preview_idle_ttl_hours", "deploy_timeout_seconds", "release_retention_count"} {
		if v := r.FormValue(key); v != "" {
			var n int
			scanned, _ := fmt.Sscanf(v, "%d", &n)
			if scanned == 1 {
				fields[key] = n
				paths = append(paths, key)
			}
		}
	}
	// checkbox
	fields["previews_enabled"] = r.FormValue("previews_enabled") == "true" || r.FormValue("previews_enabled") == "on"
	paths = append(paths, "previews_enabled")

	updated, err := h.client.UpdateApplication(r.Context(), token, newIdemKey(), appID, fields, paths)
	if err != nil {
		render(w, r, pages.AppSettings(*app, *proj, user, org, nil, err.Error()))
		return
	}
	_ = updated
	redirectWithFlash(w, r, "/apps/"+appID+"/settings", "success", "Settings saved.")
}

func (h *Handler) getLogs(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	appID := chi.URLParam(r, "appID")
	envID := r.URL.Query().Get("env")
	cursor := r.URL.Query().Get("cursor")
	query := r.URL.Query().Get("q")
	data, err := h.loadLogs(r.Context(), token, appID, envID, cursor, query)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	if strings.EqualFold(r.Header.Get("HX-Request"), "true") && (cursor != "" || r.URL.Query().Get("load_more") == "true") {
		render(w, r, pages.LogsEntriesFragment(appID, data.entries, data.nextCursor, envID, query))
		return
	}
	render(w, r, pages.Logs(data.app, data.project, data.entries, data.nextCursor, data.envID, data.query, data.user, data.org, flashFromCookie(w, r)))
}

func (h *Handler) loadLogsPage(ctx context.Context, token, appID, envID, cursor, query string) (logsPageData, error) {
	user, org, err := h.pageCtx(ctx, token)
	if err != nil {
		return logsPageData{}, err
	}
	app, err := h.client.GetApplication(ctx, token, appID)
	if err != nil {
		return logsPageData{}, err
	}
	proj, _ := h.client.GetProject(ctx, token, app.ProjectID)
	if proj == nil {
		proj = &store.Project{ID: app.ProjectID}
	}
	entries, nextCursor, err := h.client.QueryLogs(ctx, token, appID, envID, cursor, query, 100)
	if err != nil {
		entries = nil
		nextCursor = ""
	}
	return logsPageData{app: *app, project: *proj, entries: entries, nextCursor: nextCursor, envID: envID, query: query, user: user, org: org}, nil
}

// ── Environments ──────────────────────────────────────────────────────────────

func (h *Handler) getEnvironment(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	envID := chi.URLParam(r, "envID")
	env, err := h.client.GetEnvironment(r.Context(), token, envID)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	app, err := h.client.GetApplication(r.Context(), token, env.AppID)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	proj, _ := h.client.GetProject(r.Context(), token, app.ProjectID)
	if proj == nil {
		proj = &store.Project{ID: app.ProjectID}
	}
	releases, _, _ := h.client.ListReleases(r.Context(), token, envID, "")
	aliases, _ := h.client.ListAliases(r.Context(), token, envID)
	certs, _ := h.client.ListCertificates(r.Context(), token, envID)
	domains, _ := h.client.ListDomains(r.Context(), token, app.ID)
	envVars, _ := h.client.GetEnvVars(r.Context(), token, envID)
	var alias *store.Alias
	if len(aliases) > 0 {
		a := aliases[0]
		alias = &a
	}
	render(w, r, pages.EnvironmentDetail(*env, *app, *proj, releases, alias, certs, domains, envVars, user, org, flashFromCookie(w, r)))
}

func (h *Handler) postRollback(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	envID := chi.URLParam(r, "envID")
	releaseID := r.FormValue("release_id")
	err := h.client.Rollback(r.Context(), token, newIdemKey(), envID, releaseID)
	if err != nil {
		redirectWithFlash(w, r, "/environments/"+envID, "error", "Rollback failed: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/environments/"+envID, "success", "Rollback initiated.")
}

func (h *Handler) getEnvStatusFragment(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	envID := chi.URLParam(r, "envID")
	env, err := h.client.GetEnvironment(r.Context(), token, envID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	aliases, _ := h.client.ListAliases(r.Context(), token, envID)
	var alias *store.Alias
	if len(aliases) > 0 {
		a := aliases[0]
		alias = &a
	}
	var release *store.Release
	if env.CurrentReleaseID != nil {
		release, _ = h.client.GetRelease(r.Context(), token, *env.CurrentReleaseID)
	}
	render(w, r, pages.EnvStatusFragment(*env, alias, release))
}

// ── Settings ──────────────────────────────────────────────────────────────────

func (h *Handler) getGitConnections(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	conns, _ := h.client.ListGitConnections(r.Context(), token, org.ID)
	render(w, r, settingspages.GitConnections(conns, user, org, flashFromCookie(w, r), ""))
}

func (h *Handler) getRegistries(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	regs, _ := h.client.ListRegistries(r.Context(), token, org.ID)
	render(w, r, settingspages.Registries(regs, user, org, flashFromCookie(w, r), ""))
}

func (h *Handler) getWebhooks(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	hooks, _ := h.client.ListWebhooks(r.Context(), token, org.ID)
	render(w, r, settingspages.Webhooks(hooks, user, org, flashFromCookie(w, r), ""))
}

func (h *Handler) postWebhook(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	var events []string
	if ev := r.FormValue("events"); ev != "" {
		for _, e := range strings.Split(ev, ",") {
			if t := strings.TrimSpace(e); t != "" {
				events = append(events, t)
			}
		}
	}
	wh := service.WebhookInput{TargetURL: r.FormValue("target_url"), Events: events, SecretRef: r.FormValue("secret_ref")}
	_, err = h.client.CreateWebhook(r.Context(), token, newIdemKey(), org.ID, wh)
	if err != nil {
		hooks, _ := h.client.ListWebhooks(r.Context(), token, org.ID)
		render(w, r, settingspages.Webhooks(hooks, user, org, nil, err.Error()))
		return
	}
	redirectWithFlash(w, r, "/settings/webhooks", "success", "Webhook added.")
}

func (h *Handler) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	id := chi.URLParam(r, "id")
	err := h.client.DeleteWebhook(r.Context(), token, newIdemKey(), id)
	if err != nil {
		redirectWithFlash(w, r, "/settings/webhooks", "error", "Delete failed: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/settings/webhooks", "success", "Webhook deleted.")
}

func (h *Handler) getTokens(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	tokens, _ := h.client.ListAPITokens(r.Context(), token)
	render(w, r, settingspages.Tokens(tokens, "", user, org, flashFromCookie(w, r), ""))
}

func (h *Handler) postToken(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	name := r.FormValue("name")
	var scopes []string
	if sv := r.FormValue("scopes"); sv != "" {
		for _, s := range strings.Split(sv, ",") {
			if t := strings.TrimSpace(s); t != "" {
				scopes = append(scopes, t)
			}
		}
	}
	secret, err := h.client.CreateAPIToken(r.Context(), token, newIdemKey(), name, scopes)
	if err != nil {
		tokens, _ := h.client.ListAPITokens(r.Context(), token)
		render(w, r, settingspages.Tokens(tokens, "", user, org, nil, err.Error()))
		return
	}
	tokens, _ := h.client.ListAPITokens(r.Context(), token)
	render(w, r, settingspages.Tokens(tokens, secret, user, org, nil, ""))
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	id := chi.URLParam(r, "id")
	err := h.client.RevokeAPIToken(r.Context(), token, newIdemKey(), id)
	if err != nil {
		redirectWithFlash(w, r, "/settings/tokens", "error", "Revoke failed: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/settings/tokens", "success", "Token revoked.")
}

func (h *Handler) getAudit(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	pageToken := r.URL.Query().Get("page_token")
	events, nextPageToken, _ := h.client.ListAuditEvents(r.Context(), token, org.ID, pageToken)
	render(w, r, settingspages.AuditLog(events, nextPageToken, user, org, flashFromCookie(w, r)))
}

// ── Admin ─────────────────────────────────────────────────────────────────────

func (h *Handler) getReconcile(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	runs, _ := h.client.ListReconciliations(r.Context(), token, org.ID)
	render(w, r, adminpages.Reconcile(runs, user, org, flashFromCookie(w, r)))
}

func (h *Handler) postReconcile(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	_, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	dryRun := r.FormValue("dry_run") == "true" || r.FormValue("dry_run") == "on"
	err = h.client.StartReconciliation(r.Context(), token, newIdemKey(), org.ID, dryRun)
	if err != nil {
		redirectWithFlash(w, r, "/admin/reconcile", "error", "Reconciliation failed: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/admin/reconcile", "success", "Reconciliation started.")
}

// metricsRangeFromRequest validates the `range` query param against the fixed
// chart windows. An empty or unrecognized value yields the default 24h window
// (and false for unrecognized, so tests can exercise that path).
func metricsRangeFromRequest(r *http.Request) (observability.MetricsTimeRangeID, bool) {
	raw := r.URL.Query().Get("range")
	if raw == "" {
		return observability.DefaultMetricsTimeRange, true
	}
	switch observability.MetricsTimeRangeID(raw) {
	case observability.MetricsRange1h, observability.MetricsRange12h, observability.MetricsRange24h, observability.MetricsRange1w:
		return observability.MetricsTimeRangeID(raw), true
	}
	return observability.DefaultMetricsTimeRange, false
}

func (h *Handler) getMetrics(w http.ResponseWriter, r *http.Request) {
	token := ui.TokenFromContext(r.Context())
	user, org, err := h.pageCtx(r.Context(), token)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	rangeID, _ := metricsRangeFromRequest(r)
	nodes, metricsErr := h.client.GetNodeSystemDetailsInRange(r.Context(), token, rangeID)
	var errMessage string
	if metricsErr != nil {
		errMessage = metricsErr.Error()
	}
	selectedNodeID := r.URL.Query().Get("node")

	if r.URL.Query().Get("fragment") == "true" {
		if r.URL.Query().Get("part") == "charts" {
			render(w, r, adminpages.MetricsChartsFragment(nodes, selectedNodeID, string(rangeID)))
			return
		}
		render(w, r, adminpages.MetricsFragment(nodes, selectedNodeID, string(rangeID), errMessage))
		return
	}
	render(w, r, adminpages.Metrics(nodes, selectedNodeID, string(rangeID), errMessage, user, org, flashFromCookie(w, r)))
}
