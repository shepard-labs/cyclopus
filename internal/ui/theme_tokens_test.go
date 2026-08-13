package ui

import (
	"regexp"
	"strings"
	"testing"
)

// The no-JS fallback in tokens.css must exactly match theme.js's Obsidian
// preset. theme.js is the source of truth; this test prevents silent drift.
func TestObsidianFallbackMatchesThemeJS(t *testing.T) {
	themeJS, err := staticFS.ReadFile("static/theme.js")
	if err != nil {
		t.Fatal(err)
	}
	tokensCSS, err := staticFS.ReadFile("static/css/tokens.css")
	if err != nil {
		t.Fatal(err)
	}

	obsidianRE := regexp.MustCompile(`(?s)obsidian:\s*\{.*?vars:\s*\{(.*?)\}\s*\}`)
	match := obsidianRE.FindSubmatch(themeJS)
	if len(match) != 2 {
		t.Fatal("obsidian vars block not found in theme.js")
	}
	jsVars := parseTokenPairs(string(match[1]), regexp.MustCompile(`"(--[a-z0-9-]+)"\s*:\s*"([^"]*)"`))
	cssVars := parseTokenPairs(string(tokensCSS), regexp.MustCompile(`(--[a-z0-9-]+)\s*:\s*([^;]+);`))

	if len(jsVars) != len(cssVars) {
		t.Fatalf("fallback token count differs: theme.js=%d tokens.css=%d", len(jsVars), len(cssVars))
	}
	for name, want := range jsVars {
		if got, ok := cssVars[name]; !ok || got != want {
			t.Errorf("%s mismatch: theme.js=%q tokens.css=%q", name, want, got)
		}
	}
}

func parseTokenPairs(source string, re *regexp.Regexp) map[string]string {
	vars := make(map[string]string)
	for _, match := range re.FindAllStringSubmatch(source, -1) {
		vars[match[1]] = strings.TrimSpace(match[2])
	}
	return vars
}
