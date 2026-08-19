package oci

import (
	"errors"
	"time"
)

type Code string

const (
	CodeImageResolved                  Code = "image_resolved"
	CodeImageNotPublished              Code = "image_not_published"
	CodeRegistryAuthRequired           Code = "registry_auth_required"
	CodeRegistryAuthFailed             Code = "registry_auth_failed"
	CodeRegistryAuthChallengeInvalid   Code = "registry_auth_challenge_invalid"
	CodeRegistryForbidden              Code = "registry_forbidden"
	CodeRegistryRateLimited            Code = "registry_rate_limited"
	CodeRegistryUnavailable            Code = "registry_unavailable"
	CodeRegistryResponseInvalid        Code = "registry_response_invalid"
	CodeRegistryDigestInvalid          Code = "registry_digest_invalid"
	CodeRegistryMediaTypeUnsupported   Code = "registry_media_type_unsupported"
	CodeRegistryConfigInvalid          Code = "registry_config_invalid"
	CodeOCIAnnotationConflict          Code = "oci_annotation_conflict"
	CodeOCISourceMismatch              Code = "oci_source_mismatch"
	CodeOCIRevisionMismatch            Code = "oci_revision_mismatch"
	CodeCredentialValidationFailed     Code = "credential_validation_failed"
	CodeCredentialOperationExhausted   Code = "credential_operation_exhausted"
	CodeImagePullSecretReconcileFailed Code = "image_pull_secret_reconcile_failed"
	CodeKubernetesConflict             Code = "kubernetes_conflict"
	CodeKubernetesOwnershipConflict    Code = "kubernetes_ownership_conflict"
	CodeKubernetesUnavailable          Code = "kubernetes_unavailable"
	CodePostgresRetryable              Code = "postgres_retryable"
	CodePostgresPermanent              Code = "postgres_permanent"
	CodeSuperseded                     Code = "superseded"
	CodeAncestorTombstoned             Code = "ancestor_tombstoned"
)

type Error struct {
	code           Code
	retryable      bool
	retryAfter     int64
	exhaustionCode Code
}

type ResponseKind string

const (
	ResponseManifest   ResponseKind = "manifest"
	ResponseConfigBlob ResponseKind = "config_blob"
	ResponseValidation ResponseKind = "validation"
)

type HTTPStatusError struct {
	StatusCode      int
	Kind            ResponseKind
	RetryAfter      time.Duration
	HasRetryAfter   bool
	DeadlineExpired bool
}

func (e *HTTPStatusError) Error() string { return "oci: registry status" }

func NewError(code Code) error { return &Error{code: code} }

func NewRetryableError(code, exhaustionCode Code, retryAfter time.Duration) error {
	return &Error{code: code, retryable: true, retryAfter: int64(retryAfter), exhaustionCode: exhaustionCode}
}

func (e *Error) Error() string { return "oci: " + string(e.code) }
func (e *Error) Code() Code    { return e.code }

func IsCode(err error, code Code) bool {
	var target *Error
	return errors.As(err, &target) && target.code == code
}
