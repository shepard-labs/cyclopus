package service

import "context"

// CustomDomainRequest is the fully-resolved, service-authorized description of a
// custom domain's routing objects. Every field is derived by the service from
// records it has already loaded org-scoped (the domain, its app, and the app's
// production environment) — no untrusted caller input reaches the adapter as a
// namespace or hostname.
type CustomDomainRequest struct {
	// Namespace is the production environment's Cyclopus-managed namespace where
	// the custom-domain Ingress is created. The adapter mutates only this
	// namespace.
	Namespace string
	// EnvironmentID scopes the ownership label (cyclopus.dev/environment-id) so
	// the Ingress is recognizably Cyclopus-managed and attributable.
	EnvironmentID string
	// DomainID is the domains-table row ID; it makes the Ingress name
	// deterministic and unique per domain.
	DomainID string
	// Hostname is the customer's custom domain (the served host).
	Hostname string
	// ServiceName + ServicePort identify the environment's application Service the
	// custom hostname routes to (the same Service the environment Ingress
	// targets).
	ServiceName string
	ServicePort int32
	// TLSSecretName is the DNS-safe Secret name cert-manager populates for the
	// custom hostname's certificate.
	TLSSecretName string
	// ClusterIssuer, when non-empty, is annotated onto the Ingress
	// (cert-manager.io/cluster-issuer) so cert-manager issues the certificate.
	ClusterIssuer string
}

// CustomDomainProvisioner is the seam the service uses to create/tear down the
// live routing objects (an Ingress + TLS + cert-manager issuer annotation) for a
// verified custom domain (§7.5). Like CertReader/TelemetryProvider it is
// injected because the Kubernetes client only becomes available once cluster
// wiring is built; when nil, verification/deletion still succeed and routing
// provisioning is skipped. Implementations MUST be idempotent (get-or-create on
// provision, not-found-is-success on deprovision) and MUST bound all mutations
// to CustomDomainRequest.Namespace.
type CustomDomainProvisioner interface {
	ProvisionCustomDomain(ctx context.Context, req CustomDomainRequest) error
	DeprovisionCustomDomain(ctx context.Context, req CustomDomainRequest) error
}

// SetCustomDomainProvisioner installs the cluster provisioner after
// construction. A nil provisioner is ignored, leaving custom-domain routing
// unprovisioned (verification still sets verified_at).
func (s *Service) SetCustomDomainProvisioner(provisioner CustomDomainProvisioner) {
	if provisioner != nil {
		s.customDomainProvisioner = provisioner
	}
}

// WithCustomDomainProvisioner wires the provisioner at construction time.
func WithCustomDomainProvisioner(provisioner CustomDomainProvisioner) Option {
	return func(s *Service) {
		if provisioner != nil {
			s.customDomainProvisioner = provisioner
		}
	}
}
