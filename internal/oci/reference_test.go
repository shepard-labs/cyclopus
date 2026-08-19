package oci

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestReferenceRepositoryTagAndDigest(t *testing.T) {
	sha := strings.Repeat("a", 40)
	validRepositories := []string{"owner/repo", "owner/repo.path", "owner/repo_name", "owner/repo__name", "owner/repo---name", strings.Repeat("a", 255)}
	for _, repository := range validRepositories {
		if _, err := ParseReference(GHCRHost, repository, sha); err != nil {
			t.Fatalf("ParseReference(%q): %v", repository, err)
		}
	}
	invalid := []Reference{
		{Host: "GHCR.io", Repository: "owner/repo", Tag: sha},
		{Host: "https://ghcr.io", Repository: "owner/repo", Tag: sha},
		{Host: GHCRHost, Repository: "Owner/repo", Tag: sha},
		{Host: GHCRHost, Repository: "owner//repo", Tag: sha},
		{Host: GHCRHost, Repository: "owner/../repo", Tag: sha},
		{Host: GHCRHost, Repository: "owner/repo:tag", Tag: sha},
		{Host: GHCRHost, Repository: "owner/repo@sha256:x", Tag: sha},
		{Host: GHCRHost, Repository: strings.Repeat("a", 256), Tag: sha},
		{Host: GHCRHost, Repository: "owner/repo", Tag: strings.ToUpper(sha)},
		{Host: GHCRHost, Repository: "owner/repo", Tag: sha[:39]},
		{Host: GHCRHost, Repository: "owner/repo", Tag: "latest"},
	}
	for _, value := range invalid {
		if _, err := ParseReference(value.Host, value.Repository, value.Tag); err == nil {
			t.Fatalf("ParseReference(%+v) succeeded", value)
		}
	}

	body := []byte("manifest bytes")
	sum := sha256.Sum256(body)
	digest := "sha256:" + strings.Repeat("0", 64)
	if _, canonical, err := ParseDigest(digest); err != nil || canonical != digest {
		t.Fatalf("ParseDigest() = %q, %v", canonical, err)
	}
	for _, value := range []string{"", "sha512:" + strings.Repeat("0", 64), "sha256:" + strings.Repeat("A", 64), "sha256:" + strings.Repeat("0", 63), "sha256: " + strings.Repeat("0", 64)} {
		if _, _, err := ParseDigest(value); err == nil {
			t.Fatalf("ParseDigest(%q) succeeded", value)
		}
	}
	actual := "sha256:" + fmtHex(sum[:])
	if got, err := VerifyBodyDigest(body, actual); err != nil || got != actual {
		t.Fatalf("VerifyBodyDigest() = %q, %v", got, err)
	}
	if _, err := VerifyBodyDigest(append(body, '!'), actual); !IsCode(err, CodeRegistryDigestInvalid) {
		t.Fatalf("digest mismatch error = %v", err)
	}
	if got, err := ImmutableReference("owner/repo", actual); err != nil || got != "ghcr.io/owner/repo@"+actual {
		t.Fatalf("ImmutableReference() = %q, %v", got, err)
	}
}

func TestCredentialCleanup(t *testing.T) {
	username := []byte("secret-user")
	pat := []byte("secret-pat")
	credential := &Credential{Username: username, PAT: pat}
	credential.Cleanup()
	for _, bytes := range [][]byte{username, pat} {
		for _, value := range bytes {
			if value != 0 {
				t.Fatal("Cleanup did not zero credential bytes")
			}
		}
	}
	if credential.Username != nil || credential.PAT != nil {
		t.Fatal("Cleanup retained credential slices")
	}
}

func fmtHex(value []byte) string {
	const alphabet = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for i, b := range value {
		result[i*2], result[i*2+1] = alphabet[b>>4], alphabet[b&15]
	}
	return string(result)
}
