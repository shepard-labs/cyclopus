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
			if name == "application" {
				requireContains(t, output,
					`<body class="min-h-full bg-base-100 font-sans text-base-content antialiased" hx-boost="true">`,
					`class="min-w-0 lg:pl-56"`,
					`class="sticky top-0 z-30 border-b border-line bg-base-100"`,
					`class="relative mx-auto flex h-14 max-w-5xl items-center justify-center px-4 sm:px-6 lg:px-8"`,
					`class="fixed inset-y-0 left-0 z-40 hidden w-56 flex-col border-r border-line bg-base-100 lg:flex"`,
					`class="flex h-14 shrink-0 items-center gap-3 border-b border-line px-4"`,
					`<main class="mx-auto max-w-5xl p-4 sm:p-6 lg:p-8">`,
					`id="mobile-navigation"`,
				)
				requireNotContains(t, output, `lg:pl-72`, `w-72`, `h-16`)
			} else {
				requireContains(t, output, `bg-base-100`)
			}
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
		withChildren(Dropdown("actions-trigger", "actions-menu", "Actions", nil), DropdownItem("/projects", "Projects")),
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
		`border-line bg-base-200`,
		`rounded-full border px-2 py-0.5 text-xs font-medium`,
		`size-1.5 shrink-0 rounded-full bg-success`,
		`size-1.5 shrink-0 rounded-full bg-warning`,
		`size-1.5 shrink-0 rounded-full bg-base-content/40`,
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
		`<el-dialog-panel class="relative w-full max-w-lg transform overflow-hidden rounded-box border border-line bg-base-200 p-6 text-left text-base-content shadow-theme transition data-closed:translate-y-4 data-closed:opacity-0 data-closed:sm:scale-95"`,
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
		`text-sm font-normal bg-primary/10 text-base-content`,
		`<span class="text-primary"><svg`,
	)
	requireContains(t, output,
		`border border-line bg-base-300`,
		`rounded-box border border-line bg-base-200`,
		`text-xs font-medium`,
		`text-sm font-medium`,
		`shadow-theme`,
		`focus:border-primary focus-visible:ring-1 focus-visible:ring-primary`,
		`rounded-field px-3 outline-none focus-visible:ring-2 focus-visible:ring-base-content focus-visible:ring-offset-2 focus-visible:ring-offset-base-200`,
	)

	if strings.Contains(output, `id="advanced" hidden="false"`) {
		t.Error("collapsed disclosure must use the boolean hidden attribute, not hidden=false")
	}
	if strings.Contains(output, `UNSANDBOXED`) && strings.Count(output, `UNSANDBOXED`) != 1 {
		t.Error("RuntimeClassBadge must render only for runc")
	}
}

func TestAlertsAndBadgesUseReadableTextWithSemanticCues(t *testing.T) {
	output := renderComponent(t, componentGroup(
		Alert("success", "Saved"),
		Alert("warning", "Check this"),
		Alert("error", "Failed"),
		Alert("info", "Notice"),
		Badge("success", "Healthy"),
		Badge("warning", "Pending"),
		Badge("error", "Failed"),
		Badge("info", "Informational"),
	))

	requireContains(t, output,
		`border-success/30 bg-success/10`,
		`border-warning/30 bg-warning/10`,
		`border-error/30 bg-error/10`,
		`border-info/30 bg-info/10`,
		`border-success/20 bg-success/10 text-base-content`,
		`border-warning/20 bg-warning/10 text-base-content`,
		`border-error/20 bg-error/10 text-base-content`,
		`border-primary/20 bg-primary/10 text-base-content`,
		`text-sm text-base-content shadow-theme`,
	)
	requireNotContains(t, output, `text-success`, `text-warning`, `text-error`, `text-info`, `bg-primary text-primary-content`)
}

func TestOperationalStatusesUseDotAndLabelSemantics(t *testing.T) {
	tests := []struct {
		name      string
		component templ.Component
		label     string
		tone      string
	}{
		{name: "release live", component: ReleaseStatusBadge("live"), label: "Live", tone: "bg-success"},
		{name: "release deploying", component: ReleaseStatusBadge("deploying"), label: "Deploying", tone: "bg-warning"},
		{name: "environment provisioning", component: EnvLifecycleBadge("reserved"), label: "Provisioning", tone: "bg-warning"},
		{name: "operation running", component: OperationStatusBadge("running"), label: "Running", tone: "bg-warning"},
		{name: "operation failed", component: OperationStatusBadge("failed"), label: "Failed", tone: "bg-error"},
		{name: "unknown", component: OperationStatusBadge("mystery"), label: "mystery", tone: "bg-base-content/40"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := renderComponent(t, tt.component)
			requireContains(t, output,
				`class="inline-flex items-center gap-1.5 text-xs font-medium"`,
				`<span aria-hidden="true" class="size-1.5 shrink-0 rounded-full `+tt.tone+`"></span>`,
				`<span>`+tt.label+`</span>`,
			)
			requireNotContains(t, output, `rounded-full border px-2`, `bg-primary`)
		})
	}
}

func TestSharedControlsUseThemeRadiusUtilities(t *testing.T) {
	output := renderComponent(t, componentGroup(
		Combobox("combo", "combo", "Combo", "", nil),
		Select("select", "select", "Select", "", nil, false),
		withChildren(Dropdown("trigger", "menu", "Actions", nil), DropdownItem("/", "Home")),
		withChildren(Dialog("dialog", "Dialog"), markup("Body")),
	))
	requireContains(t, output, `rounded-field`, `rounded-box`)
	requireNotContains(t, output, `rounded-md`, `rounded-lg`, `rounded-xl`, `rounded-2xl`)
}

func TestElementsWrapperTags(t *testing.T) {
	options := []SelectOption{{Value: "one", Label: "One"}}
	tabs := []TabItem{{ID: "first", Label: "First", Content: markup("First panel")}}
	output := renderComponent(t, componentGroup(
		Combobox("combo", "combo", "Combo", "", options),
		withChildren(Dialog("dialog", "Dialog"), markup("Dialog body")),
		withChildren(Disclosure("disclosure", "Disclosure", false), markup("Disclosure body")),
		withChildren(Dropdown("dropdown-trigger", "dropdown-menu", "Dropdown", nil), DropdownItem("/", "Home")),
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

func TestCommandPaletteSearchUsesBaseContentFocusIndicator(t *testing.T) {
	output := renderComponent(t, withChildren(CommandPalette("palette-trigger", "palette-dialog", "Deploy commands"), markup(`<button data-command-value="deploy">Deploy</button>`)))
	requireContains(t, output, `id="palette-dialog-input" type="search" role="combobox" aria-label="Search commands" aria-autocomplete="list" aria-controls="palette-dialog-listbox" aria-expanded="false" autocomplete="off" class="w-full bg-transparent text-sm outline-none focus-visible:ring-2 focus-visible:ring-base-content focus-visible:ring-offset-2 focus-visible:ring-offset-base-300" placeholder="Type a command…"`)
}

func TestButtonVariantsAndTypes(t *testing.T) {
	tests := []struct {
		name        string
		component   templ.Component
		wantContain []string
		wantAbsent  []string
	}{
		{
			name: "primary submit button",
			component: withChildren(Button(ButtonProps{
				Variant: ButtonVariantPrimary,
				Type:    ButtonTypeSubmit,
			}), markup("Save")),
			wantContain: []string{
				`<button`,
				`type="submit"`,
				`bg-primary`,
				`text-primary-content`,
				`Save`,
			},
			wantAbsent: []string{`disabled?`, `disabled=""`, `disabled `},
		},
		{
			name: "secondary default type button",
			component: withChildren(Button(ButtonProps{
				Variant: ButtonVariantSecondary,
			}), markup("Cancel")),
			wantContain: []string{
				`<button`,
				`type="button"`,
				`border-line`,
				`bg-base-300`,
				`Cancel`,
			},
			wantAbsent: []string{`disabled?`, `disabled=""`, `disabled `},
		},
		{
			name: "danger reset disabled button with custom class",
			component: withChildren(Button(ButtonProps{
				Variant:  ButtonVariantDanger,
				Type:     ButtonTypeReset,
				Disabled: true,
				Class:    "w-full",
			}), markup("Delete")),
			wantContain: []string{
				`<button`,
				`type="reset"`,
				`border-error/30`,
				`hover:bg-error/10`,
				` disabled`,
				`w-full`,
				`Delete`,
			},
		},
		{
			name:      "button link",
			component: ButtonLink(ButtonVariantSecondary, "/settings", "Settings"),
			wantContain: []string{
				`<a`,
				`href="/settings"`,
				`border-line`,
				`Settings`,
			},
			wantAbsent: []string{`<button`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := renderComponent(t, tt.component)
			requireContains(t, output, tt.wantContain...)
			if len(tt.wantAbsent) > 0 {
				requireNotContains(t, output, tt.wantAbsent...)
			}
		})
	}
}

func TestBreadcrumbs(t *testing.T) {
	t.Run("renders multiple items links and active leaf", func(t *testing.T) {
		items := []BreadcrumbItem{
			{Label: "Home", Href: "/"},
			{Label: "Projects", Href: "/projects"},
			{Label: "Settings", Href: "/projects/settings"},
		}
		output := renderComponent(t, Breadcrumbs(items))
		requireContains(t, output,
			`<nav aria-label="Breadcrumb" class="flex">`,
			`<ol role="list" class="flex items-center gap-2 text-xs text-base-content/65">`,
			`<span class="text-base-content/40">/</span>`,
			`href="/"`,
			`href="/projects"`,
			`aria-current="page"`,
			`<span aria-current="page" class="text-base-content font-medium">Settings</span>`,
		)
		requireNotContains(t, output, `href="/projects/settings"`)
	})

	t.Run("single item without separator", func(t *testing.T) {
		items := []BreadcrumbItem{
			{Label: "Dashboard", Href: "/"},
		}
		output := renderComponent(t, Breadcrumbs(items))
		requireContains(t, output,
			`aria-current="page"`,
			`Dashboard`,
		)
		requireNotContains(t, output, `<span class="text-base-content/40">/</span>`, `href="/"`)
	})

	t.Run("empty items", func(t *testing.T) {
		output := renderComponent(t, Breadcrumbs(nil))
		requireContains(t, output,
			`<nav aria-label="Breadcrumb" class="flex">`,
			`<ol role="list" class="flex items-center gap-2 text-xs text-base-content/65"></ol>`,
		)
	})
}

func TestCardComponents(t *testing.T) {
	t.Run("Card with title and content", func(t *testing.T) {
		output := renderComponent(t, withChildren(Card("Project Details"), markup("<p>Card body content</p>")))
		requireContains(t, output,
			`class="rounded-box border border-line bg-base-200 p-6"`,
			`<h2 class="text-sm font-semibold text-base-content/80 mb-4">Project Details</h2>`,
			`<p>Card body content</p>`,
		)
	})

	t.Run("Card without title", func(t *testing.T) {
		output := renderComponent(t, withChildren(Card(""), markup("<p>Only content</p>")))
		requireContains(t, output,
			`class="rounded-box border border-line bg-base-200 p-6"`,
			`<p>Only content</p>`,
		)
		requireNotContains(t, output, `<h2`)
	})

	t.Run("SectionCard with title and content", func(t *testing.T) {
		output := renderComponent(t, withChildren(SectionCard("Section Title"), markup("<span>Section body</span>")))
		requireContains(t, output,
			`class="rounded-box border border-line bg-base-200 p-6"`,
			`<h2 class="text-sm font-semibold text-base-content/80 mb-4">Section Title</h2>`,
			`<span>Section body</span>`,
		)
	})

	t.Run("CardWithHeader", func(t *testing.T) {
		output := renderComponent(t, withChildren(CardWithHeader("Config", "Manage your settings"), markup("<p>Body</p>")))
		requireContains(t, output,
			`<h2 class="text-sm font-semibold text-base-content/80">Config</h2>`,
			`<p class="mt-1 text-xs text-base-content/65">Manage your settings</p>`,
			`<p>Body</p>`,
		)
	})
}

func TestFormFieldAndInput(t *testing.T) {
	t.Run("FormField standard rendering", func(t *testing.T) {
		output := renderComponent(t, FormField("username", "username", "User Name", "text", "admin", "Enter username", true))
		requireContains(t, output,
			`for="username"`,
			`User Name`,
			`<span class="text-error" aria-hidden="true">*</span>`,
			`id="username"`,
			`name="username"`,
			`type="text"`,
			`value="admin"`,
			`placeholder="Enter username"`,
			`required`,
		)
	})

	t.Run("FormFieldWithOptions with hint without error", func(t *testing.T) {
		output := renderComponent(t, FormFieldWithOptions(FormFieldProps{
			ID:          "token",
			Name:        "token",
			Label:       "API Token",
			Type:        "password",
			Value:       "secret",
			Placeholder: "tok_...",
			Required:    false,
			Hint:        "Must be at least 32 characters.",
		}))
		requireContains(t, output,
			`for="token"`,
			`API Token`,
			`type="password"`,
			`value="secret"`,
			`placeholder="tok_..."`,
			`aria-describedby="token-hint"`,
			`id="token-hint" class="text-xs text-base-content/60">Must be at least 32 characters.</p>`,
		)
		requireNotContains(t, output, `required`, `aria-invalid="true"`, `*</span>`)
	})

	t.Run("FormFieldWithOptions with error taking precedence over hint", func(t *testing.T) {
		output := renderComponent(t, FormFieldWithOptions(FormFieldProps{
			ID:       "email",
			Name:     "email",
			Label:    "Email",
			Type:     "",
			Value:    "bad-email",
			Required: true,
			Hint:     "We will send a code.",
			Error:    "Invalid email address",
			Disabled: true,
		}))
		requireContains(t, output,
			`type="text"`,
			`value="bad-email"`,
			`aria-invalid="true"`,
			`aria-describedby="email-error"`,
			`id="email-error" class="text-xs text-error">Invalid email address</p>`,
			` disabled`,
			`required`,
		)
		requireNotContains(t, output, `id="email-hint"`, `We will send a code.`)
	})

	t.Run("Input standalone component", func(t *testing.T) {
		output := renderComponent(t, Input("search-input", "q", "search", "term", "Search...", false))
		requireContains(t, output,
			`id="search-input"`,
			`name="q"`,
			`type="search"`,
			`value="term"`,
			`placeholder="Search..."`,
			`class="rounded-field border border-line bg-base-300 px-3 py-2 text-sm font-medium outline-none focus:border-primary focus-visible:ring-1 focus-visible:ring-primary"`,
		)
		requireNotContains(t, output, `required`, `<label`, `<p`)
	})

	t.Run("Input with empty type defaults to text", func(t *testing.T) {
		output := renderComponent(t, Input("plain", "plain", "", "", "", true))
		requireContains(t, output,
			`id="plain"`,
			`name="plain"`,
			`type="text"`,
			`required`,
		)
	})
}
