package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/store"
)

type ImageResolutionDisposition string

const (
	ImageResolutionComplete   ImageResolutionDisposition = "complete"
	ImageResolutionRetry      ImageResolutionDisposition = "retry"
	ImageResolutionFail       ImageResolutionDisposition = "fail"
	ImageResolutionSuperseded ImageResolutionDisposition = "superseded"
	ImageResolutionLeaseLost  ImageResolutionDisposition = "lease_lost"
)

type ImageResolutionOutcome struct {
	Disposition ImageResolutionDisposition
	Code        string
	RetryAfter  time.Duration
	Image       oci.ResolvedImage
}

type CredentialReader interface {
	ReadCredential(context.Context, string, string, int64, string) (*oci.Credential, error)
}

var (
	ErrRegistryCredentialMissing = errors.New("registry credential is missing")
	ErrRegistryCredentialInvalid = errors.New("registry credential is invalid")
)

type ImageResolver struct {
	resolver    oci.Resolver
	credentials CredentialReader
	now         func() time.Time
}

func NewImageResolver(resolver oci.Resolver, credentials CredentialReader) (*ImageResolver, error) {
	if resolver == nil {
		return nil, errors.New("image resolver is required")
	}
	return &ImageResolver{resolver: resolver, credentials: credentials, now: time.Now}, nil
}

func (r *ImageResolver) Resolve(ctx context.Context, claim store.ImageResolutionClaim) ImageResolutionOutcome {
	if claim.Job.PendingCommitSHA != nil || claim.Job.DesiredGeneration != claim.EnvironmentDesiredGeneration ||
		claim.Environment.TornDownAt != nil || claim.App.ImageRepository == nil || claim.Job.CommitSHA == "" {
		return ImageResolutionOutcome{Disposition: ImageResolutionSuperseded, Code: string(oci.CodeSuperseded)}
	}
	repository := *claim.App.ImageRepository
	if claim.App.ImageVisibility == "public" {
		repository = strings.TrimPrefix(repository, oci.GHCRHost+"/")
	}
	request := oci.ResolveRequest{Host: oci.GHCRHost, Repository: repository, Tag: claim.Job.CommitSHA,
		ExpectedSource:   oci.RepositoryIdentity{Owner: claim.RepositoryOwner, Repository: claim.RepositoryName},
		ExpectedRevision: claim.Job.CommitSHA, AnnotationPolicy: oci.AnnotationPolicy(claim.App.AnnotationPolicy)}
	var credential *oci.Credential
	if claim.App.ImageVisibility == "private" {
		if r.credentials == nil || claim.Registry == nil || claim.RegistryCredentialSecretRef == nil ||
			claim.Registry.ActiveGeneration < 1 || claim.Registry.ID == "" {
			return ImageResolutionOutcome{Disposition: ImageResolutionFail, Code: string(oci.CodeRegistryAuthRequired)}
		}
		var err error
		credential, err = r.credentials.ReadCredential(ctx, claim.Job.OrgID, claim.Registry.ID,
			claim.Registry.ActiveGeneration, *claim.RegistryCredentialSecretRef)
		if errors.Is(err, ErrRegistryCredentialMissing) || credential == nil && err == nil {
			return ImageResolutionOutcome{Disposition: ImageResolutionFail, Code: string(oci.CodeRegistryAuthRequired)}
		}
		if errors.Is(err, ErrRegistryCredentialInvalid) {
			return ImageResolutionOutcome{Disposition: ImageResolutionFail, Code: string(oci.CodeRegistryAuthFailed)}
		}
		if err != nil {
			return ImageResolutionOutcome{Disposition: ImageResolutionRetry, Code: string(oci.CodeRegistryUnavailable)}
		}
		defer credential.Cleanup()
		if credential.OrgID != claim.Job.OrgID || credential.RegistryID != claim.Registry.ID ||
			credential.Generation != claim.Registry.ActiveGeneration {
			return ImageResolutionOutcome{Disposition: ImageResolutionFail, Code: string(oci.CodeRegistryAuthFailed)}
		}
		request.Credential = credential
	} else if claim.App.ImageVisibility != "public" || claim.Registry != nil {
		return ImageResolutionOutcome{Disposition: ImageResolutionSuperseded, Code: string(oci.CodeSuperseded)}
	}

	image, err := r.resolver.Resolve(ctx, request)
	if err == nil {
		return ImageResolutionOutcome{Disposition: ImageResolutionComplete, Code: string(oci.CodeImageResolved), Image: image}
	}
	now := r.now()
	deadlineExpired := !claim.Deadline.IsZero() && !now.Before(claim.Deadline)
	var status *oci.HTTPStatusError
	if errors.As(err, &status) && status.StatusCode == 404 && status.Kind == oci.ResponseManifest {
		copy := *status
		copy.DeadlineExpired = deadlineExpired
		err = &copy
	}
	disposition := oci.Classify(err, now)
	if disposition.Retryable && !deadlineExpired {
		return ImageResolutionOutcome{Disposition: ImageResolutionRetry, Code: disposition.Code, RetryAfter: disposition.RetryAfter}
	}
	code := disposition.Code
	if deadlineExpired && (disposition.Retryable || isManifestNotFound(err)) {
		code = string(oci.CodeImageNotPublished)
	}
	return ImageResolutionOutcome{Disposition: ImageResolutionFail, Code: code}
}

func isManifestNotFound(err error) bool {
	var status *oci.HTTPStatusError
	return errors.As(err, &status) && status.StatusCode == 404 && status.Kind == oci.ResponseManifest
}

func ImageResolutionCompleteParams(claim store.ImageResolutionClaim, image oci.ResolvedImage) store.CompleteImageResolutionParams {
	repository := strings.TrimPrefix(*claim.App.ImageRepository, oci.GHCRHost+"/")
	tagReference := oci.GHCRHost + "/" + repository + ":" + claim.Job.CommitSHA
	immutableReference := oci.GHCRHost + "/" + repository + "@" + image.Digest
	params := store.CompleteImageResolutionParams{OrgID: claim.Job.OrgID, JobID: claim.Job.ID,
		Repository: repository, TagReference: tagReference,
		Digest: image.Digest, ImmutableReference: immutableReference, ManifestMediaType: image.ManifestMediaType,
		ConfigDigest: image.ConfigDigest, ConfigMediaType: image.ConfigMediaType, Source: image.Source, Revision: image.Revision,
		AnnotationStatus: string(image.AnnotationStatus), DesiredGeneration: claim.Job.DesiredGeneration,
		CommitSHA: claim.Job.CommitSHA, Visibility: claim.App.ImageVisibility, Lease: claim.Lease,
		RepositoryOwner: claim.RepositoryOwner, RepositoryName: claim.RepositoryName, RepositoryNodeID: claim.RepositoryNodeID,
		Claim: claim}
	if claim.Registry != nil {
		params.RegistryID = &claim.Registry.ID
		params.RegistryGeneration = &claim.Registry.ActiveGeneration
	}
	return params
}

func SanitizeImageResolutionCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 64 {
		return string(oci.CodeRegistryResponseInvalid)
	}
	return code
}
