package observability

import "context"

// Provider composes the OpenObserve (tenant-scoped logs/traces) and Beszel
// (operator-level metrics) clients behind one value. It is what the service
// layer holds: the service constructs the TenantScope and calls through; the
// upstream backends are never reachable except via these bounded clients.
type Provider struct {
	logs    *OpenObserveClient
	metrics *BeszelClient
}

// NewProvider builds a composite provider from already-resolved backend
// configuration. Either client may be nil if that backend is not configured;
// the corresponding methods then return ErrScopeIncomplete-free "unavailable"
// style errors from the nil client guard below.
func NewProvider(openobserve *OpenObserveClient, beszel *BeszelClient) *Provider {
	return &Provider{logs: openobserve, metrics: beszel}
}

// ErrBackendUnavailable is returned when the relevant backend client is unset.
var ErrBackendUnavailable = errBackendUnavailable{}

type errBackendUnavailable struct{}

func (errBackendUnavailable) Error() string { return "observability: backend not configured" }

func (p *Provider) QueryLogs(ctx context.Context, scope TenantScope, window TimeRange, filter LogFilter) (LogPage, error) {
	if p.logs == nil {
		return LogPage{}, ErrBackendUnavailable
	}
	return p.logs.QueryLogs(ctx, scope, window, filter)
}

func (p *Provider) ListTraces(ctx context.Context, scope TenantScope, window TimeRange, filter TraceFilter) (TracePage, error) {
	if p.logs == nil {
		return TracePage{}, ErrBackendUnavailable
	}
	return p.logs.ListTraces(ctx, scope, window, filter)
}

func (p *Provider) GetTrace(ctx context.Context, scope TenantScope, window TimeRange, traceID string) (TraceSummary, bool, error) {
	if p.logs == nil {
		return TraceSummary{}, false, ErrBackendUnavailable
	}
	return p.logs.GetTrace(ctx, scope, window, traceID)
}

func (p *Provider) GetOperatorMetrics(ctx context.Context) ([]OperatorMetric, error) {
	if p.metrics == nil {
		return nil, ErrBackendUnavailable
	}
	return p.metrics.GetOperatorMetrics(ctx)
}

func (p *Provider) GetNodeSystemDetails(ctx context.Context) ([]NodeSystemDetail, error) {
	if p.metrics == nil {
		return nil, ErrBackendUnavailable
	}
	return p.metrics.GetNodeSystemDetails(ctx)
}
