package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/store"
)

type imageResolutionResolverFake struct {
	request oci.ResolveRequest
	image   oci.ResolvedImage
	err     error
	calls   int
}

func (f *imageResolutionResolverFake) Resolve(_ context.Context, request oci.ResolveRequest) (oci.ResolvedImage, error) {
	f.calls++
	f.request = request
	return f.image, f.err
}
func (*imageResolutionResolverFake) ValidatePull(context.Context, oci.PullValidationRequest) error {
	return nil
}

type imageResolutionCredentialFake struct {
	credential *oci.Credential
	err        error
}

func (f imageResolutionCredentialFake) ReadCredential(context.Context, string, string, int64, string) (*oci.Credential, error) {
	return f.credential, f.err
}

func TestImageResolutionPublicComplete(t *testing.T) {
	image := oci.ResolvedImage{Digest: "sha256:" + repeatImageResolution("a", 64), ImmutableReference: "ghcr.io/acme/api@sha256:" + repeatImageResolution("a", 64), ManifestMediaType: oci.MediaTypeOCIImageManifest, ConfigDigest: "sha256:" + repeatImageResolution("b", 64), ConfigMediaType: oci.MediaTypeOCIConfig, AnnotationStatus: oci.AnnotationStatusAbsent}
	resolver := &imageResolutionResolverFake{image: image}
	processor, err := NewImageResolver(resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim := imageResolutionClaim("public")
	outcome := processor.Resolve(t.Context(), claim)
	if outcome.Disposition != ImageResolutionComplete || resolver.calls != 1 || resolver.request.Repository != "acme/api" || resolver.request.Credential != nil {
		t.Fatalf("outcome=%+v request=%+v", outcome, resolver.request)
	}
}

func TestImageResolutionCompleteParamsCanonicalReferences(t *testing.T) {
	claim := imageResolutionClaim("private")
	image := oci.ResolvedImage{Digest: "sha256:" + repeatImageResolution("a", 64), ImmutableReference: "acme/api@sha256:" + repeatImageResolution("a", 64)}
	params := ImageResolutionCompleteParams(claim, image)
	if params.Repository != "acme/api" || params.TagReference != "ghcr.io/acme/api:"+claim.Job.CommitSHA || params.ImmutableReference != "ghcr.io/acme/api@"+image.Digest {
		t.Fatalf("completion references = %q/%q/%q", params.Repository, params.TagReference, params.ImmutableReference)
	}
}

func TestImageResolutionPrivateCredentialMetadataAndCleanup(t *testing.T) {
	credential := &oci.Credential{Username: []byte("user"), PAT: []byte("secret"), OrgID: "org", RegistryID: "registry", Generation: 7}
	resolver := &imageResolutionResolverFake{image: oci.ResolvedImage{Digest: "sha256:" + repeatImageResolution("a", 64)}}
	processor, err := NewImageResolver(resolver, imageResolutionCredentialFake{credential: credential})
	if err != nil {
		t.Fatal(err)
	}
	claim := imageResolutionClaim("private")
	claim.Registry = &store.Registry{ID: "registry", ActiveGeneration: 7}
	secretRef := "k8s-secret://cyclopus/source#dockerconfigjson"
	claim.RegistryCredentialSecretRef = &secretRef
	outcome := processor.Resolve(t.Context(), claim)
	if outcome.Disposition != ImageResolutionComplete || resolver.request.Credential != credential || credential.Username != nil || credential.PAT != nil {
		t.Fatalf("outcome=%+v credential=%+v", outcome, credential)
	}
}

func TestImageResolutionCredentialReadDisposition(t *testing.T) {
	claim := imageResolutionClaim("private")
	claim.Registry = &store.Registry{ID: "registry", ActiveGeneration: 7}
	secretRef := "k8s-secret://cyclopus/source#dockerconfigjson"
	claim.RegistryCredentialSecretRef = &secretRef
	for name, test := range map[string]struct {
		err         error
		disposition ImageResolutionDisposition
		code        string
	}{
		"missing":   {ErrRegistryCredentialMissing, ImageResolutionFail, string(oci.CodeRegistryAuthRequired)},
		"invalid":   {ErrRegistryCredentialInvalid, ImageResolutionFail, string(oci.CodeRegistryAuthFailed)},
		"transient": {errors.New("temporary secret API failure"), ImageResolutionRetry, string(oci.CodeRegistryUnavailable)},
		"timeout":   {context.DeadlineExceeded, ImageResolutionRetry, string(oci.CodeRegistryUnavailable)},
	} {
		t.Run(name, func(t *testing.T) {
			processor, err := NewImageResolver(&imageResolutionResolverFake{}, imageResolutionCredentialFake{err: test.err})
			if err != nil {
				t.Fatal(err)
			}
			outcome := processor.Resolve(t.Context(), claim)
			if outcome.Disposition != test.disposition || outcome.Code != test.code {
				t.Fatalf("outcome = %+v", outcome)
			}
		})
	}
}

func TestImageResolutionRetryNotFoundAndDeadline(t *testing.T) {
	resolver := &imageResolutionResolverFake{err: &oci.HTTPStatusError{StatusCode: 404, Kind: oci.ResponseManifest}}
	processor, _ := NewImageResolver(resolver, nil)
	now := time.Unix(1000, 0)
	processor.now = func() time.Time { return now }
	claim := imageResolutionClaim("public")
	claim.Deadline = now.Add(time.Minute)
	if outcome := processor.Resolve(t.Context(), claim); outcome.Disposition != ImageResolutionRetry {
		t.Fatalf("before deadline=%+v", outcome)
	}
	claim.Deadline = now
	if outcome := processor.Resolve(t.Context(), claim); outcome.Disposition != ImageResolutionFail || outcome.Code != string(oci.CodeImageNotPublished) {
		t.Fatalf("at deadline=%+v", outcome)
	}
}

func TestImageResolutionAuthPermanentAndNetworkRetry(t *testing.T) {
	claim := imageResolutionClaim("public")
	claim.Deadline = time.Now().Add(time.Minute)
	for name, test := range map[string]struct {
		err         error
		disposition ImageResolutionDisposition
		code        string
	}{
		"auth":      {oci.NewError(oci.CodeRegistryAuthRequired), ImageResolutionFail, string(oci.CodeRegistryAuthRequired)},
		"permanent": {oci.NewError(oci.CodeRegistryConfigInvalid), ImageResolutionFail, string(oci.CodeRegistryConfigInvalid)},
		"network":   {errors.New("opaque failure"), ImageResolutionFail, string(oci.CodeRegistryResponseInvalid)},
		"rate":      {oci.NewRetryableError(oci.CodeRegistryRateLimited, oci.CodeRegistryUnavailable, time.Second), ImageResolutionRetry, string(oci.CodeRegistryRateLimited)},
	} {
		t.Run(name, func(t *testing.T) {
			processor, _ := NewImageResolver(&imageResolutionResolverFake{err: test.err}, nil)
			outcome := processor.Resolve(t.Context(), claim)
			if outcome.Disposition != test.disposition || outcome.Code != test.code {
				t.Fatalf("outcome=%+v", outcome)
			}
		})
	}
}

func imageResolutionClaim(visibility string) store.ImageResolutionClaim {
	repository := "ghcr.io/acme/api"
	if visibility == "private" {
		repository = "acme/api"
	}
	return store.ImageResolutionClaim{Job: store.OrchestrationJob{ID: "job", OrgID: "org", CommitSHA: repeatImageResolution("a", 40), DesiredGeneration: 3},
		App:         store.App{ImageRepository: &repository, ImageVisibility: visibility, AnnotationPolicy: string(oci.AnnotationPolicyValidateIfPresent)},
		Environment: store.Environment{ID: "environment"}, EnvironmentDesiredGeneration: 3,
		RepositoryOwner: "acme", RepositoryName: "api", Deadline: time.Now().Add(time.Minute)}
}

func repeatImageResolution(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}
