package components

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/avatar"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/shepard-labs/cyclopus/internal/ui"
)

const browserFixtureRoot = "/var/folders/4r/t9xgfxbx1glcf1f0h482klp80000gn/T/opencode"

func TestWriteBrowserFixture(t *testing.T) {
	outputPath := os.Getenv("CYCLOPUS_BROWSER_FIXTURE_OUTPUT")
	if outputPath == "" {
		t.Skip("set CYCLOPUS_BROWSER_FIXTURE_OUTPUT to write the standalone browser fixture")
	}
	outputPath = validateBrowserFixtureOutputPath(t, outputPath)

	options := []SelectOption{{Value: "preview", Label: "Preview"}, {Value: "production", Label: "Production"}}
	tabs := []TabItem{
		{ID: "fixture-overview", Label: "Overview", Content: markup(`<p id="fixture-overview-content">Overview panel</p>`)},
		{ID: "fixture-activity", Label: "Activity", Content: markup(`<p id="fixture-activity-content">Activity panel</p>`)},
	}
	user := store.User{Email: "operator@example.test", DisplayName: "Browser Operator", AvatarKey: avatar.DefaultKey}
	org := store.Organization{Name: "Browser Fixtures"}
	flash := &ui.FlashMessage{Kind: "success", Message: "Fixture loaded"}

	icons := componentGroup(
		withChildren(Icon("size-5"), markup(`<path d="M4 12h16"></path>`)),
		IconLogo("size-5"), IconSquares("size-5"), IconCodeBranch("size-5"), IconCube("size-5"),
		IconBolt("size-5"), IconKey("size-5"), IconList("size-5"), IconArrowPath("size-5"),
		IconCog("size-5"), IconPalette("size-5"), IconArrowRightOnRectangle("size-5"),
		IconChevronDown("size-5"), IconChevronRight("size-5"), IconXMark("size-5"), IconCheck("size-5"),
		IconExclamationTriangle("size-5"), IconInformationCircle("size-5"), IconMagnifyingGlass("size-5"),
	)

	fixtureContent := componentGroup(
		markup(`<div id="browser-fixture" class="mx-auto max-w-5xl space-y-8"><header><h1 class="text-2xl font-bold">Production wrapper browser fixture</h1><p>Every production wrapper and icon helper is rendered below.</p></header>`),
		markup(`<section aria-label="Theme controls" class="flex flex-wrap gap-2"><button id="theme-obsidian" type="button" data-fixture-theme="obsidian">Obsidian</button><button id="theme-mono" type="button" data-fixture-theme="mono">Mono</button><button id="theme-ember" type="button" data-fixture-theme="ember">Ember</button><button id="theme-graphite" type="button" data-fixture-theme="graphite">Graphite</button><button id="theme-terminal" type="button" data-fixture-theme="terminal">Terminal</button><button id="theme-paper" type="button" data-fixture-theme="paper">Paper</button><button id="theme-console" type="button" data-fixture-theme="console">Console</button><button id="theme-solar" type="button" data-fixture-theme="solar">Solar</button></section>`),
		Alert("info", "Informational alert"), Alert("error", "Error alert"),
		componentGroup(Badge("success", "Healthy"), ReleaseStatusBadge("live"), EnvLifecycleBadge("ready"), RuntimeClassBadge("runc"), OperationStatusBadge("running")),
		markup(`<form id="fixture-form" class="space-y-4"><fieldset class="space-y-4"><legend class="font-semibold">Form wrappers</legend>`),
		Select("select-control", "environment", "Environment", "preview", options, true),
		Combobox("combobox-input", "region", "Region", "iad", []SelectOption{{Value: "iad", Label: "Ashburn"}, {Value: "fra", Label: "Frankfurt"}}),
		markup(`<button id="fixture-submit" type="submit" class="rounded-field bg-primary px-3 py-2 text-primary-content">Observe form output</button></fieldset></form><output id="fixture-form-output" for="fixture-form" aria-live="polite" class="block rounded-box bg-base-200 p-3">No form submitted.</output>`),
		withChildren(CommandPalette("command-trigger", "command", "Open commands"), markup(`<button type="button" data-command-value="deploy" data-command-text="Deploy application">Deploy application</button><button type="button" data-command-value="rollback" data-command-text="Rollback release">Rollback release</button>`)),
		markup(`<button id="dialog-trigger" type="button" command="show-modal" commandfor="dialog-panel">Open dialog</button>`),
		withChildren(Dialog("dialog-panel", "Confirm deployment"), markup(`<button id="fixture-dialog-confirm" type="button" command="close" commandfor="dialog-panel">Confirm</button>`)),
		withChildren(Disclosure("disclosure", "Advanced settings", false), markup(`<p id="fixture-disclosure-content">Advanced content</p>`)),
		withChildren(Dropdown("dropdown-trigger", "dropdown-menu", "Actions", nil), markup(`<button id="dropdown-action-table" type="button" role="menuitem" data-dropdown-value="table">View table</button><button id="dropdown-action-tabs" type="button" role="menuitem" data-dropdown-value="tabs">View tabs</button>`)),
		markup(`<output id="dropdown-selected-action" aria-live="polite">No action selected.</output>`),
		withChildren(EmptyState("No deployments", "Create a deployment to continue."), markup(`<button id="fixture-empty-action" type="button">Create deployment</button>`)),
		Flash(flash),
		withChildren(Popover("popover-trigger", "fixture-popover-panel", "Open popover"), markup(`<p id="fixture-popover-content">Popover content</p>`)),
		withChildren(Table("fixture-table", "Fixture deployments"), markup(`<thead><tr><th scope="col">Application</th><th scope="col">Status</th></tr></thead><tbody><tr><th scope="row">api</th><td>Live</td></tr></tbody>`)),
		Tabs("tabs-list", tabs, 0),
		markup(`<button id="toast-trigger" type="button">Show toast</button><div id="toast-region" aria-live="polite" hidden></div><template id="toast-template">`), Toast("fixture-toast", "info", "Fixture toast"), markup(`</template><output id="toast-dismissed-output" aria-live="polite">No toast dismissed.</output>`),
		withChildren(Tooltip("tooltip-trigger", "fixture-tooltip", "Tooltip content"), IconInformationCircle("size-5")),
		markup(`<section id="fixture-icons" aria-labelledby="fixture-icons-title"><h2 id="fixture-icons-title">Inline icons</h2><div class="flex flex-wrap gap-3">`), icons, markup(`</div></section>`),
		markup(`<template id="fixture-direct-nav-wrappers">`), Sidebar(org, user, "/projects"), markup(`</template>`),
		markup(`</div><script>
document.getElementById('fixture-form').addEventListener('submit', function (event) {
  event.preventDefault();
  var values = new FormData(event.currentTarget);
  document.getElementById('fixture-form-output').textContent = JSON.stringify(Object.fromEntries(values.entries()));
});
document.getElementById('fixture-empty-action').addEventListener('click', function () {
	document.getElementById('toast-trigger').click();
});
document.getElementById('dropdown-menu').addEventListener('click', function (event) {
  var action = event.target.closest('[data-dropdown-value]');
  if (action) document.getElementById('dropdown-selected-action').textContent = action.dataset.dropdownValue;
});
document.querySelectorAll('[data-fixture-theme]').forEach(function (button) {
  button.addEventListener('click', function () { window.cyclopusTheme.apply(button.dataset.fixtureTheme); });
});
(function () {
  var timer;
  var cleanupTimer;
  var region = document.getElementById('toast-region');
  var template = document.getElementById('toast-template');
  document.getElementById('toast-trigger').addEventListener('click', function () {
    clearTimeout(timer);
    clearTimeout(cleanupTimer);
    var toast = template.content.firstElementChild.cloneNode(true);
    toast.id = 'fixture-toast-' + String(Date.now());
    toast.querySelector('[aria-label="Dismiss notification"]').addEventListener('click', function () {
      document.getElementById('toast-dismissed-output').textContent = 'dismissed';
      clearTimeout(timer);
      cleanupTimer = setTimeout(function () { region.replaceChildren(); region.hidden = true; }, 300);
    });
    region.replaceChildren(toast);
    region.hidden = false;
    timer = setTimeout(function () {
      toast.dataset.leaving = 'true';
      cleanupTimer = setTimeout(function () { region.replaceChildren(); region.hidden = true; }, 300);
    }, 1200);
  });
})();
</script>`),
	)

	page := Page("Browser fixture", user, org, nil, "/projects", fixtureContent)
	var rendered bytes.Buffer
	if err := page.Render(context.Background(), &rendered); err != nil {
		t.Fatalf("render Page fixture: %v", err)
	}

	// Call AuthPage directly as part of the production-wrapper fixture contract,
	// without nesting a second HTML document inside the standalone Page fixture.
	renderComponent(t, withChildren(AuthPage("Fixture authentication"), markup(`<form id="fixture-auth-form"><label for="fixture-email">Email</label><input id="fixture-email" name="email" type="email"></form>`)))

	fixture := rendered.Bytes()
	fixture = replaceFixtureAsset(t, fixture, `/static/theme.js`, `/theme.js`)
	fixture = replaceFixtureAsset(t, fixture, `/static/css/output.css`, `/output.css`)
	fixture = replaceFixtureAsset(t, fixture, `/static/htmx.min.js`, `/htmx.min.js`)

	fixtureDir := filepath.Dir(outputPath)
	staticDir := fixtureStaticSourceDir(t)
	copyFixtureFile(t, filepath.Join(staticDir, "theme.js"), filepath.Join(fixtureDir, "theme.js"))
	copyFixtureFile(t, filepath.Join(staticDir, "css", "output.css"), filepath.Join(fixtureDir, "output.css"))
	copyFixtureFile(t, filepath.Join(staticDir, "htmx.min.js"), filepath.Join(fixtureDir, "htmx.min.js"))
	copyFixtureFile(t,
		filepath.Join(staticDir, "avatars", "critters", avatar.DefaultKey+".svg"),
		filepath.Join(fixtureDir, "avatars", avatar.DefaultKey+".svg"),
	)
	copiedFonts := copyFixtureTree(t, filepath.Join(staticDir, "fonts"), filepath.Join(fixtureDir, "fonts"))
	if len(copiedFonts) == 0 {
		t.Fatal("fixture font source contained no files")
	}

	if err := os.WriteFile(outputPath, fixture, 0o644); err != nil {
		t.Fatalf("write browser fixture %q: %v", outputPath, err)
	}

	written, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read browser fixture %q: %v", outputPath, err)
	}
	output := string(written)
	requireContains(t, output,
		`<!doctype html>`,
		`href="/output.css"`,
		`src="/theme.js"`,
		`src="/htmx.min.js"`,
		`src="/avatars/`+avatar.DefaultKey+`.svg"`,
		`@tailwindplus/elements@1`,
		`id="browser-fixture"`,
		`id="theme-obsidian"`,
		`id="theme-mono"`,
		`id="theme-ember"`,
		`id="theme-graphite"`,
		`id="theme-terminal"`,
		`id="theme-paper"`,
		`id="theme-console"`,
		`id="theme-solar"`,
		`id="fixture-form"`,
		`id="select-control"`,
		`<el-select id="select-control" name="environment" value="preview" required`,
		`id="select-control-trigger" type="button"`,
		`<el-selectedcontent id="select-control-selected"`,
		`id="select-control-options" role="listbox"`,
		`<el-option value="production"`,
		`id="combobox-input"`,
		`id="fixture-submit"`,
		`id="fixture-form-output"`,
		`id="command-trigger"`,
		`id="command"`,
		`id="command-input"`,
		`id="command-listbox"`,
		`id="command-value" type="hidden" data-command-value-output`,
		`id="command-output" aria-live="polite"`,
		`data-command-value="deploy"`,
		`option.hidden = option.`,
		`.toLowerCase().indexOf(query) === -1`,
		`root.dispatchEvent(new CustomEvent('commandselect'`,
		`id="dialog-trigger"`,
		`id="dialog-panel"`,
		`id="fixture-dialog-confirm"`,
		`id="disclosure-trigger"`,
		`id="fixture-disclosure-content"`,
		`id="dropdown-trigger"`,
		`id="dropdown-menu"`,
		`id="dropdown-action-table"`,
		`id="dropdown-action-tabs"`,
		`data-dropdown-value="tabs"`,
		`id="dropdown-selected-action" aria-live="polite">No action selected.`,
		`dropdown-selected-action').textContent = action.dataset.dropdownValue`,
		`id="fixture-empty-action"`,
		`id="flash-region"`,
		`id="popover-trigger"`,
		`id="fixture-popover-panel"`,
		`id="fixture-table"`,
		`id="tabs-list"`,
		`id="fixture-overview-tab"`,
		`id="fixture-activity-tab"`,
		`id="toast-trigger"`,
		`id="toast-region" aria-live="polite" hidden></div>`,
		`id="toast-template"`,
		`id="fixture-toast" role="status"`,
		`Fixture toast`,
		`cloneNode(true)`,
		`region.replaceChildren(toast)`,
		`region.hidden = false`,
		`region.hidden = true`,
		`id="toast-dismissed-output" aria-live="polite">No toast dismissed.`,
		`toast-dismissed-output').textContent = 'dismissed'`,
		`id="tooltip-trigger"`,
		`id="fixture-tooltip"`,
		`id="fixture-icons"`,
		`id="fixture-direct-nav-wrappers"`,
	)
	requireNotContains(t, output, `/static/theme.js`, `/static/css/output.css`, `/static/htmx.min.js`)
	requireNotContains(t, output, "ui.css", "web"+"awesome", "font"+"awesome", "wa-")
	assertFixtureFile(t, filepath.Join(fixtureDir, "theme.js"))
	assertFixtureFile(t, filepath.Join(fixtureDir, "output.css"))
	assertFixtureFile(t, filepath.Join(fixtureDir, "htmx.min.js"))
	assertFixtureFile(t, filepath.Join(fixtureDir, "avatars", avatar.DefaultKey+".svg"))
	for _, font := range copiedFonts {
		assertFixtureFile(t, font)
	}
}

func fixtureStaticSourceDir(t testing.TB) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate browser fixture test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "static"))
}

func copyFixtureTree(t testing.TB, sourceRoot, destinationRoot string) []string {
	t.Helper()
	var copied []string
	err := filepath.WalkDir(sourceRoot, func(sourcePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceRoot, sourcePath)
		if err != nil {
			return err
		}
		destinationPath := filepath.Join(destinationRoot, rel)
		if entry.IsDir() {
			return os.MkdirAll(destinationPath, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("fixture asset source must be a regular file: %q", sourcePath)
		}
		copyFixtureFile(t, sourcePath, destinationPath)
		copied = append(copied, destinationPath)
		return nil
	})
	if err != nil {
		t.Fatalf("copy fixture asset tree %q: %v", sourceRoot, err)
	}
	return copied
}

func copyFixtureFile(t testing.TB, sourcePath, destinationPath string) {
	t.Helper()
	contents, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read fixture asset %q: %v", sourcePath, err)
	}
	if len(contents) == 0 {
		t.Fatalf("fixture asset %q is empty", sourcePath)
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		t.Fatalf("create fixture asset directory %q: %v", filepath.Dir(destinationPath), err)
	}
	if info, err := os.Lstat(destinationPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("fixture asset destination must not be a symlink: %q", destinationPath)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("inspect fixture asset destination %q: %v", destinationPath, err)
	}
	if err := os.WriteFile(destinationPath, contents, 0o644); err != nil {
		t.Fatalf("write fixture asset %q: %v", destinationPath, err)
	}
}

func assertFixtureFile(t testing.TB, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("fixture asset %q does not exist: %v", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		t.Fatalf("fixture asset %q must be a non-empty regular file", path)
	}
}

func validateBrowserFixtureOutputPath(t testing.TB, outputPath string) string {
	t.Helper()
	cleanPath, err := approvedBrowserFixtureOutputPath(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	return cleanPath
}

func approvedBrowserFixtureOutputPath(outputPath string) (string, error) {
	if !filepath.IsAbs(outputPath) {
		return "", fmt.Errorf("browser fixture output path must be absolute: %q", outputPath)
	}
	cleanPath := filepath.Clean(outputPath)
	if outputPath != cleanPath {
		return "", fmt.Errorf("browser fixture output path must be clean: %q", outputPath)
	}
	cleanRoot := filepath.Clean(browserFixtureRoot)
	if cleanPath == cleanRoot {
		return "", fmt.Errorf("browser fixture output path must be a file strictly under approved root %q", cleanRoot)
	}
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("browser fixture output path %q is outside approved root %q", cleanPath, cleanRoot)
	}
	realRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		return "", fmt.Errorf("resolve browser fixture approved root %q: %w", cleanRoot, err)
	}
	realParent, err := filepath.EvalSymlinks(filepath.Dir(cleanPath))
	if err != nil {
		return "", fmt.Errorf("resolve browser fixture output parent %q: %w", filepath.Dir(cleanPath), err)
	}
	realRel, err := filepath.Rel(realRoot, realParent)
	if err != nil || realRel == ".." || filepath.IsAbs(realRel) || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("browser fixture output parent %q escapes approved root %q", realParent, realRoot)
	}
	if info, err := os.Lstat(cleanPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("browser fixture output path must not be a symlink: %q", cleanPath)
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect browser fixture output path %q: %w", cleanPath, err)
	}
	return cleanPath, nil
}

func TestApprovedBrowserFixtureOutputPath(t *testing.T) {
	approvedParent := filepath.Join(browserFixtureRoot, "fixture-contract")
	if err := os.MkdirAll(approvedParent, 0o755); err != nil {
		t.Skipf("create approved fixture test directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(approvedParent) })
	valid := filepath.Join(approvedParent, "index.html")
	if got, err := approvedBrowserFixtureOutputPath(valid); err != nil || got != valid {
		t.Fatalf("approvedBrowserFixtureOutputPath(%q) = %q, %v", valid, got, err)
	}

	for name, path := range map[string]string{
		"relative":  "fixture/index.html",
		"outside":   filepath.Join(t.TempDir(), "index.html"),
		"traversal": browserFixtureRoot + "/../escape/index.html",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := approvedBrowserFixtureOutputPath(path); err == nil {
				t.Fatalf("approvedBrowserFixtureOutputPath(%q) succeeded, want rejection", path)
			}
		})
	}
}

func TestApprovedBrowserFixtureOutputPathRejectsSymlinkEscape(t *testing.T) {
	link := filepath.Join(browserFixtureRoot, fmt.Sprintf("fixture-link-%d", os.Getpid()))
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Skipf("create approved-root symlink: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	if _, err := approvedBrowserFixtureOutputPath(filepath.Join(link, "index.html")); err == nil {
		t.Fatal("symlink escape succeeded, want rejection")
	}
}

func replaceFixtureAsset(t testing.TB, fixture []byte, productionURL, fixtureURL string) []byte {
	t.Helper()
	if count := bytes.Count(fixture, []byte(productionURL)); count != 1 {
		t.Fatalf("production fixture asset %q occurred %d times, want exactly 1", productionURL, count)
	}
	return bytes.Replace(fixture, []byte(productionURL), []byte(fixtureURL), 1)
}
