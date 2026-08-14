package ui

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
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
		"--color-base-100", "--color-base-content", "--color-primary", "--color-line",
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

func TestThemeFoundationTokens(t *testing.T) {
	source := readThemeJS(t)
	wantLines := map[string]string{
		"obsidian": "rgba(255,255,255,0.06)",
		"mono":     "rgba(255,255,255,0.10)",
		"ember":    "rgba(250,250,249,0.08)",
		"graphite": "rgba(255,255,255,0.07)",
		"terminal": "rgba(255,255,255,0.12)",
		"paper":    "rgba(28,25,23,0.08)",
		"console":  "rgba(32,33,36,0.10)",
		"solar":    "rgba(7,54,66,0.10)",
	}
	wantAuthShadows := map[string]string{
		"obsidian": "0 8px 30px rgba(0,0,0,0.40), 0 0 0 1px rgba(45,212,191,0.35), 0 0 40px -10px rgba(45,212,191,0.25)",
		"mono":     "0 8px 30px rgba(0,0,0,0.40), 0 0 0 1px rgba(59,130,246,0.35), 0 0 40px -10px rgba(59,130,246,0.25)",
		"ember":    "0 8px 30px rgba(0,0,0,0.40), 0 0 0 1px rgba(251,146,60,0.35), 0 0 40px -10px rgba(251,146,60,0.25)",
		"graphite": "0 8px 30px rgba(0,0,0,0.40), 0 0 0 1px rgba(94,106,210,0.35), 0 0 40px -10px rgba(94,106,210,0.25)",
		"terminal": "0 8px 30px rgba(0,0,0,0.40), 0 0 0 1px rgba(0,220,130,0.35), 0 0 40px -10px rgba(0,220,130,0.25)",
		"paper":    "0 8px 30px rgba(0,0,0,0.18), 0 0 0 1px rgba(13,148,136,0.35), 0 0 40px -10px rgba(13,148,136,0.25)",
		"console":  "0 8px 30px rgba(0,0,0,0.18), 0 0 0 1px rgba(26,115,232,0.35), 0 0 40px -10px rgba(26,115,232,0.25)",
		"solar":    "0 8px 30px rgba(0,0,0,0.18), 0 0 0 1px rgba(42,161,152,0.35), 0 0 40px -10px rgba(42,161,152,0.25)",
	}

	for name, line := range wantLines {
		pattern := `(?s)    ` + regexp.QuoteMeta(name) + `: \{.*?vars: \{(.*?)\n      \}`
		match := regexp.MustCompile(pattern).FindStringSubmatch(source)
		if len(match) != 2 {
			t.Fatalf("theme %s vars block not found", name)
		}
		vars := match[1]
		for _, required := range []string{
			`'--color-line': '` + line + `'`,
			`'--shadow': '0 8px 30px `,
			`'--shadow-auth-glow': '` + wantAuthShadows[name] + `'`,
			"ui-sans-serif, system-ui, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji', 'Segoe UI Symbol', 'Noto Color Emoji'",
			"ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace",
		} {
			if !strings.Contains(vars, required) {
				t.Errorf("theme %s missing %q", name, required)
			}
		}
	}
	if strings.Contains(source, "color-mix(") {
		t.Error("theme.js auth shadows must not depend on color-mix compatibility")
	}

	for _, prohibited := range []string{"--border-w", "--dim", "--violet", "--teal", "--text-"} {
		if strings.Contains(source, prohibited) {
			t.Errorf("theme.js contains prohibited token prefix %q", prohibited)
		}
	}
}

func TestBaseContentFocusAndSmallTextContrastAcrossThemes(t *testing.T) {
	source := readThemeJS(t)
	for _, name := range []string{"obsidian", "mono", "ember", "graphite", "terminal", "paper", "console", "solar"} {
		pattern := `(?s)    ` + regexp.QuoteMeta(name) + `: \{.*?vars: \{(.*?)\n      \}`
		match := regexp.MustCompile(pattern).FindStringSubmatch(source)
		if len(match) != 2 {
			t.Fatalf("theme %s vars block not found", name)
		}
		vars := match[1]
		content := themeHexColor(t, vars, "--color-base-content")
		for _, backgroundToken := range []string{"--color-base-200", "--color-base-300"} {
			background := themeHexColor(t, vars, backgroundToken)
			ratio := contrastRatio(content, background)
			t.Run(name+"/"+backgroundToken, func(t *testing.T) {
				if ratio < 3 {
					t.Errorf("base-content focus indicator contrast = %.2f:1, want >= 3:1", ratio)
				}
				if ratio < 4.5 {
					t.Errorf("base-content small-text contrast = %.2f:1, want >= 4.5:1", ratio)
				}
			})
		}
	}
}

func TestPrimaryContentSmallTextContrastAcrossThemes(t *testing.T) {
	source := readThemeJS(t)
	for _, name := range []string{"obsidian", "mono", "ember", "graphite", "terminal", "paper", "console", "solar"} {
		pattern := `(?s)    ` + regexp.QuoteMeta(name) + `: \{.*?vars: \{(.*?)\n      \}`
		match := regexp.MustCompile(pattern).FindStringSubmatch(source)
		if len(match) != 2 {
			t.Fatalf("theme %s vars block not found", name)
		}
		primary := themeHexColor(t, match[1], "--color-primary")
		primaryContent := themeHexColor(t, match[1], "--color-primary-content")
		if ratio := contrastRatio(primary, primaryContent); ratio < 4.5 {
			t.Errorf("theme %s primary-content contrast = %.2f:1, want >= 4.5:1", name, ratio)
		}
	}
}

func themeHexColor(t testing.TB, vars, token string) [3]float64 {
	t.Helper()
	match := regexp.MustCompile(regexp.QuoteMeta(`'`+token+`': '#`) + `([0-9A-Fa-f]{6})'`).FindStringSubmatch(vars)
	if len(match) != 2 {
		t.Fatalf("theme token %s hex color not found", token)
	}
	var color [3]float64
	for i := range color {
		value, err := strconv.ParseUint(match[1][i*2:i*2+2], 16, 8)
		if err != nil {
			t.Fatal(fmt.Errorf("parse %s: %w", token, err))
		}
		color[i] = float64(value) / 255
	}
	return color
}

func contrastRatio(a, b [3]float64) float64 {
	aLuminance := relativeLuminance(a)
	bLuminance := relativeLuminance(b)
	return (math.Max(aLuminance, bLuminance) + 0.05) / (math.Min(aLuminance, bLuminance) + 0.05)
}

func relativeLuminance(color [3]float64) float64 {
	for i, channel := range color {
		if channel <= 0.04045 {
			color[i] = channel / 12.92
		} else {
			color[i] = math.Pow((channel+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*color[0] + 0.7152*color[1] + 0.0722*color[2]
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
	for _, required := range []string{
		"--color-line: var(--tw-color-line);",
		"--shadow-theme: var(--tw-shadow-theme);",
		"--shadow-auth-glow: var(--tw-shadow-auth-glow);",
		"--font-sans: var(--tw-font-sans);",
		"--font-mono: var(--tw-font-mono);",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("missing Tailwind foundation bridge %q", required)
		}
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
