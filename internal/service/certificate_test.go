package service

import (
	"context"
	"errors"
	"testing"
)

// fakeCertReader is a canned CertReader for tests. It records the scope it was
// invoked with so tests can assert the service only ever passes an authorized,
// namespace-bounded scope.
type fakeCertReader struct {
	called    bool
	lastScope CertScope
	statuses  []CertificateStatus
	err       error
}

func (f *fakeCertReader) ListCertificates(_ context.Context, scope CertScope) ([]CertificateStatus, error) {
	f.called = true
	f.lastScope = scope
	return f.statuses, f.err
}

func TestListCertificatesRequiresActor(t *testing.T) {
	svc := New(nil, WithCertReader(&fakeCertReader{}))
	if _, err := svc.ListCertificates(context.Background(), ActorContext{}, "env"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ListCertificates without actor = %v, want ErrForbidden", err)
	}
}

func TestSetCertReaderIgnoresNil(t *testing.T) {
	reader := &fakeCertReader{}
	svc := New(nil, WithCertReader(reader))
	svc.SetCertReader(nil)
	if svc.certReader != reader {
		t.Fatal("SetCertReader(nil) replaced the wired reader")
	}
}

func TestListAliasesRequiresActor(t *testing.T) {
	svc := New(nil)
	if _, err := svc.ListAliases(context.Background(), ActorContext{}, "env"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ListAliases without actor = %v, want ErrForbidden", err)
	}
	if _, err := svc.GetAlias(context.Background(), ActorContext{}, "id"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("GetAlias without actor = %v, want ErrForbidden", err)
	}
}

func TestListAuditEventsRequiresActor(t *testing.T) {
	svc := New(nil)
	if _, err := svc.ListAuditEvents(context.Background(), ActorContext{}, AuditEventFilter{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ListAuditEvents without actor = %v, want ErrForbidden", err)
	}
}
