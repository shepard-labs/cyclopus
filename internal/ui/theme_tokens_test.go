package ui

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

func readThemeJS(t *testing.T) string {
	t.Helper()
	contents, err := staticFS.ReadFile("static/theme.js")
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func TestThemePresetNames(t *testing.T) {
	source := readThemeJS(t)
	block := regexp.MustCompile(`(?s)var THEMES = \{(.*?)\n  \};`).FindStringSubmatch(source)
	if len(block) != 2 {
		t.Fatal("THEMES block not found")
	}
	matches := regexp.MustCompile(`(?m)^    ([a-z]+): \{$`).FindAllStringSubmatch(block[1], -1)
	got := make([]string, 0, len(matches))
	for _, match := range matches {
		got = append(got, match[1])
	}
	sort.Strings(got)
	want := []string{"console", "ember", "graphite", "mono", "obsidian", "paper", "solar", "terminal"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("theme presets = %v, want %v", got, want)
	}
}

func TestThemeCanonicalTokenContract(t *testing.T) {
	source := readThemeJS(t)
	for _, token := range []string{
		"--color-base-100", "--color-base-content", "--color-primary",
		"--color-success", "--color-warning", "--color-error",
	} {
		if !strings.Contains(source, token) {
			t.Errorf("missing canonical token %s", token)
		}
	}
	for _, prohibited := range []string{"--w" + "a-", "wa-dark", "wa-light", "--cyclopus-"} {
		if strings.Contains(source, prohibited) {
			t.Errorf("theme.js contains prohibited legacy vocabulary %q", prohibited)
		}
	}
}

func TestThemeLocalStoragePersistence(t *testing.T) {
	source := readThemeJS(t)
	for _, required := range []string{
		"var KEY = 'cyclopus-theme-v2'", "storageGet(KEY)", "storageSet(KEY, themeName)",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("missing persistence contract %q", required)
		}
	}
}

func TestThemeRemembersDarkAndLightSelections(t *testing.T) {
	source := readThemeJS(t)
	for _, required := range []string{
		"KEY + (theme.dark ? '-dark' : '-light')",
		"KEY + (targetDark ? '-dark' : '-light')",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("missing mode-memory contract %q", required)
		}
	}
}

func TestThemeDefaultsToObsidianAndPaper(t *testing.T) {
	source := readThemeJS(t)
	if !strings.Contains(source, "var DEFAULT_DARK = 'obsidian'") ||
		!strings.Contains(source, "var DEFAULT_LIGHT = 'paper'") {
		t.Fatal("theme defaults must be obsidian and paper")
	}
}

func TestThemeHTMXResync(t *testing.T) {
	if !strings.Contains(readThemeJS(t), "document.addEventListener('htmx:load', sync)") {
		t.Fatal("theme chrome is not resynchronized after htmx loads")
	}
}

func TestInputCSSContainsOnlyApprovedConstructs(t *testing.T) {
	contents, err := staticFS.ReadFile("static/css/input.css")
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	if !strings.Contains(source, `@import "tailwindcss";`) {
		t.Error("missing Tailwind import")
	}
	if !strings.Contains(source, `@source "../../**/*.templ";`) {
		t.Error("missing explicit internal/ui templ source")
	}
	if !strings.Contains(source, "@theme inline {") {
		t.Error("missing Tailwind theme bridge")
	}
	for _, prohibited := range []string{
		"@keyframes", "scrollbar", "::-webkit", "::-moz", ".toast", ".log",
		".sidebar", ".card", ".table", ".button",
	} {
		if strings.Contains(source, prohibited) {
			t.Errorf("input.css contains prohibited construct %q", prohibited)
		}
	}

	withoutApprovedBlocks := regexp.MustCompile(`(?s)@theme\s+inline\s*\{.*?\}|@font-face\s*\{.*?\}`).ReplaceAllString(source, "")
	withoutApprovedBlocks = regexp.MustCompile(`(?m)^\s*@(import|source)\b[^;]*;\s*$`).ReplaceAllString(withoutApprovedBlocks, "")
	if strings.TrimSpace(withoutApprovedBlocks) != "" {
		t.Fatalf("input.css contains content outside approved Tailwind/theme/font constructs: %q", strings.TrimSpace(withoutApprovedBlocks))
	}
}
