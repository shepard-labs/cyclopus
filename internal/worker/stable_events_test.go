package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/store"
)

// captureRecords decodes the newline-delimited JSON slog records written to buf.
func captureRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode slog record: %v", err)
		}
		records = append(records, record)
	}
	return records
}

// findEvent returns the first captured record whose "event" attribute matches name.
func findEvent(records []map[string]any, name string) map[string]any {
	for _, record := range records {
		if record["event"] == name {
			return record
		}
	}
	return nil
}

func newBufferRunner(t *testing.T, buf *bytes.Buffer, recoveryStore RecoveryStore, adapters RecoveryAdapters) *Runner {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return New(Config{ScanInterval: time.Second, LeaseDuration: time.Minute, MaxConcurrentDeploys: 4,
		RequestTimeout: time.Second, Owner: "worker-one"}, logger, recoveryStore, adapters)
}

func TestStableEventDeployLeaseSweptEmittedOncePerSweptLease(t *testing.T) {
	var buf bytes.Buffer
	fake := &fakeRecoveryStore{
		deploys:     []store.ExpiredDeploy{{OrgID: "org-one", EnvironmentID: "environment-one"}},
		deployLease: store.DeployLease{OrgID: "org-one", EnvironmentID: "environment-one", ReleaseID: "release-one"},
	}
	runner := newBufferRunner(t, &buf, fake, RecoveryAdapters{
		Capabilities: RecoveryCapabilities{Kubernetes: true},
		Kubernetes:   &fakeKubernetesRecoveryAdapter{},
	})

	if err := runner.Startup(context.Background()); err != nil {
		t.Fatalf("Startup() error = %v", err)
	}

	records := captureRecords(t, &buf)
	var swept []map[string]any
	for _, record := range records {
		if record["event"] == "deploy.lease_swept" {
			swept = append(swept, record)
		}
	}
	if len(swept) != 1 {
		t.Fatalf("deploy.lease_swept count = %d, want exactly 1", len(swept))
	}
	event := swept[0]
	if event["msg"] != "deploy.lease_swept" {
		t.Fatalf("msg = %v, want deploy.lease_swept", event["msg"])
	}
	if event["org_id"] != "org-one" || event["environment_id"] != "environment-one" || event["release_id"] != "release-one" {
		t.Fatalf("fields = %+v, want org/environment/release identity", event)
	}
	if event["startup"] != true {
		t.Fatalf("startup = %v, want true", event["startup"])
	}
}

func TestStableEventPreviewReapedEmittedWhenTeardownRequested(t *testing.T) {
	var buf bytes.Buffer
	fake := &fakeRecoveryStore{
		idlePreviewCandidates: []store.IdlePreviewCandidate{{OrgID: "org-one", EnvironmentID: "environment-one"}},
		idlePreviewReaped:     true,
	}
	runner := newBufferRunner(t, &buf, fake, RecoveryAdapters{})

	if err := runner.scan(context.Background(), false); err != nil {
		t.Fatalf("scan() error = %v", err)
	}

	event := findEvent(captureRecords(t, &buf), "preview.reaped")
	if event == nil {
		t.Fatal("preview.reaped event not emitted")
	}
	if event["msg"] != "preview.reaped" {
		t.Fatalf("msg = %v, want preview.reaped", event["msg"])
	}
	if event["org_id"] != "org-one" || event["environment_id"] != "environment-one" {
		t.Fatalf("fields = %+v, want org/environment identity", event)
	}
}

func TestStableEventPreviewReapedNotEmittedWhenTeardownNotRequested(t *testing.T) {
	var buf bytes.Buffer
	fake := &fakeRecoveryStore{
		idlePreviewCandidates: []store.IdlePreviewCandidate{{OrgID: "org-one", EnvironmentID: "environment-one"}},
		idlePreviewReaped:     false,
	}
	runner := newBufferRunner(t, &buf, fake, RecoveryAdapters{})

	if err := runner.scan(context.Background(), false); err != nil {
		t.Fatalf("scan() error = %v", err)
	}

	if event := findEvent(captureRecords(t, &buf), "preview.reaped"); event != nil {
		t.Fatalf("preview.reaped emitted for a non-reaped candidate: %+v", event)
	}
}
