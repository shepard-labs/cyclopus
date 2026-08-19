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
	"fmt"
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
	// ID is the named chart window this range was derived from, if any. It is
	// set by MetricsTimeRange; adapters that need window-shape context beyond
	// Start/End (e.g. Beszel's per-tier record types) read it from here.
	ID    MetricsTimeRangeID
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

// MetricsTimeRangeID names one of the fixed chart time windows the admin
// metrics dashboard offers. The string form is what the UI sends as the
// `range` query parameter and is stable API surface.
type MetricsTimeRangeID string

const (
	MetricsRange1h  MetricsTimeRangeID = "1h"
	MetricsRange12h MetricsTimeRangeID = "12h"
	MetricsRange24h MetricsTimeRangeID = "24h"
	MetricsRange1w  MetricsTimeRangeID = "1w"
)

// DefaultMetricsTimeRange is the window used when a request carries no
// explicit range.
const DefaultMetricsTimeRange = MetricsRange24h

// MetricsTimeRange returns the fixed window (ending at `now`) that the ID
// selects. An unrecognized ID is reported as an error so handlers can reject
// it rather than silently serving a different window.
func MetricsTimeRange(id MetricsTimeRangeID, now time.Time) (TimeRange, error) {
	end := now.UTC()
	var start time.Time
	switch id {
	case MetricsRange1h:
		start = end.Add(-time.Hour)
	case MetricsRange12h:
		start = end.Add(-12 * time.Hour)
	case MetricsRange24h:
		start = end.Add(-24 * time.Hour)
	case MetricsRange1w:
		start = end.Add(-7 * 24 * time.Hour)
	default:
		return TimeRange{}, fmt.Errorf("observability: invalid metrics time range %q", id)
	}
	return TimeRange{ID: id, Start: start, End: end}, nil
}

// RecordType returns the Beszel system_stats record type (pre-aggregated
// retention tier) that best matches this named chart window, mirroring
// Beszel's own chartTimeData mapping. Beszel stores per-tier pre-aggregated
// rows distinguished by a `type` field; querying without it returns raw 1m
// rows (dense recent data only), which collapses long windows to the last
// ~40 minutes. Unknown/unset IDs fall back to "20m", the 24h tier.
func (id MetricsTimeRangeID) RecordType() string {
	switch id {
	case MetricsRange1h:
		return "1m"
	case MetricsRange12h:
		return "10m"
	case MetricsRange1w:
		return "120m"
	default:
		// MetricsRange24h and any unknown ID.
		return "20m"
	}
}

// MaxHistoryPoints caps how many time-series samples a node's chart may
// carry. Longer windows are downsampled evenly to this bound so the rendered
// SVG paths stay small.
const MaxHistoryPoints = 500

// downsampleHistory evenly reduces a chronological point series to at most
// limit points, always keeping the first and last sample. Series at or below
// the limit are returned unchanged.
func downsampleHistory(points []SystemStatPoint, limit int) []SystemStatPoint {
	if limit < 2 || len(points) <= limit {
		return points
	}
	out := make([]SystemStatPoint, limit)
	for i := range out {
		out[i] = points[i*(len(points)-1)/(limit-1)]
	}
	return out
}

// SystemStatPoint is one time-series sample for a system/node from Beszel.
type SystemStatPoint struct {
	Timestamp   time.Time
	CPU         float64
	CPUUser     float64
	CPUSystem   float64
	CPUIOWait   float64
	Memory      float64
	MemoryMB    float64
	MemoryUsed  float64
	Disk        float64
	DiskUsed    float64 // disk used, GB
	DiskTotal   float64 // disk total, GB
	DiskRead    float64
	DiskWrite   float64
	Bandwidth   float64
	NetSent     float64 // network sent, MB/s
	NetRecv     float64 // network received, MB/s
	Load1       float64
	Load5       float64
	Load15      float64
	Temperature float64
}

// SystemdService is one systemd unit as reported by a node's Beszel agent
// (hub collection `systemd_services`). State and SubState are decoded to
// Beszel's display labels at the adapter boundary so templates stay dumb.
// CPU/CPUPeak are percentages and are only meaningful while SubState is
// "Running" (the agent samples no cgroup for exited/dead units; the Beszel UI
// renders N/A for the same condition). Memory values are bytes; zero means
// the agent reported nothing (also rendered N/A).
type SystemdService struct {
	Name            string
	State           string // Active, Inactive, Failed, Activating, Deactivating, Reloading
	SubState        string // Dead, Running, Exited, Failed, Unknown
	CPU             float64
	CPUPeak         float64
	MemoryBytes     uint64
	MemoryPeakBytes uint64
	Updated         time.Time
}

// NodeSystemDetail holds live info and recent timeseries samples for one node.
type NodeSystemDetail struct {
	ID          string
	Name        string
	Host        string
	Port        string
	Status      string
	Updated     time.Time
	OS          string
	CPUModel    string
	Hostname    string
	Kernel      string
	Arch        string
	Cores       int
	Threads     int
	MemoryBytes int64
	UptimeSec   float64
	CPUPercent  float64
	MemoryUsed  float64
	DiskPercent float64
	DiskUsed    float64 // disk used, GB
	DiskTotal   float64 // disk total, GB
	Bandwidth   float64
	Load1       float64
	Load5       float64
	Load15      float64
	Temperature float64
	History     []SystemStatPoint
	// Services holds the node's systemd units as last reported by the
	// Beszel agent. Empty when the agent reports none (non-Linux host or
	// SKIP_SYSTEMD) — the UI hides the services card in that case.
	Services []SystemdService
}
