package ui_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

var (
	classTokenPattern   = regexp.MustCompile(`(?:[a-z][a-z0-9_-]*:)*!?[a-z][a-z0-9_./%\[\]-]*`)
	h1Pattern           = regexp.MustCompile(`(?s)<h1\b[^>]*>`)
	staticClassPattern  = regexp.MustCompile(`class\s*=\s*"([^"]*)"`)
	classListPattern    = regexp.MustCompile(`class\s*=\s*["']([^"']*)["']`)
	elementClassPattern = regexp.MustCompile(`(?s)<([a-z][a-z0-9-]*)\b[^>]*\bclass\s*=\s*["']([^"']*)["']`)
)

func TestApprovedDesignSystemSourcePolicy(t *testing.T) {
	files := productionTemplSources(t)

	t.Run("typography uses approved size tokens", func(t *testing.T) {
		for _, file := range files {
			forEachClassToken(t, file, func(line int, class string) {
				if reason := prohibitedTypographyClass(class); reason != "" {
					t.Errorf("%s:%d: offending class %q: %s", file.name, line, class, reason)
				}
			})
		}
	})

	t.Run("shell omits legacy dimensions", func(t *testing.T) {
		for _, name := range []string{"components/layout.templ", "components/nav.templ"} {
			file := requireTemplSource(t, files, name)
			forEachClassToken(t, file, func(line int, class string) {
				if baseClass(class) == "w-72" || baseClass(class) == "pl-72" || baseClass(class) == "h-16" {
					t.Errorf("%s:%d: offending legacy shell class %q", file.name, line, class)
				}
			})
		}
	})

	t.Run("production templates use semantic palette", func(t *testing.T) {
		for _, file := range files {
			forEachClassToken(t, file, func(line int, class string) {
				if fixedPaletteClass(baseClass(class)) && !approvedFixedPaletteException(file.name, class) {
					t.Errorf("%s:%d: offending fixed palette class %q; use a semantic color token", file.name, line, class)
				}
			})
		}
	})

	t.Run("fixed palette exceptions stay narrow and in use", func(t *testing.T) {
		for exception := range fixedPaletteExceptions {
			file := requireTemplSource(t, files, exception.file)
			found := false
			forEachClassToken(t, file, func(_ int, class string) {
				if class == exception.class {
					found = true
				}
			})
			if !found {
				t.Errorf("%s: fixed palette exception for %q is no longer present; remove the exception", exception.file, exception.class)
			}
		}
	})

	t.Run("passive separators use line semantics", func(t *testing.T) {
		for _, file := range files {
			forEachClassList(t, file, func(line int, classes string) {
				for _, class := range passiveSeparatorViolations("", classes) {
					t.Errorf("%s:%d: offending passive separator class %q; use border-line or divide-line", file.name, line, class)
				}
			})
			forEachElementClassList(t, file, func(line int, element, classes string) {
				for _, class := range passiveSeparatorViolations(element, classes) {
					if strings.Contains(baseClass(class), "base-content/") {
						t.Errorf("%s:%d: offending passive separator class %q on <%s>; use border-line or divide-line", file.name, line, class, element)
					}
				}
			})
		}
	})

	t.Run("operational helpers render dot statuses", func(t *testing.T) {
		file := requireTemplSource(t, files, "components/badge.templ")
		for _, helper := range []string{"ReleaseStatusBadge", "EnvLifecycleBadge", "OperationStatusBadge"} {
			body, line, ok := templBody(file.content, helper)
			if !ok {
				t.Errorf("%s: missing operational helper %s", file.name, helper)
				continue
			}
			if !strings.Contains(body, "@OperationalStatus(") {
				t.Errorf("%s:%d: %s must render OperationalStatus dot semantics", file.name, line, helper)
			}
			if strings.Contains(body, "@Badge(") {
				t.Errorf("%s:%d: %s must not render Badge pills", file.name, line, helper)
			}
		}
	})

	t.Run("page headings use compact title typography", func(t *testing.T) {
		// Auth pages intentionally use their own centered-page typography. Component
		// fixtures are Go test strings and are not part of this .templ source scan.
		exceptions := map[string]string{
			"pages/bootstrap.templ": "auth page",
			"pages/login.templ":     "auth page",
		}
		for _, file := range files {
			if !strings.HasPrefix(file.name, "pages/") || exceptions[file.name] != "" {
				continue
			}
			for _, location := range h1Pattern.FindAllStringIndex(file.content, -1) {
				tag := file.content[location[0]:location[1]]
				line := sourceLine(file.content, location[0])
				match := staticClassPattern.FindStringSubmatch(tag)
				if len(match) != 2 {
					t.Errorf("%s:%d: page h1 must expose a static text-lg/7 class", file.name, line)
					continue
				}
				if !containsClass(match[1], "text-lg/7") {
					t.Errorf("%s:%d: page h1 has offending classes %q; expected text-lg/7", file.name, line, match[1])
				}
			}
		}
	})

	t.Run("theme settings use approved surfaces and typography", func(t *testing.T) {
		file := requireTemplSource(t, files, "pages/settings/theme.templ")
		requireSourceContracts(t, file,
			"rounded-box border border-line bg-base-300",
		)
	})
}

type templSource struct {
	name    string
	content string
}

type fixedPaletteException struct {
	file  string
	class string
}

// Fixed palette exceptions must identify one structural use in one source file.
// There are currently no authored fixed-palette utilities that need an exception.
var fixedPaletteExceptions = map[fixedPaletteException]string{}

func productionTemplSources(t testing.TB) []templSource {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate design-system contract test")
	}
	root := filepath.Dir(currentFile)
	var files []templSource
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".templ" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, templSource{name: filepath.ToSlash(name), content: string(content)})
		return nil
	})
	if err != nil {
		t.Fatalf("scan production templ sources: %v", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	if len(files) == 0 {
		t.Fatal("no production .templ sources found under internal/ui")
	}
	return files
}

func requireTemplSource(t testing.TB, files []templSource, name string) templSource {
	t.Helper()
	for _, file := range files {
		if file.name == name {
			return file
		}
	}
	t.Fatalf("required production source %s was not scanned", name)
	return templSource{}
}

func forEachClassToken(t testing.TB, file templSource, check func(line int, class string)) {
	t.Helper()
	for _, quoted := range quotedSourceStrings(file.content) {
		for _, location := range classTokenPattern.FindAllStringIndex(quoted.value, -1) {
			class := quoted.value[location[0]:location[1]]
			if strings.Contains(class, "-") {
				check(sourceLine(file.content, quoted.offset+location[0]), class)
			}
		}
	}
}

func forEachClassList(t testing.TB, file templSource, check func(line int, classes string)) {
	t.Helper()
	for _, quoted := range quotedSourceStrings(file.content) {
		matches := classListPattern.FindAllStringSubmatchIndex(quoted.value, -1)
		for _, match := range matches {
			check(sourceLine(file.content, quoted.offset+match[2]), quoted.value[match[2]:match[3]])
		}
		// A normal templ class attribute is itself the quoted source string, so it
		// does not contain the surrounding class="..." syntax seen in script HTML.
		if strings.Contains(quoted.value, "-") && !strings.Contains(quoted.value, "class=") {
			check(sourceLine(file.content, quoted.offset), quoted.value)
		}
	}
}

func forEachElementClassList(t testing.TB, file templSource, check func(line int, element, classes string)) {
	t.Helper()
	for _, match := range elementClassPattern.FindAllStringSubmatchIndex(file.content, -1) {
		check(sourceLine(file.content, match[0]), file.content[match[2]:match[3]], file.content[match[4]:match[5]])
	}
}

type sourceString struct {
	offset int
	value  string
}

func quotedSourceStrings(content string) []sourceString {
	var stringsInSource []sourceString
	for index := 0; index < len(content); index++ {
		quote := content[index]
		if quote != '\'' && quote != '"' && quote != '`' {
			continue
		}
		start := index + 1
		for index++; index < len(content); index++ {
			if quote != '`' && content[index] == '\\' {
				index++
				continue
			}
			if content[index] == quote {
				stringsInSource = append(stringsInSource, sourceString{offset: start, value: content[start:index]})
				break
			}
		}
	}
	return stringsInSource
}

func prohibitedTypographyClass(class string) string {
	class = baseClass(class)
	if !strings.HasPrefix(class, "text-") {
		return ""
	}
	suffix := strings.TrimPrefix(class, "text-")
	if strings.HasPrefix(suffix, "[") {
		return "arbitrary text sizes are not approved"
	}
	size := suffix
	if slash := strings.IndexByte(size, '/'); slash >= 0 {
		size = size[:slash]
	}
	switch size {
	case "xs", "sm", "base", "lg", "xl":
		return ""
	case "2xl", "3xl", "4xl", "5xl", "6xl", "7xl", "8xl", "9xl":
		return "typography sizes are limited to text-xs/sm/base/lg/xl"
	}
	if standardTextUtility(suffix) || semanticTextColor(suffix) || paletteTextColor(suffix) {
		return ""
	}
	return "custom text token classes are not approved"
}

func standardTextUtility(value string) bool {
	switch value {
	case "left", "center", "right", "justify", "start", "end",
		"wrap", "nowrap", "balance", "pretty", "clip", "ellipsis",
		"uppercase", "lowercase", "capitalize", "normal-case",
		"current", "transparent", "inherit", "black", "white":
		return true
	default:
		return false
	}
}

func semanticTextColor(value string) bool {
	value = strings.SplitN(value, "/", 2)[0]
	switch value {
	case "base-content", "primary", "primary-content", "secondary", "secondary-content",
		"accent", "accent-content", "neutral", "neutral-content", "info", "info-content",
		"success", "success-content", "warning", "warning-content", "error", "error-content":
		return true
	default:
		return false
	}
}

func paletteTextColor(value string) bool {
	value = strings.SplitN(value, "/", 2)[0]
	parts := strings.Split(value, "-")
	if len(parts) < 2 {
		return false
	}
	for _, r := range parts[len(parts)-1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func fixedPaletteClass(class string) bool {
	for _, prefix := range []string{"bg-", "text-", "border-", "ring-", "divide-"} {
		if !strings.HasPrefix(class, prefix) {
			continue
		}
		color := strings.SplitN(strings.TrimPrefix(class, prefix), "/", 2)[0]
		if color == "black" || color == "white" {
			return true
		}
		name, shade, hasShade := strings.Cut(color, "-")
		if name == "zinc" || name == "violet" || name == "teal" {
			return !hasShade || numericShade(shade)
		}
		return hasShade && tailwindHue(name) && numericShade(shade)
	}
	return false
}

func approvedFixedPaletteException(file, class string) bool {
	_, ok := fixedPaletteExceptions[fixedPaletteException{file: file, class: class}]
	return ok
}

func numericShade(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func tailwindHue(value string) bool {
	switch value {
	case "slate", "gray", "zinc", "neutral", "stone", "red", "orange", "amber", "yellow",
		"lime", "green", "emerald", "teal", "cyan", "sky", "blue", "indigo", "violet",
		"purple", "fuchsia", "pink", "rose":
		return true
	default:
		return false
	}
}

func passiveSeparatorViolations(element, classes string) []string {
	classSet := make(map[string]bool)
	for _, class := range strings.Fields(classes) {
		classSet[baseClass(class)] = true
	}
	passiveBorder := classSet["border-t"] || classSet["border-r"] || classSet["border-b"] || classSet["border-l"] ||
		classSet["border-x"] || classSet["border-y"]
	fullBorder := classSet["border"] && !interactiveControlElement(element)

	var violations []string
	for _, class := range strings.Fields(classes) {
		base := baseClass(class)
		switch {
		case base == "border-base-300" || base == "divide-base-300":
			violations = append(violations, class)
		case strings.HasPrefix(base, "divide-base-content/"):
			violations = append(violations, class)
		case (passiveBorder || fullBorder) && strings.HasPrefix(base, "border-base-content/"):
			violations = append(violations, class)
		}
	}
	return violations
}

func interactiveControlElement(element string) bool {
	switch element {
	case "a", "button", "input", "select", "textarea":
		return true
	default:
		return false
	}
}

func baseClass(class string) string {
	if colon := strings.LastIndexByte(class, ':'); colon >= 0 {
		class = class[colon+1:]
	}
	return strings.TrimPrefix(class, "!")
}

func containsClass(classes, expected string) bool {
	for _, class := range strings.Fields(classes) {
		if class == expected {
			return true
		}
	}
	return false
}

func templBody(content, name string) (body string, line int, ok bool) {
	startPattern := regexp.MustCompile(`templ\s+` + regexp.QuoteMeta(name) + `\s*\([^)]*\)\s*\{`)
	location := startPattern.FindStringIndex(content)
	if location == nil {
		return "", 0, false
	}
	depth := 1
	for index := location[1]; index < len(content); index++ {
		switch content[index] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return content[location[1]:index], sourceLine(content, location[0]), true
			}
		}
	}
	return "", sourceLine(content, location[0]), false
}

func requireSourceContracts(t testing.TB, file templSource, contracts ...string) {
	t.Helper()
	for _, contract := range contracts {
		if !strings.Contains(file.content, contract) {
			t.Errorf("%s: missing source contract %q", file.name, contract)
		}
	}
}

func sourceLine(content string, offset int) int {
	return 1 + strings.Count(content[:offset], "\n")
}

func TestDesignSystemPolicyAllowsIntentionalVariants(t *testing.T) {
	for _, class := range []string{
		"text-xs/5",
		"sm:text-lg/7",
		"text-base-content/65",
		"hover:text-primary",
		"border-base-content/20",
	} {
		t.Run(fmt.Sprintf("allows_%s", strings.NewReplacer("/", "_", ":", "_").Replace(class)), func(t *testing.T) {
			if reason := prohibitedTypographyClass(class); reason != "" {
				t.Fatalf("approved class %q rejected: %s", class, reason)
			}
			if class == "border-base-content/20" && (baseClass(class) == "border-base-300" || baseClass(class) == "divide-base-300") {
				t.Fatalf("intentional alpha content border %q treated as a passive separator", class)
			}
		})
	}

	for _, classes := range []string{
		"rounded-field border border-base-content/20 focus:border-primary",
		"ring-1 ring-base-content/10 focus:ring-primary",
	} {
		if violations := passiveSeparatorViolations("input", classes); len(violations) != 0 {
			t.Errorf("interactive control classes %q rejected as passive separators: %v", classes, violations)
		}
	}
}

func TestDesignSystemPolicyRejectsUnapprovedClasses(t *testing.T) {
	t.Run("typography", func(t *testing.T) {
		for _, class := range []string{"text-[13px]", "text-2xl", "sm:text-caption"} {
			if reason := prohibitedTypographyClass(class); reason == "" {
				t.Errorf("unapproved typography class %q was accepted", class)
			}
		}
	})

	t.Run("fixed palette in components and non-log pages", func(t *testing.T) {
		cases := []struct {
			file  string
			class string
		}{
			{file: "components/dialog.templ", class: "bg-black/60"},
			{file: "components/dialog.templ", class: "text-white"},
			{file: "components/alert.templ", class: "border-zinc-300"},
			{file: "pages/projects.templ", class: "bg-violet-500"},
			{file: "pages/settings/user.templ", class: "text-teal-600"},
			{file: "pages/environment.templ", class: "ring-blue-400"},
		}
		for _, testCase := range cases {
			if !fixedPaletteClass(baseClass(testCase.class)) {
				t.Errorf("%s: fixed palette class %q was accepted", testCase.file, testCase.class)
			}
			if approvedFixedPaletteException(testCase.file, testCase.class) {
				t.Errorf("%s: negative case %q unexpectedly matched an approved exception", testCase.file, testCase.class)
			}
		}
	})

	t.Run("passive base-content separators", func(t *testing.T) {
		for _, testCase := range []struct {
			element string
			classes string
		}{
			{element: "div", classes: "border-b border-base-content/10"},
			{element: "section", classes: "border-y border-base-content/20"},
			{element: "ul", classes: "divide-y divide-base-content/10"},
			{element: "div", classes: "border border-base-content/10"},
			{element: "div", classes: "border border-base-300"},
			{element: "table", classes: "divide-x divide-base-300"},
		} {
			if violations := passiveSeparatorViolations(testCase.element, testCase.classes); len(violations) == 0 {
				t.Errorf("passive <%s> separator classes %q were accepted", testCase.element, testCase.classes)
			}
		}
	})
}
