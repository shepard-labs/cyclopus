package secretref

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type Resolver interface {
	Resolve(context.Context, string) ([]byte, error)
}

type KubernetesResolver struct{ client kubernetes.Interface }

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
