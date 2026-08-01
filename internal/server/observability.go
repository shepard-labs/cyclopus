package server

import (
	"context"
	"time"

	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
	"github.com/shepard-labs/cyclopus/internal/service"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (h *apiHandlers) QueryApplicationLogs(ctx context.Context, req *cyclopusv1.QueryApplicationLogsRequest) (*cyclopusv1.QueryApplicationLogsResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	violations.uuid("application_id", req.GetApplicationId())
	if req.GetEnvironmentId() != "" {
		violations.uuid("environment_id", req.GetEnvironmentId())
	}
	if req.GetReleaseId() != "" {
		violations.uuid("release_id", req.GetReleaseId())
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	input := service.LogQueryInput{
		AppID:         req.GetApplicationId(),
		EnvironmentID: req.GetEnvironmentId(),
		ReleaseID:     req.GetReleaseId(),
		Start:         optionalTime(req.GetStartTime()),
		End:           optionalTime(req.GetEndTime()),
		MinSeverity:   severityFromProto(req.GetMinSeverity()),
		Query:         req.GetQuery(),
		Limit:         int(req.GetLimit()),
		Cursor:        req.GetCursor(),
	}
	page, err := h.service.QueryApplicationLogs(ctx, actor, input)
	if err != nil {
		return nil, telemetryError(err)
	}
	response := &cyclopusv1.QueryApplicationLogsResponse{Entries: make([]*cyclopusv1.LogEntry, 0, len(page.Entries)), NextCursor: page.NextCursor}
	for _, entry := range page.Entries {
		response.Entries = append(response.Entries, logEntryProto(entry))
	}
	return response, nil
}

func (h *apiHandlers) ListApplicationTraces(ctx context.Context, req *cyclopusv1.ListApplicationTracesRequest) (*cyclopusv1.ListApplicationTracesResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	violations.uuid("application_id", req.GetApplicationId())
	if req.GetEnvironmentId() != "" {
		violations.uuid("environment_id", req.GetEnvironmentId())
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	input := service.TraceListInput{
		AppID:         req.GetApplicationId(),
		EnvironmentID: req.GetEnvironmentId(),
		Start:         optionalTime(req.GetStartTime()),
		End:           optionalTime(req.GetEndTime()),
		Limit:         int(req.GetLimit()),
		Cursor:        req.GetCursor(),
	}
	page, err := h.service.ListApplicationTraces(ctx, actor, input)
	if err != nil {
		return nil, telemetryError(err)
	}
	response := &cyclopusv1.ListApplicationTracesResponse{Traces: make([]*cyclopusv1.TraceSummary, 0, len(page.Traces)), NextCursor: page.NextCursor}
	for _, summary := range page.Traces {
		response.Traces = append(response.Traces, traceSummaryProto(summary))
	}
	return response, nil
}

func (h *apiHandlers) GetApplicationTrace(ctx context.Context, req *cyclopusv1.GetApplicationTraceRequest) (*cyclopusv1.GetApplicationTraceResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	violations.uuid("application_id", req.GetApplicationId())
	if req.GetEnvironmentId() != "" {
		violations.uuid("environment_id", req.GetEnvironmentId())
	}
	if req.GetTraceId() == "" {
		violations = append(violations, fieldViolation{"trace_id", "is required"})
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	summary, err := h.service.GetApplicationTrace(ctx, actor, service.TraceGetInput{
		AppID:         req.GetApplicationId(),
		EnvironmentID: req.GetEnvironmentId(),
		TraceID:       req.GetTraceId(),
		Start:         optionalTime(req.GetStartTime()),
		End:           optionalTime(req.GetEndTime()),
	})
	if err != nil {
		return nil, telemetryError(err)
	}
	return &cyclopusv1.GetApplicationTraceResponse{Trace: traceSummaryProto(summary)}, nil
}

func (h *apiHandlers) GetOperatorMetrics(ctx context.Context, _ *cyclopusv1.GetOperatorMetricsRequest) (*cyclopusv1.GetOperatorMetricsResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	metrics, err := h.service.GetOperatorMetrics(ctx, actor)
	if err != nil {
		return nil, telemetryError(err)
	}
	response := &cyclopusv1.GetOperatorMetricsResponse{Metrics: make([]*cyclopusv1.OperatorMetric, 0, len(metrics))}
	for _, metric := range metrics {
		response.Metrics = append(response.Metrics, &cyclopusv1.OperatorMetric{
			Name:       metric.Name,
			Value:      metric.Value,
			Unit:       metric.Unit,
			ObservedAt: metricTime(metric.ObservedAt),
		})
	}
	return response, nil
}

func telemetryError(err error) error {
	if err == service.ErrTelemetryUnavailable {
		return typedError(codes.Unavailable, "telemetry backend unavailable", "UNAVAILABLE")
	}
	return rpcError(err)
}

func logEntryProto(entry observability.LogEntry) *cyclopusv1.LogEntry {
	return &cyclopusv1.LogEntry{
		Timestamp:  metricTime(entry.Timestamp),
		Severity:   severityProto(entry.Severity),
		Body:       entry.Body,
		Attributes: entry.Attributes,
	}
}

func traceSummaryProto(summary observability.TraceSummary) *cyclopusv1.TraceSummary {
	return &cyclopusv1.TraceSummary{
		TraceId:    summary.TraceID,
		RootName:   summary.RootName,
		DurationMs: summary.DurationMs,
		StartTime:  metricTime(summary.StartTime),
		Status:     traceStatusProto(summary.Status),
	}
}

func metricTime(value time.Time) *timestamppb.Timestamp {
	if value.IsZero() {
		return nil
	}
	return timestamppb.New(value)
}

func optionalTime(value *timestamppb.Timestamp) *time.Time {
	if value == nil || !value.IsValid() {
		return nil
	}
	converted := value.AsTime()
	return &converted
}

func severityFromProto(value cyclopusv1.LogSeverity) observability.Severity {
	return map[cyclopusv1.LogSeverity]observability.Severity{
		cyclopusv1.LogSeverity_LOG_SEVERITY_TRACE: observability.SeverityTrace,
		cyclopusv1.LogSeverity_LOG_SEVERITY_DEBUG: observability.SeverityDebug,
		cyclopusv1.LogSeverity_LOG_SEVERITY_INFO:  observability.SeverityInfo,
		cyclopusv1.LogSeverity_LOG_SEVERITY_WARN:  observability.SeverityWarn,
		cyclopusv1.LogSeverity_LOG_SEVERITY_ERROR: observability.SeverityError,
		cyclopusv1.LogSeverity_LOG_SEVERITY_FATAL: observability.SeverityFatal,
	}[value]
}

func severityProto(value observability.Severity) cyclopusv1.LogSeverity {
	return map[observability.Severity]cyclopusv1.LogSeverity{
		observability.SeverityTrace: cyclopusv1.LogSeverity_LOG_SEVERITY_TRACE,
		observability.SeverityDebug: cyclopusv1.LogSeverity_LOG_SEVERITY_DEBUG,
		observability.SeverityInfo:  cyclopusv1.LogSeverity_LOG_SEVERITY_INFO,
		observability.SeverityWarn:  cyclopusv1.LogSeverity_LOG_SEVERITY_WARN,
		observability.SeverityError: cyclopusv1.LogSeverity_LOG_SEVERITY_ERROR,
		observability.SeverityFatal: cyclopusv1.LogSeverity_LOG_SEVERITY_FATAL,
	}[value]
}

func traceStatusProto(value observability.TraceStatus) cyclopusv1.TraceStatus {
	return map[observability.TraceStatus]cyclopusv1.TraceStatus{
		observability.TraceStatusOK:    cyclopusv1.TraceStatus_TRACE_STATUS_OK,
		observability.TraceStatusError: cyclopusv1.TraceStatus_TRACE_STATUS_ERROR,
	}[value]
}
