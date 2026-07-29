//go:build k3s_integration

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cyclopus-dev/cyclopus/internal/store"
	"github.com/cyclopus-dev/cyclopus/internal/worker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestRecoveryWorkerK3sPostgresIntegration(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("KUBECONFIG is required")
	}
	clientConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	client, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	dataStore, pool := newK3sPostgresStore(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	namespace := fmt.Sprintf("cyclopus-recovery-worker-%d", time.Now().UnixNano())
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = client.CoreV1().Namespaces().Delete(cleanupCtx, namespace, metav1.DeleteOptions{})
	})

	const (
		orgID              = "10000000-0000-0000-0000-000000000001"
		projectID          = "10000000-0000-0000-0000-000000000002"
		appID              = "10000000-0000-0000-0000-000000000003"
		environmentID      = "10000000-0000-0000-0000-000000000004"
		previousID         = "10000000-0000-0000-0000-000000000005"
		failedID           = "10000000-0000-0000-0000-000000000006"
		repositoryID       = "10000000-0000-0000-0000-000000000007"
		previousRevisionID = "10000000-0000-0000-0000-000000000008"
		failedRevisionID   = "10000000-0000-0000-0000-000000000009"
	)
	seedK3sRecoveryScope(t, pool, orgID, projectID, appID, environmentID, repositoryID, namespace)

	immutable := true
	secretName := "runtime-r1"
	previousSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace}, Immutable: &immutable,
		Data: map[string][]byte{"DATABASE_URL": []byte("postgres://previous")}}
	replicas := int32(1)
	previous := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace, Labels: map[string]string{
			ManagedByLabel: ManagedByValue, EnvironmentIDLabel: environmentID,
		}},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "recovery-worker"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "recovery-worker"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry.k8s.io/pause:3.10",
					EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}}}},
				}}}},
		},
	}
	previousEnvelope, previousSnapshot, previousHash := k3sEnvReleaseSnapshot(t, previous, previousRevisionID, namespace, secretName)
	previousSecret.Annotations = map[string]string{EnvRevisionAnnotation: previousRevisionID, ConfigHashAnnotation: previousHash}
	if _, err := client.CoreV1().Secrets(namespace).Create(ctx, previousSecret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create previous Secret revision: %v", err)
	}
	previous.Annotations = map[string]string{ReleaseAnnotation: previousID, ConfigHashAnnotation: previousHash}
	previous.Spec.Template.Annotations = map[string]string{ReleaseAnnotation: previousID, ConfigHashAnnotation: previousHash}
	created, err := client.AppsV1().Deployments(namespace).Create(ctx, previous, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	_ = waitForK3sDeploymentReady(t, ctx, client, namespace, created.Name)

	failed := previous.DeepCopy()
	failed.Spec.Template.Spec.Containers[0].Image = "registry.invalid/cyclopus/recovery:not-found"
	failed.Spec.Template.Spec.Containers[0].EnvFrom[0].SecretRef.Name = "runtime-r2"
	failed.Annotations[ReleaseAnnotation] = failedID
	failed.Spec.Template.Annotations[ReleaseAnnotation] = failedID
	failedEnvelope := previousEnvelope
	failedEnvelope.EnvVarRevision = &store.EnvVarRevision{Version: store.EnvVarRevisionVersion, ID: failedRevisionID, Revision: 2, Variables: []store.ReleaseEnvironment{{
		Key: "DATABASE_URL", SecretRef: "k8s-secret://" + namespace + "/runtime-r2#DATABASE_URL", IsSecret: true,
	}}}
	failedEnvelope.EnvVarRevision.Hash = store.ComputeEnvVarRevisionHash(failedEnvelope.EnvVarRevision.Variables)
	failedEnvelope.DesiredState.Deployment.Container.Image = failed.Spec.Template.Spec.Containers[0].Image
	failedEnvelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{"runtime-r2"}
	failedEnvelope.DesiredState.Deployment.PodAnnotations[EnvRevisionAnnotation] = failedRevisionID
	failedEnvelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = failedEnvelope.EnvVarRevision.Hash
	failedSnapshot, failedHash, err := store.EncodeReleaseSnapshot(failedEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	failed.Annotations[ConfigHashAnnotation] = failedHash
	failed.Spec.Template.Annotations[ConfigHashAnnotation] = failedHash
	failedSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-r2", Namespace: namespace, Annotations: map[string]string{
		EnvRevisionAnnotation: failedRevisionID, ConfigHashAnnotation: failedHash,
	}}, Immutable: &immutable, Data: map[string][]byte{"DATABASE_URL": []byte("postgres://failed")}}
	if _, err := client.CoreV1().Secrets(namespace).Create(ctx, failedSecret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create failed Secret revision: %v", err)
	}
	current, err := client.AppsV1().Deployments(namespace).Get(ctx, failed.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("refresh deployment before failed update: %v", err)
	}
	failed.ResourceVersion = current.ResourceVersion
	if _, err := client.AppsV1().Deployments(namespace).Update(ctx, failed, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("install failed deployment: %v", err)
	}
	seedK3sRecoveryReleases(t, pool, orgID, environmentID, repositoryID, previousID, failedID, previousRevisionID, failedRevisionID,
		previousSnapshot, previousHash, failedSnapshot, failedHash)
	lease, err := dataStore.AcquireDeployLease(ctx, orgID, environmentID, failedID)
	if err != nil {
		t.Fatal(err)
	}
	if err := dataStore.SetPendingCommitSHA(ctx, lease, fmt.Sprintf("%040d", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET lock_acquired_at = now() - interval '3 seconds'
		WHERE id = $1`, environmentID); err != nil {
		t.Fatal(err)
	}

	adapter, err := NewRecoveryAdapter(Config{PollInterval: 500 * time.Millisecond, ConflictRetries: 5}, client, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	runner := worker.New(worker.Config{ScanInterval: time.Second, LeaseDuration: time.Minute, RequestTimeout: time.Second, Owner: "k3s-worker"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, worker.RecoveryAdapters{
			Capabilities: worker.RecoveryCapabilities{Kubernetes: true}, Kubernetes: adapter, KubernetesErrorDisposition: RecoveryErrorDisposition,
		})
	if err := runner.Startup(ctx); err != nil {
		t.Fatalf("worker startup recovery: %v", err)
	}
	recovered := waitForK3sDeploymentReady(t, ctx, client, namespace, "app")
	if recovered.Spec.Template.Spec.Containers[0].Image != "registry.k8s.io/pause:3.10" ||
		recovered.Annotations[ReleaseAnnotation] != previousID || recovered.Annotations[ConfigHashAnnotation] != previousHash {
		t.Fatalf("recovered deployment identity = image:%s annotations:%v", recovered.Spec.Template.Spec.Containers[0].Image, recovered.Annotations)
	}
	var releaseStatus, auditAction, queuedStatus, queuedCommit, currentRevision string
	var inProgress *string
	if err := pool.QueryRow(ctx, `SELECT failed.status, audit.action, environment.in_progress_release_id::text,
		job.status, job.commit_sha, environment.current_env_var_revision_id::text FROM releases failed
		JOIN environments environment ON environment.id = failed.environment_id
		JOIN audit_events audit ON audit.target_id = failed.id
		JOIN orchestration_jobs job ON job.environment_id = environment.id AND job.status = 'queued'
		WHERE failed.id = $1`, failedID).Scan(&releaseStatus, &auditAction, &inProgress, &queuedStatus, &queuedCommit, &currentRevision); err != nil {
		t.Fatal(err)
	}
	if releaseStatus != "failed" || auditAction != "release.recovery_finalized_failed" || inProgress != nil ||
		queuedStatus != "queued" || queuedCommit != fmt.Sprintf("%040d", 2) || currentRevision != previousRevisionID {
		t.Fatalf("persisted recovery = release:%s audit:%s lease:%v queued:%s/%s revision:%s", releaseStatus, auditAction, inProgress, queuedStatus, queuedCommit, currentRevision)
	}
}

func TestRecoveryWorkerUsesRenewedFenceForErrorPersistence(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
		code      string
	}{
		{name: "transient reschedule", err: fmt.Errorf("temporary Kubernetes failure"), retryable: true},
		{name: "permanent quarantine", err: fmt.Errorf("invalid recovery state"), code: "recovery_state_invalid"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataStore, pool := newK3sPostgresStore(t)
			ctx := context.Background()
			suffix := fmt.Sprintf("%012d", index+101)
			orgID := "20000000-0000-0000-0000-" + suffix
			projectID := "20000000-0000-0000-0001-" + suffix
			appID := "20000000-0000-0000-0002-" + suffix
			environmentID := "20000000-0000-0000-0003-" + suffix
			releaseID := "20000000-0000-0000-0004-" + suffix
			repositoryID := "20000000-0000-0000-0005-" + suffix
			seedK3sRecoveryScope(t, pool, orgID, projectID, appID, environmentID, repositoryID, "recovery-fence")
			deployment := testDeployment("registry.example.test/app@sha256:" + strings.Repeat("a", 64))
			deployment.Namespace = "recovery-fence"
			snapshot, err := EncodeDeploymentSnapshot(deployment)
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := store.DecodeReleaseSnapshot(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			_, snapshotHash, err := store.EncodeReleaseSnapshot(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO releases
					(id, org_id, environment_id, image_ref, kind, config_snapshot, config_hash, status)
					VALUES ($1, $2, $3, $4, 'deploy', $5, $6, 'deploying')`,
				releaseID, orgID, environmentID, deployment.Spec.Template.Spec.Containers[0].Image, snapshot, snapshotHash); err != nil {
				t.Fatal(err)
			}
			claimed, err := dataStore.AcquireDeployLease(ctx, orgID, environmentID, releaseID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE environments SET lock_acquired_at = now() - interval '3 seconds' WHERE id = $1`, environmentID); err != nil {
				t.Fatal(err)
			}
			adapter := &renewingFailureAdapter{store: dataStore, err: test.err}
			runner := worker.New(worker.Config{ScanInterval: time.Second, LeaseDuration: time.Minute, RequestTimeout: time.Second, Owner: "fence-worker"},
				slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, worker.RecoveryAdapters{
					Capabilities: worker.RecoveryCapabilities{Kubernetes: true}, Kubernetes: adapter,
					KubernetesErrorDisposition: func(error) (bool, time.Duration, string) { return test.retryable, 5 * time.Second, test.code },
				})
			if err := runner.Startup(ctx); err != nil {
				t.Fatalf("worker recovery: %v", err)
			}
			if adapter.renewed.Fence.IsZero() || adapter.renewed.Fence.Equal(claimed.Fence) {
				t.Fatalf("adapter renewed fence = %v, original = %v", adapter.renewed.Fence, claimed.Fence)
			}
			var persistedFence time.Time
			var code *string
			if err := pool.QueryRow(ctx, `SELECT lock_acquired_at, deploy_recovery_error_code FROM environments WHERE id = $1`, environmentID).Scan(&persistedFence, &code); err != nil {
				t.Fatal(err)
			}
			if test.retryable {
				if !persistedFence.After(adapter.renewed.Fence) || code != nil {
					t.Fatalf("reschedule fence/code = %v/%v, renewed = %v", persistedFence, code, adapter.renewed.Fence)
				}
			} else if !persistedFence.Equal(adapter.renewed.Fence) || code == nil || *code != test.code {
				t.Fatalf("quarantine fence/code = %v/%v, renewed = %v", persistedFence, code, adapter.renewed.Fence)
			}
		})
	}
}

type renewingFailureAdapter struct {
	store   *store.Store
	err     error
	renewed store.DeployLease
}

func (a *renewingFailureAdapter) RecoverExpiredDeploy(ctx context.Context, lease store.DeployLease) (store.DeployRecoveryResult, error) {
	renewed, err := a.store.RenewDeployLease(ctx, lease)
	if err != nil {
		return store.DeployRecoveryResult{Lease: lease}, err
	}
	a.renewed = renewed
	return store.DeployRecoveryResult{Lease: renewed}, a.err
}

func k3sEnvReleaseSnapshot(t *testing.T, deployment *appsv1.Deployment, revisionID, namespace, secretName string) (store.ReleaseSnapshotEnvelope, []byte, string) {
	t.Helper()
	encoded, err := EncodeDeploymentSnapshot(deployment)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := store.DecodeReleaseSnapshot(encoded)
	if err != nil {
		t.Fatal(err)
	}
	envelope.EnvVarRevision = &store.EnvVarRevision{Version: store.EnvVarRevisionVersion, ID: revisionID, Revision: 1, Variables: []store.ReleaseEnvironment{{
		Key: "DATABASE_URL", SecretRef: "k8s-secret://" + namespace + "/" + secretName + "#DATABASE_URL", IsSecret: true,
	}}}
	envelope.EnvVarRevision.Hash = store.ComputeEnvVarRevisionHash(envelope.EnvVarRevision.Variables)
	envelope.DesiredState.Deployment.PodAnnotations[EnvRevisionAnnotation] = revisionID
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = envelope.EnvVarRevision.Hash
	snapshot, hash, err := store.EncodeReleaseSnapshot(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return envelope, snapshot, hash
}

func newK3sPostgresStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("CYCLOPUS_TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("POSTGRES_TEST_URL")
	}
	if databaseURL == "" {
		t.Fatal("CYCLOPUS_TEST_DATABASE_URL or POSTGRES_TEST_URL is required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("k3s_recovery_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_ = admin.Close(context.Background())
	})
	testURL := k3sSearchPathURL(t, databaseURL, schema)
	if err := store.Migrate(ctx, store.MigrationConfig{URL: testURL, LockTimeout: 5 * time.Second, Migrations: store.Migrations}); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool), pool
}

func k3sSearchPathURL(t *testing.T, databaseURL, schema string) string {
	t.Helper()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(config.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", pgx.Identifier{schema}.Sanitize())
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func seedK3sRecoveryScope(t *testing.T, pool *pgxpool.Pool, orgID, projectID, appID, environmentID, repositoryID, namespace string) {
	t.Helper()
	ctx := context.Background()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO orgs (id, name, slug) VALUES ($1, 'Recovery Org', 'recovery-org')`, []any{orgID}},
		{`INSERT INTO git_connections (id, org_id, installation_id, repo_full_name, webhook_secret_ref)
			VALUES ($1, $2, 1, 'cyclopus/recovery', 'k8s-secret://cyclopus/github#webhook')`, []any{repositoryID, orgID}},
		{`INSERT INTO projects (id, org_id, name, slug, git_connection_id) VALUES ($1, $2, 'Recovery', 'recovery', $3)`, []any{projectID, orgID, repositoryID}},
		{`INSERT INTO apps (id, org_id, project_id, name, slug, hostname_label, deploy_timeout_seconds)
			VALUES ($1, $2, $3, 'Recovery', 'recovery', 'recovery-host', 1)`, []any{appID, orgID, projectID}},
		{`INSERT INTO environments (id, org_id, app_id, branch, kind, namespace, planned_hostname)
			VALUES ($1, $2, $3, 'main', 'production', $4, 'recovery.example.test')`, []any{environmentID, orgID, appID, namespace}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func seedK3sRecoveryReleases(t *testing.T, pool *pgxpool.Pool, orgID, environmentID, repositoryID, previousID, failedID, previousRevisionID, failedRevisionID string, previousSnapshot []byte, previousHash string, failedSnapshot []byte, failedHash string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO env_var_revisions (id, org_id, environment_id, revision) VALUES
		($1, $3, $4, 1), ($2, $3, $4, 2)`, previousRevisionID, failedRevisionID, orgID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO env_var_revision_entries (org_id, environment_id, env_var_revision_id, key, secret_ref)
		VALUES ($1, $2, $3, 'DATABASE_URL', 'k8s-secret://' || (SELECT namespace FROM environments WHERE id = $2) || '/runtime-r1#DATABASE_URL'),
		       ($1, $2, $4, 'DATABASE_URL', 'k8s-secret://' || (SELECT namespace FROM environments WHERE id = $2) || '/runtime-r2#DATABASE_URL')`,
		orgID, environmentID, previousRevisionID, failedRevisionID); err != nil {
		t.Fatal(err)
	}
	image := "registry.k8s.io/pause@sha256:" + strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO releases
		(id, org_id, environment_id, image_ref, git_commit_sha, kind, env_var_revision_id, config_snapshot, config_hash, status)
		VALUES ($1, $2, $3, $4, $5, 'env_change', $6, $7, $8, 'live'),
		       ($9, $2, $3, $4, $10, 'env_change', $11, $12, $13, 'deploying')`,
		previousID, orgID, environmentID, image, fmt.Sprintf("%040d", 1), previousRevisionID, previousSnapshot, previousHash,
		failedID, fmt.Sprintf("%040d", 2), failedRevisionID, failedSnapshot, failedHash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET current_env_var_revision_id = $2 WHERE id = $1`, environmentID, failedRevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO orchestration_jobs
		(org_id, repository_id, workflow_id, git_ref, environment_id, commit_sha, idempotency_key, status)
		VALUES ($1, $2, 'deploy.yml', 'refs/heads/main', $3, $4, 'completed-route', 'completed')`,
		orgID, repositoryID, environmentID, fmt.Sprintf("%040d", 1)); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryAdapterK3sIntegration(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("KUBECONFIG is required")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	namespace := fmt.Sprintf("cyclopus-recovery-%d", time.Now().UnixNano())
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = client.CoreV1().Namespaces().Delete(cleanupCtx, namespace, metav1.DeleteOptions{})
	})

	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace, Labels: map[string]string{
			ManagedByLabel: ManagedByValue, EnvironmentIDLabel: "environment",
		}, Annotations: map[string]string{ReleaseAnnotation: "previous", ConfigHashAnnotation: "previous-hash"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "recovery-test"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "recovery-test"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "app", Image: "registry.k8s.io/pause:3.10",
				}}},
			},
		},
	}
	if _, err := client.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	previous := waitForK3sDeploymentReady(t, ctx, client, namespace, deployment.Name)
	previous.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/os": "linux"}
	previous.APIVersion = "apps/v1"
	previous.Kind = "Deployment"
	previousSnapshot, err := EncodeDeploymentSnapshot(previous)
	if err != nil {
		t.Fatalf("encode previous snapshot: %v", err)
	}

	failed := previous.DeepCopy()
	failed.Spec.Replicas = int32Pointer(2)
	failed.Spec.Template.Spec.Containers[0].Image = "registry.invalid/cyclopus/recovery:not-found"
	failed.Annotations[ReleaseAnnotation] = "failed"
	failed.Annotations[ConfigHashAnnotation] = "failed-hash"
	failed, err = client.AppsV1().Deployments(namespace).Update(ctx, failed, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("install failed deployment: %v", err)
	}
	failed.APIVersion = "apps/v1"
	failed.Kind = "Deployment"
	failedSnapshot, err := EncodeDeploymentSnapshot(failed)
	if err != nil {
		t.Fatalf("encode failed snapshot: %v", err)
	}
	if len(previousSnapshot) == 0 || len(failedSnapshot) == 0 {
		t.Fatalf("encoded snapshot lengths = previous:%d failed:%d", len(previousSnapshot), len(failedSnapshot))
	}

	recoveryStore := &k3sRecoveryStore{
		lease: store.DeployLease{
			OrgID: "org", EnvironmentID: "environment", ReleaseID: "release",
			Fence: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Minute), Namespace: namespace, Timeout: 2 * time.Minute,
			FailedRelease:   store.ImmutableReleaseSnapshot{ID: "failed", ConfigHash: "failed-hash", ConfigSnapshot: failedSnapshot},
			PreviousRelease: &store.ImmutableReleaseSnapshot{ID: "previous", ConfigHash: "previous-hash", ConfigSnapshot: previousSnapshot},
		},
	}
	if _, err := DecodeDeploymentSnapshot(recoveryStore.lease.FailedRelease.ConfigSnapshot); err != nil {
		t.Fatalf("decode stored failed snapshot (%d bytes): %v", len(recoveryStore.lease.FailedRelease.ConfigSnapshot), err)
	}
	adapter, err := NewRecoveryAdapter(Config{
		PollInterval: 500 * time.Millisecond, ConflictRetries: 5,
	}, client, recoveryStore)
	if err != nil {
		t.Fatalf("create recovery adapter: %v", err)
	}
	if adapter.store != recoveryStore {
		t.Fatalf("adapter recovery store = %T, want %T", adapter.store, recoveryStore)
	}
	if _, err := adapter.RecoverExpiredDeploy(ctx, recoveryStore.lease); err != nil {
		t.Fatalf("recover expired deploy: %v", err)
	}

	recovered := waitForK3sDeploymentReady(t, ctx, client, namespace, deployment.Name)
	if got := recovered.Spec.Template.Spec.Containers[0].Image; got != "registry.k8s.io/pause:3.10" {
		t.Fatalf("recovered image = %q", got)
	}
	if recovered.Annotations[RecoveryReleaseAnnotation] != recoveryStore.lease.ReleaseID ||
		recovered.Annotations[RecoveryFenceAnnotation] == "" ||
		recovered.Annotations[RecoveryStartedAnnotation] == "" {
		t.Fatalf("recovery annotations = %#v", recovered.Annotations)
	}
	if recovered.Spec.Replicas == nil || *recovered.Spec.Replicas != 2 || recovered.Spec.Template.Spec.NodeSelector["kubernetes.io/os"] != "linux" {
		t.Fatalf("runtime/admission fields were not preserved: %#v", recovered.Spec)
	}
	if recoveryStore.finalizeCalls != 1 || recoveryStore.failureReason == "" {
		t.Fatalf("finalization calls/reason = %d/%q", recoveryStore.finalizeCalls, recoveryStore.failureReason)
	}
}

func waitForK3sDeploymentReady(t *testing.T, ctx context.Context, client kubernetes.Interface, namespace, name string) *appsv1.Deployment {
	t.Helper()
	var ready *appsv1.Deployment
	err := wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		desired := int32(1)
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		if deployment.Generation > 0 && deployment.Status.ObservedGeneration >= deployment.Generation &&
			deployment.Status.ReadyReplicas == desired && deployment.Status.AvailableReplicas == desired &&
			deployment.Status.UpdatedReplicas == desired {
			ready = deployment
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("wait for deployment %s/%s: %v", namespace, name, err)
	}
	return ready
}

type k3sRecoveryStore struct {
	lease         store.DeployLease
	finalizeCalls int
	failureReason string
}

func (s *k3sRecoveryStore) RenewDeployLease(_ context.Context, lease store.DeployLease) (store.DeployLease, error) {
	lease.Fence = lease.Fence.Add(time.Microsecond)
	lease.ExpiresAt = time.Now().Add(time.Minute)
	s.lease = lease
	return lease, nil
}

func (s *k3sRecoveryStore) FinalizeRecoveredReleaseFailed(_ context.Context, _ store.DeployLease, failureReason string) (store.FinalizeReleaseResult, error) {
	s.finalizeCalls++
	s.failureReason = failureReason
	return store.FinalizeReleaseResult{}, nil
}

func (s *k3sRecoveryStore) CheckpointDeployRecovery(_ context.Context, lease store.DeployLease, checkpoint string) error {
	s.lease = lease
	s.lease.RecoveryCheckpoint = checkpoint
	return nil
}

func (s *k3sRecoveryStore) AdoptRecoveredReleaseLive(_ context.Context, _ store.DeployLease) (store.FinalizeReleaseResult, error) {
	return store.FinalizeReleaseResult{}, nil
}
