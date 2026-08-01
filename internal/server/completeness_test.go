package server

import (
	"reflect"
	"testing"

	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
)

func TestAPIHandlersImplementEveryDeclaredRPC(t *testing.T) {
	handlerType := reflect.TypeOf((*apiHandlers)(nil))
	services := []reflect.Type{
		reflect.TypeOf((*cyclopusv1.AuthenticationServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.OrganizationServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.ProjectServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.ApplicationServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.EnvironmentServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.ReleaseServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.ReconciliationServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.WebhookServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.DomainServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.RegistryServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.GitConnectionServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.ObservabilityServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.AliasServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.AuditEventServiceServer)(nil)).Elem(),
		reflect.TypeOf((*cyclopusv1.CertificateServiceServer)(nil)).Elem(),
	}
	for _, serviceType := range services {
		for index := 0; index < serviceType.NumMethod(); index++ {
			method := serviceType.Method(index)
			if len(method.Name) >= len("mustEmbedUnimplemented") && method.Name[:len("mustEmbedUnimplemented")] == "mustEmbedUnimplemented" {
				continue
			}
			if _, exists := handlerType.MethodByName(method.Name); !exists {
				t.Errorf("%s.%s has no API handler", serviceType.Name(), method.Name)
			}
		}
	}
}

func TestDeferredRPCsAreNotDeclared(t *testing.T) {
	deferred := map[string]bool{"UpdateOrganization": true, "CreateEnvironment": true, "TearDownEnvironment": true}
	services := cyclopusv1.File_cyclopus_v1_cyclopus_proto.Services()
	for serviceIndex := 0; serviceIndex < services.Len(); serviceIndex++ {
		service := services.Get(serviceIndex)
		for methodIndex := 0; methodIndex < service.Methods().Len(); methodIndex++ {
			name := string(service.Methods().Get(methodIndex).Name())
			if deferred[name] {
				t.Errorf("unsafe deferred RPC remains declared: %s.%s", service.FullName(), name)
			}
		}
	}
}
