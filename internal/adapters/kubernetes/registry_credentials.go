package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/secretref"
)

const maxRegistryCredentialMaterialBytes = 4096
const maxImagePullSecretConflictRetries = 5

const (
	RegistryCredentialKey = corev1.DockerConfigJsonKey

	registryCredentialPurposeLabel     = "cyclopus.dev/credential-purpose"
	registryCredentialPurpose          = "ghcr-pull-source"
	registryCredentialOrgHashLabel     = "cyclopus.dev/org-hash"
	registryCredentialRegistryLabel    = "cyclopus.dev/registry-id"
	registryCredentialGenerationLabel  = "cyclopus.dev/generation"
	registryCredentialOperationLabel   = "cyclopus.dev/operation-id"
	registryCredentialOrgAnnotation    = "cyclopus.dev/org-id"
	registryCredentialOwnerAnnotation  = "cyclopus.dev/owner-namespace"
	registryCredentialDigestAnnotation = "cyclopus.dev/data-sha256"

	ImagePullSecretName                   = "cyclopus-ghcr-pull"
	imagePullSecretPurpose                = "ghcr-pull"
	imagePullSecretNamespaceUIDAnnotation = "cyclopus.dev/namespace-uid"
	imagePullSecretOrgAnnotation          = "cyclopus.dev/org-id"
	imagePullSecretRegistryAnnotation     = "cyclopus.dev/registry-id"
	imagePullSecretEnvironmentAnnotation  = "cyclopus.dev/environment-id"
	imagePullSecretSourceNameAnnotation   = "cyclopus.dev/source-secret-name"
	imagePullSecretSourceUIDAnnotation    = "cyclopus.dev/source-secret-uid"
	imagePullSecretGenerationAnnotation   = "cyclopus.dev/generation"
	imagePullSecretContentSHAAnnotation   = "cyclopus.dev/content-sha256"
)

// Typed adapter errors let the worker classify read failures without
// string-matching: missing Secrets are permanent for the operation, malformed
// Secrets are permanent, and anything else is transient.
var (
	ErrRegistryCredentialCollision = errors.New("registry credential Secret collision")
	ErrRegistryCredentialInvalid   = errors.New("registry credential Secret is invalid")
	ErrRegistryCredentialNotFound  = errors.New("registry credential Secret is missing")
	ErrImagePullSecretOwnership    = errors.New("image pull Secret ownership mismatch")
	ErrImagePullSecretConflict     = errors.New("image pull Secret update conflict")
)

type RegistryCredentialAdapterError struct {
	code      oci.Code
	retryable bool
	cause     error
}

func (e *RegistryCredentialAdapterError) Error() string {
	return "kubernetes registry credential: " + string(e.code)
}
func (e *RegistryCredentialAdapterError) Unwrap() error   { return e.cause }
func (e *RegistryCredentialAdapterError) Code() oci.Code  { return e.code }
func (e *RegistryCredentialAdapterError) Retryable() bool { return e.retryable }

func RegistryCredentialErrorDisposition(err error) (retryable bool, code oci.Code, ok bool) {
	var adapterError *RegistryCredentialAdapterError
	if !errors.As(err, &adapterError) {
		return false, "", false
	}
	return adapterError.retryable, adapterError.code, true
}

type RegistryCredentialSecretAdapter struct {
	client    kubernetes.Interface
	namespace string
}

type RegistryCredentialSecretIdentity struct {
	SecretRef, UID, ResourceVersion string
}

type ImagePullSecretIdentity struct {
	SourceSecretName, SourceUID, NamespaceUID string
}

type ImagePullSecretRequest struct {
	OrgID, AppID, EnvironmentID string
	Namespace, NamespaceUID     string
	RegistryID                  string
	SourceSecretName, SourceUID string
	Generation                  int64
}

func NewRegistryCredentialSecretAdapter(client kubernetes.Interface, namespace string) (*RegistryCredentialSecretAdapter, error) {
	namespace = strings.TrimSpace(namespace)
	if client == nil || namespace == "" {
		return nil, errors.New("registry credential Secret client and namespace are required")
	}
	return &RegistryCredentialSecretAdapter{client: client, namespace: namespace}, nil
}

func RegistryCredentialSecretName(orgID string, generation int64) (string, error) {
	if strings.TrimSpace(orgID) != orgID || orgID == "" || generation < 1 {
		return "", ErrRegistryCredentialInvalid
	}
	sum := sha256.Sum256([]byte(orgID))
	return "cyclopus-ghcr-" + hex.EncodeToString(sum[:10]) + "-g" + strconv.FormatInt(generation, 10), nil
}

func (a *RegistryCredentialSecretAdapter) WriteCredential(ctx context.Context, orgID, registryID, operationID string, generation int64, credential *oci.Credential) (string, string, string, error) {
	// Reject malformed credential material before any Secret side effect:
	// identity mismatch, empty or oversized fields, and non-UTF-8 or
	// control-bearing bytes never reach the API server.
	if credential == nil || credential.OrgID != orgID || credential.RegistryID != registryID || credential.Generation != generation ||
		!validRegistryCredentialMaterial(credential.Username, false) || !validRegistryCredentialMaterial(credential.PAT, true) {
		return "", "", "", ErrRegistryCredentialInvalid
	}
	name, err := RegistryCredentialSecretName(orgID, generation)
	if err != nil {
		return "", "", "", err
	}
	payload, err := marshalDockerConfig(credential.Username, credential.PAT)
	if err != nil {
		return "", "", "", ErrRegistryCredentialInvalid
	}
	defer clear(payload)
	digest := sha256.Sum256(payload)
	digestText := hex.EncodeToString(digest[:])
	immutable := true
	desired := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace,
		Labels: registryCredentialLabels(orgID, registryID, operationID, generation), Annotations: map[string]string{
			registryCredentialOrgAnnotation: orgID, registryCredentialOwnerAnnotation: a.namespace,
			registryCredentialDigestAnnotation: digestText,
		}}, Type: corev1.SecretTypeDockerConfigJson, Immutable: &immutable,
		Data: map[string][]byte{RegistryCredentialKey: append([]byte(nil), payload...)}}
	defer clear(desired.Data[RegistryCredentialKey])
	created, err := a.client.CoreV1().Secrets(a.namespace).Create(ctx, desired, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		created, err = a.client.CoreV1().Secrets(a.namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil && !validRegistryCredentialSecret(created, orgID, registryID, operationID, generation, a.namespace, digestText) {
			return "", "", "", ErrRegistryCredentialCollision
		}
	}
	if err != nil {
		return "", "", "", err
	}
	if created.UID == "" {
		return "", "", "", ErrRegistryCredentialInvalid
	}
	identity := registrySecretIdentity(a.namespace, created)
	return identity.SecretRef, identity.UID, identity.ResourceVersion, nil
}

func (a *RegistryCredentialSecretAdapter) ReadCredential(ctx context.Context, orgID, registryID string, generation int64, value string) (*oci.Credential, error) {
	reference, err := secretref.Parse(value)
	if err != nil || reference.Namespace != a.namespace || reference.Key != RegistryCredentialKey {
		return nil, ErrRegistryCredentialInvalid
	}
	name, err := RegistryCredentialSecretName(orgID, generation)
	if err != nil || reference.Name != name {
		return nil, ErrRegistryCredentialInvalid
	}
	secret, err := a.client.CoreV1().Secrets(a.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrRegistryCredentialNotFound
	}
	if err != nil {
		return nil, err
	}
	operationID := secret.Labels[registryCredentialOperationLabel]
	payload, ok := secret.Data[RegistryCredentialKey]
	digest := sha256.Sum256(payload)
	if !ok || !validRegistryCredentialSecret(secret, orgID, registryID, operationID, generation, a.namespace, hex.EncodeToString(digest[:])) {
		return nil, ErrRegistryCredentialInvalid
	}
	username, password, err := parseDockerConfig(payload)
	if err != nil {
		return nil, ErrRegistryCredentialInvalid
	}
	return &oci.Credential{Username: username, PAT: password, OrgID: orgID, RegistryID: registryID, Generation: generation}, nil
}

// ReadImagePullSecretIdentity validates the persisted source reference and
// immutable source Secret without returning credential material, then reads the
// current namespace UID needed to fence the destination reconciliation.
func (a *RegistryCredentialSecretAdapter) ReadImagePullSecretIdentity(ctx context.Context, orgID, registryID string, generation int64, value, namespace string) (ImagePullSecretIdentity, error) {
	reference, err := secretref.Parse(value)
	if err != nil || reference.Namespace != a.namespace || reference.Key != RegistryCredentialKey {
		return ImagePullSecretIdentity{}, imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialInvalid)
	}
	name, err := RegistryCredentialSecretName(orgID, generation)
	if err != nil || reference.Name != name || namespace == "" {
		return ImagePullSecretIdentity{}, imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialInvalid)
	}
	secret, err := a.client.CoreV1().Secrets(a.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ImagePullSecretIdentity{}, imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialNotFound)
	}
	if err != nil {
		return ImagePullSecretIdentity{}, imagePullSecretAPIError(err)
	}
	operationID := secret.Labels[registryCredentialOperationLabel]
	payload, ok := secret.Data[RegistryCredentialKey]
	digest := sha256.Sum256(payload)
	if !ok || !validRegistryCredentialSecret(secret, orgID, registryID, operationID, generation, a.namespace, hex.EncodeToString(digest[:])) {
		return ImagePullSecretIdentity{}, imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialInvalid)
	}
	namespaceObject, err := a.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return ImagePullSecretIdentity{}, imagePullSecretAPIError(err)
	}
	if namespaceObject.UID == "" {
		return ImagePullSecretIdentity{}, imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialInvalid)
	}
	return ImagePullSecretIdentity{SourceSecretName: name, SourceUID: string(secret.UID), NamespaceUID: string(namespaceObject.UID)}, nil
}

// ReadImagePullSecretCleanupIdentity authorizes cleanup from persisted target
// identity and exact destination metadata. The immutable source may already
// have been garbage-collected; in that case its UID is recovered only from the
// owned destination annotation and remains part of the delete ownership fence.
func (a *RegistryCredentialSecretAdapter) ReadImagePullSecretCleanupIdentity(ctx context.Context, orgID, registryID string, generation int64, value, namespace string) (ImagePullSecretIdentity, error) {
	reference, err := secretref.Parse(value)
	if err != nil || reference.Namespace != a.namespace || reference.Key != RegistryCredentialKey || namespace == "" {
		return ImagePullSecretIdentity{}, imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialInvalid)
	}
	name, err := RegistryCredentialSecretName(orgID, generation)
	if err != nil || reference.Name != name {
		return ImagePullSecretIdentity{}, imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialInvalid)
	}
	namespaceObject, err := a.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ImagePullSecretIdentity{SourceSecretName: name, SourceUID: "namespace-missing", NamespaceUID: "namespace-missing"}, nil
	}
	if err != nil {
		return ImagePullSecretIdentity{}, imagePullSecretAPIError(err)
	}
	if namespaceObject.UID == "" {
		return ImagePullSecretIdentity{}, imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialInvalid)
	}
	destination, err := a.client.CoreV1().Secrets(namespace).Get(ctx, ImagePullSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ImagePullSecretIdentity{SourceSecretName: name, SourceUID: "destination-missing", NamespaceUID: string(namespaceObject.UID)}, nil
	}
	if err != nil {
		return ImagePullSecretIdentity{}, imagePullSecretAPIError(err)
	}
	request := ImagePullSecretRequest{OrgID: orgID, AppID: namespaceObject.Labels[AppIDLabel],
		EnvironmentID: namespaceObject.Labels[EnvironmentIDLabel], Namespace: namespace,
		NamespaceUID: string(namespaceObject.UID), RegistryID: registryID, SourceSecretName: name,
		SourceUID: destination.Annotations[imagePullSecretSourceUIDAnnotation], Generation: generation}
	if request.SourceUID == "" || verifyImagePullSecretNamespace(namespaceObject, request) != nil || !ownedImagePullSecret(destination, request) {
		return ImagePullSecretIdentity{}, imagePullSecretError(oci.CodeKubernetesOwnershipConflict, false, ErrImagePullSecretOwnership)
	}
	return ImagePullSecretIdentity{SourceSecretName: name, SourceUID: request.SourceUID, NamespaceUID: request.NamespaceUID}, nil
}

// validRegistryCredentialMaterial requires valid UTF-8 within a bounded size
// and rejects ASCII control bytes (and DEL) that could corrupt logs, JSON, or
// the Basic-auth wire format.
func validRegistryCredentialMaterial(value []byte, allowColon bool) bool {
	if len(value) == 0 || len(value) > maxRegistryCredentialMaterialBytes || !utf8.Valid(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f || (!allowColon && character == ':') {
			return false
		}
	}
	return true
}

// DeleteCredential deletes only an exactly-owned immutable generation after the
// caller has separately established that persisted GC state authorizes deletion.
func (a *RegistryCredentialSecretAdapter) DeleteCredential(ctx context.Context, orgID, registryID, operationID string, generation int64, gcAuthorized bool) error {
	if !gcAuthorized {
		return ErrRegistryCredentialInvalid
	}
	name, err := RegistryCredentialSecretName(orgID, generation)
	if err != nil {
		return err
	}
	secret, err := a.client.CoreV1().Secrets(a.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	payload := secret.Data[RegistryCredentialKey]
	digest := sha256.Sum256(payload)
	if !validRegistryCredentialSecret(secret, orgID, registryID, operationID, generation, a.namespace, hex.EncodeToString(digest[:])) {
		return ErrRegistryCredentialCollision
	}
	uid := secret.UID
	return a.client.CoreV1().Secrets(a.namespace).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
}

// ReconcileImagePullSecret copies an immutable source generation into the
// stable, mutable Secret consumed by workloads in an exactly-owned namespace.
func (a *RegistryCredentialSecretAdapter) ReconcileImagePullSecret(ctx context.Context, request ImagePullSecretRequest) error {
	if !validImagePullSecretRequest(request) {
		return imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialInvalid)
	}
	for attempt := 0; attempt <= maxImagePullSecretConflictRetries; attempt++ {
		namespace, source, destination, err := a.readImagePullSecretState(ctx, request)
		if err != nil {
			return err
		}
		if err = verifyImagePullSecretNamespace(namespace, request); err != nil {
			return err
		}
		payload, digest, err := a.verifyImagePullSecretSource(source, request)
		if err != nil {
			return err
		}
		desired := desiredImagePullSecret(request, digest, payload)
		if destination == nil {
			_, err = a.client.CoreV1().Secrets(request.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		} else {
			if !ownedImagePullSecret(destination, request) {
				return imagePullSecretError(oci.CodeKubernetesOwnershipConflict, false, ErrImagePullSecretOwnership)
			}
			desired.ResourceVersion = destination.ResourceVersion
			desired.UID = destination.UID
			_, err = a.client.CoreV1().Secrets(request.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
		}
		if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return imagePullSecretAPIError(err)
		}
		verifiedNamespace, verifiedSource, verified, err := a.readImagePullSecretState(ctx, request)
		if err != nil {
			return err
		}
		verifiedPayload, verifiedDigest, err := a.verifyImagePullSecretSource(verifiedSource, request)
		if err != nil {
			return err
		}
		if err = verifyImagePullSecretNamespace(verifiedNamespace, request); err != nil || verifiedDigest != digest ||
			!bytes.Equal(verifiedPayload, payload) || !matchesImagePullSecret(verified, request, digest, payload) {
			return imagePullSecretError(oci.CodeKubernetesOwnershipConflict, false, ErrImagePullSecretOwnership)
		}
		return nil
	}
	return imagePullSecretError(oci.CodeKubernetesConflict, true, ErrImagePullSecretConflict)
}

// DeleteImagePullSecret removes only the stable Secret bound to the exact
// namespace UID in the request. A missing or recreated namespace is already
// converged and is never touched.
func (a *RegistryCredentialSecretAdapter) DeleteImagePullSecret(ctx context.Context, request ImagePullSecretRequest) error {
	if !validImagePullSecretRequest(request) {
		return imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialInvalid)
	}
	for attempt := 0; attempt <= maxImagePullSecretConflictRetries; attempt++ {
		namespace, err := a.client.CoreV1().Namespaces().Get(ctx, request.Namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return imagePullSecretAPIError(err)
		}
		if string(namespace.UID) != request.NamespaceUID {
			return nil
		}
		if err = verifyImagePullSecretNamespace(namespace, request); err != nil {
			return err
		}
		// Re-read the source on every attempt even though deletion is authorized
		// solely by destination and namespace ownership.
		if _, err = a.client.CoreV1().Secrets(a.namespace).Get(ctx, request.SourceSecretName, metav1.GetOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return imagePullSecretAPIError(err)
		}
		destination, err := a.client.CoreV1().Secrets(request.Namespace).Get(ctx, ImagePullSecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return imagePullSecretAPIError(err)
		}
		if !ownedImagePullSecret(destination, request) {
			return imagePullSecretError(oci.CodeKubernetesOwnershipConflict, false, ErrImagePullSecretOwnership)
		}
		uid := destination.UID
		err = a.client.CoreV1().Secrets(request.Namespace).Delete(ctx, ImagePullSecretName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if apierrors.IsConflict(err) {
			continue
		}
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return imagePullSecretAPIError(err)
		}
		if _, err = a.client.CoreV1().Secrets(request.Namespace).Get(ctx, ImagePullSecretName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return imagePullSecretAPIError(err)
		}
		return imagePullSecretError(oci.CodeKubernetesOwnershipConflict, false, ErrImagePullSecretOwnership)
	}
	return imagePullSecretError(oci.CodeKubernetesConflict, true, ErrImagePullSecretConflict)
}

func (a *RegistryCredentialSecretAdapter) readImagePullSecretState(ctx context.Context, request ImagePullSecretRequest) (*corev1.Namespace, *corev1.Secret, *corev1.Secret, error) {
	namespace, err := a.client.CoreV1().Namespaces().Get(ctx, request.Namespace, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, imagePullSecretAPIError(err)
	}
	source, err := a.client.CoreV1().Secrets(a.namespace).Get(ctx, request.SourceSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil, nil, imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialNotFound)
	}
	if err != nil {
		return nil, nil, nil, imagePullSecretAPIError(err)
	}
	destination, err := a.client.CoreV1().Secrets(request.Namespace).Get(ctx, ImagePullSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		destination, err = nil, nil
	}
	if err != nil {
		return nil, nil, nil, imagePullSecretAPIError(err)
	}
	return namespace, source, destination, nil
}

func (a *RegistryCredentialSecretAdapter) verifyImagePullSecretSource(source *corev1.Secret, request ImagePullSecretRequest) ([]byte, string, error) {
	operationID := source.Labels[registryCredentialOperationLabel]
	payload, ok := source.Data[RegistryCredentialKey]
	digest := sha256.Sum256(payload)
	digestText := hex.EncodeToString(digest[:])
	if !ok || string(source.UID) != request.SourceUID || source.Name != request.SourceSecretName ||
		!validRegistryCredentialSecret(source, request.OrgID, request.RegistryID, operationID, request.Generation, a.namespace, digestText) {
		return nil, "", imagePullSecretError(oci.CodeImagePullSecretReconcileFailed, false, ErrRegistryCredentialInvalid)
	}
	return payload, digestText, nil
}

func validImagePullSecretRequest(request ImagePullSecretRequest) bool {
	return request.OrgID != "" && request.AppID != "" && request.EnvironmentID != "" && request.Namespace != "" &&
		request.NamespaceUID != "" && request.RegistryID != "" && request.SourceSecretName != "" && request.SourceUID != "" && request.Generation > 0
}

func verifyImagePullSecretNamespace(namespace *corev1.Namespace, request ImagePullSecretRequest) error {
	if namespace == nil || namespace.Name != request.Namespace || string(namespace.UID) != request.NamespaceUID ||
		namespace.Labels[ManagedByLabel] != ManagedByValue || namespace.Labels[OrgIDLabel] != request.OrgID ||
		namespace.Labels[AppIDLabel] != request.AppID || namespace.Labels[EnvironmentIDLabel] != request.EnvironmentID ||
		namespace.Annotations[NamespaceOwnerAnnotation] != request.EnvironmentID || namespace.Annotations[NamespaceUIDAnnotation] != request.NamespaceUID {
		return imagePullSecretError(oci.CodeKubernetesOwnershipConflict, false, ErrImagePullSecretOwnership)
	}
	return nil
}

func desiredImagePullSecret(request ImagePullSecretRequest, digest string, payload []byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ImagePullSecretName, Namespace: request.Namespace,
		Labels: map[string]string{ManagedByLabel: ManagedByValue, OrgIDLabel: request.OrgID, AppIDLabel: request.AppID,
			EnvironmentIDLabel: request.EnvironmentID, registryCredentialPurposeLabel: imagePullSecretPurpose},
		Annotations: map[string]string{imagePullSecretNamespaceUIDAnnotation: request.NamespaceUID, imagePullSecretOrgAnnotation: request.OrgID,
			imagePullSecretRegistryAnnotation: request.RegistryID, imagePullSecretEnvironmentAnnotation: request.EnvironmentID,
			imagePullSecretSourceNameAnnotation: request.SourceSecretName, imagePullSecretSourceUIDAnnotation: request.SourceUID,
			imagePullSecretGenerationAnnotation: strconv.FormatInt(request.Generation, 10), imagePullSecretContentSHAAnnotation: digest}},
		Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{RegistryCredentialKey: append([]byte(nil), payload...)}}
}

func ownedImagePullSecret(secret *corev1.Secret, request ImagePullSecretRequest) bool {
	return secret != nil && secret.Name == ImagePullSecretName && secret.Namespace == request.Namespace && secret.UID != "" &&
		secret.Labels[ManagedByLabel] == ManagedByValue && secret.Labels[OrgIDLabel] == request.OrgID &&
		secret.Labels[AppIDLabel] == request.AppID && secret.Labels[EnvironmentIDLabel] == request.EnvironmentID &&
		secret.Labels[registryCredentialPurposeLabel] == imagePullSecretPurpose &&
		secret.Annotations[imagePullSecretNamespaceUIDAnnotation] == request.NamespaceUID &&
		secret.Annotations[imagePullSecretOrgAnnotation] == request.OrgID &&
		secret.Annotations[imagePullSecretRegistryAnnotation] == request.RegistryID &&
		secret.Annotations[imagePullSecretEnvironmentAnnotation] == request.EnvironmentID &&
		secret.Annotations[imagePullSecretSourceNameAnnotation] == request.SourceSecretName &&
		secret.Annotations[imagePullSecretSourceUIDAnnotation] == request.SourceUID &&
		secret.Annotations[imagePullSecretGenerationAnnotation] == strconv.FormatInt(request.Generation, 10)
}

func matchesImagePullSecret(secret *corev1.Secret, request ImagePullSecretRequest, digest string, payload []byte) bool {
	if !ownedImagePullSecret(secret, request) || secret.Immutable != nil && *secret.Immutable || secret.Type != corev1.SecretTypeDockerConfigJson ||
		len(secret.Data) != 1 || !bytes.Equal(secret.Data[RegistryCredentialKey], payload) ||
		secret.Annotations[imagePullSecretContentSHAAnnotation] != digest {
		return false
	}
	return len(secret.Labels) == 5 && len(secret.Annotations) == 8
}

func imagePullSecretError(code oci.Code, retryable bool, cause error) error {
	return &RegistryCredentialAdapterError{code: code, retryable: retryable, cause: cause}
}

func imagePullSecretAPIError(err error) error {
	if apierrors.IsConflict(err) {
		return imagePullSecretError(oci.CodeKubernetesConflict, true, ErrImagePullSecretConflict)
	}
	return imagePullSecretError(oci.CodeKubernetesUnavailable, true, nil)
}

func registryCredentialLabels(orgID, registryID, operationID string, generation int64) map[string]string {
	sum := sha256.Sum256([]byte(orgID))
	return map[string]string{ManagedByLabel: ManagedByValue, registryCredentialPurposeLabel: registryCredentialPurpose,
		registryCredentialOrgHashLabel: hex.EncodeToString(sum[:10]), registryCredentialRegistryLabel: registryID,
		registryCredentialGenerationLabel: strconv.FormatInt(generation, 10), registryCredentialOperationLabel: operationID}
}

func validRegistryCredentialSecret(secret *corev1.Secret, orgID, registryID, operationID string, generation int64, namespace, digest string) bool {
	name, err := RegistryCredentialSecretName(orgID, generation)
	if err != nil || secret == nil || secret.Name != name || secret.Namespace != namespace || secret.UID == "" ||
		secret.Immutable == nil || !*secret.Immutable || secret.Type != corev1.SecretTypeDockerConfigJson || len(secret.Data) != 1 ||
		secret.Annotations[registryCredentialOrgAnnotation] != orgID || secret.Annotations[registryCredentialOwnerAnnotation] != namespace ||
		secret.Annotations[registryCredentialDigestAnnotation] != digest {
		return false
	}
	want := registryCredentialLabels(orgID, registryID, operationID, generation)
	if len(secret.Labels) != len(want) {
		return false
	}
	for key, value := range want {
		if secret.Labels[key] != value {
			return false
		}
	}
	_, ok := secret.Data[RegistryCredentialKey]
	return ok
}

func registrySecretIdentity(namespace string, secret *corev1.Secret) RegistryCredentialSecretIdentity {
	return RegistryCredentialSecretIdentity{SecretRef: "k8s-secret://" + namespace + "/" + secret.Name + "#" + RegistryCredentialKey,
		UID: string(secret.UID), ResourceVersion: secret.ResourceVersion}
}

func marshalDockerConfig(username, password []byte) ([]byte, error) {
	if !validRegistryCredentialMaterial(username, false) || !validRegistryCredentialMaterial(password, true) {
		return nil, ErrRegistryCredentialInvalid
	}
	authSource := make([]byte, 0, len(username)+1+len(password))
	authSource = append(authSource, username...)
	authSource = append(authSource, ':')
	authSource = append(authSource, password...)
	defer clear(authSource)
	auth := make([]byte, base64.StdEncoding.EncodedLen(len(authSource)))
	base64.StdEncoding.Encode(auth, authSource)
	defer clear(auth)
	result := make([]byte, 0, len(username)+len(password)+len(auth)+96)
	result = append(result, `{"auths":{"ghcr.io":{"username":`...)
	result = appendJSONString(result, username)
	result = append(result, `,"password":`...)
	result = appendJSONString(result, password)
	result = append(result, `,"auth":`...)
	result = appendJSONString(result, auth)
	result = append(result, `}}}`...)
	return result, nil
}

func appendJSONString(dst, value []byte) []byte {
	dst = append(dst, '"')
	for _, character := range value {
		switch character {
		case '\\', '"':
			dst = append(dst, '\\', character)
		case '\b':
			dst = append(dst, `\b`...)
		case '\f':
			dst = append(dst, `\f`...)
		case '\n':
			dst = append(dst, `\n`...)
		case '\r':
			dst = append(dst, `\r`...)
		case '\t':
			dst = append(dst, `\t`...)
		default:
			if character < 0x20 {
				dst = append(dst, fmt.Sprintf(`\u%04x`, character)...)
			} else {
				dst = append(dst, character)
			}
		}
	}
	return append(dst, '"')
}

// parseDockerConfig is a strict, allocation-minimal flat parser over the
// mutable Secret payload. Unlike encoding/json it never materializes
// credential material in immutable Go strings: username, password, and the
// base64 auth value stay in []byte scratch that is zeroed before return, and
// every raw body buffer is cleared. It preserves the decoder contract exactly:
// duplicate keys, unknown fields, nested content, and trailing data are all
// rejected, exactly one host entry (ghcr.io) is allowed, and auth must equal
// base64(username + ':' + password) and agree with the discrete fields.
// dockerConfigObserve is a test-only hook invoked with every credential-bearing
// scratch buffer the parser allocates so tests can verify all are zeroed.
// It is nil in production builds and must never retain buffer contents.
var dockerConfigObserve func([]byte)

func parseDockerConfig(payload []byte) ([]byte, []byte, error) {
	scratch := append([]byte(nil), payload...)
	defer clear(scratch)
	if dockerConfigObserve != nil {
		dockerConfigObserve(scratch)
	}
	parser := dockerConfigParser{data: scratch}
	if err := parser.consumeByte('{'); err != nil {
		return nil, nil, err
	}
	parser.skipSpace()
	if parser.peek() == '}' {
		return nil, nil, ErrRegistryCredentialInvalid
	}
	authsSeen := false
	for {
		key, err := parser.string()
		if err != nil {
			return nil, nil, err
		}
		if err = parser.consumeByte(':'); err != nil {
			return nil, nil, err
		}
		if !bytes.Equal(key, dockerKeyAuths) || authsSeen {
			return nil, nil, ErrRegistryCredentialInvalid
		}
		authsSeen = true
		username, password, auth, err := parser.auths()
		defer func() {
			clear(username)
			clear(password)
			clear(auth)
		}()
		if dockerConfigObserve != nil {
			dockerConfigObserve(username)
			dockerConfigObserve(password)
			dockerConfigObserve(auth)
		}
		if err != nil {
			return nil, nil, ErrRegistryCredentialInvalid
		}
		parser.skipSpace()
		switch next := parser.next(); {
		case next == ',':
			if parser.peek() == '}' {
				return nil, nil, ErrRegistryCredentialInvalid
			}
		case next == '}':
			parser.skipSpace()
			if parser.position != len(parser.data) {
				return nil, nil, ErrRegistryCredentialInvalid
			}
			if !validRegistryCredentialMaterial(username, false) || !validRegistryCredentialMaterial(password, true) {
				return nil, nil, ErrRegistryCredentialInvalid
			}
			decoded := make([]byte, base64.StdEncoding.Strict().DecodedLen(len(auth)))
			size, err := base64.StdEncoding.Strict().Decode(decoded, auth)
			if err != nil {
				return nil, nil, ErrRegistryCredentialInvalid
			}
			decoded = decoded[:size]
			defer clear(decoded)
			want := make([]byte, 0, len(username)+1+len(password))
			want = append(want, username...)
			want = append(want, ':')
			want = append(want, password...)
			defer clear(want)
			if !bytes.Equal(decoded, want) {
				return nil, nil, ErrRegistryCredentialInvalid
			}
			return append([]byte(nil), username...), append([]byte(nil), password...), nil
		default:
			return nil, nil, ErrRegistryCredentialInvalid
		}
	}
}

var (
	dockerKeyAuths    = []byte("auths")
	dockerKeyUsername = []byte("username")
	dockerKeyPassword = []byte("password")
	dockerKeyAuth     = []byte("auth")
	dockerHostGHCR    = []byte(oci.GHCRHost)
)

// dockerConfigParser is a byte-level cursor over mutable scratch. string
// returns freshly allocated mutable buffers, never immutable strings.
type dockerConfigParser struct {
	data     []byte
	position int
}

func (p *dockerConfigParser) skipSpace() {
	for p.position < len(p.data) {
		switch p.data[p.position] {
		case ' ', '\t', '\n', '\r':
			p.position++
		default:
			return
		}
	}
}

func (p *dockerConfigParser) peek() byte {
	p.skipSpace()
	if p.position >= len(p.data) {
		return 0
	}
	return p.data[p.position]
}

func (p *dockerConfigParser) next() byte {
	p.skipSpace()
	if p.position >= len(p.data) {
		return 0
	}
	value := p.data[p.position]
	p.position++
	return value
}

func (p *dockerConfigParser) consumeByte(want byte) error {
	if p.next() != want {
		return ErrRegistryCredentialInvalid
	}
	return nil
}

func (p *dockerConfigParser) string() ([]byte, error) {
	if p.next() != '"' {
		return nil, ErrRegistryCredentialInvalid
	}
	result := make([]byte, 0, 16)
	failed := true
	defer func() {
		if failed {
			clear(result)
		}
	}()
	for {
		if p.position >= len(p.data) {
			return nil, ErrRegistryCredentialInvalid
		}
		character := p.data[p.position]
		p.position++
		switch {
		case character == '"':
			failed = false
			return result, nil
		case character < 0x20:
			return nil, ErrRegistryCredentialInvalid
		case character == '\\':
			if p.position >= len(p.data) {
				return nil, ErrRegistryCredentialInvalid
			}
			escaped := p.data[p.position]
			p.position++
			switch escaped {
			case '"', '\\', '/':
				result = append(result, escaped)
			case 'b':
				result = append(result, '\b')
			case 'f':
				result = append(result, '\f')
			case 'n':
				result = append(result, '\n')
			case 'r':
				result = append(result, '\r')
			case 't':
				result = append(result, '\t')
			case 'u':
				if len(p.data)-p.position < 4 {
					return nil, ErrRegistryCredentialInvalid
				}
				code, ok := dockerConfigHex4(p.data[p.position : p.position+4])
				if !ok {
					return nil, ErrRegistryCredentialInvalid
				}
				p.position += 4
				result = utf8.AppendRune(result, dockerConfigRune(code, p))
			default:
				return nil, ErrRegistryCredentialInvalid
			}
		default:
			result = append(result, character)
		}
	}
}

// dockerConfigRune resolves \uXXXX escapes including surrogate pairs; lone or
// malformed surrogates are rejected like encoding/json rejects invalid UTF-8.
func dockerConfigRune(code uint16, p *dockerConfigParser) rune {
	if code >= 0xd800 && code <= 0xdbff {
		if len(p.data)-p.position >= 6 && p.data[p.position] == '\\' && p.data[p.position+1] == 'u' {
			if low, ok := dockerConfigHex4(p.data[p.position+2 : p.position+6]); ok && low >= 0xdc00 && low <= 0xdfff {
				p.position += 6
				return (rune(code)-0xd800)<<10 + (rune(low) - 0xdc00) + 0x10000
			}
		}
		return utf8.RuneError
	}
	if code >= 0xdc00 && code <= 0xdfff {
		return utf8.RuneError
	}
	return rune(code)
}

func dockerConfigHex4(data []byte) (uint16, bool) {
	var value uint16
	for _, character := range data {
		var digit byte
		switch {
		case character >= '0' && character <= '9':
			digit = character - '0'
		case character >= 'a' && character <= 'f':
			digit = character - 'a' + 10
		case character >= 'A' && character <= 'F':
			digit = character - 'A' + 10
		default:
			return 0, false
		}
		value = value<<4 | uint16(digit)
	}
	return value, true
}

// auths parses {"host":{...}} requiring exactly one ghcr.io entry whose object
// contains exactly username, password, and auth in any order, each once.
func (p *dockerConfigParser) auths() (username, password, auth []byte, err error) {
	defer func() {
		if err != nil {
			clear(username)
			clear(password)
			clear(auth)
			username, password, auth = nil, nil, nil
		}
	}()
	if err := p.consumeByte('{'); err != nil {
		return nil, nil, nil, err
	}
	if p.peek() == '}' {
		return nil, nil, nil, ErrRegistryCredentialInvalid
	}
	entrySeen := false
	for {
		key, keyErr := p.string()
		if keyErr != nil {
			return nil, nil, nil, keyErr
		}
		if err = p.consumeByte(':'); err != nil {
			return nil, nil, nil, err
		}
		if !bytes.Equal(key, dockerHostGHCR) || entrySeen {
			return nil, nil, nil, ErrRegistryCredentialInvalid
		}
		entrySeen = true
		if username, password, auth, err = p.entry(); err != nil {
			return nil, nil, nil, err
		}
		switch next := p.next(); next {
		case ',':
			if p.peek() == '}' {
				return nil, nil, nil, ErrRegistryCredentialInvalid
			}
		case '}':
			return username, password, auth, nil
		default:
			return nil, nil, nil, ErrRegistryCredentialInvalid
		}
	}
}

func (p *dockerConfigParser) entry() (username, password, auth []byte, err error) {
	defer func() {
		if err != nil {
			clear(username)
			clear(password)
			clear(auth)
			username, password, auth = nil, nil, nil
		}
	}()
	if err := p.consumeByte('{'); err != nil {
		return nil, nil, nil, err
	}
	if p.peek() == '}' {
		return nil, nil, nil, ErrRegistryCredentialInvalid
	}
	var seen byte
	for {
		key, keyErr := p.string()
		if keyErr != nil {
			return nil, nil, nil, keyErr
		}
		var field byte
		var target *[]byte
		switch {
		case bytes.Equal(key, dockerKeyUsername):
			field, target = 1, &username
		case bytes.Equal(key, dockerKeyPassword):
			field, target = 2, &password
		case bytes.Equal(key, dockerKeyAuth):
			field, target = 4, &auth
		default:
			return nil, nil, nil, ErrRegistryCredentialInvalid
		}
		if seen&field != 0 {
			return nil, nil, nil, ErrRegistryCredentialInvalid
		}
		seen |= field
		if err = p.consumeByte(':'); err != nil {
			return nil, nil, nil, err
		}
		value, valueErr := p.string()
		if valueErr != nil {
			return nil, nil, nil, valueErr
		}
		*target = value
		switch next := p.next(); next {
		case ',':
			if p.peek() == '}' {
				return nil, nil, nil, ErrRegistryCredentialInvalid
			}
		case '}':
			if seen != 7 || len(username) == 0 || len(password) == 0 || len(auth) == 0 {
				return nil, nil, nil, ErrRegistryCredentialInvalid
			}
			return username, password, auth, nil
		default:
			return nil, nil, nil, ErrRegistryCredentialInvalid
		}
	}
}
