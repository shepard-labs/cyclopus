package secretref

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type Resolver interface {
	Resolve(context.Context, string) ([]byte, error)
}

type KubernetesResolver struct{ client kubernetes.Interface }

const (
	GitHubAppSecretName = "cyclopus-github-app"
	GitHubPrivateKeyKey = "private-key.pem"
	GitHubWebhookKey    = "webhook-secret"
)

type ManagedSecret struct {
	Data            map[string][]byte
	ResourceVersion string
}

type ManagedWriter interface {
	ReadGitHubApp(context.Context) (ManagedSecret, error)
	WriteGitHubApp(context.Context, ManagedSecret) (string, error)
}

type KubernetesManagedWriter struct {
	client    kubernetes.Interface
	namespace string
}

func NewKubernetesManagedWriter(client kubernetes.Interface, namespace string) (*KubernetesManagedWriter, error) {
	namespace = strings.TrimSpace(namespace)
	if client == nil || namespace == "" {
		return nil, fmt.Errorf("managed Secret client and namespace are required")
	}
	return &KubernetesManagedWriter{client: client, namespace: namespace}, nil
}

func (w *KubernetesManagedWriter) ReadGitHubApp(ctx context.Context) (ManagedSecret, error) {
	secret, err := w.client.CoreV1().Secrets(w.namespace).Get(ctx, GitHubAppSecretName, metav1.GetOptions{})
	if err != nil {
		return ManagedSecret{}, fmt.Errorf("read managed GitHub App Secret: %w", err)
	}
	return ManagedSecret{Data: managedGitHubAppData(secret.Data), ResourceVersion: secret.ResourceVersion}, nil
}

func (w *KubernetesManagedWriter) WriteGitHubApp(ctx context.Context, value ManagedSecret) (string, error) {
	if err := ValidateGitHubAppSecret(value.Data); err != nil {
		return "", err
	}
	secrets := w.client.CoreV1().Secrets(w.namespace)
	current, err := secrets.Get(ctx, GitHubAppSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if value.ResourceVersion != "" {
			return "", fmt.Errorf("managed GitHub App Secret resource version is stale")
		}
		created, createErr := secrets.Create(ctx, managedGitHubAppSecret(w.namespace, value.Data), metav1.CreateOptions{})
		if createErr != nil {
			return "", fmt.Errorf("create managed GitHub App Secret: %w", createErr)
		}
		return created.ResourceVersion, nil
	}
	if err != nil {
		return "", fmt.Errorf("read managed GitHub App Secret before update: %w", err)
	}
	if value.ResourceVersion != "" && value.ResourceVersion != current.ResourceVersion {
		return "", fmt.Errorf("managed GitHub App Secret resource version is stale")
	}
	current.Type = corev1.SecretTypeOpaque
	current.Data = mergeGitHubAppData(current.Data, value.Data)
	current.Labels = mergeManagedGitHubLabels(current.Labels)
	current.Annotations = mergeManagedGitHubAnnotations(current.Annotations)
	updated, err := secrets.Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		return "", fmt.Errorf("update managed GitHub App Secret: %w", err)
	}
	return updated.ResourceVersion, nil
}

func managedGitHubAppSecret(namespace string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: GitHubAppSecretName, Namespace: namespace,
		Labels: mergeManagedGitHubLabels(nil), Annotations: mergeManagedGitHubAnnotations(nil)},
		Type: corev1.SecretTypeOpaque, Data: cloneSecretData(data)}
}

func mergeManagedGitHubLabels(existing map[string]string) map[string]string {
	labels := cloneStringMap(existing)
	if _, ownedByHelm := labels["app.kubernetes.io/managed-by"]; !ownedByHelm {
		labels["app.kubernetes.io/managed-by"] = "cyclopus"
	}
	labels["cyclopus.app/managed-credential"] = "true"
	return labels
}

func mergeManagedGitHubAnnotations(existing map[string]string) map[string]string {
	annotations := cloneStringMap(existing)
	annotations["cyclopus.app/credential-purpose"] = "github-app"
	return annotations
}

func mergeGitHubAppData(existing, managed map[string][]byte) map[string][]byte {
	data := cloneSecretData(existing)
	data[GitHubPrivateKeyKey] = append([]byte(nil), managed[GitHubPrivateKeyKey]...)
	data[GitHubWebhookKey] = append([]byte(nil), managed[GitHubWebhookKey]...)
	return data
}

func managedGitHubAppData(data map[string][]byte) map[string][]byte {
	return map[string][]byte{
		GitHubPrivateKeyKey: append([]byte(nil), data[GitHubPrivateKeyKey]...),
		GitHubWebhookKey:    append([]byte(nil), data[GitHubWebhookKey]...),
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values)+1)
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func ValidateGitHubAppSecret(data map[string][]byte) error {
	if len(data) != 2 || len(data[GitHubPrivateKeyKey]) == 0 || len(data[GitHubWebhookKey]) < 20 {
		return fmt.Errorf("managed GitHub App Secret must contain a private key and webhook secret")
	}
	if len(data[GitHubPrivateKeyKey]) > 64<<10 || len(data[GitHubWebhookKey]) > 4<<10 {
		return fmt.Errorf("managed GitHub App Secret value exceeds size limit")
	}
	return nil
}

func cloneSecretData(data map[string][]byte) map[string][]byte {
	cloned := make(map[string][]byte, len(data))
	for key, value := range data {
		cloned[key] = append([]byte(nil), value...)
	}
	return cloned
}

func NewKubernetesResolver(client kubernetes.Interface) *KubernetesResolver {
	return &KubernetesResolver{client: client}
}

func (r *KubernetesResolver) Resolve(ctx context.Context, value string) ([]byte, error) {
	reference, err := Parse(value)
	if err != nil {
		return nil, err
	}
	secret, err := r.client.CoreV1().Secrets(reference.Namespace).Get(ctx, reference.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read Secret %s/%s: %w", reference.Namespace, reference.Name, err)
	}
	resolved, ok := secret.Data[reference.Key]
	if !ok {
		return nil, fmt.Errorf("Secret %s/%s has no key %s", reference.Namespace, reference.Name, reference.Key)
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("Secret %s/%s key %s is empty", reference.Namespace, reference.Name, reference.Key)
	}
	return append([]byte(nil), resolved...), nil
}
