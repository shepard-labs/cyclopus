package oci

import (
	"strings"
	"testing"
)

func TestSourceEvidence(t *testing.T) {
	identity := RepositoryIdentity{Owner: "Acme", Repository: "API"}
	revision := strings.Repeat("a", 40)
	validSources := []string{
		"https://github.com/acme/api",
		"https://github.com/ACME/API.git",
		"https://github.com/acme/api/",
	}
	for _, source := range validSources {
		evidence, err := VerifySourceEvidence(identity, revision, AnnotationPolicyValidateIfPresent, EvidenceValues{
			ManifestAnnotations: map[string]string{AnnotationSource: source, AnnotationRevision: revision},
			ConfigLabels:        map[string]string{AnnotationSource: "https://github.com/Acme/API.git", AnnotationRevision: revision},
		})
		if err != nil || evidence.Status != AnnotationStatusMatched || evidence.Source != "https://github.com/acme/api" {
			t.Fatalf("VerifySourceEvidence(%q) = %+v, %v", source, evidence, err)
		}
	}
	for name, values := range map[string]EvidenceValues{
		"absent":        {},
		"source only":   {ManifestAnnotations: map[string]string{AnnotationSource: validSources[0]}},
		"revision only": {ManifestAnnotations: map[string]string{AnnotationRevision: revision}},
	} {
		evidence, err := VerifySourceEvidence(identity, revision, AnnotationPolicyValidateIfPresent, values)
		if err != nil || evidence.Status != AnnotationStatusAbsent {
			t.Fatalf("%s = %+v, %v", name, evidence, err)
		}
	}

	invalidSources := []string{
		"http://github.com/acme/api", "https://user@github.com/acme/api", "https://github.com:443/acme/api",
		"https://github.com/acme/api?q=1", "https://github.com/acme/api#x", "https://github.com/acme/api/extra",
		"https://github.com/acme%2fother/api", "https://gitHub.com/acme/api", " https://github.com/acme/api",
	}
	for _, source := range invalidSources {
		if _, err := NormalizeGitHubSource(source, identity); err == nil {
			t.Fatalf("NormalizeGitHubSource(%q) succeeded", source)
		}
	}
	if _, err := NormalizeGitHubSource("https://github.com/other/api", identity); !IsCode(err, CodeOCISourceMismatch) {
		t.Fatalf("source mismatch = %v", err)
	}
	if _, err := VerifySourceEvidence(identity, revision, AnnotationPolicyValidateIfPresent, EvidenceValues{
		ManifestAnnotations: map[string]string{AnnotationSource: "https://github.com/acme/api", AnnotationRevision: revision},
		ConfigLabels:        map[string]string{AnnotationSource: "https://github.com/other/api", AnnotationRevision: revision},
	}); !IsCode(err, CodeOCIAnnotationConflict) {
		t.Fatalf("source conflict error = %v", err)
	}
	if _, err := VerifySourceEvidence(identity, revision, AnnotationPolicyValidateIfPresent, EvidenceValues{
		ManifestAnnotations: map[string]string{AnnotationSource: "https://github.com/other/api", AnnotationRevision: revision},
		ConfigLabels:        map[string]string{AnnotationSource: "https://github.com/other/api", AnnotationRevision: revision},
	}); !IsCode(err, CodeOCISourceMismatch) {
		t.Fatalf("source mismatch after agreement = %v", err)
	}
	if _, err := VerifySourceEvidence(identity, revision, AnnotationPolicyValidateIfPresent, EvidenceValues{
		ManifestAnnotations: map[string]string{AnnotationSource: validSources[0], AnnotationRevision: revision},
		ConfigLabels:        map[string]string{AnnotationSource: validSources[0], AnnotationRevision: strings.Repeat("b", 40)},
	}); !IsCode(err, CodeOCIAnnotationConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := VerifySourceEvidence(identity, revision, AnnotationPolicyValidateIfPresent, EvidenceValues{
		ManifestAnnotations: map[string]string{AnnotationRevision: strings.Repeat("b", 40)},
	}); !IsCode(err, CodeOCIRevisionMismatch) {
		t.Fatalf("revision mismatch = %v", err)
	}
}
