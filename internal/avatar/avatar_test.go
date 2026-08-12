package avatar

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBundledAvatarManifest(t *testing.T) {
	if !Valid(DefaultKey) {
		t.Fatalf("default avatar %q is not in the manifest", DefaultKey)
	}
	seen := make(map[string]struct{}, len(Keys))
	for _, key := range Keys {
		if _, exists := seen[key]; exists {
			t.Fatalf("duplicate avatar key %q", key)
		}
		seen[key] = struct{}{}
		path := filepath.Join("..", "ui", "static", "avatars", "critters", key+".svg")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("avatar %q has no bundled SVG: %v", key, err)
		}
	}
	files, err := filepath.Glob(filepath.Join("..", "ui", "static", "avatars", "critters", "*.svg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(Keys) {
		t.Fatalf("bundled SVG count = %d, manifest count = %d", len(files), len(Keys))
	}
	for _, file := range files {
		key := filepath.Base(file[:len(file)-len(filepath.Ext(file))])
		if !Valid(key) {
			t.Errorf("bundled SVG %q is missing from the manifest", filepath.Base(file))
		}
	}
}
