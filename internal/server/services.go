package server

import cyclopusv1 "github.com/cyclopus-dev/cyclopus/gen/go/cyclopus/v1"

type Services struct {
	Authentication   cyclopusv1.AuthenticationServiceServer
	Organization     cyclopusv1.OrganizationServiceServer
	Project          cyclopusv1.ProjectServiceServer
	Application      cyclopusv1.ApplicationServiceServer
	Environment      cyclopusv1.EnvironmentServiceServer
	Release          cyclopusv1.ReleaseServiceServer
	Reconciliation   cyclopusv1.ReconciliationServiceServer
	Webhook          cyclopusv1.WebhookServiceServer
	Domain           cyclopusv1.DomainServiceServer
	Registry         cyclopusv1.RegistryServiceServer
	GitConnection    cyclopusv1.GitConnectionServiceServer
	Observability    cyclopusv1.ObservabilityServiceServer
	Alias            cyclopusv1.AliasServiceServer
	AuditEvent       cyclopusv1.AuditEventServiceServer
	Certificate      cyclopusv1.CertificateServiceServer
	Authenticator    *Authenticator
	RateLimiter      *authRateLimiter
	RequestBodyBytes int64
}

func DefaultServices() Services {
	return Services{
		Authentication: unimplementedAuthenticationService{},
		Organization:   unimplementedOrganizationService{},
		Project:        unimplementedProjectService{},
		Application:    unimplementedApplicationService{},
		Environment:    unimplementedEnvironmentService{},
		Release:        unimplementedReleaseService{},
		Reconciliation: unimplementedReconciliationService{},
		Webhook:        unimplementedWebhookService{},
		Domain:         unimplementedDomainService{},
		Registry:       unimplementedRegistryService{},
		GitConnection:  unimplementedGitConnectionService{},
		Observability:  unimplementedObservabilityService{},
		Alias:          unimplementedAliasService{},
		AuditEvent:     unimplementedAuditEventService{},
		Certificate:    unimplementedCertificateService{},
	}
}

type unimplementedAuthenticationService struct {
	cyclopusv1.UnimplementedAuthenticationServiceServer
}
type unimplementedOrganizationService struct {
	cyclopusv1.UnimplementedOrganizationServiceServer
}
type unimplementedProjectService struct {
	cyclopusv1.UnimplementedProjectServiceServer
}
type unimplementedApplicationService struct {
	cyclopusv1.UnimplementedApplicationServiceServer
}
type unimplementedEnvironmentService struct {
	cyclopusv1.UnimplementedEnvironmentServiceServer
}
type unimplementedReleaseService struct {
	cyclopusv1.UnimplementedReleaseServiceServer
}
type unimplementedReconciliationService struct {
	cyclopusv1.UnimplementedReconciliationServiceServer
}
type unimplementedWebhookService struct {
	cyclopusv1.UnimplementedWebhookServiceServer
}
type unimplementedDomainService struct {
	cyclopusv1.UnimplementedDomainServiceServer
}
type unimplementedRegistryService struct {
	cyclopusv1.UnimplementedRegistryServiceServer
}
type unimplementedGitConnectionService struct {
	cyclopusv1.UnimplementedGitConnectionServiceServer
}
type unimplementedObservabilityService struct {
	cyclopusv1.UnimplementedObservabilityServiceServer
}
type unimplementedAliasService struct {
	cyclopusv1.UnimplementedAliasServiceServer
}
type unimplementedAuditEventService struct {
	cyclopusv1.UnimplementedAuditEventServiceServer
}
type unimplementedCertificateService struct {
	cyclopusv1.UnimplementedCertificateServiceServer
}

func withDefaults(services Services) Services {
	defaults := DefaultServices()
	if services.Authentication == nil {
		services.Authentication = defaults.Authentication
	}
	if services.Organization == nil {
		services.Organization = defaults.Organization
	}
	if services.Project == nil {
		services.Project = defaults.Project
	}
	if services.Application == nil {
		services.Application = defaults.Application
	}
	if services.Environment == nil {
		services.Environment = defaults.Environment
	}
	if services.Release == nil {
		services.Release = defaults.Release
	}
	if services.Reconciliation == nil {
		services.Reconciliation = defaults.Reconciliation
	}
	if services.Webhook == nil {
		services.Webhook = defaults.Webhook
	}
	if services.Domain == nil {
		services.Domain = defaults.Domain
	}
	if services.Registry == nil {
		services.Registry = defaults.Registry
	}
	if services.GitConnection == nil {
		services.GitConnection = defaults.GitConnection
	}
	if services.Observability == nil {
		services.Observability = defaults.Observability
	}
	if services.Alias == nil {
		services.Alias = defaults.Alias
	}
	if services.AuditEvent == nil {
		services.AuditEvent = defaults.AuditEvent
	}
	if services.Certificate == nil {
		services.Certificate = defaults.Certificate
	}
	return services
}
