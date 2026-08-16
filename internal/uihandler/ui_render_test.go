package uihandler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
	"github.com/shepard-labs/cyclopus/internal/avatar"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/shepard-labs/cyclopus/internal/ui"
	"github.com/shepard-labs/cyclopus/internal/ui/pages"
	adminpages "github.com/shepard-labs/cyclopus/internal/ui/pages/admin"
	settingspages "github.com/shepard-labs/cyclopus/internal/ui/pages/settings"
)

func TestPreviewRoutesReturn404(t *testing.T) {
	t.Setenv("CYCLOPUS_UI_PREVIEW", "1")
	handler := New(nil)

	for _, path := range []string{"/__theme-preview", "/__theme-preview/flash"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want %d", path, response.Code, http.StatusNotFound)
			}
		})
	}
}

func TestEnvironmentStatusHTMXFragmentContract(t *testing.T) {
	releaseID := "release-1"
	env := store.Environment{ID: "env-1", InProgressReleaseID: &releaseID}
	request := httptest.NewRequest(http.MethodGet, "/environments/env-1/status-fragment", nil)
	request.Header.Set("HX-Request", "true")

	body := renderComponent(t, request.Context(), pages.EnvStatusFragment(env, nil, nil))

	for _, expected := range []string{
		`<div id="prod-status"`,
		`role="status"`,
		`hx-get="/environments/env-1/status-fragment"`,
		`hx-trigger="every 3s"`,
		`hx-swap="outerHTML"`,
		`hx-target="#prod-status"`,
		`Deploy in progress`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("fragment missing %q:\n%s", expected, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "<!doctype html") || strings.Contains(strings.ToLower(body), "<html") {
		t.Fatalf("fragment contains full-page HTML:\n%s", body)
	}
}

func TestEnvironmentStatusHTMXFragmentStopsPollingWhenIdle(t *testing.T) {
	body := renderComponent(t, context.Background(), pages.EnvStatusFragment(store.Environment{ID: "env-1"}, nil, nil))

	if !strings.Contains(body, `<div id="prod-status"`) {
		t.Fatalf("fragment missing targetable root:\n%s", body)
	}
	for _, pollingAttribute := range []string{"hx-get=", "hx-trigger=", "hx-swap=", "hx-target="} {
		if strings.Contains(body, pollingAttribute) {
			t.Fatalf("idle fragment retained %s:\n%s", pollingAttribute, body)
		}
	}
}

func TestPageHTMXContracts(t *testing.T) {
	t.Run("application production status polling", func(t *testing.T) {
		releaseID := "release-1"
		body := renderComponent(t, context.Background(), pages.AppOverview(
			store.App{ID: "app-1", Name: "API"},
			store.Project{ID: "project-1", Name: "Platform"},
			&store.Environment{ID: "env-1", Kind: "production", InProgressReleaseID: &releaseID},
			nil, nil, nil, store.User{}, store.Organization{}, nil,
		))
		assertContainsAll(t, body,
			`id="prod-status"`,
			`hx-get="/environments/env-1/status-fragment"`,
			`hx-trigger="every 3s"`,
			`hx-target="#prod-status"`,
			`hx-swap="outerHTML"`,
		)
	})

	t.Run("logs pagination", func(t *testing.T) {
		body := renderComponent(t, context.Background(), pages.Logs(
			store.App{ID: "app-1", ProjectID: "project-1", Name: "API"},
			store.Project{ID: "project-1", Name: "Platform"},
			[]observability.LogEntry{{Body: "first page"}},
			"next cursor", "env-1", "error rate", store.User{}, store.Organization{}, nil,
		))
		assertContainsAll(t, body,
			`id="log-entries"`,
			`value="error rate"`,
			`hx-get="/apps/app-1/logs?cursor=next+cursor&amp;env=env-1&amp;load_more=true&amp;q=error+rate"`,
			`hx-target="closest .log-load-more"`,
			`hx-swap="outerHTML"`,
			`hx-indicator="find .load-more-spinner"`,
			`role="status" aria-live="polite" class="load-more-spinner htmx-indicator`,
		)
	})

	t.Run("public forms use current handler routes", func(t *testing.T) {
		handler := New(nil)
		for _, test := range []struct {
			path     string
			contract string
		}{
			{path: "/login", contract: `<form method="POST" action="/login"`},
			{path: "/bootstrap", contract: `<form method="POST" action="/bootstrap"`},
		} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d", test.path, response.Code, http.StatusOK)
			}
			assertContainsAll(t, response.Body.String(), test.contract)
		}
	})
}

func TestLogsHTMXPaginationOmitsControlWithoutNextPage(t *testing.T) {
	body := renderComponent(t, context.Background(), pages.Logs(
		store.App{ID: "app-1", Name: "API"}, store.Project{}, nil, "", "", "", store.User{}, store.Organization{}, nil,
	))
	for _, unexpected := range []string{`hx-get=`, `hx-target=`, `load-more-spinner`} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("logs page without a next cursor contains %q:\n%s", unexpected, body)
		}
	}
}

func TestLogsEmptyPagePaginationContract(t *testing.T) {
	t.Run("empty page with next cursor keeps pagination", func(t *testing.T) {
		body := renderComponent(t, context.Background(), pages.Logs(
			store.App{ID: "app-1", Name: "API"}, store.Project{}, nil, "next-page", "env-1", "", store.User{}, store.Organization{}, nil,
		))
		assertContainsAll(t, body,
			`hx-get="/apps/app-1/logs?cursor=next-page&amp;env=env-1&amp;load_more=true"`,
			`Load older entries`,
		)
		if strings.Contains(body, `No log entries found.`) {
			t.Fatalf("empty intermediate logs page rendered terminal empty state:\n%s", body)
		}
	})

	t.Run("empty page without next cursor shows only empty state", func(t *testing.T) {
		body := renderComponent(t, context.Background(), pages.Logs(
			store.App{ID: "app-1", Name: "API"}, store.Project{}, nil, "", "", "", store.User{}, store.Organization{}, nil,
		))
		assertContainsAll(t, body, `No log entries found.`)
		if strings.Contains(body, `Load older entries`) || strings.Contains(body, `hx-get=`) {
			t.Fatalf("empty terminal logs page rendered pagination:\n%s", body)
		}
	})
}

func TestLogsHTMXPaginationActualRoute(t *testing.T) {
	requested := struct {
		appID, envID, cursor, query string
	}{}
	handler := newHandler(nil, func(_ context.Context, _, appID, envID, cursor, query string) (logsPageData, error) {
		requested.appID, requested.envID, requested.cursor, requested.query = appID, envID, cursor, query
		return logsPageData{
			entries:    []observability.LogEntry{{Body: "older entry"}},
			nextCursor: "third page",
		}, nil
	})
	request := httptest.NewRequest(http.MethodGet, "/apps/app-1/logs?cursor=second+page&env=env-1&q=error+rate&load_more=true", nil)
	request.Header.Set("HX-Request", "true")
	request.AddCookie(&http.Cookie{Name: "cyclopus_session", Value: "session-token"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("GET logs HTMX status = %d, want %d", response.Code, http.StatusOK)
	}
	if requested != (struct{ appID, envID, cursor, query string }{"app-1", "env-1", "second page", "error rate"}) {
		t.Fatalf("loader inputs = %+v", requested)
	}
	body := response.Body.String()
	assertContainsAll(t, body,
		`older entry`,
		`hx-get="/apps/app-1/logs?cursor=third+page&amp;env=env-1&amp;load_more=true&amp;q=error+rate"`,
		`hx-target="closest .log-load-more"`,
		`hx-swap="outerHTML"`,
	)
	if strings.Contains(strings.ToLower(body), "<!doctype html") || strings.Contains(body, `id="log-entries"`) {
		t.Fatalf("logs pagination returned a full page instead of the entries fragment:\n%s", body)
	}
}

func TestFormErrorHTMXFragmentContract(t *testing.T) {
	// There is no product HTMX form-error fragment contract today. Verify the
	// observable public forms remain ordinary POST forms rather than inventing
	// hx-target/hx-swap behavior in tests.
	handler := New(nil)
	for _, path := range []string{"/login", "/bootstrap"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		body := response.Body.String()
		assertContainsAll(t, body, `method="POST"`)
		for _, invented := range []string{"hx-post=", "hx-target=", "hx-swap="} {
			if strings.Contains(body, invented) {
				t.Fatalf("GET %s unexpectedly defines form-error HTMX contract %q:\n%s", path, invented, body)
			}
		}
	}
}

func TestSelectAndFormFieldContracts(t *testing.T) {
	handler := New(nil)

	t.Run("public auth handlers", func(t *testing.T) {
		for _, test := range []struct {
			path   string
			fields []string
		}{
			{path: "/login", fields: []string{`name="email"`, `name="password"`}},
			{path: "/bootstrap", fields: []string{`name="email"`, `name="password"`, `name="bootstrap_token"`}},
		} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d", test.path, response.Code, http.StatusOK)
			}
			assertContainsAll(t, response.Body.String(), test.fields...)
		}
	})

	t.Run("project fields", func(t *testing.T) {
		body := renderComponent(t, context.Background(), pages.Projects(
			[]store.Project{{ID: "project-1", Name: "Platform", Slug: "platform-api"}},
			store.Organization{}, store.User{}, nil, "",
		))
		assertContainsAll(t, body,
			`id="project-search" type="search" aria-label="Search projects"`,
			`command="show-modal" commandfor="create-project-dialog" aria-controls="create-project-dialog"`,
			`id="create-project-dialog" role="dialog"`,
			`name="name"`,
			`name="slug"`,
			`data-project-search="Platform platform-api"`,
			`card.dataset.projectSearch.toLowerCase().indexOf(query) === -1`,
			`data-project-no-results`,
		)
	})

	t.Run("project creation error reopens dialog", func(t *testing.T) {
		body := renderComponent(t, context.Background(), pages.Projects(nil, store.Organization{}, store.User{}, nil, "create failed"))
		assertContainsAll(t, body, `create failed`, `document.getElementById('create-project-dialog')`, `dialog.showModal()`)
	})

	t.Run("application search and creation dialog", func(t *testing.T) {
		body := renderComponent(t, context.Background(), pages.ProjectDetail(
			store.Project{ID: "project-1", Name: "Platform"},
			[]store.App{{ID: "app-1", Name: "API", Slug: "api-service", HostnameLabel: "api", ContainerPort: 8080}},
			store.User{}, store.Organization{}, nil, "",
		))
		assertContainsAll(t, body,
			`id="application-search" type="search" aria-label="Search applications"`,
			`command="show-modal" commandfor="create-application-dialog" aria-controls="create-application-dialog"`,
			`id="create-application-dialog" role="dialog"`,
			`data-application-search="API api-service api"`,
			`card.dataset.applicationSearch.toLowerCase().indexOf(query) === -1`,
			`data-application-no-results`,
			`name="hostname_label"`,
			`name="container_port"`,
		)
	})

	t.Run("application creation error reopens dialog", func(t *testing.T) {
		body := renderComponent(t, context.Background(), pages.ProjectDetail(
			store.Project{ID: "project-1", Name: "Platform"}, nil,
			store.User{}, store.Organization{}, nil, "create failed",
		))
		assertContainsAll(t, body, `create failed`, `document.getElementById('create-application-dialog')`, `dialog.showModal()`)
	})

	t.Run("user fields preserve current values", func(t *testing.T) {
		user := store.User{Email: "operator@example.com", AvatarKey: avatar.DefaultKey}
		body := renderComponent(t, context.Background(), settingspages.User(user, store.Organization{}, nil, "", "", ""))
		assertContainsAll(t, body,
			`name="email"`,
			`value="operator@example.com"`,
			`name="current_password"`,
			`name="avatar_key"`,
			`value="`+avatar.DefaultKey+`" checked`,
		)
	})

	t.Run("select preserves selected value", func(t *testing.T) {
		app := store.App{ID: "app-1", Name: "API", ScalingMode: "keda_http"}
		body := renderComponent(t, context.Background(), pages.AppSettings(app, store.Project{}, store.User{}, store.Organization{}, nil, ""))
		assertContainsAll(t, body,
			`<el-select id="s-scaling" name="scaling_mode" value="keda_http" required`,
			`<el-selectedcontent id="s-scaling-selected"`,
			`<el-options id="s-scaling-options" role="listbox"`,
			`<el-option value="keda_http"`,
			`>KEDA HTTP (beta, opt-in)</span>`,
			`name="name"`,
			`value="API"`,
		)
		if strings.Contains(body, `<select`) {
			t.Fatalf("app settings select nested a native select:\n%s", body)
		}
	})

	t.Run("reconcile checkbox values", func(t *testing.T) {
		body := renderComponent(t, context.Background(), adminpages.Reconcile(nil, store.User{}, store.Organization{}, nil))
		assertContainsAll(t, body,
			`name="dry_run" value="true" checked`,
			`name="prune" value="true"`,
		)
	})
}

func TestLogsHTMXPaginationReturnsFragment(t *testing.T) {
	loader := func(_ context.Context, token, appID, envID, cursor, query string) (logsPageData, error) {
		if token != "session-token" || appID != "app-1" || envID != "env/blue" || cursor != "current+cursor==" || query != "error & retry" {
			t.Fatalf("unexpected logs request: token=%q app=%q env=%q cursor=%q query=%q", token, appID, envID, cursor, query)
		}
		return logsPageData{
			entries:    []observability.LogEntry{{Timestamp: time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC), Severity: observability.SeverityError, Body: "paged log entry"}},
			nextCursor: "next+cursor==",
		}, nil
	}
	handler := newHandler(nil, loader)
	request := httptest.NewRequest(http.MethodGet, "/apps/app-1/logs?cursor=current%2Bcursor%3D%3D&env=env%2Fblue&q=error+%26+retry&load_more=true", nil)
	request.Header.Set("HX-Request", "true")
	cookieResponse := httptest.NewRecorder()
	ui.SetSession(cookieResponse, "session-token", time.Now().Add(time.Hour))
	request.AddCookie(cookieResponse.Result().Cookies()[0])
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("GET logs pagination status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	body := response.Body.String()
	assertContainsAll(t, body,
		`class="log-entries-fragment contents"`,
		`paged log entry`,
		`hx-get="/apps/app-1/logs?cursor=next%2Bcursor%3D%3D&amp;env=env%2Fblue&amp;load_more=true&amp;q=error+%26+retry"`,
		`hx-target="closest .log-load-more"`,
		`hx-swap="outerHTML"`,
		`hx-indicator="find .load-more-spinner"`,
		`role="status"`,
	)
	if strings.Contains(strings.ToLower(body), "<html") || strings.Contains(strings.ToLower(body), "<!doctype html") {
		t.Fatalf("logs pagination response contains full-page HTML:\n%s", body)
	}
	if strings.Contains(body, `id="log-entries"`) || strings.Contains(body, `id="load-more-spinner"`) {
		t.Fatalf("logs pagination response contains a duplicate-prone ID:\n%s", body)
	}
}

func TestLogsNormalGETReturnsFullPage(t *testing.T) {
	loader := func(_ context.Context, _, _, envID, _, query string) (logsPageData, error) {
		return logsPageData{
			app:     store.App{ID: "app-1", Name: "API", ProjectID: "project-1"},
			project: store.Project{ID: "project-1", Name: "Platform"},
			envID:   envID,
			query:   query,
		}, nil
	}
	handler := newHandler(nil, loader)
	request := httptest.NewRequest(http.MethodGet, "/apps/app-1/logs?env=env-1&q=timeout", nil)
	cookieResponse := httptest.NewRecorder()
	ui.SetSession(cookieResponse, "session-token", time.Now().Add(time.Hour))
	request.AddCookie(cookieResponse.Result().Cookies()[0])
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("GET logs status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	body := response.Body.String()
	assertContainsAll(t, body, `<html`, `id="log-entries"`, `name="env" value="env-1"`, `name="q" value="timeout"`)
}

func renderComponent(t *testing.T, ctx context.Context, component templ.Component) string {
	t.Helper()
	var output bytes.Buffer
	if err := component.Render(ctx, &output); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return output.String()
}

func assertContainsAll(t *testing.T, body string, expected ...string) {
	t.Helper()
	for _, value := range expected {
		if !strings.Contains(body, value) {
			t.Errorf("rendered HTML missing %q:\n%s", value, body)
		}
	}
}
