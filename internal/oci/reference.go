package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

const (
	GHCRHost           = "ghcr.io"
	MaxRepositoryBytes = 255
)

var (
	repositoryComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_{1,2}|-+)[a-z0-9]+)*$`)
	lowerCommitSHA      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	lowerSHA256Digest   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type Reference struct {
	Host       string
	Repository string
	Tag        string
}

func ParseReference(host, repository, tag string) (Reference, error) {
	if host != GHCRHost || !ValidRepository(repository, 1) || !lowerCommitSHA.MatchString(tag) {
		return Reference{}, NewError(CodeRegistryResponseInvalid)
	}
	return Reference{Host: host, Repository: repository, Tag: tag}, nil
}

// ValidRepository accepts only already-canonical hostless repository paths. It
// never trims, case-folds, unescapes, or repairs caller input.
func ValidRepository(value string, minimumComponents int) bool {
	if minimumComponents < 1 || len(value) == 0 || len(value) > MaxRepositoryBytes {
		return false
	}
	components := strings.Split(value, "/")
	if len(components) < minimumComponents {
		return false
	}
	for _, component := range components {
		if !repositoryComponent.MatchString(component) {
			return false
		}
	}
	return true
}

func ParseDigest(value string) ([sha256.Size]byte, string, error) {
	var digest [sha256.Size]byte
	if !lowerSHA256Digest.MatchString(value) {
		return digest, "", NewError(CodeRegistryDigestInvalid)
	}
	decoded, err := hex.DecodeString(value[len("sha256:"):])
	if err != nil || len(decoded) != sha256.Size {
		return digest, "", NewError(CodeRegistryDigestInvalid)
	}
	copy(digest[:], decoded)
	return digest, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func VerifyBodyDigest(body []byte, headerDigest string) (string, error) {
	expected, _, err := ParseDigest(headerDigest)
	if err != nil {
		return "", err
	}
	actual := sha256.Sum256(body)
	if actual != expected {
		return "", NewError(CodeRegistryDigestInvalid)
	}
	return "sha256:" + hex.EncodeToString(actual[:]), nil
}

func ImmutableReference(repository, digest string) (string, error) {
	if !ValidRepository(repository, 1) {
		return "", NewError(CodeRegistryResponseInvalid)
	}
	_, canonical, err := ParseDigest(digest)
	if err != nil {
		return "", err
	}
	return GHCRHost + "/" + repository + "@" + canonical, nil
}

type Credential struct {
	Username   []byte
	PAT        []byte
	OrgID      string
	RegistryID string
	Generation int64
}

func (c *Credential) Cleanup() {
	if c == nil {
		return
	}
	clear(c.Username)
	clear(c.PAT)
	c.Username = nil
	c.PAT = nil
}
