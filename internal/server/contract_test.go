package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
	"github.com/shepard-labs/cyclopus/internal/store"
	annotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestGitProviderReplacementContract(t *testing.T) {
	services := cyclopusv1.File_cyclopus_v1_cyclopus_proto.Services()
	gitProvider := services.ByName("GitProviderService")
	if gitProvider == nil {
		t.Fatal("GitProviderService is absent")
	}
	wantMethods := []string{"GetGitHubAppStatus", "BeginGitHubAppSetup", "BeginGitHubInstallation", "GetGitHubInstallation", "ListGitHubInstallations", "RefreshGitHubInstallation", "ListGitHubRepositories", "ListGitHubRepositoryWorkflows", "GetGitConnection", "ListGitConnections", "DisconnectGitConnection", "ImportGitRepository", "GetImportOperation", "GetGhcrCredentialStatus", "RotateGhcrCredential"}
	for _, name := range wantMethods {
		if gitProvider.Methods().ByName(protoreflect.Name(name)) == nil {
			t.Errorf("GitProviderService.%s is absent", name)
		}
	}
	for _, removed := range []string{"RegistryService", "GitConnectionService"} {
		if services.ByName(protoreflect.Name(removed)) != nil {
			t.Errorf("%s still exists", removed)
		}
	}
	applicationFields := (&cyclopusv1.Application{}).ProtoReflect().Descriptor().Fields()
	if applicationFields.ByName("ci_workflow_id") != nil {
		t.Fatal("Application.ci_workflow_id still exists")
	}
	for _, name := range []protoreflect.Name{"github_workflow_path", "production_branch", "image_repository", "previews_enabled", "annotation_policy", "annotation_status"} {
		if applicationFields.ByName(name) == nil {
			t.Errorf("Application.%s is absent", name)
		}
	}
	if applicationFields.ByNumber(25) != nil || applicationFields.ByName("github_workflow_path").Number() == 25 {
		t.Fatal("Application field 25 was reused instead of reserved")
	}
	connectionFields := (&cyclopusv1.GitConnection{}).ProtoReflect().Descriptor().Fields()
	for number := protoreflect.FieldNumber(2); number <= 8; number++ {
		if connectionFields.ByNumber(number) != nil {
			t.Errorf("GitConnection legacy field number %d was reused", number)
		}
	}
	listFields := (&cyclopusv1.ListGitConnectionsRequest{}).ProtoReflect().Descriptor().Fields()
	if listFields.ByNumber(1) != nil {
		t.Fatal("ListGitConnectionsRequest organization_id field number was reused")
	}
	importFields := (&cyclopusv1.ImportGitRepositoryRequest{}).ProtoReflect().Descriptor().Fields()
	for _, removed := range []protoreflect.Name{"organization_id", "installation_id", "repo_full_name", "default_branch", "secret_ref", "workflow_id", "ci_workflow_id"} {
		if importFields.ByName(removed) != nil {
			t.Errorf("ImportGitRepositoryRequest.%s exists", removed)
		}
	}
}

func TestGitProviderGeneratedTransportContracts(t *testing.T) {
	want := map[string]struct{ verb, path string }{
		"GetGitHubAppStatus": {http.MethodGet, "/v1/github/app"}, "BeginGitHubAppSetup": {http.MethodPost, "/v1/github/app:beginSetup"},
		"BeginGitHubInstallation": {http.MethodPost, "/v1/github/installations:begin"}, "GetGitHubInstallation": {http.MethodGet, "/v1/github/installations/{installation_id}"},
		"ListGitHubInstallations": {http.MethodGet, "/v1/github/installations"}, "RefreshGitHubInstallation": {http.MethodPost, "/v1/github/installations/{installation_id}:refresh"},
		"ListGitHubRepositories": {http.MethodGet, "/v1/github/installations/{installation_id}/repositories"}, "ListGitHubRepositoryWorkflows": {http.MethodGet, "/v1/github/repositories/{repository_id}/workflows"},
		"GetGitConnection": {http.MethodGet, "/v1/git-connections/{git_connection_id}"}, "ListGitConnections": {http.MethodGet, "/v1/git-connections"},
		"DisconnectGitConnection": {http.MethodPost, "/v1/git-connections/{git_connection_id}:disconnect"}, "ImportGitRepository": {http.MethodPost, "/v1/github/repositories:import"},
		"GetImportOperation": {http.MethodGet, "/v1/import-operations/{import_operation_id}"}, "GetGhcrCredentialStatus": {http.MethodGet, "/v1/github/ghcr-credential"},
		"RotateGhcrCredential": {http.MethodPost, "/v1/github/ghcr-credential:rotate"},
	}
	service := cyclopusv1.File_cyclopus_v1_cyclopus_proto.Services().ByName("GitProviderService")
	if service.Methods().Len() != len(want) {
		t.Fatalf("GitProviderService methods = %d, want %d", service.Methods().Len(), len(want))
	}
	for name, contract := range want {
		method := service.Methods().ByName(protoreflect.Name(name))
		rule := proto.GetExtension(method.Options(), annotations.E_Http).(*annotations.HttpRule)
		verb, path := gitProviderHTTPRule(rule)
		if verb != contract.verb || path != contract.path {
			t.Errorf("%s HTTP rule = %s %s, want %s %s", name, verb, path, contract.verb, contract.path)
		}
	}
	for _, artifact := range []string{"../../gen/go/cyclopus/v1/cyclopus_grpc.pb.go", "../../gen/go/cyclopus/v1/cyclopus.pb.gw.go"} {
		body, err := os.ReadFile(artifact)
		if err != nil {
			t.Fatal(err)
		}
		for name := range want {
			if !strings.Contains(string(body), name) {
				t.Errorf("%s lacks %s", artifact, name)
			}
		}
	}
	for _, artifact := range []string{"../../gen/openapi/cyclopus/v1/cyclopus.swagger.json", "../apidocs/openapi.json"} {
		body, err := os.ReadFile(artifact)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(body, &document); err != nil {
			t.Fatal(err)
		}
		paths := document["paths"].(map[string]any)
		for name, contract := range want {
			operation := paths[openAPIPath(contract.path)].(map[string]any)[strings.ToLower(contract.verb)].(map[string]any)
			if operation["operationId"] != "GitProviderService_"+name {
				t.Errorf("%s %s operationId = %v", artifact, name, operation["operationId"])
			}
		}
	}
}

func TestReplacementMessagesReserveDeletedWireContracts(t *testing.T) {
	tests := []struct {
		message protoreflect.MessageDescriptor
		names   []protoreflect.Name
		numbers []protoreflect.FieldNumber
		fields  map[protoreflect.Name]protoreflect.FieldNumber
	}{
		{(&cyclopusv1.Application{}).ProtoReflect().Descriptor(), []protoreflect.Name{"ci_workflow_id"}, []protoreflect.FieldNumber{25}, map[protoreflect.Name]protoreflect.FieldNumber{"production_branch": 30, "annotation_policy": 31, "annotation_status": 32, "github_workflow_path": 33}},
		{(&cyclopusv1.GitConnection{}).ProtoReflect().Descriptor(), []protoreflect.Name{"organization_id", "provider", "installation_id", "repo_full_name", "default_branch", "webhook_secret_ref", "created_at"}, []protoreflect.FieldNumber{2, 3, 4, 5, 6, 7, 8}, map[protoreflect.Name]protoreflect.FieldNumber{"repository": 9, "status": 10, "connected_at": 11, "disconnected_at": 12}},
		{(&cyclopusv1.ListGitConnectionsRequest{}).ProtoReflect().Descriptor(), []protoreflect.Name{"organization_id"}, []protoreflect.FieldNumber{1}, map[protoreflect.Name]protoreflect.FieldNumber{"page_size": 2, "page_token": 3}},
	}
	for _, test := range tests {
		for _, name := range test.names {
			if !messageReservesName(test.message, name) {
				t.Errorf("%s does not reserve %s", test.message.Name(), name)
			}
		}
		for _, number := range test.numbers {
			if !messageReservesNumber(test.message, number) || test.message.Fields().ByNumber(number) != nil {
				t.Errorf("%s does not safely reserve %d", test.message.Name(), number)
			}
		}
		for name, number := range test.fields {
			field := test.message.Fields().ByName(name)
			if field == nil || field.Number() != number {
				t.Errorf("%s.%s = %v, want %d", test.message.Name(), name, field, number)
			}
		}
	}
}

func TestImportRequestContainsOnlyApprovedInputs(t *testing.T) {
	descriptor := (&cyclopusv1.ImportGitRepositoryRequest{}).ProtoReflect().Descriptor()
	want := []protoreflect.Name{"repository_id", "github_workflow_path", "project_name", "project_slug", "application_name", "application_slug", "hostname_label", "production_branch", "image_repository", "previews_enabled", "container_port", "health_check_path", "scaling_mode", "min_replicas", "max_replicas", "hpa_target_cpu_percent", "cpu_request_millicores", "cpu_limit_millicores", "memory_request_mib", "memory_limit_mib", "max_preview_environments", "preview_idle_ttl_hours", "deploy_timeout_seconds", "release_retention_count", "runtime_class", "keda_cron_schedules", "keda_http_target_pending_requests", "annotation_policy", "ghcr_credential", "idempotency_key"}
	if descriptor.Fields().Len() != len(want) {
		t.Fatalf("fields = %d, want %d", descriptor.Fields().Len(), len(want))
	}
	for index, name := range want {
		field := descriptor.Fields().ByName(name)
		if field == nil || int(field.Number()) != index+1 {
			t.Errorf("%s = %v, want number %d", name, field, index+1)
		}
	}
}

func TestGitProviderTypedResourcesExposeApprovedFields(t *testing.T) {
	resources := map[protoreflect.MessageDescriptor][]protoreflect.Name{
		(&cyclopusv1.GitHubAppStatus{}).ProtoReflect().Descriptor():    {"setup_state", "app_id", "slug", "owner_login", "html_url", "credential_readiness", "failure_code", "created_at", "updated_at"},
		(&cyclopusv1.GitHubInstallation{}).ProtoReflect().Descriptor(): {"github_installation_id", "account_id", "account_login", "repository_selection", "lifecycle_status", "synced_at", "settings_url"},
		(&cyclopusv1.GitHubRepository{}).ProtoReflect().Descriptor():   {"id", "installation_id", "node_id", "full_name", "private", "archived", "default_branch", "access_status", "synced_at"},
		(&cyclopusv1.GitHubWorkflow{}).ProtoReflect().Descriptor():     {"path", "name", "state", "compatibility", "incompatibility_reasons"},
		(&cyclopusv1.GitConnection{}).ProtoReflect().Descriptor():      {"id", "repository", "status", "connected_at", "disconnected_at"},
		(&cyclopusv1.ImportOperation{}).ProtoReflect().Descriptor():    {"id", "state", "github_repository_id", "git_connection_id", "project_id", "application_id", "environment_id", "orchestration_job_id", "failure_code"},
		(&cyclopusv1.GhcrCredential{}).ProtoReflect().Descriptor():     {"username", "personal_access_token"},
	}
	for resource, fields := range resources {
		for _, name := range fields {
			if resource.Fields().ByName(name) == nil {
				t.Errorf("%s.%s is absent", resource.Name(), name)
			}
		}
	}
}

func TestGeneratedAPIArtifactsExcludeManualGitAndRegistryContracts(t *testing.T) {
	for _, path := range []string{"../../api/cyclopus/v1/cyclopus.proto", "../../gen/go/cyclopus/v1/cyclopus.pb.go", "../../gen/go/cyclopus/v1/cyclopus.pb.gw.go", "../../gen/go/cyclopus/v1/cyclopus_grpc.pb.go", "../../gen/openapi/cyclopus/v1/cyclopus.swagger.json", "../apidocs/openapi.json"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, removed := range []string{"CreateGitConnection", "credential_secret_ref", "CreateRegistry"} {
			if strings.Contains(text, removed) {
				t.Errorf("%s contains removed contract %q", path, removed)
			}
		}
		for _, required := range []string{"GitProviderService", "ImportGitRepository", "GetGhcrCredentialStatus"} {
			if !strings.Contains(text, required) {
				t.Errorf("%s lacks %q", path, required)
			}
		}
	}
}

func TestProductionSourceExcludesRemovedManualAPIs(t *testing.T) {
	for _, root := range []string{"../server", "../service", "../ui", "../uihandler"} {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, removed := range []string{"CreateGitConnection", "CreateRegistry", "ci_workflow_id", "webhook_secret_ref"} {
				if strings.Contains(string(body), removed) {
					t.Errorf("%s contains removed production contract %q", path, removed)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntegrationFailureCodeGeneratedArtifactContract(t *testing.T) {
	const enumName = "INTEGRATION_FAILURE_CODE_GITHUB_RUN_TIMED_OUT"
	for _, path := range []string{"../../api/cyclopus/v1/cyclopus.proto", "../../gen/go/cyclopus/v1/cyclopus.pb.go", "../../gen/openapi/cyclopus/v1/cyclopus.swagger.json", "../apidocs/openapi.json"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), enumName) {
			t.Errorf("%s lacks %s", path, enumName)
		}
	}
	descriptor := cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_RUN_TIMED_OUT.Descriptor().Values().ByName(enumName)
	if descriptor == nil || descriptor.Number() != 21 {
		t.Fatalf("generated enum value = %v, want %s (21)", descriptor, enumName)
	}
}

func TestGeneratedDocsMarkPATSensitive(t *testing.T) {
	for _, path := range []string{"../../gen/openapi/cyclopus/v1/cyclopus.swagger.json", "../apidocs/openapi.json"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if !strings.Contains(text, `"personalAccessToken"`) || !strings.Contains(text, `"format": "password"`) {
			t.Errorf("%s does not mark personalAccessToken as password", path)
		}
		if path == "../apidocs/openapi.json" && !strings.Contains(text, `"writeOnly": true`) {
			t.Errorf("%s does not mark personalAccessToken writeOnly", path)
		}
		if path != "../apidocs/openapi.json" && !strings.Contains(text, `"x-writeOnly": true`) {
			t.Errorf("%s does not mark personalAccessToken x-writeOnly", path)
		}
		for _, forbidden := range []string{`"personalAccessToken": "`, `"example": "ghp_`, `"default": "ghp_`} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s exposes a PAT example or value", path)
			}
		}
	}
}

func TestPATIsInputOnlyAcrossDescriptorAndGeneratedDocs(t *testing.T) {
	credential := (&cyclopusv1.GhcrCredential{}).ProtoReflect().Descriptor()
	parents := map[protoreflect.FullName]bool{}
	messages := cyclopusv1.File_cyclopus_v1_cyclopus_proto.Messages()
	for index := 0; index < messages.Len(); index++ {
		message := messages.Get(index)
		for fieldIndex := 0; fieldIndex < message.Fields().Len(); fieldIndex++ {
			if message.Fields().Get(fieldIndex).Message() == credential {
				parents[message.FullName()] = true
			}
		}
	}
	for _, name := range []protoreflect.FullName{"cyclopus.v1.ImportGitRepositoryRequest", "cyclopus.v1.RotateGhcrCredentialRequest"} {
		if !parents[name] {
			t.Errorf("%s does not accept GhcrCredential", name)
		}
		delete(parents, name)
	}
	if len(parents) != 0 {
		t.Fatalf("GhcrCredential appears in non-input messages: %v", parents)
	}
	field := credential.Fields().ByName("personal_access_token")
	behaviors := proto.GetExtension(field.Options(), annotations.E_FieldBehavior).([]annotations.FieldBehavior)
	if len(behaviors) != 1 || behaviors[0] != annotations.FieldBehavior_INPUT_ONLY {
		t.Fatalf("PAT behavior = %v", behaviors)
	}
	for _, artifact := range []string{"../../gen/openapi/cyclopus/v1/cyclopus.swagger.json", "../apidocs/openapi.json"} {
		body, err := os.ReadFile(artifact)
		if err != nil {
			t.Fatal(err)
		}
		definition := credentialSchemaJSON(t, artifact, body)
		for _, forbidden := range []string{`"example"`, `"default"`, "ghp_", "github_pat_"} {
			if strings.Contains(definition, forbidden) {
				t.Errorf("%s credential schema contains %q: %s", artifact, forbidden, definition)
			}
		}
	}
}

func TestGitConnectionRepositoryMapping(t *testing.T) {
	syncedAt := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	value := gitConnectionProto(store.GitConnection{
		ID: "connection", GitHubRepositoryID: 42, GitHubInstallationID: 7, RepositoryNodeID: "R_node",
		OwnerLogin: "acme", RepositoryName: "api", RepositoryFullName: "acme/api", RepositoryPrivate: true,
		RepositoryArchived: true, RepositoryDefaultBranch: "main", RepositoryAccessState: "active",
		RepositorySyncedAt: &syncedAt, State: "active", AccessState: "active", CreatedAt: syncedAt,
	})
	if value.GetRepository().GetNodeId() != "R_node" || !value.GetRepository().GetPrivate() || !value.GetRepository().GetArchived() {
		t.Fatalf("repository mapping = %+v", value.GetRepository())
	}
	if value.GetRepository().GetSyncedAt().AsTime() != syncedAt || value.GetConnectedAt().AsTime() != syncedAt {
		t.Fatalf("timestamp mapping = repository %v connection %v", value.GetRepository().GetSyncedAt(), value.GetConnectedAt())
	}
}

func gitProviderHTTPRule(rule *annotations.HttpRule) (string, string) {
	switch pattern := rule.Pattern.(type) {
	case *annotations.HttpRule_Get:
		return http.MethodGet, pattern.Get
	case *annotations.HttpRule_Post:
		return http.MethodPost, pattern.Post
	default:
		return "", ""
	}
}

func messageReservesName(message protoreflect.MessageDescriptor, want protoreflect.Name) bool {
	for index := 0; index < message.ReservedNames().Len(); index++ {
		if message.ReservedNames().Get(index) == want {
			return true
		}
	}
	return false
}

func messageReservesNumber(message protoreflect.MessageDescriptor, want protoreflect.FieldNumber) bool {
	for index := 0; index < message.ReservedRanges().Len(); index++ {
		fieldRange := message.ReservedRanges().Get(index)
		if want >= fieldRange[0] && want < fieldRange[1] {
			return true
		}
	}
	return false
}

func credentialSchemaJSON(t *testing.T, path string, body []byte) string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	var schema any
	if strings.Contains(path, "swagger") {
		schema = document["definitions"].(map[string]any)["v1GhcrCredential"]
	} else {
		schema = document["components"].(map[string]any)["schemas"].(map[string]any)["v1GhcrCredential"]
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func openAPIPath(path string) string {
	replacer := strings.NewReplacer("{installation_id}", "{installationId}", "{repository_id}", "{repositoryId}", "{git_connection_id}", "{gitConnectionId}", "{import_operation_id}", "{importOperationId}")
	return replacer.Replace(path)
}

func TestRESTDeployMapping(t *testing.T) {
	releases := &contractReleaseService{}
	handler, err := Handler(nil, Services{Release: releases})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/environments/68f33e0d-dcd7-4ed1-b701-b2472ec94242/deploys",
		strings.NewReader(`{"imageRef":"ghcr.io/acme/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","gitCommitSha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","idempotencyKey":"deploy-1"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if releases.deploy == nil || releases.deploy.EnvironmentId != "68f33e0d-dcd7-4ed1-b701-b2472ec94242" || releases.deploy.IdempotencyKey != "deploy-1" {
		t.Fatalf("mapped request = %+v", releases.deploy)
	}
	if !strings.Contains(response.Body.String(), `"resourceName":"releases/release-id"`) {
		t.Fatalf("response body = %s", response.Body.String())
	}
}

func TestRESTPaginationMapping(t *testing.T) {
	projects := &contractProjectService{}
	handler, err := Handler(nil, Services{Project: projects})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/organizations/org-id/projects?pageSize=25&pageToken=cursor&includeDeleted=true", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if projects.list == nil || projects.list.OrganizationId != "org-id" || projects.list.PageSize != 25 || projects.list.PageToken != "cursor" || !projects.list.IncludeDeleted {
		t.Fatalf("mapped request = %+v", projects.list)
	}
}

func TestRESTPreviewLifecycleMappings(t *testing.T) {
	environments := &contractEnvironmentService{}
	handler, err := Handler(nil, Services{Environment: environments})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path  string
		body  string
		check func()
	}{
		{path: "/v1/internal/preview-pushes", body: `{"applicationId":"app","branch":"feature/api","commitSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","idempotencyKey":"push-1"}`, check: func() {
			if environments.push == nil || environments.push.Branch != "feature/api" || environments.push.IdempotencyKey != "push-1" {
				t.Fatalf("push mapping = %+v", environments.push)
			}
		}},
		{path: "/v1/internal/preview-branch-deletes", body: `{"applicationId":"app","branch":"feature/api","idempotencyKey":"delete-1"}`, check: func() {
			if environments.deleted == nil || environments.deleted.Branch != "feature/api" {
				t.Fatalf("delete mapping = %+v", environments.deleted)
			}
		}},
		{path: "/v1/environments/environment:teardown", body: `{"reason":"operator_requested","idempotencyKey":"teardown-1"}`, check: func() {
			if environments.teardown == nil || environments.teardown.EnvironmentId != "environment" || environments.teardown.Reason != "operator_requested" {
				t.Fatalf("teardown mapping = %+v", environments.teardown)
			}
		}},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", test.path, response.Code, response.Body.String())
		}
		test.check()
	}
}

func TestBootstrapContractHasNoIdempotencyKey(t *testing.T) {
	fields := (&cyclopusv1.BootstrapRequest{}).ProtoReflect().Descriptor().Fields()
	if fields.ByName("idempotency_key") != nil || fields.ByNumber(3) != nil {
		t.Fatal("BootstrapRequest still exposes idempotency_key")
	}
}

func TestRESTReconciliationEvidencePaginationMapping(t *testing.T) {
	reconciliations := &contractReconciliationService{}
	handler, err := Handler(nil, Services{Reconciliation: reconciliations})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/reconciliations/run-id/evidence?pageSize=25&pageToken=cursor", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if reconciliations.evidence == nil || reconciliations.evidence.ReconciliationId != "run-id" || reconciliations.evidence.PageSize != 25 || reconciliations.evidence.PageToken != "cursor" {
		t.Fatalf("mapped request = %+v", reconciliations.evidence)
	}
}

func TestRESTTypedErrorJSON(t *testing.T) {
	projects := &contractProjectService{getError: typedValidationError(t)}
	handler, err := Handler(nil, Services{Project: projects})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/projects/not-a-uuid", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != float64(codes.InvalidArgument) || body["message"] != "request validation failed" {
		t.Fatalf("status body = %#v", body)
	}
	details, ok := body["details"].([]any)
	if !ok || len(details) != 2 {
		t.Fatalf("details = %#v", body["details"])
	}
	for _, want := range []string{"google.rpc.ErrorInfo", "INVALID_UUID", "google.rpc.BadRequest", "project_id"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("body %s does not contain %q", response.Body.String(), want)
		}
	}
}

func typedValidationError(t *testing.T) error {
	t.Helper()
	withDetails, err := status.New(codes.InvalidArgument, "request validation failed").WithDetails(
		&errdetails.ErrorInfo{Reason: "INVALID_UUID", Domain: "cyclopus.dev", Metadata: map[string]string{"resource": "project"}},
		&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: "project_id", Description: "must be a UUID"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return withDetails.Err()
}

type contractReleaseService struct {
	cyclopusv1.UnimplementedReleaseServiceServer
	deploy *cyclopusv1.DeployRequest
}

func (s *contractReleaseService) Deploy(_ context.Context, request *cyclopusv1.DeployRequest) (*cyclopusv1.DeployResponse, error) {
	s.deploy = request
	return &cyclopusv1.DeployResponse{Operation: &cyclopusv1.Operation{Id: "operation-id", Status: cyclopusv1.OperationStatus_OPERATION_STATUS_QUEUED, ResourceName: "releases/release-id"}}, nil
}

type contractProjectService struct {
	cyclopusv1.UnimplementedProjectServiceServer
	list     *cyclopusv1.ListProjectsRequest
	getError error
}

type contractEnvironmentService struct {
	cyclopusv1.UnimplementedEnvironmentServiceServer
	push     *cyclopusv1.PreviewPushRequest
	deleted  *cyclopusv1.PreviewBranchDeleteRequest
	teardown *cyclopusv1.TeardownEnvironmentRequest
}

func (s *contractEnvironmentService) AcceptPreviewPush(_ context.Context, request *cyclopusv1.PreviewPushRequest) (*cyclopusv1.PreviewPushResponse, error) {
	s.push = request
	return &cyclopusv1.PreviewPushResponse{Disposition: "queued"}, nil
}
func (s *contractEnvironmentService) AcceptPreviewBranchDelete(_ context.Context, request *cyclopusv1.PreviewBranchDeleteRequest) (*cyclopusv1.PreviewBranchDeleteResponse, error) {
	s.deleted = request
	return &cyclopusv1.PreviewBranchDeleteResponse{}, nil
}
func (s *contractEnvironmentService) TeardownEnvironment(_ context.Context, request *cyclopusv1.TeardownEnvironmentRequest) (*cyclopusv1.TeardownEnvironmentResponse, error) {
	s.teardown = request
	return &cyclopusv1.TeardownEnvironmentResponse{}, nil
}

type contractReconciliationService struct {
	cyclopusv1.UnimplementedReconciliationServiceServer
	evidence *cyclopusv1.ListReconciliationEvidenceRequest
}

func (s *contractReconciliationService) ListReconciliationEvidence(_ context.Context, request *cyclopusv1.ListReconciliationEvidenceRequest) (*cyclopusv1.ListReconciliationEvidenceResponse, error) {
	s.evidence = request
	return &cyclopusv1.ListReconciliationEvidenceResponse{NextPageToken: "next"}, nil
}

func (s *contractProjectService) ListProjects(_ context.Context, request *cyclopusv1.ListProjectsRequest) (*cyclopusv1.ListProjectsResponse, error) {
	s.list = request
	return &cyclopusv1.ListProjectsResponse{NextPageToken: "next"}, nil
}

func (s *contractProjectService) GetProject(context.Context, *cyclopusv1.GetProjectRequest) (*cyclopusv1.GetProjectResponse, error) {
	return nil, s.getError
}
