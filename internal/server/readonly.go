package server

import (
	"context"
	"encoding/json"

	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// GetAlias returns a single alias org-scoped to the caller. A cross-org or
// unknown id maps to NotFound (the service/store apply the org filter).
func (h *apiHandlers) GetAlias(ctx context.Context, req *cyclopusv1.GetAliasRequest) (*cyclopusv1.GetAliasResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("alias_id", req.GetAliasId()); err != nil {
		return nil, err
	}
	value, err := h.service.GetAlias(ctx, actor, req.GetAliasId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetAliasResponse{Alias: aliasProto(value)}, nil
}

// ListAliases returns the aliases for an environment, org-scoped. An environment
// in another org yields an empty list (the store filters by org_id), so it never
// leaks another tenant's aliases.
func (h *apiHandlers) ListAliases(ctx context.Context, req *cyclopusv1.ListAliasesRequest) (*cyclopusv1.ListAliasesResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("environment_id", req.GetEnvironmentId()); err != nil {
		return nil, err
	}
	values, err := h.service.ListAliases(ctx, actor, req.GetEnvironmentId())
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("aliases", actor.OrgID+"/"+req.GetEnvironmentId())
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.Alias) cursorKey { return cursorKey{CreatedAt: v.UpdatedAt, ID: v.ID} })
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.Alias) cursorKey { return cursorKey{CreatedAt: v.UpdatedAt, ID: v.ID} })
	response := &cyclopusv1.ListAliasesResponse{NextPageToken: next, Aliases: make([]*cyclopusv1.Alias, 0, len(page))}
	for _, value := range page {
		response.Aliases = append(response.Aliases, aliasProto(value))
	}
	return response, nil
}

// ListAuditEvents returns the caller's org's audit trail, most-recent first. The
// organization_id in the request MUST equal the actor's org; the service also
// scopes by actor.OrgID so a mismatched or spoofed org can never read another
// tenant's events.
func (h *apiHandlers) ListAuditEvents(ctx context.Context, req *cyclopusv1.ListAuditEventsRequest) (*cyclopusv1.ListAuditEventsResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("organization_id", req.GetOrganizationId()); err != nil {
		return nil, err
	}
	if req.GetOrganizationId() != actor.OrgID {
		return nil, status.Error(codes.PermissionDenied, "permission denied")
	}
	violations := validationViolations{}
	if req.GetTargetId() != "" {
		violations.uuid("target_id", req.GetTargetId())
		if req.GetTargetType() == "" {
			violations.add("target_type", "is required when target_id is set")
		}
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	values, err := h.service.ListAuditEvents(ctx, actor, service.AuditEventFilter{TargetType: req.GetTargetType(), TargetID: req.GetTargetId()})
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("audit_events", actor.OrgID, req.GetTargetType(), req.GetTargetId())
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.AuditEventRecord) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.AuditEventRecord) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	response := &cyclopusv1.ListAuditEventsResponse{NextPageToken: next, AuditEvents: make([]*cyclopusv1.AuditEvent, 0, len(page))}
	for _, value := range page {
		event, err := auditEventProto(value)
		if err != nil {
			return nil, rpcError(err)
		}
		response.AuditEvents = append(response.AuditEvents, event)
	}
	return response, nil
}

// ListCertificates returns live cert-manager Certificate status for an
// environment. Org-scoping is enforced in the service, which resolves the
// environment within the caller's org before reading only that environment's
// managed namespace.
func (h *apiHandlers) ListCertificates(ctx context.Context, req *cyclopusv1.ListCertificatesRequest) (*cyclopusv1.ListCertificatesResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("environment_id", req.GetEnvironmentId()); err != nil {
		return nil, err
	}
	statuses, err := h.service.ListCertificates(ctx, actor, req.GetEnvironmentId())
	if err != nil {
		return nil, certReaderError(err)
	}
	response := &cyclopusv1.ListCertificatesResponse{Certificates: make([]*cyclopusv1.CertificateStatus, 0, len(statuses))}
	for _, status := range statuses {
		response.Certificates = append(response.Certificates, certificateStatusProto(status))
	}
	return response, nil
}

func certReaderError(err error) error {
	if err == service.ErrCertReaderUnavailable {
		return typedError(codes.Unavailable, "certificate reading unavailable", "UNAVAILABLE")
	}
	return rpcError(err)
}

func aliasProto(v store.Alias) *cyclopusv1.Alias {
	return &cyclopusv1.Alias{
		Id: v.ID, OrganizationId: v.OrgID, EnvironmentId: v.EnvironmentID,
		Hostname: v.Hostname, ReleaseId: v.ReleaseID, UpdatedAt: timestamppb.New(v.UpdatedAt),
	}
}

func auditEventProto(v store.AuditEventRecord) (*cyclopusv1.AuditEvent, error) {
	before, err := jsonStruct(v.Before)
	if err != nil {
		return nil, err
	}
	after, err := jsonStruct(v.After)
	if err != nil {
		return nil, err
	}
	return &cyclopusv1.AuditEvent{
		Id: v.ID, OrganizationId: v.OrgID, ActorUserId: stringValue(v.ActorUserID),
		Action: v.Action, TargetType: v.TargetType, TargetId: v.TargetID,
		Before: before, After: after, CreatedAt: timestamppb.New(v.CreatedAt),
	}, nil
}

// jsonStruct converts stored jsonb (a JSON object or null) into a
// google.protobuf.Struct. A JSON null or empty value yields a nil Struct. A
// non-object JSON value (which the audit tables never store) is rejected rather
// than silently dropped.
func jsonStruct(raw json.RawMessage) (*structpb.Struct, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return structpb.NewStruct(decoded)
}

func certificateStatusProto(v service.CertificateStatus) *cyclopusv1.CertificateStatus {
	return &cyclopusv1.CertificateStatus{
		Hostname: v.Hostname, SecretName: v.SecretName, Ready: v.Ready, Reason: v.Reason,
		NotAfter: timeProto(v.NotAfter), Issuer: v.Issuer, DnsNames: append([]string(nil), v.DNSNames...),
	}
}
