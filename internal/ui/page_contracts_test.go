package ui_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/shepard-labs/cyclopus/internal/ui/pages"
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

func TestThemeSettingsRendersAllEightPresets(t *testing.T) {
	output := renderPage(t, settingspages.Theme(store.User{}, store.Organization{}, nil))
	requirePageContracts(t, output, `href="/settings/theme"`)

	for _, preset := range []string{"obsidian", "mono", "ember", "graphite", "terminal", "paper", "console", "solar"} {
		t.Run(preset, func(t *testing.T) {
			requirePageContracts(t, output,
				`id="theme-`+preset+`" type="button" data-theme-name="`+preset+`" aria-pressed="false"`,
			)
		})
	}
	if got := strings.Count(output, `data-theme-name="`); got != 8 {
		t.Fatalf("theme preset control count = %d, want 8", got)
	}
	if got := strings.Count(output, `aria-pressed="false"`); got != 8 {
		t.Fatalf("theme aria-pressed count = %d, want 8", got)
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
			`<body class="min-h-full bg-base-200 font-sans text-base-content" hx-boost="true">`,
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
