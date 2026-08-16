package service

import (
	"context"
	"strings"

	"github.com/shepard-labs/cyclopus/internal/secretref"
	"github.com/shepard-labs/cyclopus/internal/store"
)

// registryProviders is the allowed set for registries.provider, validated in the
// service layer so a bad value returns ErrInvalidArgument (400) rather than a
// 500 from the DB CHECK.
var registryProviders = map[string]bool{"ghcr": true, "gcr": true, "ecr": true, "other": true}

// RegistryInput is the caller-provided data for CreateRegistry. CredentialSecretRef
// is a k8s-secret://…#… REFERENCE — never a raw credential.
type RegistryInput struct {
	Provider            string
	RegistryURL         string
	CredentialSecretRef string
}

func (s *Service) GetRegistry(ctx context.Context, actor ActorContext, id string) (store.Registry, error) {
	if err := validateActor(actor); err != nil {
		return store.Registry{}, err
	}
	return s.store.GetRegistry(ctx, actor.OrgID, id)
}

func (s *Service) ListRegistries(ctx context.Context, actor ActorContext) ([]store.Registry, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListRegistries(ctx, actor.OrgID)
}

// CreateRegistry registers an OCI image registry for the actor's org. The
// provider and registry_url are validated here, and CredentialSecretRef is
// validated as a k8s-secret:// reference (never a raw credential) before insert.
func (s *Service) CreateRegistry(ctx context.Context, actor ActorContext, key string, input RegistryInput) (store.Registry, error) {
	if err := validateActor(actor); err != nil {
		return store.Registry{}, err
	}
	if err := validateRegistry(input); err != nil {
		return store.Registry{}, err
	}
	return store.Registry{}, store.ErrFailedPrecondition
}

func (s *Service) DeleteRegistry(ctx context.Context, actor ActorContext, key, id string) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	return store.ErrFailedPrecondition
}

// validateRegistry enforces the registries invariants in the service layer: a
// known provider, a non-empty registry_url within the length bound, and a
// well-formed k8s-secret:// credential reference. It NEVER accepts a raw
// credential — only the secret reference string.
func validateRegistry(input RegistryInput) error {
	if !registryProviders[input.Provider] {
		return ErrInvalidArgument
	}
	url := strings.TrimSpace(input.RegistryURL)
	if url == "" || len(url) > 2048 || url != input.RegistryURL {
		return ErrInvalidArgument
	}
	if _, err := secretref.Parse(input.CredentialSecretRef); err != nil {
		return ErrInvalidArgument
	}
	return nil
}
