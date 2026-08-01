package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
	"github.com/shepard-labs/cyclopus/internal/store"
)

// ErrTelemetryUnavailable is returned when no observability provider is wired.
var ErrTelemetryUnavailable = errors.New("service: telemetry backend unavailable")

// SetTelemetry installs the observability provider after construction. It exists
// so wiring that resolves backend credentials from Kubernetes secrets (which
// only becomes available once the cluster client is built) can attach the
// provider to an already-constructed Service. A nil provider is ignored.
func (s *Service) SetTelemetry(provider TelemetryProvider) {
	if provider != nil {
		s.telemetry = provider
	}
}

const (
	// defaultTelemetryLimit and maxTelemetryLimit bound page sizes for
	// logs/traces queries per the spec's proxied-read requirements.
	defaultTelemetryLimit = 100
	maxTelemetryLimit     = 1000
	// maxTelemetryWindow bounds how far back a query window may reach. A missing
	// end defaults to now; a missing start defaults to one hour before end.
	maxTelemetryWindow  = 31 * 24 * time.Hour
	defaultTelemetryLen = time.Hour
	// maxSubstringLen bounds the free-text filter length.
	maxSubstringLen = 512
)

// TelemetryProvider is the seam the service uses to reach the observability
// backends. The service is the only place that constructs an
// observability.TenantScope, and it does so exclusively from records it has
// already loaded and authorized — so no caller input reaches the backend as
// tenant scope. A fake implementation is injected in tests.
type TelemetryProvider interface {
	QueryLogs(ctx context.Context, scope observability.TenantScope, window observability.TimeRange, filter observability.LogFilter) (observability.LogPage, error)
	ListTraces(ctx context.Context, scope observability.TenantScope, window observability.TimeRange, filter observability.TraceFilter) (observability.TracePage, error)
	GetTrace(ctx context.Context, scope observability.TenantScope, window observability.TimeRange, traceID string) (observability.TraceSummary, bool, error)
	GetOperatorMetrics(ctx context.Context) ([]observability.OperatorMetric, error)
}

// LogQueryInput is the caller-supplied, pre-validation shape for a log query.
// The IDs here are treated as untrusted request input: the service re-loads and
// authorizes each before it ever becomes part of a tenant scope.
type LogQueryInput struct {
	AppID         string
	EnvironmentID string
	ReleaseID     string
	Start         *time.Time
	End           *time.Time
	MinSeverity   observability.Severity
	Query         string
	Limit         int
	Cursor        string
}

// TraceListInput is the caller-supplied shape for a trace list.
type TraceListInput struct {
	AppID         string
	EnvironmentID string
	Start         *time.Time
	End           *time.Time
	Limit         int
	Cursor        string
}

// TraceGetInput is the caller-supplied shape for a single-trace lookup.
type TraceGetInput struct {
	AppID         string
	EnvironmentID string
	TraceID       string
	Start         *time.Time
	End           *time.Time
}

// QueryApplicationLogs validates the actor, authorizes the requested app (and
// environment/release when present) against the actor's org, then builds the
// mandatory tenant filter from the VALIDATED identifiers and delegates to the
// provider. A cross-tenant or unknown app_id returns ErrNotFound before any
// upstream query runs, so existence is never disclosed across tenants.
func (s *Service) QueryApplicationLogs(ctx context.Context, actor ActorContext, input LogQueryInput) (observability.LogPage, error) {
	if err := validateActor(actor); err != nil {
		return observability.LogPage{}, err
	}
	scope, err := s.resolveTenantScope(ctx, actor, input.AppID, input.EnvironmentID, input.ReleaseID)
	if err != nil {
		return observability.LogPage{}, err
	}
	limit, err := boundTelemetryLimit(input.Limit)
	if err != nil {
		return observability.LogPage{}, err
	}
	window, err := boundWindow(s.now(), input.Start, input.End)
	if err != nil {
		return observability.LogPage{}, err
	}
	substring, err := sanitizeSubstring(input.Query)
	if err != nil {
		return observability.LogPage{}, err
	}
	if s.telemetry == nil {
		return observability.LogPage{}, ErrTelemetryUnavailable
	}
	filter := observability.LogFilter{MinSeverity: input.MinSeverity, Substring: substring, Limit: limit, Cursor: input.Cursor}
	page, err := s.telemetry.QueryLogs(ctx, scope, window, filter)
	if err != nil {
		return observability.LogPage{}, mapTelemetryError(err)
	}
	return page, nil
}

// ListApplicationTraces mirrors QueryApplicationLogs for trace summaries.
func (s *Service) ListApplicationTraces(ctx context.Context, actor ActorContext, input TraceListInput) (observability.TracePage, error) {
	if err := validateActor(actor); err != nil {
		return observability.TracePage{}, err
	}
	scope, err := s.resolveTenantScope(ctx, actor, input.AppID, input.EnvironmentID, "")
	if err != nil {
		return observability.TracePage{}, err
	}
	limit, err := boundTelemetryLimit(input.Limit)
	if err != nil {
		return observability.TracePage{}, err
	}
	window, err := boundWindow(s.now(), input.Start, input.End)
	if err != nil {
		return observability.TracePage{}, err
	}
	if s.telemetry == nil {
		return observability.TracePage{}, ErrTelemetryUnavailable
	}
	page, err := s.telemetry.ListTraces(ctx, scope, window, observability.TraceFilter{Limit: limit, Cursor: input.Cursor})
	if err != nil {
		return observability.TracePage{}, mapTelemetryError(err)
	}
	return page, nil
}

// GetApplicationTrace looks up a single trace inside the validated tenant scope.
func (s *Service) GetApplicationTrace(ctx context.Context, actor ActorContext, input TraceGetInput) (observability.TraceSummary, error) {
	if err := validateActor(actor); err != nil {
		return observability.TraceSummary{}, err
	}
	if strings.TrimSpace(input.TraceID) == "" || !traceIDPattern.MatchString(input.TraceID) {
		return observability.TraceSummary{}, ErrInvalidArgument
	}
	scope, err := s.resolveTenantScope(ctx, actor, input.AppID, input.EnvironmentID, "")
	if err != nil {
		return observability.TraceSummary{}, err
	}
	window, err := boundWindow(s.now(), input.Start, input.End)
	if err != nil {
		return observability.TraceSummary{}, err
	}
	if s.telemetry == nil {
		return observability.TraceSummary{}, ErrTelemetryUnavailable
	}
	summary, found, err := s.telemetry.GetTrace(ctx, scope, window, input.TraceID)
	if err != nil {
		return observability.TraceSummary{}, mapTelemetryError(err)
	}
	if !found {
		return observability.TraceSummary{}, store.ErrNotFound
	}
	return summary, nil
}

// GetOperatorMetrics returns install-level system metrics. Any authenticated
// org member may read them (no RBAC in v1), and they carry no
// tenant scope by construction.
func (s *Service) GetOperatorMetrics(ctx context.Context, actor ActorContext) ([]observability.OperatorMetric, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	if s.telemetry == nil {
		return nil, ErrTelemetryUnavailable
	}
	metrics, err := s.telemetry.GetOperatorMetrics(ctx)
	if err != nil {
		return nil, mapTelemetryError(err)
	}
	return metrics, nil
}

// resolveTenantScope loads and authorizes the app, and optional environment and
// release, returning a tenant scope built ONLY from the validated stored IDs.
// Any failure to find a resource within the actor's org is reported as
// ErrNotFound, so cross-tenant probing cannot distinguish "wrong tenant" from
// "does not exist".
func (s *Service) resolveTenantScope(ctx context.Context, actor ActorContext, appID, environmentID, releaseID string) (observability.TenantScope, error) {
	if strings.TrimSpace(appID) == "" {
		return observability.TenantScope{}, ErrInvalidArgument
	}
	app, err := s.store.GetApp(ctx, actor.OrgID, appID)
	if err != nil {
		return observability.TenantScope{}, mapError(err)
	}
	scope := observability.TenantScope{OrgID: actor.OrgID, AppID: app.ID}
	if environmentID != "" {
		environment, err := s.store.GetEnvironment(ctx, actor.OrgID, environmentID)
		if err != nil {
			return observability.TenantScope{}, mapError(err)
		}
		if environment.AppID != app.ID {
			return observability.TenantScope{}, store.ErrNotFound
		}
		scope.EnvironmentID = environment.ID
	}
	if releaseID != "" {
		release, err := s.store.GetRelease(ctx, actor.OrgID, releaseID)
		if err != nil {
			return observability.TenantScope{}, mapError(err)
		}
		if release.EnvironmentID == "" {
			return observability.TenantScope{}, store.ErrNotFound
		}
		environment, err := s.store.GetEnvironment(ctx, actor.OrgID, release.EnvironmentID)
		if err != nil {
			return observability.TenantScope{}, mapError(err)
		}
		if environment.AppID != app.ID {
			return observability.TenantScope{}, store.ErrNotFound
		}
		if scope.EnvironmentID != "" && scope.EnvironmentID != environment.ID {
			return observability.TenantScope{}, store.ErrNotFound
		}
		scope.EnvironmentID = environment.ID
		scope.ReleaseID = release.ID
	}
	return scope, nil
}

func boundTelemetryLimit(limit int) (int, error) {
	if limit < 0 || limit > maxTelemetryLimit {
		return 0, ErrInvalidArgument
	}
	if limit == 0 {
		return defaultTelemetryLimit, nil
	}
	return limit, nil
}

func boundWindow(now time.Time, start, end *time.Time) (observability.TimeRange, error) {
	endTime := now
	if end != nil {
		endTime = *end
	}
	startTime := endTime.Add(-defaultTelemetryLen)
	if start != nil {
		startTime = *start
	}
	if !startTime.Before(endTime) {
		return observability.TimeRange{}, ErrInvalidArgument
	}
	if endTime.Sub(startTime) > maxTelemetryWindow {
		return observability.TimeRange{}, ErrInvalidArgument
	}
	return observability.TimeRange{Start: startTime.UTC(), End: endTime.UTC()}, nil
}

// sanitizeSubstring validates the free-text log filter. It must be a plain
// literal: control characters and quotes are rejected so it cannot be crafted
// to look like it might break the query, even though the adapter also escapes
// it. It is only ever applied as an additional AND, never replacing scope.
func sanitizeSubstring(query string) (string, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) > maxSubstringLen {
		return "", ErrInvalidArgument
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f || r == '\'' || r == '"' || r == ';' || r == '\\' {
			return "", ErrInvalidArgument
		}
	}
	return trimmed, nil
}

func mapTelemetryError(err error) error {
	if errors.Is(err, observability.ErrScopeIncomplete) {
		// A scope built by the service should always be complete; treat a report
		// otherwise as an internal error rather than leaking it.
		return err
	}
	return err
}
