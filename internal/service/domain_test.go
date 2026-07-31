package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cyclopus-dev/cyclopus/internal/store"
)

// fakeDNSResolver is the test double for DNSResolver; each lookup returns the
// preconfigured value or error, letting verification tests drive success and
// failure paths without real DNS.
type fakeDNSResolver struct {
	txt     map[string][]string
	txtErr  map[string]error
	cname   map[string]string
	cnameEr map[string]error
}

func (f fakeDNSResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if err := f.txtErr[name]; err != nil {
		return nil, err
	}
	if records, ok := f.txt[name]; ok {
		return records, nil
	}
	return nil, errors.New("no such host")
}

func (f fakeDNSResolver) LookupCNAME(_ context.Context, name string) (string, error) {
	if err := f.cnameEr[name]; err != nil {
		return "", err
	}
	if target, ok := f.cname[name]; ok {
		return target, nil
	}
	return "", errors.New("no such host")
}

func TestResolveVerificationMethodByShape(t *testing.T) {
	cases := []struct {
		hostname, override, want string
		wantErr                  bool
	}{
		{hostname: "customer.com", want: verificationMethodDNSTXT},
		{hostname: "app.customer.com", want: verificationMethodCNAME},
		{hostname: "a.b.customer.com", want: verificationMethodCNAME},
		{hostname: "customer.com", override: verificationMethodCNAME, want: verificationMethodCNAME},
		{hostname: "app.customer.com", override: verificationMethodDNSTXT, want: verificationMethodDNSTXT},
		{hostname: "customer.com", override: "bogus", wantErr: true},
		{hostname: "not_a_host", wantErr: true},
		{hostname: "nodot", wantErr: true},
		{hostname: "-bad.customer.com", wantErr: true},
		{hostname: strings.Repeat("a", 250) + ".com", wantErr: true},
	}
	for _, tc := range cases {
		got, err := resolveVerificationMethod(tc.hostname, tc.override)
		if tc.wantErr {
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("resolveVerificationMethod(%q,%q) error = %v, want ErrInvalidArgument", tc.hostname, tc.override, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("resolveVerificationMethod(%q,%q) = %q, %v, want %q", tc.hostname, tc.override, got, err, tc.want)
		}
	}
}

func TestGenerateVerificationTokenIsRandomHex(t *testing.T) {
	first, err := generateVerificationToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateVerificationToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("verification tokens must be unique")
	}
	if len(first) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(first))
	}
	for _, r := range first {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("token %q is not lowercase hex", first)
		}
	}
}

func TestCanonicalDNSName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"App.Example.COM.", "app.example.com"},
		{" ingress.example.test ", "ingress.example.test"},
		{"ingress.example.test", "ingress.example.test"},
	} {
		if got := canonicalDNSName(tc.in); got != tc.want {
			t.Fatalf("canonicalDNSName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

const ingressTarget = "ingress.platform.test"

func txtChallenge(hostname string) string { return challengeLabelPrefix + "." + hostname }

func stringPointer(v string) *string { return &v }

func TestCheckDNSChallengeTXTSuccess(t *testing.T) {
	hostname := "customer.com"
	resolver := fakeDNSResolver{txt: map[string][]string{
		txtChallenge(hostname): {"unrelated-record", "cyclopus-token-value"},
	}}
	svc := New(nil, WithDNSResolver(resolver), WithIngressTarget(ingressTarget))
	domain := store.Domain{Hostname: hostname, VerificationMethod: verificationMethodDNSTXT, VerificationToken: stringPointer("cyclopus-token-value")}
	if err := svc.checkDNSChallenge(context.Background(), domain); err != nil {
		t.Fatalf("TXT challenge success = %v", err)
	}
}

func TestCheckDNSChallengeTXTFailures(t *testing.T) {
	hostname := "customer.com"
	svc := func(resolver fakeDNSResolver) *Service {
		return New(nil, WithDNSResolver(resolver), WithIngressTarget(ingressTarget))
	}
	domain := store.Domain{Hostname: hostname, VerificationMethod: verificationMethodDNSTXT, VerificationToken: stringPointer("expected-token")}

	// Missing record (lookup error).
	missing := svc(fakeDNSResolver{})
	if err := missing.checkDNSChallenge(context.Background(), domain); !errors.Is(err, store.ErrFailedPrecondition) {
		t.Fatalf("missing TXT error = %v, want ErrFailedPrecondition", err)
	}
	// Wrong token value present.
	wrong := svc(fakeDNSResolver{txt: map[string][]string{txtChallenge(hostname): {"some-other-token"}}})
	if err := wrong.checkDNSChallenge(context.Background(), domain); !errors.Is(err, store.ErrFailedPrecondition) {
		t.Fatalf("wrong TXT error = %v, want ErrFailedPrecondition", err)
	}
	// dns_txt method with a nil token is a corrupt row -> invalid argument.
	corrupt := svc(fakeDNSResolver{txt: map[string][]string{txtChallenge(hostname): {"expected-token"}}})
	if err := corrupt.checkDNSChallenge(context.Background(), store.Domain{Hostname: hostname, VerificationMethod: verificationMethodDNSTXT}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil-token TXT error = %v, want ErrInvalidArgument", err)
	}
}

func TestCheckDNSChallengeCNAMESuccessAndFailure(t *testing.T) {
	hostname := "app.customer.com"
	// Success: CNAME resolves to ingress target (with trailing dot / case noise).
	ok := New(nil, WithDNSResolver(fakeDNSResolver{cname: map[string]string{hostname: "Ingress.Platform.Test."}}), WithIngressTarget(ingressTarget))
	if err := ok.checkDNSChallenge(context.Background(), store.Domain{Hostname: hostname, VerificationMethod: verificationMethodCNAME}); err != nil {
		t.Fatalf("CNAME success = %v", err)
	}
	// Failure: CNAME points somewhere else (attacker-controlled target).
	wrong := New(nil, WithDNSResolver(fakeDNSResolver{cname: map[string]string{hostname: "evil.attacker.example."}}), WithIngressTarget(ingressTarget))
	if err := wrong.checkDNSChallenge(context.Background(), store.Domain{Hostname: hostname, VerificationMethod: verificationMethodCNAME}); !errors.Is(err, store.ErrFailedPrecondition) {
		t.Fatalf("wrong CNAME error = %v, want ErrFailedPrecondition", err)
	}
	// Failure: no CNAME record at all.
	missing := New(nil, WithDNSResolver(fakeDNSResolver{}), WithIngressTarget(ingressTarget))
	if err := missing.checkDNSChallenge(context.Background(), store.Domain{Hostname: hostname, VerificationMethod: verificationMethodCNAME}); !errors.Is(err, store.ErrFailedPrecondition) {
		t.Fatalf("missing CNAME error = %v, want ErrFailedPrecondition", err)
	}
	// Failure: no configured ingress target -> fail closed even if CNAME resolves.
	noTarget := New(nil, WithDNSResolver(fakeDNSResolver{cname: map[string]string{hostname: "anything.test."}}))
	if err := noTarget.checkDNSChallenge(context.Background(), store.Domain{Hostname: hostname, VerificationMethod: verificationMethodCNAME}); !errors.Is(err, store.ErrFailedPrecondition) {
		t.Fatalf("no-target CNAME error = %v, want ErrFailedPrecondition", err)
	}
}

func TestDomainInstructions(t *testing.T) {
	svc := New(nil, WithIngressTarget(ingressTarget))
	txt := svc.domainInstructions(store.Domain{Hostname: "customer.com", VerificationMethod: verificationMethodDNSTXT, VerificationToken: stringPointer("tok")})
	if txt.RecordType != "TXT" || txt.RecordName != "_cyclopus-challenge.customer.com" || txt.RecordValue != "tok" || txt.ApexARecordTarget != ingressTarget {
		t.Fatalf("dns_txt instructions = %+v", txt)
	}
	cname := svc.domainInstructions(store.Domain{Hostname: "app.customer.com", VerificationMethod: verificationMethodCNAME})
	if cname.RecordType != "CNAME" || cname.RecordName != "app.customer.com" || cname.RecordValue != ingressTarget || cname.ApexARecordTarget != "" {
		t.Fatalf("cname instructions = %+v", cname)
	}
}

func TestCustomDomainTLSSecretName(t *testing.T) {
	for _, tc := range []struct{ domainID, want string }{
		{"0192f1a0-1111-7abc-8def-000000000001", "custom-domain-0192f1a0-1111-7abc-8def-000000000001-tls"},
		{"0192F1A0-2222-7ABC-8DEF-000000000002", "custom-domain-0192f1a0-2222-7abc-8def-000000000002-tls"},
		{"", "custom-domain-domain-tls"},
	} {
		got := CustomDomainTLSSecretName(tc.domainID)
		if got != tc.want {
			t.Fatalf("CustomDomainTLSSecretName(%q) = %q, want %q", tc.domainID, got, tc.want)
		}
		if len(got) > 253 {
			t.Fatalf("secret name %q exceeds 253 chars", got)
		}
	}
	// Collision safety: distinct domain IDs never share a TLS secret, so two
	// hostnames that would sanitize identically (e.g. "a.b.com" and "a-b.com")
	// cannot fight over one Secret because each domain row has a distinct ID.
	a := CustomDomainTLSSecretName("0192f1a0-aaaa-7abc-8def-00000000000a")
	b := CustomDomainTLSSecretName("0192f1a0-bbbb-7abc-8def-00000000000b")
	if a == b {
		t.Fatalf("distinct domain IDs must yield distinct secret names, both = %q", a)
	}
}

func TestDomainActorValidation(t *testing.T) {
	svc := New(nil)
	if _, _, err := svc.CreateDomain(context.Background(), ActorContext{}, "key", DomainInput{AppID: "app", Hostname: "customer.com"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("CreateDomain without actor error = %v, want ErrForbidden", err)
	}
	if _, _, err := svc.VerifyDomain(context.Background(), ActorContext{}, "key", "id"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("VerifyDomain without actor error = %v, want ErrForbidden", err)
	}
	if err := svc.DeleteDomain(context.Background(), ActorContext{}, "key", "id"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("DeleteDomain without actor error = %v, want ErrForbidden", err)
	}
}
