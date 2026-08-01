package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strings"

	"github.com/shepard-labs/cyclopus/internal/store"
)

// challengeLabelPrefix is the fixed subdomain label under which apex/root
// domains publish their dns_txt challenge, e.g. _cyclopus-challenge.customer.com
// (§7.5).
const challengeLabelPrefix = "_cyclopus-challenge"

const (
	verificationMethodDNSTXT = "dns_txt"
	verificationMethodCNAME  = "cname"
)

// hostnamePattern validates a DNS hostname: dot-separated labels of letters,
// digits and hyphens (no leading/trailing hyphen), total length <= 253 to match
// the domains.hostname CHECK. Leading underscores and wildcards are rejected —
// callers register concrete apex or subdomain names only.
var hostnamePattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// DNSResolver is the seam over DNS lookups so custom-domain verification can be
// exercised with a fake in tests. The default implementation wraps
// net.DefaultResolver.
type DNSResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupCNAME(ctx context.Context, name string) (string, error)
}

type stdlibDNSResolver struct{}

func (stdlibDNSResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, name)
}

func (stdlibDNSResolver) LookupCNAME(ctx context.Context, name string) (string, error) {
	return net.DefaultResolver.LookupCNAME(ctx, name)
}

// DomainInput is the caller-provided data for CreateDomain. VerificationMethod
// is optional: when empty the method is derived from the hostname shape; when
// provided it must be one of the allowed methods and overrides the heuristic.
type DomainInput struct {
	AppID              string
	Hostname           string
	VerificationMethod string
}

// DomainInstructions is the DNS guidance returned to callers so the UI/CLI can
// render the exact records the customer must create (§7.5).
type DomainInstructions struct {
	RecordType        string
	RecordName        string
	RecordValue       string
	ApexARecordTarget string
}

// domainVerification bundles a stored domain with the DNS setup instructions the
// caller needs. Instructions are recomputed from the domain and platform ingress
// target rather than persisted.
type domainVerification struct {
	Domain       store.Domain
	Instructions DomainInstructions
}

func (s *Service) GetDomain(ctx context.Context, actor ActorContext, id string) (store.Domain, DomainInstructions, error) {
	if err := validateActor(actor); err != nil {
		return store.Domain{}, DomainInstructions{}, err
	}
	value, err := s.store.GetDomain(ctx, actor.OrgID, id)
	if err != nil {
		return store.Domain{}, DomainInstructions{}, err
	}
	return value, s.domainInstructions(value), nil
}

func (s *Service) ListDomains(ctx context.Context, actor ActorContext, appID string) ([]store.Domain, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	if err := validateHostnameID(appID); err != nil {
		return nil, err
	}
	return s.store.ListDomains(ctx, actor.OrgID, appID)
}

// CreateDomain registers a custom domain for an app. It selects the verification
// method (dns_txt for apex, cname for subdomains — or an explicit override),
// generates a random challenge token for dns_txt, and persists the row. The
// method/token invariant is enforced here and again by the DB CHECK.
func (s *Service) CreateDomain(ctx context.Context, actor ActorContext, key string, input DomainInput) (store.Domain, DomainInstructions, error) {
	if err := validateActor(actor); err != nil {
		return store.Domain{}, DomainInstructions{}, err
	}
	hostname := strings.ToLower(strings.TrimSpace(input.Hostname))
	method, err := resolveVerificationMethod(hostname, input.VerificationMethod)
	if err != nil {
		return store.Domain{}, DomainInstructions{}, err
	}
	if err := validateHostnameID(input.AppID); err != nil {
		return store.Domain{}, DomainInstructions{}, err
	}
	var token *string
	if method == verificationMethodDNSTXT {
		generated, err := generateVerificationToken()
		if err != nil {
			return store.Domain{}, DomainInstructions{}, err
		}
		token = &generated
	}
	request := struct {
		AppID, Hostname, Method string
	}{input.AppID, hostname, method}
	var replay domainVerification
	claimed, err := s.claim(ctx, actor, "domain.create", key, request, &replay)
	if err != nil || !claimed {
		return replay.Domain, replay.Instructions, err
	}
	value := store.Domain{ID: newID(), OrgID: actor.OrgID, AppID: input.AppID, Hostname: hostname, VerificationMethod: method, VerificationToken: token}
	value, err = s.store.CreateDomain(ctx, store.SaveDomainParams{Domain: value, Idempotency: s.domainCompletion(actor, "domain.create", key, request, value, 201), Audit: audit(actor)})
	if err != nil {
		return store.Domain{}, DomainInstructions{}, mapError(err)
	}
	return value, s.domainInstructions(value), nil
}

// VerifyDomain resolves the domain's challenge record and, on success, sets
// verified_at. dns_txt confirms the challenge TXT contains the stored token;
// cname confirms the hostname resolves to the platform ingress target. On any
// failure verified_at is left unchanged.
func (s *Service) VerifyDomain(ctx context.Context, actor ActorContext, key, id string) (store.Domain, DomainInstructions, error) {
	if err := validateActor(actor); err != nil {
		return store.Domain{}, DomainInstructions{}, err
	}
	if err := validateHostnameID(id); err != nil {
		return store.Domain{}, DomainInstructions{}, err
	}
	domain, err := s.store.GetDomain(ctx, actor.OrgID, id)
	if err != nil {
		return store.Domain{}, DomainInstructions{}, err
	}
	if err := s.checkDNSChallenge(ctx, domain); err != nil {
		return store.Domain{}, DomainInstructions{}, err
	}
	request := struct{ ID string }{id}
	var replay domainVerification
	claimed, err := s.claim(ctx, actor, "domain.verify", key, request, &replay)
	if err != nil || !claimed {
		return replay.Domain, replay.Instructions, err
	}
	verified, err := s.store.VerifyDomain(ctx, actor.OrgID, id, s.domainCompletion(actor, "domain.verify", key, request, domain, 200), audit(actor))
	if err != nil {
		return store.Domain{}, DomainInstructions{}, mapError(err)
	}
	// verified_at is now committed: the customer proved DNS control. Provision the
	// live routing (custom-domain Ingress + TLS + cert-manager issuer annotation)
	// so traffic actually reaches the app (§7.5). DECISION: provisioning failure
	// must NOT roll back verified_at — verification is a durable fact about DNS
	// control, distinct from whether cluster routing is currently live. On a
	// provisioning error we therefore return the verified domain together with the
	// wrapped error so the caller learns routing is not live yet and can retry
	// (re-verify is idempotent). A nil provisioner (no cluster wiring) is not an
	// error: verification succeeds and provisioning is skipped, mirroring how a
	// nil CertReader degrades gracefully.
	if err := s.provisionCustomDomain(ctx, actor, verified); err != nil {
		return verified, s.domainInstructions(verified), err
	}
	return verified, s.domainInstructions(verified), nil
}

// provisionCustomDomain resolves the app's production environment (org-scoped)
// and asks the provisioner to create/patch the custom-domain Ingress. It is a
// no-op when no provisioner is wired. Provisioning is skipped (not an error)
// when the app has no ready production environment yet — routing is created once
// the environment exists and the operator re-verifies or reconcile runs. It is
// idempotent: the provisioner get-or-creates the Ingress.
func (s *Service) provisionCustomDomain(ctx context.Context, actor ActorContext, domain store.Domain) error {
	if s.customDomainProvisioner == nil {
		s.log().Info("custom_domain.provision_skipped", "event", "custom_domain.provision_skipped",
			"reason", "provisioner_unavailable", "domain_id", domain.ID, "org_id", domain.OrgID)
		return nil
	}
	req, ok, err := s.customDomainRequest(ctx, actor, domain)
	if err != nil {
		return mapError(err)
	}
	if !ok {
		s.log().Info("custom_domain.provision_skipped", "event", "custom_domain.provision_skipped",
			"reason", "production_environment_not_ready", "domain_id", domain.ID, "org_id", domain.OrgID)
		return nil
	}
	if err := s.customDomainProvisioner.ProvisionCustomDomain(ctx, req); err != nil {
		s.log().Warn("custom_domain.provision_failed", "event", "custom_domain.provision_failed",
			"domain_id", domain.ID, "org_id", domain.OrgID, "hostname", domain.Hostname, "error", err.Error())
		return err
	}
	s.log().Info("custom_domain.provisioned", "event", "custom_domain.provisioned",
		"domain_id", domain.ID, "org_id", domain.OrgID, "hostname", domain.Hostname, "namespace", req.Namespace)
	return nil
}

// customDomainRequest resolves the app's production environment (org-scoped) and
// builds the provisioning request. ok is false (with no error) when the app has
// no ready production environment — that is a normal, non-fatal state (the domain
// may be verified before the app is deployed). A cross-org app_id can never
// surface another tenant's environment because the lookup is org-scoped.
func (s *Service) customDomainRequest(ctx context.Context, actor ActorContext, domain store.Domain) (CustomDomainRequest, bool, error) {
	environment, err := s.store.GetProductionEnvironment(ctx, actor.OrgID, domain.AppID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return CustomDomainRequest{}, false, nil
		}
		return CustomDomainRequest{}, false, err
	}
	if environment.Namespace == "" {
		return CustomDomainRequest{}, false, nil
	}
	app, err := s.store.GetApp(ctx, actor.OrgID, domain.AppID)
	if err != nil {
		return CustomDomainRequest{}, false, err
	}
	return CustomDomainRequest{
		Namespace:     environment.Namespace,
		EnvironmentID: environment.ID,
		DomainID:      domain.ID,
		Hostname:      domain.Hostname,
		ServiceName:   app.Slug,
		ServicePort:   80,
		TLSSecretName: CustomDomainTLSSecretName(domain.ID),
		ClusterIssuer: s.certManagerClusterIssuer,
	}, true, nil
}

// CustomDomainTLSSecretName derives the per-domain TLS Secret name from the
// domain's ID (a uuidv7). It is keyed on the ID rather than the hostname so it
// is collision-free: two distinct hostnames (e.g. "a.b.com" and "a-b.com") that
// would sanitize to the same string cannot share — and thereby fight over — one
// TLS Secret. This mirrors the custom-domain Ingress name, which is also
// ID-derived. The result is deterministic so re-provisioning targets the same
// Secret, and is a valid DNS-1123 Secret name (uuid chars are [0-9a-f-], the
// prefix and suffix are label-safe, total length well under 253).
func CustomDomainTLSSecretName(domainID string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, strings.TrimSpace(domainID))
	sanitized = strings.Trim(sanitized, "-")
	if sanitized == "" {
		sanitized = "domain"
	}
	return "custom-domain-" + sanitized + "-tls"
}

func (s *Service) DeleteDomain(ctx context.Context, actor ActorContext, key, id string) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	if err := validateHostnameID(id); err != nil {
		return err
	}
	// Load the domain first so teardown can target the custom-domain Ingress by
	// AppID/Hostname. A missing domain maps to NotFound here rather than at the
	// store delete, and gives us the app_id needed to resolve the namespace.
	domain, err := s.store.GetDomain(ctx, actor.OrgID, id)
	if err != nil {
		return mapError(err)
	}
	request := struct{ ID string }{id}
	var replay struct{}
	claimed, err := s.claim(ctx, actor, "domain.delete", key, request, &replay)
	if err != nil || !claimed {
		return err
	}
	// Tear down the live routing before deleting the row. Deprovision is
	// idempotent (not-found is a no-op) so deleting a never-verified/unprovisioned
	// domain does not error, and a nil provisioner is skipped. Deleting the
	// Ingress lets cert-manager GC the Certificate and TLS Secret it created.
	if err := s.deprovisionCustomDomain(ctx, actor, domain); err != nil {
		return err
	}
	idem := s.completion(actor, "domain.delete", key, request, json.RawMessage(`{}`), 200)
	idem.OrgID = actor.OrgID
	return mapError(s.store.DeleteDomain(ctx, actor.OrgID, id, idem, audit(actor)))
}

// deprovisionCustomDomain deletes the custom-domain Ingress for a domain being
// removed. It resolves the production environment org-scoped; when the app has
// no production environment (nothing was provisioned) it is a no-op. A nil
// provisioner is skipped.
func (s *Service) deprovisionCustomDomain(ctx context.Context, actor ActorContext, domain store.Domain) error {
	if s.customDomainProvisioner == nil {
		return nil
	}
	req, ok, err := s.customDomainRequest(ctx, actor, domain)
	if err != nil {
		return mapError(err)
	}
	if !ok {
		return nil
	}
	if err := s.customDomainProvisioner.DeprovisionCustomDomain(ctx, req); err != nil {
		s.log().Warn("custom_domain.deprovision_failed", "event", "custom_domain.deprovision_failed",
			"domain_id", domain.ID, "org_id", domain.OrgID, "hostname", domain.Hostname, "error", err.Error())
		return err
	}
	s.log().Info("custom_domain.deprovisioned", "event", "custom_domain.deprovisioned",
		"domain_id", domain.ID, "org_id", domain.OrgID, "hostname", domain.Hostname, "namespace", req.Namespace)
	return nil
}

// checkDNSChallenge performs the live DNS lookup that proves control. It is the
// security-critical core of verification: it must fail closed if the record is
// absent, wrong, or the platform ingress target is unknown.
func (s *Service) checkDNSChallenge(ctx context.Context, domain store.Domain) error {
	switch domain.VerificationMethod {
	case verificationMethodDNSTXT:
		if domain.VerificationToken == nil || *domain.VerificationToken == "" {
			return ErrInvalidArgument
		}
		records, err := s.dnsResolver.LookupTXT(ctx, challengeLabelPrefix+"."+domain.Hostname)
		if err != nil {
			return store.ErrFailedPrecondition
		}
		for _, record := range records {
			if strings.TrimSpace(record) == *domain.VerificationToken {
				return nil
			}
		}
		return store.ErrFailedPrecondition
	case verificationMethodCNAME:
		target := s.ingressTarget
		if target == "" {
			// No configured ingress target: we cannot assert the CNAME points at
			// the platform, so verification fails closed rather than accepting an
			// arbitrary target (domain-takeover safety).
			return store.ErrFailedPrecondition
		}
		cname, err := s.dnsResolver.LookupCNAME(ctx, domain.Hostname)
		if err != nil {
			return store.ErrFailedPrecondition
		}
		if canonicalDNSName(cname) != canonicalDNSName(target) {
			return store.ErrFailedPrecondition
		}
		return nil
	default:
		return ErrInvalidArgument
	}
}

func (s *Service) domainInstructions(domain store.Domain) DomainInstructions {
	if domain.VerificationMethod == verificationMethodCNAME {
		return DomainInstructions{RecordType: "CNAME", RecordName: domain.Hostname, RecordValue: s.ingressTarget}
	}
	token := ""
	if domain.VerificationToken != nil {
		token = *domain.VerificationToken
	}
	return DomainInstructions{RecordType: "TXT", RecordName: challengeLabelPrefix + "." + domain.Hostname, RecordValue: token, ApexARecordTarget: s.ingressTarget}
}

func (s *Service) domainCompletion(actor ActorContext, scope, key string, request any, domain store.Domain, status int) store.CompleteIdempotencyParams {
	body, _ := json.Marshal(domainVerification{Domain: domain, Instructions: s.domainInstructions(domain)})
	return s.completion(actor, scope, key, request, body, status)
}

// resolveVerificationMethod picks the verification method for a hostname. An
// explicit override wins after validation; otherwise the method is derived from
// the label count: exactly two labels (customer.com) is treated as an apex and
// uses dns_txt, three or more (app.customer.com) is treated as a subdomain and
// uses cname.
//
// LIMITATION: the label-count heuristic is imperfect for multi-label public
// suffixes (e.g. customer.co.uk is an apex but has three labels and would be
// classified as a subdomain). A full Public Suffix List check is out of scope
// for v1; callers can pass an explicit verification_method to override.
func resolveVerificationMethod(hostname, override string) (string, error) {
	if !hostnamePattern.MatchString(hostname) || len(hostname) > 253 {
		return "", ErrInvalidArgument
	}
	if override != "" {
		if override != verificationMethodDNSTXT && override != verificationMethodCNAME {
			return "", ErrInvalidArgument
		}
		return override, nil
	}
	if strings.Count(hostname, ".") <= 1 {
		return verificationMethodDNSTXT, nil
	}
	return verificationMethodCNAME, nil
}

// generateVerificationToken returns a cryptographically-random hex token used as
// the dns_txt challenge value (§7.5).
func generateVerificationToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// canonicalDNSName normalizes a DNS name for comparison: lowercase and without a
// trailing dot (LookupCNAME returns fully-qualified names ending in ".").
func canonicalDNSName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// validateHostnameID rejects empty/blank IDs before they reach the store; the
// handler layer performs strict UUID validation, this is a defensive guard for
// direct service callers and tests.
func validateHostnameID(id string) error {
	if strings.TrimSpace(id) == "" {
		return ErrInvalidArgument
	}
	return nil
}
