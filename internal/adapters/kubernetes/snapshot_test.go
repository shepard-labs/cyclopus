package kubernetes

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestDeploymentSnapshotRoundTripStripsServerState(t *testing.T) {
	deployment := testDeployment("old-image")
	deployment.UID = types.UID("uid")
	deployment.ResourceVersion = "42"
	deployment.Generation = 7
	deployment.CreationTimestamp = metav1.NewTime(time.Now())
	deployment.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "controller"}}
	deployment.Status.ReadyReplicas = 1

	encoded, err := EncodeDeploymentSnapshot(deployment)
	if err != nil {
		t.Fatalf("EncodeDeploymentSnapshot() error = %v", err)
	}
	decoded, err := DecodeDeploymentSnapshot(encoded)
	if err != nil {
		t.Fatalf("DecodeDeploymentSnapshot() error = %v", err)
	}
	if decoded.ResourceVersion != "" || decoded.UID != "" || decoded.Generation != 0 || !decoded.CreationTimestamp.IsZero() || len(decoded.ManagedFields) != 0 {
		t.Fatalf("server metadata survived snapshot: %#v", decoded.ObjectMeta)
	}
	if decoded.Status.ReadyReplicas != 0 || decoded.Spec.Template.Spec.Containers[0].Image != "old-image" {
		t.Fatalf("decoded deployment = %#v", decoded)
	}
}

func TestDecodeDeploymentSnapshotRejectsUnknownVersionAndFields(t *testing.T) {
	for _, encoded := range []string{
		`{"version":2,"deployment":{}}`,
		`{"version":1,"deployment":{},"future":true}`,
		`{"version":1,"deployment":{}} trailing`,
	} {
		if _, err := DecodeDeploymentSnapshot([]byte(encoded)); err == nil {
			t.Fatalf("DecodeDeploymentSnapshot(%q) succeeded", encoded)
		}
	}
}

func TestDecodeDeploymentSnapshotRejectsServerMetadata(t *testing.T) {
	deployment := testDeployment("old-image")
	encoded, err := EncodeDeploymentSnapshot(deployment)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(encoded), `"name":"app"`, `"name":"app","resourceVersion":"99"`, 1)
	if mutated == string(encoded) {
		t.Fatal("snapshot did not contain resourceVersion field")
	}
	if _, err := DecodeDeploymentSnapshot([]byte(mutated)); err == nil {
		t.Fatal("DecodeDeploymentSnapshot() accepted server metadata")
	}
}

func testDeployment(image string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "tenant", Labels: map[string]string{
			ManagedByLabel: ManagedByValue, EnvironmentIDLabel: "environment",
		}, Annotations: map[string]string{ReleaseAnnotation: "failed", ConfigHashAnnotation: "failed-hash"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "app"}, Annotations: map[string]string{
					ReleaseAnnotation: "failed", ConfigHashAnnotation: "failed-hash",
				}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}},
			},
		},
	}
}
