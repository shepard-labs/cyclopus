package secretref

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestParse(t *testing.T) {
	parsed, err := Parse("k8s-secret://tenant/runtime#DATABASE_URL")
	if err != nil || parsed.Namespace != "tenant" || parsed.Name != "runtime" || parsed.Key != "DATABASE_URL" {
		t.Fatalf("Parse() = %+v, %v", parsed, err)
	}
	for _, value := range []string{
		"", "tenant/runtime#key", "k8s-secret://tenant/runtime", "k8s-secret:///runtime#key",
		"k8s-secret://tenant/#key", "k8s-secret://tenant/runtime#", "k8s-secret://tenant/extra/runtime#key",
		"k8s-secret://tenant/runtime#key#extra", " k8s-secret://tenant/runtime#key",
		"k8s-secret://Tenant/runtime#key", "k8s-secret://tenant/runtime/name#key", "k8s-secret://tenant/runtime#bad/key",
	} {
		if _, err := Parse(value); err == nil {
			t.Fatalf("Parse(%q) succeeded", value)
		}
	}
}

func TestKubernetesManagedWriterCreatesAndDeterministicallyUpdatesGitHubAppSecret(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset()
	writer, err := NewKubernetesManagedWriter(client, "cyclopus")
	if err != nil {
		t.Fatal(err)
	}
	input := ManagedSecret{Data: map[string][]byte{GitHubPrivateKeyKey: []byte("private-key"), GitHubWebhookKey: []byte("01234567890123456789")}}
	version, err := writer.WriteGitHubApp(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := client.CoreV1().Secrets("cyclopus").Get(context.Background(), GitHubAppSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if secret.Type != "Opaque" || secret.Labels["app.kubernetes.io/managed-by"] != "cyclopus" || secret.Annotations["cyclopus.app/credential-purpose"] != "github-app" {
		t.Fatalf("managed Secret metadata = type:%q labels:%v annotations:%v", secret.Type, secret.Labels, secret.Annotations)
	}
	input.Data[GitHubPrivateKeyKey][0] = 'X'
	if bytes.Equal(secret.Data[GitHubPrivateKeyKey], input.Data[GitHubPrivateKeyKey]) {
		t.Fatal("managed Secret retained caller-owned secret bytes")
	}
	updated := ManagedSecret{ResourceVersion: version, Data: map[string][]byte{GitHubPrivateKeyKey: []byte("rotated-key"), GitHubWebhookKey: []byte("abcdefghijklmnopqrst")}}
	if _, err := writer.WriteGitHubApp(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	read, err := writer.ReadGitHubApp(context.Background())
	if err != nil || string(read.Data[GitHubPrivateKeyKey]) != "rotated-key" || len(read.Data) != 2 {
		t.Fatalf("ReadGitHubApp() = %+v, %v", read, err)
	}
}

func TestKubernetesManagedWriterRejectsInvalidAndStaleSecretsWithoutLeakingBytes(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset()
	writer, err := NewKubernetesManagedWriter(client, "cyclopus")
	if err != nil {
		t.Fatal(err)
	}
	private := "SUPER-SECRET-PRIVATE-KEY"
	for _, data := range []map[string][]byte{
		{GitHubPrivateKeyKey: []byte(private)},
		{GitHubPrivateKeyKey: []byte(private), GitHubWebhookKey: []byte("short")},
		{GitHubPrivateKeyKey: []byte(private), GitHubWebhookKey: []byte("01234567890123456789"), "extra": []byte("x")},
	} {
		if _, err := writer.WriteGitHubApp(context.Background(), ManagedSecret{Data: data}); err == nil || strings.Contains(err.Error(), private) {
			t.Fatalf("WriteGitHubApp() error = %v, want redacted validation failure", err)
		}
	}
	valid := ManagedSecret{Data: map[string][]byte{GitHubPrivateKeyKey: []byte(private), GitHubWebhookKey: []byte("01234567890123456789")}}
	if _, err := writer.WriteGitHubApp(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteGitHubApp(context.Background(), ManagedSecret{ResourceVersion: "stale", Data: valid.Data}); err == nil || !strings.Contains(err.Error(), "stale") || strings.Contains(err.Error(), private) {
		t.Fatalf("stale write error = %v", err)
	}
}

func TestKubernetesManagedWriterPreservesHelmMetadataAndUnmanagedData(t *testing.T) {
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: GitHubAppSecretName, Namespace: "cyclopus", ResourceVersion: "7",
		Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm", "unrelated-label": "keep"},
		Annotations: map[string]string{
			"meta.helm.sh/release-name": "cyclopus", "meta.helm.sh/release-namespace": "cyclopus", "unrelated-annotation": "keep",
		},
	}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{
		GitHubPrivateKeyKey: []byte("old-key"), GitHubWebhookKey: []byte("old-webhook-secret-value"), "unmanaged": []byte("keep"),
	}}
	client := kubernetesfake.NewSimpleClientset(existing)
	writer, err := NewKubernetesManagedWriter(client, "cyclopus")
	if err != nil {
		t.Fatal(err)
	}
	value := ManagedSecret{ResourceVersion: "7", Data: map[string][]byte{
		GitHubPrivateKeyKey: []byte("new-key"), GitHubWebhookKey: []byte("new-webhook-secret-value"),
	}}
	if _, err := writer.WriteGitHubApp(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	updated, err := client.CoreV1().Secrets("cyclopus").Get(context.Background(), GitHubAppSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ResourceVersion != "7" || updated.Labels["app.kubernetes.io/managed-by"] != "Helm" ||
		updated.Labels["unrelated-label"] != "keep" || updated.Labels["cyclopus.app/managed-credential"] != "true" ||
		updated.Annotations["meta.helm.sh/release-name"] != "cyclopus" || updated.Annotations["meta.helm.sh/release-namespace"] != "cyclopus" ||
		updated.Annotations["unrelated-annotation"] != "keep" || updated.Annotations["cyclopus.app/credential-purpose"] != "github-app" ||
		string(updated.Data["unmanaged"]) != "keep" || string(updated.Data[GitHubPrivateKeyKey]) != "new-key" {
		t.Fatalf("updated Secret did not preserve ownership metadata and unmanaged data: metadata=%+v data keys=%d", updated.ObjectMeta, len(updated.Data))
	}
	read, err := writer.ReadGitHubApp(context.Background())
	if err != nil || len(read.Data) != 2 || string(read.Data[GitHubPrivateKeyKey]) != "new-key" {
		t.Fatalf("ReadGitHubApp() exposed unmanaged data or missed credentials: keys=%d err=%v", len(read.Data), err)
	}

	client.PrependReactor("update", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, GitHubAppSecretName, nil)
	})
	value.ResourceVersion = updated.ResourceVersion
	if _, err := writer.WriteGitHubApp(context.Background(), value); err == nil || !apierrors.IsConflict(err) {
		t.Fatalf("conflicting WriteGitHubApp() error = %v", err)
	}
	afterConflict, err := client.CoreV1().Secrets("cyclopus").Get(context.Background(), GitHubAppSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if afterConflict.Labels["app.kubernetes.io/managed-by"] != "Helm" || afterConflict.Annotations["meta.helm.sh/release-name"] != "cyclopus" {
		t.Fatalf("conflict changed Helm metadata: %+v", afterConflict.ObjectMeta)
	}
}
