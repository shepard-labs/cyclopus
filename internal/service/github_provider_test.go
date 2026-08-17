package service

import (
	"context"
	"errors"
	"testing"
)

func TestListGitHubRepositoryWorkflowsRequiresGitHubDiscoveryConfiguration(t *testing.T) {
	service := New(nil)
	actor := ActorContext{UserID: "user", OrgID: "org", Operator: true}

	if _, err := service.ListGitHubRepositoryWorkflows(context.Background(), actor, 42, 50, ""); !errors.Is(err, ErrGitHubAppNotConfigured) {
		t.Fatalf("ListGitHubRepositoryWorkflows() error = %v, want ErrGitHubAppNotConfigured", err)
	}
}

func TestParsePublicGHCRRepositoryCanonicalGrammar(t *testing.T) {
	for _, test := range repositoryGrammarCases {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParsePublicGHCRRepository(test.publicRepository)
			if test.publicAccepted {
				if err != nil || got != test.publicRepository {
					t.Fatalf("ParsePublicGHCRRepository(%q) = %q, %v", test.publicRepository, got, err)
				}
			} else if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("ParsePublicGHCRRepository(%q) error = %v", test.publicRepository, err)
			}
			if got := validRegistryRepository("ghcr.io", test.registryPath); got != test.registryAccepted {
				t.Fatalf("validRegistryRepository(%q) = %t, want %t", test.registryPath, got, test.registryAccepted)
			}
		})
	}
}
