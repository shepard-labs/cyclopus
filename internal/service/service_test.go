package service

import (
	"errors"
	"strings"
	"testing"
)

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
