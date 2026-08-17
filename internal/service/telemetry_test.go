package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
)

// recordingTelemetry is a fake TelemetryProvider that records whether it was
// ever called. Service tests assert it stays untouched when validation fails
// before any upstream query.
type recordingTelemetry struct {
	called    bool
	lastScope observability.TenantScope
	logPage   observability.LogPage
	tracePage observability.TracePage
	trace     observability.TraceSummary
	found     bool
	metrics   []observability.OperatorMetric
	err       error
}

func (r *recordingTelemetry) QueryLogs(_ context.Context, scope observability.TenantScope, _ observability.TimeRange, _ observability.LogFilter) (observability.LogPage, error) {
	r.called = true
	r.lastScope = scope
	return r.logPage, r.err
}

func (r *recordingTelemetry) ListTraces(_ context.Context, scope observability.TenantScope, _ observability.TimeRange, _ observability.TraceFilter) (observability.TracePage, error) {
	r.called = true
	r.lastScope = scope
	return r.tracePage, r.err
}

func (r *recordingTelemetry) GetTrace(_ context.Context, scope observability.TenantScope, _ observability.TimeRange, _ string) (observability.TraceSummary, bool, error) {
	r.called = true
	r.lastScope = scope
	return r.trace, r.found, r.err
}

func (r *recordingTelemetry) GetOperatorMetrics(context.Context) ([]observability.OperatorMetric, error) {
	r.called = true
	return r.metrics, r.err
}

func (r *recordingTelemetry) GetNodeSystemDetails(context.Context) ([]observability.NodeSystemDetail, error) {
	r.called = true
	return nil, r.err
}

func TestBoundTelemetryLimit(t *testing.T) {
	if limit, err := boundTelemetryLimit(0); err != nil || limit != defaultTelemetryLimit {
		t.Fatalf("default limit = %d, %v", limit, err)
	}
	if limit, err := boundTelemetryLimit(500); err != nil || limit != 500 {
		t.Fatalf("explicit limit = %d, %v", limit, err)
	}
	if _, err := boundTelemetryLimit(-1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative limit error = %v", err)
	}
	if _, err := boundTelemetryLimit(maxTelemetryLimit + 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("over-max limit error = %v", err)
	}
}

func TestBoundWindow(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	// Defaults: end=now, start=now-1h.
	window, err := boundWindow(now, nil, nil)
	if err != nil {
		t.Fatalf("default window: %v", err)
	}
	if window.End.Sub(window.Start) != defaultTelemetryLen {
		t.Fatalf("default window length = %s", window.End.Sub(window.Start))
	}
	// Inverted range is rejected.
	start := now
	end := now.Add(-time.Hour)
	if _, err := boundWindow(now, &start, &end); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("inverted window error = %v", err)
	}
	// Over-wide range is rejected.
	wideStart := now.Add(-maxTelemetryWindow - time.Hour)
	if _, err := boundWindow(now, &wideStart, &now); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("over-wide window error = %v", err)
	}
}

func TestSanitizeSubstringRejectsUnsafeInput(t *testing.T) {
	if value, err := sanitizeSubstring("  hello world  "); err != nil || value != "hello world" {
		t.Fatalf("clean substring = %q, %v", value, err)
	}
	for _, bad := range []string{"drop'", "a\"b", "x;y", "back\\slash", "ctrl\x01"} {
		if _, err := sanitizeSubstring(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("unsafe substring %q error = %v", bad, err)
		}
	}
}

func TestQueryApplicationLogsRequiresActor(t *testing.T) {
	svc := New(nil, WithTelemetry(&recordingTelemetry{}))
	if _, err := svc.QueryApplicationLogs(context.Background(), ActorContext{}, LogQueryInput{AppID: "app"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("missing actor error = %v", err)
	}
}

func TestGetOperatorMetricsUnavailableWhenUnwired(t *testing.T) {
	svc := New(nil)
	if _, err := svc.GetOperatorMetrics(context.Background(), ActorContext{UserID: "u", OrgID: "o"}); !errors.Is(err, ErrTelemetryUnavailable) {
		t.Fatalf("unwired metrics error = %v", err)
	}
}

func TestGetApplicationTraceRejectsBadTraceID(t *testing.T) {
	fake := &recordingTelemetry{}
	svc := New(nil, WithTelemetry(fake))
	_, err := svc.GetApplicationTrace(context.Background(), ActorContext{UserID: "u", OrgID: "o"}, TraceGetInput{AppID: "app", TraceID: "not hex!"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad trace id error = %v", err)
	}
	if fake.called {
		t.Fatal("adapter must not be called for an invalid trace id")
	}
}
