package service

import (
	"bytes"
	"regexp"
	"testing"
)

func TestPreviewSlugCSPRNGFormatAndNamespaceBound(t *testing.T) {
	generator := randomPreviewSlugGenerator{source: bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8})}
	slug, err := generator.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9a-z]{6}$`).MatchString(slug) {
		t.Fatalf("preview slug = %q", slug)
	}
	if len("preview-"+slug) > 63 {
		t.Fatalf("namespace is too long: %q", "preview-"+slug)
	}
}

func TestPreviewSlugInjectedGeneratorIsDeterministic(t *testing.T) {
	generator := &sequencePreviewSlugGenerator{values: []string{"calm-otter-0001", "calm-otter-0002"}}
	first, _ := generator.Generate()
	second, _ := generator.Generate()
	if first != "calm-otter-0001" || second != "calm-otter-0002" {
		t.Fatalf("generated %q and %q", first, second)
	}
}

func TestPreviewSlugSpaceExceedsThirtySixBits(t *testing.T) {
	space := uint64(len(previewAdjectives) * len(previewNouns))
	for range 6 {
		space *= 36
	}
	if space < uint64(1)<<36 {
		t.Fatalf("slug space = %d, want at least 2^36", space)
	}
}

type sequencePreviewSlugGenerator struct{ values []string }

func (g *sequencePreviewSlugGenerator) Generate() (string, error) {
	value := g.values[0]
	g.values = g.values[1:]
	return value, nil
}
