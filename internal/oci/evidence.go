package oci

import (
	"net/url"
	"regexp"
	"strings"
)

const (
	AnnotationSource   = "org.opencontainers.image.source"
	AnnotationRevision = "org.opencontainers.image.revision"

	AnnotationPolicyValidateIfPresent AnnotationPolicy = "validate_if_present"
	AnnotationStatusMatched           AnnotationStatus = "matched"
	AnnotationStatusAbsent            AnnotationStatus = "absent"
)

var (
	githubOwnerGrammar      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	githubRepositoryGrammar = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

type RepositoryIdentity struct {
	Owner      string
	Repository string
}

type AnnotationPolicy string
type AnnotationStatus string

type EvidenceValues struct {
	ManifestAnnotations map[string]string
	ConfigLabels        map[string]string
}

type SourceEvidence struct {
	Source   string
	Revision string
	Status   AnnotationStatus
}

func VerifySourceEvidence(expected RepositoryIdentity, expectedRevision string, policy AnnotationPolicy, values EvidenceValues) (SourceEvidence, error) {
	if policy != AnnotationPolicyValidateIfPresent || !validIdentity(expected) || !lowerCommitSHA.MatchString(expectedRevision) {
		return SourceEvidence{}, NewError(CodeRegistryResponseInvalid)
	}
	sources := collectEvidence(values, AnnotationSource)
	revisions := collectEvidence(values, AnnotationRevision)
	canonicalSource := ""
	for _, value := range sources {
		normalized, err := normalizeGitHubSource(value)
		if err != nil {
			return SourceEvidence{}, err
		}
		if canonicalSource != "" && canonicalSource != normalized {
			return SourceEvidence{}, NewError(CodeOCIAnnotationConflict)
		}
		canonicalSource = normalized
	}
	expectedSource := "https://github.com/" + strings.ToLower(expected.Owner) + "/" + strings.ToLower(expected.Repository)
	if canonicalSource != "" && canonicalSource != expectedSource {
		return SourceEvidence{}, NewError(CodeOCISourceMismatch)
	}
	canonicalRevision := ""
	for _, value := range revisions {
		if !lowerCommitSHA.MatchString(value) {
			return SourceEvidence{}, NewError(CodeOCIAnnotationConflict)
		}
		if canonicalRevision != "" && canonicalRevision != value {
			return SourceEvidence{}, NewError(CodeOCIAnnotationConflict)
		}
		canonicalRevision = value
	}
	if canonicalRevision != "" && canonicalRevision != expectedRevision {
		return SourceEvidence{}, NewError(CodeOCIRevisionMismatch)
	}
	if canonicalSource == "" || canonicalRevision == "" {
		return SourceEvidence{Source: canonicalSource, Revision: canonicalRevision, Status: AnnotationStatusAbsent}, nil
	}
	return SourceEvidence{Source: canonicalSource, Revision: canonicalRevision, Status: AnnotationStatusMatched}, nil
}

func NormalizeGitHubSource(value string, expected RepositoryIdentity) (string, error) {
	if !validIdentity(expected) {
		return "", NewError(CodeOCIAnnotationConflict)
	}
	normalized, err := normalizeGitHubSource(value)
	if err != nil {
		return "", err
	}
	expectedSource := "https://github.com/" + strings.ToLower(expected.Owner) + "/" + strings.ToLower(expected.Repository)
	if normalized != expectedSource {
		return "", NewError(CodeOCISourceMismatch)
	}
	return normalized, nil
}

func normalizeGitHubSource(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n\t") {
		return "", NewError(CodeOCIAnnotationConflict)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", NewError(CodeOCIAnnotationConflict)
	}
	path, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil || strings.ContainsAny(path, "\\\x00\r\n\t") {
		return "", NewError(CodeOCIAnnotationConflict)
	}
	path = strings.TrimSuffix(path, "/")
	components := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(components) != 2 || components[0] == "" || components[1] == "" {
		return "", NewError(CodeOCIAnnotationConflict)
	}
	repository := strings.TrimSuffix(components[1], ".git")
	if repository == "" || !validIdentity(RepositoryIdentity{Owner: components[0], Repository: repository}) {
		return "", NewError(CodeOCIAnnotationConflict)
	}
	return "https://github.com/" + strings.ToLower(components[0]) + "/" + strings.ToLower(repository), nil
}

func collectEvidence(values EvidenceValues, key string) []string {
	result := make([]string, 0, 2)
	if value, ok := values.ManifestAnnotations[key]; ok {
		result = append(result, value)
	}
	if value, ok := values.ConfigLabels[key]; ok {
		result = append(result, value)
	}
	return result
}

func validIdentity(identity RepositoryIdentity) bool {
	return len(identity.Owner) <= 39 && len(identity.Repository) <= 100 && githubOwnerGrammar.MatchString(identity.Owner) && githubRepositoryGrammar.MatchString(identity.Repository)
}
