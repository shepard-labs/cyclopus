package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cyclopusv1 "github.com/cyclopus-dev/cyclopus/gen/go/cyclopus/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
