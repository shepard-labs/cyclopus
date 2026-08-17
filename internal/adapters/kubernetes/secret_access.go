package kubernetes

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/shepard-labs/cyclopus/internal/secretref"
	"github.com/shepard-labs/cyclopus/internal/webhook"
)

const WebhookSecretReaderRoleName = "cyclopus-webhook-secret-reader"

type WebhookSecretAuthorizer struct {
	client      kubernetes.Interface
	namespace   string
	releaseName string
	allowed     map[string]secretref.Reference
	mu          sync.Mutex
	identities  map[string]secretIdentity
	onSnapshot  func([]byte)
}

type secretIdentity struct {
	uid       string
	keyDigest [sha256.Size]byte
}

type authorizedSecret struct {
	mu       sync.Mutex
	snapshot []byte
}

func (s *authorizedSecret) Resolve() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := append([]byte(nil), s.snapshot...)
	clear(s.snapshot)
	s.snapshot = nil
	return value
}

func (s *authorizedSecret) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.snapshot)
	s.snapshot = nil
}

func NewWebhookSecretAuthorizer(client kubernetes.Interface, namespace, releaseName string, references []string) (*WebhookSecretAuthorizer, error) {
	namespace = strings.TrimSpace(namespace)
	releaseName = strings.TrimSpace(releaseName)
	if client == nil || namespace == "" {
		return nil, errors.New("webhook Secret authorizer client and namespace are required")
	}
	allowed := make(map[string]secretref.Reference, len(references))
	for _, value := range references {
		reference, err := secretref.Parse(value)
		if err != nil || reference.Namespace != namespace || !strings.HasPrefix(reference.Name, "cyclopus-webhook-") {
			return nil, fmt.Errorf("invalid allowed webhook Secret reference %q", value)
		}
		if _, exists := allowed[value]; exists {
			return nil, fmt.Errorf("duplicate allowed webhook Secret reference %q", value)
		}
		allowed[value] = reference
	}
	if len(allowed) > 0 && releaseName == "" {
		return nil, errors.New("webhook Secret release ownership is required")
	}
	return &WebhookSecretAuthorizer{client: client, namespace: namespace, releaseName: releaseName, allowed: allowed, identities: make(map[string]secretIdentity)}, nil
}

func (a *WebhookSecretAuthorizer) Authorize(_ context.Context, namespace, name string) error {
	for _, reference := range a.allowed {
		if reference.Namespace == namespace && reference.Name == name {
			return nil
		}
	}
	return fmt.Errorf("webhook Secret %s/%s is not allowed", namespace, name)
}

func (a *WebhookSecretAuthorizer) Validate(ctx context.Context, value string) error {
	authorized, err := a.AuthorizeReference(ctx, value)
	if err == nil {
		defer authorized.Cleanup()
		valueBytes := authorized.Resolve()
		clear(valueBytes)
	}
	return err
}

func (a *WebhookSecretAuthorizer) AuthorizeReference(ctx context.Context, value string) (webhook.AuthorizedSecret, error) {
	reference, allowed := a.allowed[value]
	if !allowed {
		return nil, errors.New("webhook Secret reference is not allowed")
	}
	secret, err := a.client.CoreV1().Secrets(reference.Namespace).Get(ctx, reference.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read allowed webhook Secret metadata: %w", err)
	}
	if secret.UID == "" || secret.Immutable == nil || !*secret.Immutable || secret.Labels["cyclopus.app/credential-purpose"] != "webhook-signing" ||
		secret.Labels["app.kubernetes.io/managed-by"] != "Helm" || secret.Labels["app.kubernetes.io/instance"] != a.releaseName {
		return nil, errors.New("webhook signing Secret does not satisfy immutable release ownership metadata")
	}
	valueBytes, exists := secret.Data[reference.Key]
	if !exists || len(valueBytes) == 0 {
		return nil, errors.New("webhook signing Secret key is missing or empty")
	}

	identity := secretIdentity{uid: string(secret.UID), keyDigest: sha256.Sum256(valueBytes)}
	a.mu.Lock()
	known, seen := a.identities[value]
	if !seen {
		a.identities[value] = identity
	}
	a.mu.Unlock()
	if seen && known != identity {
		return nil, errors.New("webhook signing Secret identity changed")
	}

	snapshot := append([]byte(nil), valueBytes...)
	if a.onSnapshot != nil {
		a.onSnapshot(snapshot)
	}
	return &authorizedSecret{snapshot: snapshot}, nil
}
