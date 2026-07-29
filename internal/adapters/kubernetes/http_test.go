package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestHTTPFakeAPIServerRestoresSnapshotAndPreservesMetadata(t *testing.T) {
	previous := testDeployment("old-image")
	failed := testDeployment("bad-image")
	current := failed.DeepCopy()
	current.ResourceVersion = "7"
	current.Generation = 4
	current.Labels = map[string]string{"server-owned": "keep", ManagedByLabel: ManagedByValue, EnvironmentIDLabel: "environment"}
	current.Status = readyStatus(4)

	api := &deploymentAPIServer{deployment: current}
	server := httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	recoveryStore := newFakeRecoveryStore(recoveryState(t, failed, previous))
	adapter, err := NewRecoveryAdapter(Config{PollInterval: time.Nanosecond, ConflictRetries: 3}, client, recoveryStore)
	if err != nil {
		t.Fatal(err)
	}
	adapter.now = func() time.Time { return time.Unix(100, 0) }

	if _, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease); err != nil {
		api.mu.Lock()
		puts := api.puts
		api.mu.Unlock()
		t.Fatalf("RecoverExpiredDeploy() error = %T %v, puts = %d", err, err, puts)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.puts != 1 || api.deployment.Spec.Template.Spec.Containers[0].Image != "old-image" || api.deployment.Labels["server-owned"] != "keep" {
		t.Fatalf("HTTP API state = puts:%d deployment:%#v", api.puts, api.deployment)
	}
}

type deploymentAPIServer struct {
	mu         sync.Mutex
	deployment *appsv1.Deployment
	puts       int
}

func (s *deploymentAPIServer) serveHTTP(response http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	response.Header().Set("Content-Type", "application/json")
	if request.URL.Path != "/apis/apps/v1/namespaces/tenant/deployments/app" {
		http.NotFound(response, request)
		return
	}
	switch request.Method {
	case http.MethodGet:
		_ = json.NewEncoder(response).Encode(s.deployment)
	case http.MethodPut:
		s.puts++
		var candidate appsv1.Deployment
		if err := json.NewDecoder(request.Body).Decode(&candidate); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		candidate.ResourceVersion = fmt.Sprintf("%d", s.puts+7)
		candidate.Status = readyStatus(candidate.Generation)
		s.deployment = candidate.DeepCopy()
		_ = json.NewEncoder(response).Encode(s.deployment)
	default:
		response.WriteHeader(http.StatusMethodNotAllowed)
	}
}
