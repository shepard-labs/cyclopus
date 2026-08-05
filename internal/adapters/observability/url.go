package observability

import (
	"errors"
	"net/url"
	"strings"
)

// ErrInsecureBackendURL reports a backend base URL that is neither an absolute
// https:// URL nor a plaintext in-cluster Service address. The message is
// phrased to read correctly when a caller prefixes it with the offending field
// or backend name.
var ErrInsecureBackendURL = errors.New("must be an absolute https:// URL (http:// is allowed only for in-cluster .svc addresses)")

// ValidateBackendBaseURL accepts the base URL of an observability backend
// (OpenObserve, Beszel).
//
// https:// is always accepted. http:// is accepted ONLY for a Kubernetes
// in-cluster Service address, because that traffic never leaves the cluster
// network: OpenObserve and Beszel are deployed by the installer as plain-HTTP
// Services in the `cyclopus` namespace (infra/observability/*.yaml) and are
// reached pod-to-Service, which the namespace NetworkPolicy — not TLS — is what
// actually constrains. Requiring https:// there would force the platform to
// publish both backends through a public ingress purely to obtain a
// certificate, which is exactly what §8 of the spec forbids ("never exposed
// directly").
//
// Anything reached by a name that could resolve on the public internet still
// requires TLS.
func ValidateBackendBaseURL(rawURL string) error {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return ErrInsecureBackendURL
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if inClusterHost(parsed.Hostname()) {
			return nil
		}
	}
	return ErrInsecureBackendURL
}

// inClusterHost reports whether host is a Kubernetes in-cluster Service DNS
// name, identified by a ".svc" label ("<service>.<namespace>.svc" and the
// fully-qualified "<service>.<namespace>.svc.<cluster-domain>" form).
//
// Keying on the ".svc" label rather than on a namespace or suffix allowlist is
// deliberate: ".svc" is not a delegable public TLD, so no name that resolves on
// the public internet can match, and a custom cluster domain still does.
func inClusterHost(host string) bool {
	normalized := strings.ToLower(strings.TrimSuffix(host, "."))
	return strings.HasSuffix(normalized, ".svc") || strings.Contains(normalized, ".svc.")
}
