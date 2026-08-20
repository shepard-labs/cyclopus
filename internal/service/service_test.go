package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/store"
)

type rejectingWebhookSecretValidator struct{}

func (rejectingWebhookSecretValidator) Validate(context.Context, string) error {
	return errors.New("not allowed")
}

func TestWebhookMutationsRequireOperatorAndAllowedSecret(t *testing.T) {
	input := WebhookInput{TargetURL: "https://hooks.example.test", Events: []string{"deploy.succeeded"}, SecretRef: "k8s-secret://cyclopus/cyclopus-webhook-primary#secret", Enabled: true}
	svc := New(nil, WithWebhookSecretValidator(rejectingWebhookSecretValidator{}))
	nonAdmin := ActorContext{UserID: "user", OrgID: "org"}
	if _, err := svc.CreateWebhook(context.Background(), nonAdmin, "key", input); !errors.Is(err, ErrForbidden) {
		t.Fatalf("CreateWebhook() error = %v, want ErrForbidden", err)
	}
	if _, err := svc.UpdateWebhook(context.Background(), nonAdmin, "key", "id", input); !errors.Is(err, ErrForbidden) {
		t.Fatalf("UpdateWebhook() error = %v, want ErrForbidden", err)
	}
	admin := ActorContext{UserID: "admin", OrgID: "org", Operator: true}
	if _, err := svc.CreateWebhook(context.Background(), admin, "key", input); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("CreateWebhook() disallowed Secret error = %v, want ErrInvalidArgument", err)
	}
	if _, err := svc.UpdateWebhook(context.Background(), admin, "key", "id", input); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("UpdateWebhook() disallowed Secret error = %v, want ErrInvalidArgument", err)
	}
}

func TestValidateImageRef(t *testing.T) {
	registry := "https://ghcr.io"
	repository := "acme/api"
	digest := strings.Repeat("a", 64)
	if err := ValidateImageRef("ghcr.io/acme/api@sha256:"+digest, &registry, &repository); err != nil {
		t.Fatalf("valid image rejected: %v", err)
	}
	for _, image := range []string{
		"ghcr.io/acme/api:latest",
		"ghcr.io/acme/other@sha256:" + digest,
		"docker.io/acme/api@sha256:" + digest,
		"ghcr.io/acme/api@sha256:short",
	} {
		if err := ValidateImageRef(image, &registry, &repository); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("ValidateImageRef(%q) error = %v", image, err)
		}
	}
}

func TestCreateAppRejectsImportOwnedFieldsBeforeStoreAccess(t *testing.T) {
	workflow, image := ".github/workflows/deploy.yml", "ghcr.io/acme/api"
	input := DefaultAppInput()
	input.ProjectID, input.Name, input.Slug, input.HostnameLabel = "project", "API", "api", "api"
	input.GitHubWorkflowPath, input.ImageRepository, input.ProductionBranch = &workflow, &image, "main"
	if _, err := New(nil).CreateApp(context.Background(), ActorContext{UserID: "user", OrgID: "org"}, "key", input); !errors.Is(err, ErrForbidden) {
		t.Fatalf("CreateApp() error = %v, want ErrForbidden", err)
	}
}

func TestCreateAppRuntimeClassRequiresOperator(t *testing.T) {
	input := DefaultAppInput()
	input.ProjectID = "project"
	input.Name = "API"
	input.Slug = "api"
	input.HostnameLabel = "api"
	runtimeClass := "runc"
	input.RuntimeClass = &runtimeClass
	service := New(nil)
	_, err := service.CreateApp(t.Context(), ActorContext{UserID: "user", OrgID: "org"}, "key", input)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("ordinary runtime-class change error = %v", err)
	}
}

func TestAppResourceAndScalingValidation(t *testing.T) {
	input := DefaultAppInput()
	input.ProjectID = "project"
	input.Name = "API"
	input.Slug = "api"
	input.HostnameLabel = "api"
	input.CPULimitMillicores = input.CPURequestMillicores - 1
	if err := validateApp(input, "gvisor"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid CPU limits error = %v", err)
	}
	input = DefaultAppInput()
	input.ProjectID = "project"
	input.Name = "API"
	input.Slug = "api"
	input.HostnameLabel = "api"
	input.ScalingMode = "hpa"
	input.MinReplicas = 0
	if err := validateApp(input, "gvisor"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("HPA scale-to-zero error = %v", err)
	}
}

func TestScalingKEDAHTTPDefaultIsBackwardSafe(t *testing.T) {
	input := DefaultAppInput()
	input.KEDAHTTPTargetPendingRequests = 0
	input = normalizeAppInput(input)
	if input.KEDAHTTPTargetPendingRequests != 100 {
		t.Fatalf("normalized HTTP target = %d, want 100", input.KEDAHTTPTargetPendingRequests)
	}
	input.KEDAHTTPTargetPendingRequests = -1
	input = normalizeAppInput(input)
	if err := validateApp(input, "gvisor"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative HTTP target error = %v", err)
	}
}

func TestValidateEnvironmentVariablesRequiresExactSameNamespaceSecretRefs(t *testing.T) {
	valid := []EnvVarInput{{Key: "DATABASE_URL", SecretRef: "k8s-secret://app-production/runtime#DATABASE_URL"}}
	variables, err := validateEnvironmentVariables("app-production", valid)
	if err != nil || len(variables) != 1 || variables[0].SecretRef != valid[0].SecretRef {
		t.Fatalf("valid variables = %+v, %v", variables, err)
	}
	for _, input := range []EnvVarInput{
		{Key: "DATABASE_URL", SecretRef: "runtime"},
		{Key: "DATABASE_URL", SecretRef: "k8s-secret://other/runtime#DATABASE_URL"},
		{Key: "bad-key", SecretRef: "k8s-secret://app-production/runtime#DATABASE_URL"},
	} {
		if _, err := validateEnvironmentVariables("app-production", []EnvVarInput{input}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid input %+v error = %v", input, err)
		}
	}
}

// TestDeploymentSnapshotStampsTenantIdentity asserts the release snapshot carries
// the full tenant identity on the pod so the Collector can stamp the canonical
// org_id/app_id/environment_id/release_id at ingest (§8.1). Without app-id and
// org-id here, the collected telemetry would be missing the two mandatory fields
// the API query layer filters on and would be unqueryable.
func TestDeploymentSnapshotStampsTenantIdentity(t *testing.T) {
	hostname := "api.example.test"
	environment := store.Environment{ID: "env-123", OrgID: "org-abc", AppID: "app-xyz", Kind: "production", Namespace: "ns-1", PlannedHostname: &hostname}
	app := store.App{
		ID: "app-xyz", OrgID: "org-abc", Slug: "api", ScalingMode: "hpa",
		ImageVisibility: "public",
		MinReplicas:     1, MaxReplicas: 3, HPATargetCPUPercent: 70,
		ContainerPort: 8080, HealthCheckPath: "/healthz", RuntimeClass: "runc", DeployTimeoutSeconds: 120,
		CPURequestMillicores: 100, CPULimitMillicores: 200, MemoryRequestMiB: 128, MemoryLimitMiB: 256,
	}
	encoded, err := deploymentSnapshot(environment, app, "ghcr.io/acme/api@sha256:"+strings.Repeat("a", 64), "rel-789")
	if err != nil {
		t.Fatalf("deploymentSnapshot: %v", err)
	}
	envelope, err := store.DecodeReleaseSnapshot(encoded)
	if err != nil {
		t.Fatalf("decode release snapshot: %v", err)
	}
	deployment := envelope.DesiredState.Deployment
	if got := deployment.PodAnnotations["cyclopus.dev/app-id"]; got != app.ID {
		t.Fatalf("app-id annotation = %q, want %q", got, app.ID)
	}
	if got := deployment.PodAnnotations["cyclopus.dev/org-id"]; got != app.OrgID {
		t.Fatalf("org-id annotation = %q, want %q", got, app.OrgID)
	}
	if got := deployment.PodAnnotations["cyclopus.dev/release-id"]; got != "rel-789" {
		t.Fatalf("release-id annotation = %q, want %q", got, "rel-789")
	}
	if got := deployment.PodLabels["cyclopus.dev/environment-id"]; got != environment.ID {
		t.Fatalf("environment-id label = %q, want %q", got, environment.ID)
	}
}

func TestDeploymentSnapshotSetsVisibilitySpecificImagePullSecrets(t *testing.T) {
	hostname := "api.example.test"
	environment := store.Environment{ID: "env-123", OrgID: "org-abc", AppID: "app-xyz", Kind: "production", Namespace: "ns-1", PlannedHostname: &hostname}
	for _, test := range []struct {
		visibility string
		want       []string
	}{
		{visibility: "public", want: []string{}},
		{visibility: "private", want: []string{"cyclopus-ghcr-pull"}},
	} {
		t.Run(test.visibility, func(t *testing.T) {
			app := store.App{
				ID: "app-xyz", OrgID: "org-abc", Slug: "api", ScalingMode: "hpa", ImageVisibility: test.visibility,
				MinReplicas: 1, MaxReplicas: 3, HPATargetCPUPercent: 70,
				ContainerPort: 8080, HealthCheckPath: "/healthz", RuntimeClass: "runc", DeployTimeoutSeconds: 120,
				CPURequestMillicores: 100, CPULimitMillicores: 200, MemoryRequestMiB: 128, MemoryLimitMiB: 256,
			}
			encoded, err := deploymentSnapshot(environment, app, "ghcr.io/acme/api@sha256:"+strings.Repeat("a", 64), "rel-789")
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := store.DecodeReleaseSnapshot(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(envelope.DesiredState.Deployment.ImagePullSecrets, test.want) {
				t.Fatalf("image pull secrets = %v, want %v", envelope.DesiredState.Deployment.ImagePullSecrets, test.want)
			}
		})
	}
}
