package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const validRegistrySecretRef = "k8s-secret://cyclopus/registry-creds#dockerconfigjson"

func validRegistryInput() RegistryInput {
	return RegistryInput{Provider: "ghcr", RegistryURL: "ghcr.io/acme", CredentialSecretRef: validRegistrySecretRef}
}

func TestValidateRegistryAcceptsKnownProviders(t *testing.T) {
	for _, provider := range []string{"ghcr", "gcr", "ecr", "other"} {
		input := validRegistryInput()
		input.Provider = provider
		if err := validateRegistry(input); err != nil {
			t.Fatalf("validateRegistry(provider=%q) = %v, want nil", provider, err)
		}
	}
}

func TestValidateRegistryRejectsBadInput(t *testing.T) {
	cases := map[string]RegistryInput{
		"unknown provider":       {Provider: "dockerhub", RegistryURL: "ghcr.io/acme", CredentialSecretRef: validRegistrySecretRef},
		"empty provider":         {Provider: "", RegistryURL: "ghcr.io/acme", CredentialSecretRef: validRegistrySecretRef},
		"empty url":              {Provider: "ghcr", RegistryURL: "  ", CredentialSecretRef: validRegistrySecretRef},
		"too long url":           {Provider: "ghcr", RegistryURL: strings.Repeat("a", 2049), CredentialSecretRef: validRegistrySecretRef},
		"raw credential":         {Provider: "ghcr", RegistryURL: "ghcr.io/acme", CredentialSecretRef: "hunter2"},
		"non-secretref value":    {Provider: "ghcr", RegistryURL: "ghcr.io/acme", CredentialSecretRef: "https://example.com/token"},
		"empty secret reference": {Provider: "ghcr", RegistryURL: "ghcr.io/acme", CredentialSecretRef: ""},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateRegistry(input); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("validateRegistry(%s) = %v, want ErrInvalidArgument", name, err)
			}
		})
	}
}

func TestRegistryActorValidation(t *testing.T) {
	svc := New(nil)
	if _, err := svc.CreateRegistry(context.Background(), ActorContext{}, "key", validRegistryInput()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("CreateRegistry without actor error = %v, want ErrForbidden", err)
	}
	if _, err := svc.GetRegistry(context.Background(), ActorContext{}, "id"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("GetRegistry without actor error = %v, want ErrForbidden", err)
	}
	if _, err := svc.ListRegistries(context.Background(), ActorContext{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ListRegistries without actor error = %v, want ErrForbidden", err)
	}
	if err := svc.DeleteRegistry(context.Background(), ActorContext{}, "key", "id"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("DeleteRegistry without actor error = %v, want ErrForbidden", err)
	}
}

// TestCreateRegistryRejectsBadInputBeforeStore proves validation runs before any
// store call (the store is nil, so a validation miss would panic).
func TestCreateRegistryRejectsBadInputBeforeStore(t *testing.T) {
	svc := New(nil)
	actor := ActorContext{UserID: "user", OrgID: "org"}
	bad := validRegistryInput()
	bad.CredentialSecretRef = "raw-credential-not-a-reference"
	if _, err := svc.CreateRegistry(context.Background(), actor, "key", bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("CreateRegistry(raw credential) error = %v, want ErrInvalidArgument", err)
	}
	badProvider := validRegistryInput()
	badProvider.Provider = "bogus"
	if _, err := svc.CreateRegistry(context.Background(), actor, "key", badProvider); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("CreateRegistry(bad provider) error = %v, want ErrInvalidArgument", err)
	}
}
