package components

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/shepard-labs/cyclopus/internal/ui"
)

func renderComponent(t testing.TB, component templ.Component) string {
	t.Helper()
	var output bytes.Buffer
	if err := component.Render(context.Background(), &output); err != nil {
		t.Fatalf("render component: %v", err)
	}
	return output.String()
}

func withChildren(component templ.Component, children ...templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		child := componentGroup(children...)
		return component.Render(templ.WithChildren(ctx, child), w)
	})
}

func componentGroup(components ...templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		for _, component := range components {
			if err := component.Render(ctx, w); err != nil {
				return err
			}
		}
		return nil
	})
}

func markup(value string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, value)
		return err
	})
}

func requireContains(t testing.TB, output string, contracts ...string) {
	t.Helper()
	for _, contract := range contracts {
		if !strings.Contains(output, contract) {
			t.Errorf("rendered markup missing %q\nmarkup: %s", contract, output)
		}
	}
}

func requireNotContains(t testing.TB, output string, forbidden ...string) {
	t.Helper()
	for _, value := range forbidden {
		if strings.Contains(strings.ToLower(output), strings.ToLower(value)) {
			t.Errorf("rendered markup unexpectedly contains %q\nmarkup: %s", value, output)
		}
	}
}

func TestLayoutAssetContract(t *testing.T) {
	user := store.User{Email: "operator@example.test", DisplayName: "Operator"}
	org := store.Organization{Name: "Example Org"}
	pages := map[string]templ.Component{
		"application":    Page("Contracts", user, org, nil, "/projects", markup(`<p id="page-content">content</p>`)),
		"authentication": withChildren(AuthPage("Sign in"), markup(`<p id="auth-content">content</p>`)),
	}

	for name, page := range pages {
		t.Run(name, func(t *testing.T) {
			output := renderComponent(t, page)
			requireContains(t, output,
				`<script src="/static/theme.js"></script>`,
				`<link rel="stylesheet" href="/static/css/output.css">`,
				`<script src="/static/htmx.min.js" defer></script>`,
				`<script type="module" src="https://cdn.jsdelivr.net/npm/@tailwindplus/elements@1"></script>`,
			)
			if got := strings.Count(output, `@tailwindplus/elements@1`); got != 1 {
				t.Errorf("Elements module count = %d, want 1", got)
			}
			requireNotContains(t, output,
				"ui.css",
				"web"+"awesome",
				"font"+"awesome",
				"font-"+"awesome",
				"cdnjs.cloudflare.com/ajax/libs/font-"+"awesome",
				"wa-",
			)
		})
	}
}

func TestWrapperMarkupContracts(t *testing.T) {
	options := []SelectOption{{Value: "one", Label: "One"}, {Value: "two", Label: "Two"}}
	tabs := []TabItem{
		{ID: "overview", Label: "Overview", Content: markup(`<p id="overview-content">Overview content</p>`)},
		{ID: "activity", Label: "Activity", Content: markup(`<p id="activity-content">Activity content</p>`)},
	}
	user := store.User{Email: "operator@example.test", DisplayName: "Operator"}
	org := store.Organization{Name: "Example Org"}
	flash := &ui.FlashMessage{Kind: "success", Message: "Saved"}

	output := renderComponent(t, componentGroup(
		Alert("error", "Alert message"),
		Badge("success", "Healthy"),
		ReleaseStatusBadge("live"),
		ReleaseStatusBadge("unknown-status"),
		EnvLifecycleBadge("ready"),
		EnvLifecycleBadge("unknown-status"),
		RuntimeClassBadge("runc"),
		RuntimeClassBadge("gvisor"),
		OperationStatusBadge("running"),
		OperationStatusBadge("unknown-status"),
		Combobox("region", "region", "Region", "one", options),
		withChildren(CommandPalette("command-trigger", "command-dialog", "Commands"), markup(`<button role="option" id="command-option" data-command-value="deploy" data-command-text="Deploy">Deploy</button>`)),
		withChildren(Dialog("confirm-dialog", "Confirm action"), markup(`<button id="confirm-action">Confirm</button>`)),
		withChildren(Disclosure("advanced", "Advanced", false), markup(`<p>Advanced controls</p>`)),
		withChildren(Disclosure("expanded", "Expanded", true), markup(`<p>Visible controls</p>`)),
		withChildren(Dropdown("actions-trigger", "actions-menu", "Actions"), DropdownItem("/projects", "Projects")),
		withChildren(EmptyState("Nothing here", "Create the first item."), markup(`<button id="empty-action">Create</button>`)),
		Flash(flash),
		withChildren(Popover("help-trigger", "help-panel", "Help"), markup(`<p>Popover help</p>`)),
		Select("environment", "environment", "Environment", "two", options, true),
		withChildren(Table("deployments-table", "Deployments"), markup(`<thead><tr><th scope="col">Name</th></tr></thead><tbody><tr><th scope="row">api</th></tr></tbody>`)),
		Tabs("project-tabs", tabs, 0),
		Toast("save-toast", "error", "Save failed"),
		withChildren(Tooltip("info-trigger", "info-tooltip", "More information"), IconInformationCircle("size-4")),
		Sidebar(org, user, "/projects"),
	))

	requireContains(t, output,
		`role="alert"`,
		`id="region" value="One" type="text" role="combobox" aria-controls="region-options"`,
		`<input type="hidden" name="region" value="one">`,
		`<label for="region"`,
		`id="region-options"`,
		`<el-option value="One" data-value="one"`,
		`aria-label="Show Region options"`,
		`id="command-trigger" type="button" command="show-modal" commandfor="command-dialog" aria-controls="command-dialog"`,
		`</svg><span>Commands</span></button>`,
		`type="search" role="combobox" aria-label="Search commands" aria-autocomplete="list" aria-controls="command-dialog-listbox"`,
		`id="command-dialog-listbox" class="mt-2" role="listbox"`,
		`id="command-option"`,
		`role="option"`,
		`id="command-dialog-value" type="hidden" data-command-value-output`,
		`id="command-dialog-output" aria-live="polite"`,
		`option.hidden = option.`,
		`.toLowerCase().indexOf(query) === -1`,
		`root.dispatchEvent(new CustomEvent('commandselect'`,
		`id="confirm-dialog" role="dialog" aria-modal="true" aria-labelledby="confirm-dialog-title"`,
		`id="confirm-dialog-title"`,
		`command="close" commandfor="confirm-dialog"`,
		`id="advanced-trigger" type="button" command="--toggle" commandfor="advanced" aria-controls="advanced" aria-expanded="false"`,
		`<el-disclosure id="advanced" hidden`,
		`id="expanded-trigger" type="button" command="--toggle" commandfor="expanded" aria-controls="expanded" aria-expanded="true"`,
		`id="actions-trigger" type="button" aria-haspopup="menu" aria-controls="actions-menu"`,
		`id="actions-menu" role="menu"`,
		`href="/projects" role="menuitem"`,
		`id="flash-region"`,
		`aria-live="polite"`,
		`id="help-trigger" type="button" popovertarget="help-panel" aria-controls="help-panel"`,
		`<label id="environment-label" for="environment"`,
		`<el-select id="environment" name="environment" value="two" required`,
		`aria-labelledby="environment-label"`,
		`id="environment-trigger" type="button" aria-haspopup="listbox" aria-controls="environment-options"`,
		`<el-selectedcontent id="environment-selected"`,
		`<el-options id="environment-options" role="listbox"`,
		`<el-option value="one"`, `>One</span>`,
		`<el-option value="two"`, `>Two</span>`,
		`<table id="deployments-table"`,
		`<caption class="sr-only">Deployments</caption>`,
		`<th scope="col">Name</th>`,
		`<th scope="row">api</th>`,
		`id="project-tabs" role="tablist"`,
		`id="overview-tab" aria-controls="overview-panel" aria-selected="true"`,
		`id="overview-panel" role="tabpanel" aria-labelledby="overview-tab"`,
		`id="activity-panel" role="tabpanel" aria-labelledby="activity-tab" hidden`,
		`id="save-toast" role="alert"`,
		`aria-label="Dismiss notification"`,
		`id="info-trigger" type="button" interestfor="info-tooltip" aria-describedby="info-tooltip"`,
		`id="info-tooltip" role="tooltip"`,
		`<nav aria-label="Primary"`,
		`aria-current="page"`,
	)

	if strings.Contains(output, `id="advanced" hidden="false"`) {
		t.Error("collapsed disclosure must use the boolean hidden attribute, not hidden=false")
	}
	if strings.Contains(output, `UNSANDBOXED`) && strings.Count(output, `UNSANDBOXED`) != 1 {
		t.Error("RuntimeClassBadge must render only for runc")
	}
}

func TestElementsWrapperTags(t *testing.T) {
	options := []SelectOption{{Value: "one", Label: "One"}}
	tabs := []TabItem{{ID: "first", Label: "First", Content: markup("First panel")}}
	output := renderComponent(t, componentGroup(
		Combobox("combo", "combo", "Combo", "", options),
		withChildren(Dialog("dialog", "Dialog"), markup("Dialog body")),
		withChildren(Disclosure("disclosure", "Disclosure", false), markup("Disclosure body")),
		withChildren(Dropdown("dropdown-trigger", "dropdown-menu", "Dropdown"), DropdownItem("/", "Home")),
		withChildren(Popover("popover-trigger", "popover-panel", "Popover"), markup("Popover body")),
		Select("select", "select", "Select", "one", options, false),
		Tabs("tabs", tabs, 0),
		withChildren(Tooltip("tooltip-trigger", "tooltip-panel", "Tooltip"), markup("?")),
	))

	for _, tag := range []string{
		"el-autocomplete", "el-options", "el-option", "el-dialog", "el-dialog-backdrop",
		"el-dialog-panel", "el-disclosure", "el-dropdown", "el-menu", "el-popover", "el-select", "el-selectedcontent",
		"el-tab-group", "el-tab-list", "el-tab-panels",
	} {
		t.Run(tag, func(t *testing.T) {
			requireContains(t, output, "<"+tag)
		})
	}
	requireNotContains(t, output, "<el-tabs", "<el-tab-panel ", "<el-tab-panel>", "<w"+"a-", "</w"+"a-")
}

func TestInlineHeroiconContracts(t *testing.T) {
	icons := map[string]templ.Component{
		"base": withChildren(Icon("icon-base"), markup(`<path d="M1 1h1"></path>`)),
		"logo": IconLogo("icon-logo"), "squares": IconSquares("icon-squares"),
		"code-branch": IconCodeBranch("icon-code-branch"), "cube": IconCube("icon-cube"),
		"bolt": IconBolt("icon-bolt"), "key": IconKey("icon-key"), "list": IconList("icon-list"),
		"arrow-path": IconArrowPath("icon-arrow-path"), "cog": IconCog("icon-cog"),
		"palette": IconPalette("icon-palette"), "logout": IconArrowRightOnRectangle("icon-logout"),
		"chevron-down": IconChevronDown("icon-chevron-down"), "chevron-right": IconChevronRight("icon-chevron-right"),
		"x-mark": IconXMark("icon-x-mark"), "check": IconCheck("icon-check"),
		"warning": IconExclamationTriangle("icon-warning"), "information": IconInformationCircle("icon-information"),
		"search": IconMagnifyingGlass("icon-search"),
	}

	for name, icon := range icons {
		t.Run(name, func(t *testing.T) {
			output := renderComponent(t, icon)
			requireContains(t, output, `<svg class="icon-`, `aria-hidden="true"`, `data-slot="icon"`)
			if strings.Count(output, "<svg") != 1 || strings.Count(output, "</svg>") != 1 {
				t.Errorf("icon must render exactly one inline SVG: %s", output)
			}
			requireNotContains(t, output, "<img", "<use", "http://", "https://", "font-"+"awesome", "heroicons.com")
			if name == "check" {
				requireContains(t, output, `viewBox="0 0 20 20"`, `fill="currentColor"`, `fill-rule="evenodd"`, `clip-rule="evenodd"`)
				return
			}
			requireContains(t, output, `viewBox="0 0 24 24"`, `fill="none"`, `stroke="currentColor"`, `stroke-width="1.5"`)
		})
	}
}

func TestWrapperEscapesUserContent(t *testing.T) {
	output := renderComponent(t, Alert("info", `<script id="unsafe">alert(1)</script>`))
	requireContains(t, output, `&lt;script id=&#34;unsafe&#34;&gt;alert(1)&lt;/script&gt;`)
	requireNotContains(t, output, `<script id="unsafe">`)
}

func TestTabsHandlesNoItems(t *testing.T) {
	output := renderComponent(t, Tabs("empty-tabs", nil, 0))
	requireContains(t, output, `id="empty-tabs"`, `role="tablist"`)
	requireNotContains(t, output, `role="tab"`, `role="tabpanel"`)
}

func TestSelectWithoutRequired(t *testing.T) {
	output := renderComponent(t, Select("optional", "optional", "Optional", "missing", []SelectOption{{Value: "one", Label: "One"}}, false))
	requireContains(t, output,
		`<label id="optional-label" for="optional"`, `>Optional</label>`,
		`<el-select id="optional" name="optional" value="missing"`,
		`aria-labelledby="optional-label"`,
		`<el-selectedcontent id="optional-selected"`,
		`<el-options id="optional-options" role="listbox"`,
		`<el-option value="one"`, `>One</span>`,
	)
	requireNotContains(t, output, ` required`, ` selected`)
}

func TestSelectRendersSupportedElementsControl(t *testing.T) {
	output := renderComponent(t, Select("environment", "environment", "Environment", "production", []SelectOption{
		{Value: "preview", Label: "Preview"},
		{Value: "production", Label: "Production"},
	}, true))

	requireContains(t, output,
		`<label id="environment-label" for="environment"`, `>Environment</label>`,
		`<el-select id="environment" name="environment" value="production" required`,
		`aria-labelledby="environment-label"`,
		`id="environment-trigger" type="button" aria-haspopup="listbox" aria-controls="environment-options"`,
		`aria-labelledby="environment-label environment-selected"`,
		`<el-selectedcontent id="environment-selected"`,
		`<el-options id="environment-options" role="listbox"`,
		`<el-option value="preview"`, `>Preview</span>`,
		`<el-option value="production"`, `>Production</span>`,
	)
	requireNotContains(t, output, `<select`, `<option`)
}

func TestCommandPaletteTriggerUsesSuppliedVisibleTitle(t *testing.T) {
	output := renderComponent(t, withChildren(CommandPalette("palette-trigger", "palette-dialog", "Deploy commands"), markup(`<button data-command-value="deploy">Deploy</button>`)))
	requireContains(t, output,
		`id="palette-trigger" type="button"`,
		`</svg><span>Deploy commands</span></button>`,
	)
	requireNotContains(t, output, `>title </button>`)
}
