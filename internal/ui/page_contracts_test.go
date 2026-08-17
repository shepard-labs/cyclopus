package ui_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
	"github.com/shepard-labs/cyclopus/internal/avatar"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/shepard-labs/cyclopus/internal/ui/pages"
	adminpages "github.com/shepard-labs/cyclopus/internal/ui/pages/admin"
	settingspages "github.com/shepard-labs/cyclopus/internal/ui/pages/settings"
)

func renderPage(t testing.TB, component templ.Component) string {
	t.Helper()
	var output bytes.Buffer
	if err := component.Render(context.Background(), &output); err != nil {
		t.Fatalf("render page: %v", err)
	}
	return output.String()
}

func requirePageContracts(t testing.TB, output string, contracts ...string) {
	t.Helper()
	for _, contract := range contracts {
		if !strings.Contains(output, contract) {
			t.Errorf("rendered page missing %q\nmarkup: %s", contract, output)
		}
	}
}

func TestThemeSettingsBuildsAccessibleControlsFromRegistry(t *testing.T) {
	output := renderPage(t, settingspages.Theme(store.User{}, store.Organization{}, nil))
	requirePageContracts(t, output,
		`href="/settings/theme"`,
		`var themes=window.cyclopusTheme.themes`,
		`Object.keys(themes).forEach`,
		`b.type='button'`,
		`b.setAttribute('role','button')`,
		`b.setAttribute('aria-pressed','false')`,
		`b.setAttribute('aria-label','Use '+t.label+' theme')`,
		`b.className='group cursor-pointer rounded-box border border-line bg-base-300 p-4 text-left outline-none transition focus-visible:ring-2 focus-visible:ring-base-content focus-visible:ring-offset-2 focus-visible:ring-offset-base-200 aria-pressed:border-primary'`,
		`window.cyclopusTheme.apply(name)`,
		`window.cyclopusTheme.sync()`,
		`border-line`,
		`bg-base-200`,
		`bg-base-300`,
		`rounded-box border border-line bg-base-300`,
	)
	if strings.Contains(output, `data-theme-name="`) {
		t.Fatalf("theme metadata is duplicated in server-rendered controls:\n%s", output)
	}
}

func TestUserAvatarOptionsPreserveCheckedStyleAndExposeKeyboardFocus(t *testing.T) {
	output := renderPage(t, settingspages.User(
		store.User{AvatarKey: avatar.DefaultKey}, store.Organization{}, nil, "", "", "",
	))
	requirePageContracts(t, output,
		`<label class="group relative flex cursor-pointer items-center justify-center rounded-full p-1 outline-none transition has-focus-visible:ring-2 has-focus-visible:ring-base-content has-focus-visible:ring-offset-2 has-focus-visible:ring-offset-base-200"><input type="radio" name="avatar_key" value="`+avatar.DefaultKey+`" checked required class="peer sr-only"><img src="`+avatar.URL(avatar.DefaultKey)+`" alt="Avatar option `+avatar.DefaultKey+`" class="size-14 rounded-full opacity-70 transition group-hover:scale-110 group-hover:opacity-100 peer-checked:opacity-100 peer-checked:ring-2 peer-checked:ring-primary"></label>`,
	)
}

func TestLogsUseThemeNativeSurfacesAndTypography(t *testing.T) {
	output := renderPage(t, pages.Logs(
		store.App{ID: "app-1", ProjectID: "project-1", Name: "API"},
		store.Project{ID: "project-1", Name: "Platform"},
		[]observability.LogEntry{{Body: "started", Severity: observability.SeverityInfo}},
		"next", "env-1", "", store.User{}, store.Organization{}, nil,
	))
	requirePageContracts(t, output,
		`text-sm text-base-content/65`,
		`bg-base-300`,
		`bg-base-200`,
		`border-line`,
		`font-mono text-xs/5`,
		`text-base-content`,
		`size-1.5 shrink-0 rounded-full bg-info`,
		`focus:border-primary focus-visible:ring-1 focus-visible:ring-primary`,
		`cursor-pointer rounded-field border border-line bg-base-300 px-4 py-2 text-sm font-medium text-base-content hover:bg-base-200">Search`,
		`text-sm font-medium ring-1 ring-line outline-none focus-visible:ring-2 focus-visible:ring-base-content focus-visible:ring-offset-2 focus-visible:ring-offset-base-300" hx-get=`,
	)
	for _, prohibited := range []string{"bg-[#09090b]", "text-zinc-", "border-white", "ring-white", "border-zinc"} {
		if strings.Contains(output, prohibited) {
			t.Errorf("logs contain fixed palette class %q", prohibited)
		}
	}
}

func TestPageHTMXContracts(t *testing.T) {
	t.Run("boosted layout and environment polling", func(t *testing.T) {
		releaseID := "release-1"
		output := renderPage(t, pages.AppOverview(
			store.App{ID: "app-1", Name: "API"},
			store.Project{ID: "project-1", Name: "Platform"},
			&store.Environment{ID: "env-1", Kind: "production", InProgressReleaseID: &releaseID},
			nil, nil, nil, store.User{}, store.Organization{}, nil,
		))
		requirePageContracts(t, output,
			`<body class="min-h-full bg-base-100 font-sans text-base-content antialiased" hx-boost="true">`,
			`hx-get="/environments/env-1/status-fragment"`,
			`hx-trigger="every 3s"`,
			`hx-target="#prod-status"`,
			`hx-swap="outerHTML"`,
		)
		if strings.Contains(output, `hx-post=`) {
			t.Fatalf("read-only overview unexpectedly contains an HTMX POST contract:\n%s", output)
		}
	})

	t.Run("idle environment does not poll", func(t *testing.T) {
		output := renderPage(t, pages.EnvStatusFragment(store.Environment{ID: "env-1"}, nil, nil))
		requirePageContracts(t, output, `id="prod-status"`)
		for _, attribute := range []string{`hx-get=`, `hx-trigger=`, `hx-target=`, `hx-swap=`} {
			if strings.Contains(output, attribute) {
				t.Fatalf("idle environment retained %q:\n%s", attribute, output)
			}
		}
	})

	t.Run("logs page pagination preserves filters", func(t *testing.T) {
		output := renderPage(t, pages.Logs(
			store.App{ID: "app-1", ProjectID: "project-1", Name: "API"},
			store.Project{ID: "project-1", Name: "Platform"},
			[]observability.LogEntry{{Body: "first page"}},
			"next cursor", "env-1", "error rate", store.User{}, store.Organization{}, nil,
		))
		requirePageContracts(t, output,
			`hx-get="/apps/app-1/logs?cursor=next+cursor&amp;env=env-1&amp;load_more=true&amp;q=error+rate"`,
			`hx-target="closest .log-load-more"`,
			`hx-swap="outerHTML"`,
		)
	})

	t.Run("logs fragment replaces its pagination control", func(t *testing.T) {
		output := renderPage(t, pages.LogsEntriesFragment(
			"app/one", []observability.LogEntry{{Body: "older entry"}}, "third page", "env one", "level:error",
		))
		requirePageContracts(t, output,
			`class="log-entries-fragment contents"`,
			`hx-get="/apps/app%2Fone/logs?cursor=third+page&amp;env=env+one&amp;load_more=true&amp;q=level%3Aerror"`,
			`hx-target="closest .log-load-more"`,
			`hx-swap="outerHTML"`,
		)
	})

	t.Run("terminal logs fragment omits pagination", func(t *testing.T) {
		output := renderPage(t, pages.LogsEntriesFragment("app-1", nil, "", "", ""))
		for _, attribute := range []string{`hx-get=`, `hx-target=`, `hx-swap=`} {
			if strings.Contains(output, attribute) {
				t.Fatalf("terminal logs fragment retained %q:\n%s", attribute, output)
			}
		}
	})
}

func TestAuthPagesUseCompactTypographyAndLayeredSurfaces(t *testing.T) {
	pagesUnderTest := map[string]templ.Component{
		"login":     pages.Login("Invalid credentials"),
		"bootstrap": pages.Bootstrap("Invalid bootstrap token"),
	}

	for name, page := range pagesUnderTest {
		t.Run(name, func(t *testing.T) {
			output := renderPage(t, page)
			requirePageContracts(t, output,
				`bg-base-100`,
				`rounded-box border border-line bg-base-200 p-6 shadow-auth-glow`,
				`text-xl font-medium`,
				`text-lg/7 font-medium`,
				`block text-xs font-medium`,
				`rounded-field border border-line bg-base-300`,
				`text-sm font-medium`,
				`focus:border-primary focus-visible:ring-1 focus-visible:ring-primary`,
			)
			for _, prohibited := range []string{"text-2xl", "focus-visible:ring-2 focus-visible:ring-base-content focus-visible:ring-offset-2 focus-visible:ring-offset-base-200", "bg-base-100 px-3 py-2"} {
				if strings.Contains(output, prohibited) {
					t.Errorf("auth page contains outdated class %q", prohibited)
				}
			}
		})
	}
}

func TestSettingsAndAdminPagesUseAlignedPageLocalDesignSystem(t *testing.T) {
	pagesUnderTest := map[string]templ.Component{
		"app settings": pages.AppSettings(
			store.App{ID: "app-1", Name: "API", RuntimeClass: "gvisor"},
			store.Project{ID: "project-1", Name: "Platform"},
			store.User{}, store.Organization{}, nil, "",
		),
		"user": settingspages.User(
			store.User{Email: "operator@example.com"}, store.Organization{}, nil, "", "", "",
		),
		"git connections": settingspages.GitConnections(
			[]store.GitConnection{{ID: "connection-1", RepositoryFullName: "acme/api", RepositoryDefaultBranch: "main"}},
			store.User{}, store.Organization{}, nil, "",
		),
		"registries": settingspages.Registries(
			[]store.Registry{{ID: "registry-1", Provider: "ghcr", RegistryURL: "ghcr.io"}},
			store.User{}, store.Organization{}, nil, "",
		),
		"webhooks": settingspages.Webhooks(
			[]store.Webhook{{ID: "webhook-1", TargetURL: "https://example.com/hooks/cyclopus"}},
			store.User{}, store.Organization{}, nil, "",
		),
		"tokens": settingspages.Tokens(
			[]store.APIToken{{ID: "token-1", Name: "Deploy", Scopes: []string{"deploy:app-1"}}}, "secret-token",
			store.User{}, store.Organization{}, nil, "",
		),
		"audit": settingspages.AuditLog(
			[]store.AuditEventRecord{{Action: "app.updated", TargetType: "app", TargetID: "app-12345678"}}, "next",
			store.User{}, store.Organization{}, nil,
		),
		"reconcile": adminpages.Reconcile(
			[]store.ReconciliationRun{{ID: "run-12345678", ScopeKind: "organization", Status: "completed", DryRun: true}},
			store.User{}, store.Organization{}, nil,
		),
	}

	for name, page := range pagesUnderTest {
		t.Run(name, func(t *testing.T) {
			output := renderPage(t, page)
			requirePageContracts(t, output,
				`rounded-box border border-line bg-base-200`,
			)
			for _, prohibited := range []string{
				`text-xl font-semibold`,
				`rounded-box bg-base-100`,
				`border-base-300`,
				`divide-base-300`,
				`bg-base-100 px-3 py-2 text-sm ring-1`,
			} {
				if strings.Contains(output, prohibited) {
					t.Errorf("page contains outdated design-system contract %q", prohibited)
				}
			}
		})
	}
}

func TestPhaseTwoIntegrationSettingsAreReadOnly(t *testing.T) {
	gitOutput := renderPage(t, settingspages.GitConnections([]store.GitConnection{{State: "active", AccessState: "active"}}, store.User{}, store.Organization{}, nil, ""))
	requirePageContracts(t, gitOutput, "GitHub App setup and repository import are not available in this phase", "Existing connection status is shown read-only")
	registryOutput := renderPage(t, settingspages.Registries([]store.Registry{{Provider: "ghcr", State: "ready", HasActiveCredential: true}}, store.User{}, store.Organization{}, nil, ""))
	requirePageContracts(t, registryOutput, "GHCR credential setup and rotation are not available in this phase", "Current credential status is shown read-only")
	for name, output := range map[string]string{"git connections": gitOutput, "registries": registryOutput} {
		for _, forbidden := range []string{"action=\"/settings/git-connections", "action=\"/settings/registries", "/settings/git-connections/", "/settings/registries/", "installation_id", "webhook_secret_ref", "credential_secret_ref", "k8s-secret://"} {
			if strings.Contains(output, forbidden) {
				t.Errorf("%s contains obsolete mutation or secret contract %q", name, forbidden)
			}
		}
	}
}

func TestCorePagesUseAlignedPageLocalDesignSystem(t *testing.T) {
	releaseID := "release-1"
	pagesUnderTest := map[string]templ.Component{
		"projects": pages.Projects(
			[]store.Project{{ID: "project-1", Name: "Platform", Slug: "platform"}},
			store.Organization{}, store.User{}, nil, "",
		),
		"project": pages.ProjectDetail(
			store.Project{ID: "project-1", Name: "Platform"},
			[]store.App{{ID: "app-1", Name: "API", Slug: "api", HostnameLabel: "api", ContainerPort: 8080, RuntimeClass: "runc"}},
			store.User{}, store.Organization{}, nil, "",
		),
		"app": pages.AppOverview(
			store.App{ID: "app-1", Name: "API", MaxPreviewEnvironments: 5},
			store.Project{ID: "project-1", Name: "Platform"},
			&store.Environment{ID: "env-1", Branch: "main", LifecycleStatus: "active", InProgressReleaseID: &releaseID},
			nil, nil,
			[]store.Environment{{ID: "env-2", Branch: "feature/one", LifecycleStatus: "active"}},
			store.User{}, store.Organization{}, nil,
		),
		"environment": pages.EnvironmentDetail(
			store.Environment{ID: "env-1", Branch: "main", LifecycleStatus: "active"},
			store.App{ID: "app-1", Name: "API"},
			store.Project{ID: "project-1", Name: "Platform"},
			[]store.Release{{ImmutableReleaseSnapshot: store.ImmutableReleaseSnapshot{ID: "release-1", Kind: "deploy", ImageRef: "ghcr.io/acme/api:latest"}, Status: "live"}},
			nil, nil, nil, store.EnvironmentVariableRevision{}, store.User{}, store.Organization{}, nil,
		),
	}

	for name, page := range pagesUnderTest {
		t.Run(name, func(t *testing.T) {
			output := renderPage(t, page)
			requirePageContracts(t, output,
				`rounded-box border border-line bg-base-200`,
			)
			for _, prohibited := range []string{
				`text-xl font-semibold`,
				`rounded-box bg-base-100`,
				`border-base-300`,
				`divide-base-300`,
			} {
				if strings.Contains(output, prohibited) {
					t.Errorf("page contains outdated design-system contract %q", prohibited)
				}
			}
		})
	}
}
