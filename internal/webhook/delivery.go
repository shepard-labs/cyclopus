package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shepard-labs/cyclopus/internal/secretref"
	"github.com/shepard-labs/cyclopus/internal/store"
)

const RequestTimeout = 10 * time.Second

type SecretResolver interface {
	Resolve(context.Context, string) ([]byte, error)
}

type SecretAuthorizer interface {
	Authorize(context.Context, string, string) error
}

type secretReferenceAuthorizer interface {
	AuthorizeReference(context.Context, string) error
}

type Store interface {
	PersistWebhookSignature(context.Context, store.WebhookDelivery, int64, string) error
	CompleteWebhookAttempt(context.Context, store.WebhookDelivery, store.WebhookAttemptResult) (store.WebhookDeliveryOutcome, error)
}

type Sender struct {
	store           Store
	secrets         SecretResolver
	authorizer      SecretAuthorizer
	secretNamespace string
	client          *http.Client
	now             func() time.Time
}

func NewSender(dataStore Store, secrets SecretResolver) *Sender {
	return newSender(dataStore, secrets, nil, "")
}

func NewAuthorizedSender(dataStore Store, secrets SecretResolver, authorizer SecretAuthorizer, secretNamespace string) *Sender {
	return newSender(dataStore, secrets, authorizer, secretNamespace)
}

func newSender(dataStore Store, secrets SecretResolver, authorizer SecretAuthorizer, secretNamespace string) *Sender {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: RequestTimeout}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("webhook target resolved to no addresses")
		}
		for _, ip := range ips {
			if !PublicIPAllowed(ip) {
				return nil, fmt.Errorf("webhook target resolved to non-public address %s", ip)
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
	return &Sender{store: dataStore, secrets: secrets, authorizer: authorizer, secretNamespace: secretNamespace, client: &http.Client{Timeout: RequestTimeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: time.Now}
}

func (s *Sender) Deliver(ctx context.Context, delivery store.WebhookDelivery) (store.WebhookDeliveryOutcome, error) {
	if err := validateTarget(delivery.TargetURL); err != nil {
		return s.finish(ctx, delivery, 0, err)
	}
	timestamp, signature, err := s.signature(ctx, delivery)
	if err != nil {
		return s.finish(ctx, delivery, 0, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, delivery.TargetURL, bytes.NewReader(delivery.RawBody))
	if err != nil {
		return s.finish(ctx, delivery, 0, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cyclopus-Event", delivery.Event)
	req.Header.Set("X-Cyclopus-Delivery", delivery.ID)
	req.Header.Set("X-Cyclopus-Timestamp", strconv.FormatInt(timestamp, 10))
	req.Header.Set("X-Cyclopus-Signature", signature)
	response, err := s.client.Do(req)
	if err != nil {
		return s.finish(ctx, delivery, 0, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return s.finish(ctx, delivery, response.StatusCode, errors.New("redirects are not followed"))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return s.finish(ctx, delivery, response.StatusCode, fmt.Errorf("receiver returned HTTP %d", response.StatusCode))
	}
	return s.store.CompleteWebhookAttempt(context.WithoutCancel(ctx), delivery, store.WebhookAttemptResult{StatusCode: response.StatusCode})
}

func (s *Sender) signature(ctx context.Context, delivery store.WebhookDelivery) (int64, string, error) {
	var reference secretref.Reference
	if s.authorizer != nil {
		parsed, err := secretref.Parse(delivery.SecretRef)
		if err != nil || parsed.Namespace != s.secretNamespace {
			return 0, "", fmt.Errorf("webhook Secret reference must be valid and use control-plane namespace %q", s.secretNamespace)
		}
		reference = parsed
		if exact, ok := s.authorizer.(secretReferenceAuthorizer); ok {
			if err := exact.AuthorizeReference(ctx, delivery.SecretRef); err != nil {
				return 0, "", fmt.Errorf("authorize webhook secret: %w", err)
			}
		} else if err := s.authorizer.Authorize(ctx, reference.Namespace, reference.Name); err != nil {
			return 0, "", fmt.Errorf("authorize webhook secret: %w", err)
		}
	}
	if delivery.FirstSendTimestamp != nil && delivery.Signature != nil {
		return *delivery.FirstSendTimestamp, *delivery.Signature, nil
	}
	secret, err := s.secrets.Resolve(ctx, delivery.SecretRef)
	if err != nil {
		return 0, "", fmt.Errorf("resolve webhook secret: %w", err)
	}
	timestamp := s.now().Unix()
	signature := Sign(secret, timestamp, delivery.RawBody)
	if err := s.store.PersistWebhookSignature(context.WithoutCancel(ctx), delivery, timestamp, signature); err != nil {
		return 0, "", err
	}
	return timestamp, signature, nil
}

func (s *Sender) finish(ctx context.Context, delivery store.WebhookDelivery, status int, cause error) (store.WebhookDeliveryOutcome, error) {
	return s.store.CompleteWebhookAttempt(context.WithoutCancel(ctx), delivery, store.WebhookAttemptResult{StatusCode: status, Error: sanitizeError(cause)})
}

const maxPersistedErrorBytes = 4096

func sanitizeError(cause error) string {
	if cause == nil {
		return ""
	}
	message := cause.Error()
	var urlErr *url.Error
	if errors.As(cause, &urlErr) {
		// url.Error.Error() embeds the full URL (including query secrets); record only the op and cause.
		if urlErr.Err != nil {
			message = urlErr.Op + ": " + urlErr.Err.Error()
		} else {
			message = urlErr.Op
		}
	}
	if len(message) > maxPersistedErrorBytes {
		message = message[:maxPersistedErrorBytes]
	}
	return message
}

func Sign(secret []byte, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func validateTarget(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("invalid HTTPS webhook target")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errors.New("local webhook targets are forbidden")
	}
	if ip := net.ParseIP(host); ip != nil && !PublicIPAllowed(ip) {
		return errors.New("non-public webhook targets are forbidden")
	}
	return nil
}

var nonPublicCIDRs = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT shared address space (RFC 6598)
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking (RFC 2544)
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved (includes 255.255.255.255)
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64 well-known prefix with embedded IPv4 (RFC 6052)
}

// PublicIPAllowed reports whether ip is a public address permitted as a webhook target.
func PublicIPAllowed(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range nonPublicCIDRs {
		if prefix.Contains(addr) {
			return false
		}
	}
	if addr.Is4() {
		return true
	}
	// Reject IPv6 addresses with embedded IPv4 octets unless the embedded v4 is itself allowed.
	if addr.Is4In6() {
		embedded := net.IP(addr.AsSlice()[12:16])
		return PublicIPAllowed(embedded)
	}
	return true
}
