package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/shepard-labs/cyclopus/internal/secretref"
)

const WebhookSecretReaderRoleName = "cyclopus-webhook-secret-reader"

type WebhookSecretAuthorizer struct {
	client      kubernetes.Interface
	namespace   string
	releaseName string
	allowed     map[string]secretref.Reference
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
	return &WebhookSecretAuthorizer{client: client, namespace: namespace, releaseName: releaseName, allowed: allowed}, nil
}

func (a *WebhookSecretAuthorizer) Authorize(_ context.Context, namespace, name string) error {
	for _, reference := range a.allowed {
		if reference.Namespace == namespace && reference.Name == name {
			return nil
		}
	}
	return fmt.Errorf("webhook Secret %s/%s is not allowed", namespace, name)
}

func (a *WebhookSecretAuthorizer) AuthorizeReference(_ context.Context, value string) error {
	if _, allowed := a.allowed[value]; !allowed {
		return errors.New("webhook Secret reference is not allowed")
	}
	return nil
}

func (a *WebhookSecretAuthorizer) Validate(ctx context.Context, value string) error {
	reference, allowed := a.allowed[value]
	if !allowed {
		return errors.New("webhook Secret reference is not allowed")
	}
	secret, err := a.client.CoreV1().Secrets(reference.Namespace).Get(ctx, reference.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read allowed webhook Secret metadata: %w", err)
	}
	if secret.Immutable == nil || !*secret.Immutable || secret.Labels["cyclopus.app/credential-purpose"] != "webhook-signing" ||
		secret.Labels["app.kubernetes.io/managed-by"] != "Helm" || secret.Labels["app.kubernetes.io/instance"] != a.releaseName {
		return errors.New("webhook signing Secret does not satisfy immutable release ownership metadata")
	}
	if valueBytes, exists := secret.Data[reference.Key]; !exists || len(valueBytes) == 0 {
		return errors.New("webhook signing Secret key is missing or empty")
	}
	return nil
}
