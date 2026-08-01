// Package observability holds the bounded, server-side proxy clients Cyclopus
// uses to read telemetry backends. OpenObserve (logs/traces) is queried with a
// mandatory tenant filter that Cyclopus constructs from validated IDs, so the
// caller can never inject or widen scope. Beszel exposes only a
// fixed allowlist of operator/install-level system metrics with no tenant
// parameters (Phase 4 design note: Beszel is operator-level, not a per-tenant
// proxy).
package observability

import (
	"errors"
	"time"
)

// RequestTimeout bounds every upstream request, matching the webhook sender.
const RequestTimeout = 10 * time.Second

// MaxResponseBytes caps how much of an upstream telemetry response Cyclopus will
// read and decode, so a compromised or misbehaving backend cannot exhaust
// control-plane memory within the request timeout window.
const MaxResponseBytes = 8 << 20 // 8 MiB

// ErrScopeIncomplete is returned when a tenant scope is missing the identifiers
// that make a query safely tenant-bound. It is a programmer error: the service
// layer must always supply a validated, complete scope.
var ErrScopeIncomplete = errors.New("observability: tenant scope is incomplete")

// TenantScope is the set of validated identifiers Cyclopus stamps onto every
// logs/traces query. Every field here is produced by the service layer from
// records it has already loaded and authorized; none of it comes from raw
// caller input. The adapter treats this as the authoritative, non-negotiable
// WHERE clause.
type TenantScope struct {
	OrgID         string
	AppID         string
	EnvironmentID string // optional; empty means all environments of the app
	ReleaseID     string // optional; empty means all releases
}

func (s TenantScope) valid() bool {
	return s.OrgID != "" && s.AppID != ""
}

// TimeRange bounds a query window. Both ends are required by callers; the
// adapter clamps nothing and trusts the service layer's validation.
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// Severity is the coarse severity floor applied to a log query.
type Severity int

const (
	SeverityUnspecified Severity = iota
	SeverityTrace
	SeverityDebug
	SeverityInfo
	SeverityWarn
	SeverityError
	SeverityFatal
)

// LogFilter carries the parts of a query that originate, in sanitized form,
// from the caller. None of these fields can alter tenant scope: the adapter
// only ever ANDs them onto the mandatory tenant filter.
type LogFilter struct {
	MinSeverity Severity
	// Substring is a free-text literal match. The adapter escapes it and applies
	// it with LIKE; it is never treated as SQL.
	Substring string
	Limit     int
	// Cursor is an opaque forward-only pagination token produced by a prior
	// response.
	Cursor string
}

// LogEntry is one tenant-scoped log record.
type LogEntry struct {
	Timestamp  time.Time
	Severity   Severity
	Body       string
	Attributes map[string]string
}

// LogPage is a bounded page of log entries plus the cursor for the next page.
type LogPage struct {
	Entries    []LogEntry
	NextCursor string
}

// TraceStatus is the coarse status of a trace root span.
type TraceStatus int

const (
	TraceStatusUnspecified TraceStatus = iota
	TraceStatusOK
	TraceStatusError
)

// TraceSummary is a compact description of a single trace.
type TraceSummary struct {
	TraceID    string
	RootName   string
	DurationMs int64
	StartTime  time.Time
	Status     TraceStatus
}

// TracePage is a bounded page of trace summaries plus the next cursor.
type TracePage struct {
	Traces     []TraceSummary
	NextCursor string
}

// TraceFilter carries caller-supplied, sanitized trace list parameters.
type TraceFilter struct {
	Limit  int
	Cursor string
}

// OperatorMetric is one install-level host/system metric. It carries no tenant
// attribution by construction.
type OperatorMetric struct {
	Name       string
	Value      float64
	Unit       string
	ObservedAt time.Time
}
