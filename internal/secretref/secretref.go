package secretref

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const prefix = "k8s-secret://"

type Reference struct {
	Namespace string
	Name      string
	Key       string
}

func Parse(value string) (Reference, error) {
	if !strings.HasPrefix(value, prefix) || strings.TrimSpace(value) != value {
		return Reference{}, fmt.Errorf("invalid Kubernetes secret reference %q", value)
	}
	path, key, found := strings.Cut(strings.TrimPrefix(value, prefix), "#")
	if !found || len(validation.IsConfigMapKey(key)) != 0 || strings.Contains(key, "#") {
		return Reference{}, fmt.Errorf("invalid Kubernetes secret reference %q", value)
	}
	namespace, name, found := strings.Cut(path, "/")
	if !found || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 || strings.Contains(name, "/") {
		return Reference{}, fmt.Errorf("invalid Kubernetes secret reference %q", value)
	}
	return Reference{Namespace: namespace, Name: name, Key: key}, nil
}
