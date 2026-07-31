package service

import (
	"context"
	"errors"
	"time"
)

// ErrCertReaderUnavailable is returned when no cluster cert reader is wired, so
// the Certificate RPCs report the capability as unavailable rather than panic.
var ErrCertReaderUnavailable = errors.New("service: certificate reading unavailable")

// CertScope names the single Cyclopus-managed namespace a certificate read is
// bounded to. The service resolves it from an environment it has already
// authorized against the actor's org, so a CertReader never receives an
// unauthorized or arbitrary namespace.
type CertScope struct {
	// Namespace is the environment's Cyclopus-managed namespace. The adapter
	// reads only Certificates in this namespace.
	Namespace string
	// EnvironmentID is carried for logging/attribution; the adapter must not
	// widen its read beyond Namespace based on it.
	EnvironmentID string
}

// CertificateStatus is a projection of a cert-manager Certificate's live status
// for one hostname. It is a value type owned by the service package so the
// adapter (which parses the CRD) and the server (which maps it to proto) share a
// stable shape without depending on a cert-manager Go module.
type CertificateStatus struct {
	Hostname   string
	SecretName string
	Ready      bool
	Reason     string
	NotAfter   *time.Time
	Issuer     string
	DNSNames   []string
}

// CertReader is the seam the service uses to read live cert-manager Certificate
// status from the cluster. It is injected (like TelemetryProvider) because the
// dynamic Kubernetes client only becomes available once cluster wiring is built.
// Implementations MUST bound their read to CertScope.Namespace; the service
// only ever passes a namespace it has authorized for the actor's org.
type CertReader interface {
	ListCertificates(ctx context.Context, scope CertScope) ([]CertificateStatus, error)
}

// SetCertReader installs the cluster cert reader after construction. A nil
// reader is ignored, leaving Certificate RPCs to report unavailability.
func (s *Service) SetCertReader(reader CertReader) {
	if reader != nil {
		s.certReader = reader
	}
}

// WithCertReader wires the cluster cert reader at construction time.
func WithCertReader(reader CertReader) Option {
	return func(s *Service) {
		if reader != nil {
			s.certReader = reader
		}
	}
}

// ListCertificates returns the live cert-manager Certificate statuses for an
// environment's hostname(s). Org-scoping is enforced BEFORE any cluster read:
// the environment is loaded org-scoped (a cross-org or unknown environment_id
// yields ErrNotFound), and only then is its namespace passed to the reader. A
// caller can therefore only ever read certificates for their own environments'
// managed namespaces.
func (s *Service) ListCertificates(ctx context.Context, actor ActorContext, environmentID string) ([]CertificateStatus, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	environment, err := s.store.GetEnvironment(ctx, actor.OrgID, environmentID)
	if err != nil {
		return nil, mapError(err)
	}
	if s.certReader == nil {
		return nil, ErrCertReaderUnavailable
	}
	return s.certReader.ListCertificates(ctx, CertScope{Namespace: environment.Namespace, EnvironmentID: environment.ID})
}
