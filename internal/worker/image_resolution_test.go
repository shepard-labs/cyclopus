package worker

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
)

type imageResolutionWorkerStore struct {
	mu        sync.Mutex
	claim     store.ImageResolutionClaim
	claimed   bool
	completed int
	retried   int
	failed    int
	renewErr  error
	excluded  []string
	repeat    bool
	attempts  int
}

func (*imageResolutionWorkerStore) RecoverExpiredImageResolutionLeases(context.Context, int) (int64, error) {
	return 0, nil
}
func (s *imageResolutionWorkerStore) ListImageResolutionOrgIDs(context.Context, int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed {
		return nil, nil
	}
	return []string{s.claim.Job.OrgID}, nil
}
func (s *imageResolutionWorkerStore) ClaimImageResolutions(_ context.Context, _, _ string, _, _ time.Duration, excluded []string, _ int) ([]store.ImageResolutionClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.excluded = append([]string(nil), excluded...)
	if s.repeat {
		for _, id := range excluded {
			if id == s.claim.Environment.ID {
				return nil, nil
			}
		}
		s.attempts++
		return []store.ImageResolutionClaim{s.claim}, nil
	}
	if s.claimed {
		return nil, nil
	}
	s.claimed = true
	return []store.ImageResolutionClaim{s.claim}, nil
}

func TestImageResolutionWorkerExcludesInFlightEnvironmentAcrossScans(t *testing.T) {
	dataStore := &imageResolutionWorkerStore{claim: workerClaim(), repeat: true}
	processor := blockingImageResolutionProcessor{wait: 100 * time.Millisecond}
	config := workerConfig()
	config.Concurrency = 2
	runner := newImageResolutionRunner(config, slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, processor)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	var scans sync.WaitGroup
	for range 4 {
		scans.Add(1)
		go func() {
			defer scans.Done()
			if err := runner.scan(t.Context()); err != nil {
				t.Errorf("scan: %v", err)
			}
		}()
	}
	scans.Wait()
	runner.wg.Wait()
	dataStore.mu.Lock()
	defer dataStore.mu.Unlock()
	if dataStore.attempts != 1 || len(dataStore.excluded) != 1 || dataStore.excluded[0] != dataStore.claim.Environment.ID {
		t.Fatalf("attempts=%d excluded=%v", dataStore.attempts, dataStore.excluded)
	}
}
func (s *imageResolutionWorkerStore) RenewImageResolutionLease(context.Context, string, string, store.LeaseToken, time.Duration) (store.LeaseToken, error) {
	return s.claim.Lease, s.renewErr
}
func (s *imageResolutionWorkerStore) RotateImageResolutionLease(_ context.Context, _, _ string, token store.LeaseToken, _ time.Duration) (store.LeaseToken, error) {
	token.Epoch++
	return token, nil
}
func (s *imageResolutionWorkerStore) RetryImageResolution(context.Context, store.RetryImageResolutionParams) (store.ImageResolutionWriteResult, error) {
	s.mu.Lock()
	s.retried++
	s.mu.Unlock()
	return store.ImageResolutionProcessed, nil
}
func (s *imageResolutionWorkerStore) CompleteImageResolution(context.Context, store.CompleteImageResolutionParams) (store.ImageResolutionWriteResult, error) {
	s.mu.Lock()
	s.completed++
	s.mu.Unlock()
	return store.ImageResolutionProcessed, nil
}
func (s *imageResolutionWorkerStore) FailImageResolution(context.Context, store.FailImageResolutionParams) (store.ImageResolutionWriteResult, error) {
	s.mu.Lock()
	s.failed++
	s.mu.Unlock()
	return store.ImageResolutionProcessed, nil
}

type imageResolutionProcessorFake struct {
	outcome service.ImageResolutionOutcome
}

func (p imageResolutionProcessorFake) Resolve(context.Context, store.ImageResolutionClaim) service.ImageResolutionOutcome {
	return p.outcome
}

func TestImageResolutionWorkerCompleteAndRetry(t *testing.T) {
	for name, test := range map[string]struct {
		outcome            service.ImageResolutionOutcome
		completed, retried int
	}{
		"complete": {service.ImageResolutionOutcome{Disposition: service.ImageResolutionComplete, Image: oci.ResolvedImage{Digest: "sha256:" + workerRepeat("a", 64), ImmutableReference: "ghcr.io/acme/api@sha256:" + workerRepeat("a", 64), ManifestMediaType: oci.MediaTypeOCIImageManifest, ConfigDigest: "sha256:" + workerRepeat("b", 64), ConfigMediaType: oci.MediaTypeOCIConfig, AnnotationStatus: oci.AnnotationStatusAbsent}}, 1, 0},
		"retry":    {service.ImageResolutionOutcome{Disposition: service.ImageResolutionRetry, Code: string(oci.CodeRegistryUnavailable)}, 0, 1},
	} {
		t.Run(name, func(t *testing.T) {
			dataStore := &imageResolutionWorkerStore{claim: workerClaim()}
			runner := newImageResolutionRunner(workerConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, imageResolutionProcessorFake{test.outcome})
			runner.now = func() time.Time { return time.Unix(1000, 0) }
			if err := runner.Startup(t.Context()); err != nil {
				t.Fatal(err)
			}
			runner.wg.Wait()
			if dataStore.completed != test.completed || dataStore.retried != test.retried || dataStore.failed != 0 {
				t.Fatalf("store=%+v", dataStore)
			}
		})
	}
}

func TestImageResolutionWorkerLostLeaseDuringResolver(t *testing.T) {
	dataStore := &imageResolutionWorkerStore{claim: workerClaim(), renewErr: store.ErrLeaseNotOwned}
	processor := blockingImageResolutionProcessor{wait: 70 * time.Millisecond}
	config := workerConfig()
	config.LeaseDuration = 60 * time.Millisecond
	config.RequestTimeout = 50 * time.Millisecond
	runner := newImageResolutionRunner(config, slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, processor)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	runner.wg.Wait()
	if dataStore.completed != 0 || dataStore.retried != 0 || dataStore.failed != 0 {
		t.Fatalf("lost lease wrote result: %+v", dataStore)
	}
}

type blockingImageResolutionProcessor struct{ wait time.Duration }

func (p blockingImageResolutionProcessor) Resolve(ctx context.Context, _ store.ImageResolutionClaim) service.ImageResolutionOutcome {
	select {
	case <-ctx.Done():
		return service.ImageResolutionOutcome{Disposition: service.ImageResolutionLeaseLost}
	case <-time.After(p.wait):
		return service.ImageResolutionOutcome{Disposition: service.ImageResolutionComplete}
	}
}

func workerClaim() store.ImageResolutionClaim {
	repository := "ghcr.io/acme/api"
	now := time.Unix(1000, 0)
	return store.ImageResolutionClaim{Job: store.OrchestrationJob{ID: "job", OrgID: "org", CommitSHA: workerRepeat("a", 40), DesiredGeneration: 1, OCI: store.ImageResolutionEvidence{Attempts: 1}}, App: store.App{ImageRepository: &repository, ImageVisibility: "public"}, Environment: store.Environment{ID: "environment"}, EnvironmentDesiredGeneration: 1, Lease: store.LeaseToken{Owner: "worker", Epoch: 1, ExpiresAt: now.Add(time.Minute)}, Deadline: now.Add(time.Minute)}
}

func workerConfig() ImageResolutionConfig {
	return ImageResolutionConfig{ScanInterval: time.Second, LeaseDuration: time.Minute, PublicationDeadline: 15 * time.Minute, RequestTimeout: time.Second, ShutdownGrace: time.Second, Concurrency: 1, Owner: "worker"}
}
func workerRepeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}
