package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const validGitWebhookSecretRef = "k8s-secret://cyclopus/github#webhook"

func validGitConnectionInput() GitConnectionInput {
	return GitConnectionInput{Provider: "github", InstallationID: 4242, RepoFullName: "acme/widgets", DefaultBranch: "main", WebhookSecretRef: validGitWebhookSecretRef}
}

func TestNormalizeGitConnectionInputDefaults(t *testing.T) {
	got := normalizeGitConnectionInput(GitConnectionInput{InstallationID: 1, RepoFullName: "acme/widgets", WebhookSecretRef: validGitWebhookSecretRef})
	if got.Provider != "github" || got.DefaultBranch != "main" {
		t.Fatalf("normalizeGitConnectionInput defaults = %+v, want provider=github default_branch=main", got)
	}
}

func TestValidateGitConnectionAccepts(t *testing.T) {
	if err := validateGitConnection(validGitConnectionInput()); err != nil {
		t.Fatalf("validateGitConnection(valid) = %v, want nil", err)
	}
}

func TestValidateGitConnectionRejectsBadInput(t *testing.T) {
	cases := map[string]GitConnectionInput{
		"non-github provider": {Provider: "gitlab", InstallationID: 1, RepoFullName: "acme/widgets", DefaultBranch: "main", WebhookSecretRef: validGitWebhookSecretRef},
		"zero installation":   {Provider: "github", InstallationID: 0, RepoFullName: "acme/widgets", DefaultBranch: "main", WebhookSecretRef: validGitWebhookSecretRef},
		"negative install":    {Provider: "github", InstallationID: -1, RepoFullName: "acme/widgets", DefaultBranch: "main", WebhookSecretRef: validGitWebhookSecretRef},
		"no slash repo":       {Provider: "github", InstallationID: 1, RepoFullName: "acmewidgets", DefaultBranch: "main", WebhookSecretRef: validGitWebhookSecretRef},
		"space in repo":       {Provider: "github", InstallationID: 1, RepoFullName: "acme/wid gets", DefaultBranch: "main", WebhookSecretRef: validGitWebhookSecretRef},
		"three-part repo":     {Provider: "github", InstallationID: 1, RepoFullName: "acme/team/widgets", DefaultBranch: "main", WebhookSecretRef: validGitWebhookSecretRef},
		"empty branch":        {Provider: "github", InstallationID: 1, RepoFullName: "acme/widgets", DefaultBranch: "   ", WebhookSecretRef: validGitWebhookSecretRef},
		"too long branch":     {Provider: "github", InstallationID: 1, RepoFullName: "acme/widgets", DefaultBranch: strings.Repeat("b", 256), WebhookSecretRef: validGitWebhookSecretRef},
		"raw secret":          {Provider: "github", InstallationID: 1, RepoFullName: "acme/widgets", DefaultBranch: "main", WebhookSecretRef: "supersecret"},
		"non-secretref value": {Provider: "github", InstallationID: 1, RepoFullName: "acme/widgets", DefaultBranch: "main", WebhookSecretRef: "https://example.com/hook"},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateGitConnection(input); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("validateGitConnection(%s) = %v, want ErrInvalidArgument", name, err)
			}
		})
	}
}

func TestGitConnectionActorValidation(t *testing.T) {
	svc := New(nil)
	if _, err := svc.CreateGitConnection(context.Background(), ActorContext{}, "key", validGitConnectionInput()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("CreateGitConnection without actor error = %v, want ErrForbidden", err)
	}
	if _, err := svc.GetGitConnection(context.Background(), ActorContext{}, "id"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("GetGitConnection without actor error = %v, want ErrForbidden", err)
	}
	if _, err := svc.ListGitConnections(context.Background(), ActorContext{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ListGitConnections without actor error = %v, want ErrForbidden", err)
	}
	if err := svc.DeleteGitConnection(context.Background(), ActorContext{}, "key", "id"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("DeleteGitConnection without actor error = %v, want ErrForbidden", err)
	}
}

// TestCreateGitConnectionRejectsRawSecretBeforeStore proves the secret reference
// is validated before any store call (store is nil, so a miss would panic).
func TestCreateGitConnectionRejectsRawSecretBeforeStore(t *testing.T) {
	svc := New(nil)
	actor := ActorContext{UserID: "user", OrgID: "org"}
	bad := validGitConnectionInput()
	bad.WebhookSecretRef = "raw-secret-not-a-reference"
	if _, err := svc.CreateGitConnection(context.Background(), actor, "key", bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("CreateGitConnection(raw secret) error = %v, want ErrInvalidArgument", err)
	}
}
