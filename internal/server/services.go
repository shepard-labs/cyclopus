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
	Observability    cyclopusv1.ObservabilityServiceServer
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
		Observability:  unimplementedObservabilityService{},
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
type unimplementedObservabilityService struct {
	cyclopusv1.UnimplementedObservabilityServiceServer
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
	if services.Observability == nil {
		services.Observability = defaults.Observability
	}
	return services
}
