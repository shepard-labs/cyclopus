//go:build integration

package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/store"
)

func TestCreateAppRegistryIdentityIntegration(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	const (
		orgID      = "00000000-0000-0000-0000-0000000000b1"
		userID     = "00000000-0000-0000-0000-0000000000b2"
		projectID  = "00000000-0000-0000-0000-0000000000b3"
		registryID = "00000000-0000-0000-0000-0000000000b4"
	)
	for _, statement := range []string{
		`INSERT INTO orgs(id,name,slug) VALUES('` + orgID + `','Registry Org','registry-org')`,
		`INSERT INTO users(id,org_id,email,github_user_id,operator) VALUES('` + userID + `','` + orgID + `','registry@example.test',515151,true)`,
		`INSERT INTO projects(id,org_id,name,slug) VALUES('` + projectID + `','` + orgID + `','Manual','manual')`,
		`INSERT INTO registries(id,org_id,provider,registry_host,state,active_generation,active_credential_secret_ref) VALUES('` + registryID + `','` + orgID + `','gcr','gcr.io','ready',1,'k8s-secret://cyclopus-system/gcr#token')`,
	} {
		if _, err := pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	service := New(dataStore)
	actor := ActorContext{UserID: userID, OrgID: orgID, SessionID: "session", Operator: true}
	repository := "acme/api"
	input := DefaultAppInput()
	input.ProjectID, input.Name, input.Slug, input.HostnameLabel = projectID, "Manual API", "manual-api", "manual-api"
	registry := registryID
	input.RegistryID, input.ImageRepository = &registry, &repository
	input = applyAppCreateDefaults(normalizeAppInput(input))
	if err := service.validateRegistryApp(t.Context(), orgID, input); err != nil {
		t.Fatalf("validateRegistryApp(canonical GCR): %v", err)
	}
	if err := validateApp(input, "gvisor"); err != nil {
		t.Fatalf("validateApp(canonical GCR): %v", err)
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE registries DROP CONSTRAINT registries_canonical_identity_check`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE registries SET registry_host='ghcr.io' WHERE id=$1`, registryID); err != nil {
		t.Fatal(err)
	}
	input.Slug, input.HostnameLabel = "invalid-registry", "invalid-registry"
	if _, err := service.CreateApp(t.Context(), actor, "invalid-registry", input); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("provider/host mismatch error = %v", err)
	}
	if !store.ValidRegistryIdentity(store.RegistryProviderGCR, store.RegistryHostGCR) {
		t.Fatal("GCR canonical identity unexpectedly unsupported")
	}
}

func TestUpdateAppUsesPersistedGitAndRegistryStateIntegration(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	const (
		orgID             = "00000000-0000-0000-0000-0000000000d1"
		otherOrgID        = "00000000-0000-0000-0000-0000000000d2"
		userID            = "00000000-0000-0000-0000-0000000000d3"
		gitConnectionID   = "00000000-0000-0000-0000-0000000000d4"
		importedProjectID = "00000000-0000-0000-0000-0000000000d5"
		importedAppID     = "00000000-0000-0000-0000-0000000000d6"
		manualProjectID   = "00000000-0000-0000-0000-0000000000d7"
		manualAppID       = "00000000-0000-0000-0000-0000000000d8"
		registryID        = "00000000-0000-0000-0000-0000000000d9"
	)
	for _, statement := range []string{
		`INSERT INTO orgs(id,name,slug) VALUES('` + orgID + `','Update Org','update-org')`,
		`INSERT INTO orgs(id,name,slug) VALUES('` + otherOrgID + `','Other Org','other-update-org')`,
		`INSERT INTO users(id,org_id,email,github_user_id,operator) VALUES('` + userID + `','` + orgID + `','update@example.test',616161,true)`,
		`INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation,repository_reconciliation_generation) VALUES(616163,'` + orgID + `',616164,'acme','Organization','selected','active',1,1,1)`,
		`INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(616162,'` + orgID + `',616163,'R_kgDOUpdateFixture','acme','api','acme/api','public','main','active',1,1)`,
		`INSERT INTO git_connections(id,org_id,github_repository_id) VALUES('` + gitConnectionID + `','` + orgID + `',616162)`,
		`INSERT INTO projects(id,org_id,name,slug,git_connection_id) VALUES('` + importedProjectID + `','` + orgID + `','Imported','imported','` + gitConnectionID + `')`,
		`INSERT INTO projects(id,org_id,name,slug) VALUES('` + manualProjectID + `','` + orgID + `','Manual','manual')`,
		`INSERT INTO registries(id,org_id,provider,registry_host,state,active_generation,active_credential_secret_ref) VALUES('` + registryID + `','` + orgID + `','ghcr','ghcr.io','ready',1,'k8s-secret://cyclopus-system/ghcr#token')`,
		`INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,image_repository,container_port,min_replicas,max_replicas,previews_enabled,github_workflow_path,github_workflow_revision,production_branch,image_visibility)
		VALUES('` + importedAppID + `','` + orgID + `','` + importedProjectID + `','Imported API','imported-api','imported-api','acme/api',8080,1,3,true,'.github/workflows/deploy.yml','` + strings.Repeat("a", 40) + `','main','public')`,
		`INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,registry_id,image_repository,container_port,min_replicas,max_replicas,image_visibility)
		 VALUES('` + manualAppID + `','` + orgID + `','` + manualProjectID + `','Manual API','manual-api','manual-api','` + registryID + `','acme/api',8080,1,3,'private')`,
	} {
		if _, err := pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}

	svc := New(dataStore)
	actor := ActorContext{UserID: userID, OrgID: orgID, SessionID: "session", Operator: true}
	imported, err := dataStore.GetApp(t.Context(), orgID, importedAppID)
	if err != nil {
		t.Fatal(err)
	}
	input := appInputFromStoredApp(imported)
	spoofedRegistry, spoofedImage, spoofedWorkflow := registryID, "attacker/replaced", ".github/workflows/attacker.yml"
	input.ProjectID, input.Name, input.ContainerPort, input.MaxReplicas = manualProjectID, "Imported API Renamed", 9090, 6
	input.RegistryID, input.ImageRepository, input.GitHubWorkflowPath, input.ProductionBranch, input.PreviewsEnabled = &spoofedRegistry, &spoofedImage, &spoofedWorkflow, "attacker", false
	updated, err := svc.UpdateApp(t.Context(), actor, "imported-update", importedAppID, input)
	if err != nil {
		t.Fatalf("UpdateApp(imported): %v", err)
	}
	if updated.Name != "Imported API Renamed" || updated.ContainerPort != 9090 || updated.MaxReplicas != 6 || updated.ProjectID != importedProjectID || updated.RegistryID != nil || updated.ImageRepository == nil || *updated.ImageRepository != "acme/api" || updated.GitHubWorkflowPath != ".github/workflows/deploy.yml" || updated.GitHubWorkflowRevision != strings.Repeat("a", 40) || updated.ProductionBranch != "main" || !updated.PreviewsEnabled {
		t.Fatalf("updated imported app = %+v", updated)
	}

	manual, err := dataStore.GetApp(t.Context(), orgID, manualAppID)
	if err != nil {
		t.Fatal(err)
	}
	manualInput := appInputFromStoredApp(manual)
	repository := "acme/replaced"
	manualInput.ImageRepository = &repository
	if updated, err := svc.UpdateApp(t.Context(), actor, "manual-valid", manualAppID, manualInput); err != nil || updated.ImageRepository == nil || *updated.ImageRepository != repository || updated.RegistryID == nil || *updated.RegistryID != registryID {
		t.Fatalf("UpdateApp(valid registry) = %+v, %v", updated, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE registries SET state='rotating',checkpoint='secret_written',pending_generation=2,pending_credential_secret_ref='k8s-secret://cyclopus-system/ghcr-next#token',operation_id='00000000-0000-0000-0000-0000000000da',reconciliation_generation=1 WHERE id=$1`, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateApp(t.Context(), actor, "manual-invalid-registry", manualAppID, manualInput); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("UpdateApp(invalid registry state) error = %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE registries SET state='ready',checkpoint='activated',active_generation=2,active_credential_secret_ref=pending_credential_secret_ref,pending_generation=NULL,pending_credential_secret_ref=NULL,operation_id=NULL,reconciliation_generation=1 WHERE id=$1`, registryID); err != nil {
		t.Fatal(err)
	}
	invalidRepository := "ghcr.io/acme/replaced"
	manualInput.ImageRepository = &invalidRepository
	if _, err := svc.UpdateApp(t.Context(), actor, "manual-invalid", manualAppID, manualInput); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("UpdateApp(invalid registry repository) error = %v", err)
	}
	otherActor := actor
	otherActor.OrgID = otherOrgID
	if _, err := svc.UpdateApp(t.Context(), otherActor, "cross-tenant", importedAppID, input); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateApp(cross tenant) error = %v", err)
	}
}

func appInputFromStoredApp(app store.App) AppInput {
	runtimeClass := app.RuntimeClass
	workflowPath := app.GitHubWorkflowPath
	return AppInput{ProjectID: app.ProjectID, Name: app.Name, Slug: app.Slug, HostnameLabel: app.HostnameLabel, RegistryID: app.RegistryID, ImageRepository: app.ImageRepository, GitHubWorkflowPath: &workflowPath, ProductionBranch: app.ProductionBranch, ContainerPort: app.ContainerPort, HealthCheckPath: app.HealthCheckPath, ScalingMode: app.ScalingMode, MinReplicas: app.MinReplicas, MaxReplicas: app.MaxReplicas, HPATargetCPUPercent: app.HPATargetCPUPercent, KEDACronSchedules: app.KEDACronSchedules, KEDAHTTPTargetPendingRequests: app.KEDAHTTPTargetPendingRequests, CPURequestMillicores: app.CPURequestMillicores, CPULimitMillicores: app.CPULimitMillicores, MemoryRequestMiB: app.MemoryRequestMiB, MemoryLimitMiB: app.MemoryLimitMiB, PreviewsEnabled: app.PreviewsEnabled, MaxPreviewEnvironments: app.MaxPreviewEnvironments, PreviewIdleTTLHours: app.PreviewIdleTTLHours, DeployTimeoutSeconds: app.DeployTimeoutSeconds, ReleaseRetentionCount: app.ReleaseRetentionCount, RuntimeClass: &runtimeClass}
}

func TestRegistryRepositoryGrammarMatchesDatabaseIntegration(t *testing.T) {
	_, pool := newServiceIntegrationStore(t)
	const (
		orgID          = "00000000-0000-0000-0000-0000000000c1"
		projectID      = "00000000-0000-0000-0000-0000000000c2"
		ghcrRegistryID = "00000000-0000-0000-0000-0000000000c3"
		gcrRegistryID  = "00000000-0000-0000-0000-0000000000c4"
	)
	for _, statement := range []string{
		`INSERT INTO orgs(id,name,slug) VALUES('` + orgID + `','Grammar Org','grammar-org')`,
		`INSERT INTO projects(id,org_id,name,slug) VALUES('` + projectID + `','` + orgID + `','Grammar','grammar')`,
		`INSERT INTO registries(id,org_id,provider,registry_host,state,active_generation,active_credential_secret_ref) VALUES('` + ghcrRegistryID + `','` + orgID + `','ghcr','ghcr.io','ready',1,'k8s-secret://cyclopus-system/ghcr#token')`,
		`INSERT INTO registries(id,org_id,provider,registry_host,state,active_generation,active_credential_secret_ref) VALUES('` + gcrRegistryID + `','` + orgID + `','gcr','gcr.io','ready',1,'k8s-secret://cyclopus-system/gcr#token')`,
	} {
		if _, err := pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	for index, test := range repositoryGrammarCases {
		goPublicAccepted := func() bool { _, err := ParsePublicGHCRRepository(test.publicRepository); return err == nil }()
		publicRepository := test.publicRepository
		if parsed, err := ParsePublicGHCRRepository(test.publicRepository); err == nil {
			publicRepository = parsed
		}
		_, publicInsertErr := pool.Exec(t.Context(), `INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch,image_visibility)
			VALUES(uuidv7(),$1,$2,$3,$4,$4,$5,'.github/workflows/deploy.yml','main','public')`, orgID, projectID, fmt.Sprintf("Public Grammar %d", index), fmt.Sprintf("public-grammar-%d", index), publicRepository)
		_, publicUpdateErr := pool.Exec(t.Context(), `UPDATE apps SET image_repository=$1 WHERE slug='public-grammar-0'`, publicRepository)
		if goPublicAccepted != test.publicAccepted || publicInsertErr == nil != test.publicAccepted || publicUpdateErr == nil != test.publicAccepted {
			t.Fatalf("public repository %q accepted go=%t insert=%t update=%t, want %t (insert err=%v update err=%v)", test.publicRepository, goPublicAccepted, publicInsertErr == nil, publicUpdateErr == nil, test.publicAccepted, publicInsertErr, publicUpdateErr)
		}
		for _, registry := range []struct{ name, id, host string }{
			{"ghcr", ghcrRegistryID, store.RegistryHostGHCR},
			{"gcr", gcrRegistryID, store.RegistryHostGCR},
		} {
			repository := strings.Replace(test.registryPath, store.RegistryHostGHCR+"/", registry.host+"/", 1)
			goRegistryAccepted := validRegistryRepository(registry.host, repository)
			_, registryInsertErr := pool.Exec(t.Context(), `INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,registry_id,image_repository,github_workflow_path,production_branch,image_visibility)
				VALUES(uuidv7(),$1,$2,$3,$4,$4,$5,$6,NULL,NULL,'private')`, orgID, projectID, fmt.Sprintf("Registry Grammar %s %d", registry.name, index), fmt.Sprintf("registry-grammar-%s-%d", registry.name, index), registry.id, repository)
			_, registryUpdateErr := pool.Exec(t.Context(), `UPDATE apps SET image_repository=$1 WHERE slug=$2`, repository, "registry-grammar-"+registry.name+"-0")
			if goRegistryAccepted != test.registryAccepted || registryInsertErr == nil != test.registryAccepted || registryUpdateErr == nil != test.registryAccepted {
				t.Fatalf("%s repository %q accepted go=%t insert=%t update=%t, want %t (insert err=%v update err=%v)", registry.name, repository, goRegistryAccepted, registryInsertErr == nil, registryUpdateErr == nil, test.registryAccepted, registryInsertErr, registryUpdateErr)
			}
		}
	}
}
