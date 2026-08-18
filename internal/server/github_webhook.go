package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shepard-labs/cyclopus/internal/config"
	"github.com/shepard-labs/cyclopus/internal/githubmetrics"
	"github.com/shepard-labs/cyclopus/internal/store"
)

type GitHubWebhookSecretResolver interface {
	Resolve(context.Context, string) ([]byte, error)
}
type GitHubWebhookStore interface {
	LookupGitHubWebhook(context.Context, store.LookupGitHubWebhookParams) (store.GitHubWebhookLookupDisposition, store.GitHubWebhookIdentity, error)
	ResolveGitHubWebhookIdentity(context.Context, *int64, *int64, *string) (store.GitHubWebhookIdentity, error)
	ReceiveGitHubWebhook(context.Context, store.ReceiveGitHubWebhookParams) (store.GitHubWebhookReceiveDisposition, store.GitHubWebhookDelivery, error)
}

type githubWebhookHandler struct {
	cfg     config.Webhook
	secrets GitHubWebhookSecretResolver
	store   GitHubWebhookStore
	sem     chan struct{}
	limiter *authRateLimiter
	metrics githubmetrics.Recorder
	now     func() time.Time
}

var githubEvent = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func NewGitHubWebhookHandler(cfg config.Webhook, secrets GitHubWebhookSecretResolver, inbox GitHubWebhookStore, recorders ...githubmetrics.Recorder) http.Handler {
	if cfg.GitHubRateLimitPerMinute == 0 && cfg.GitHubRateLimitBurst == 0 {
		cfg.GitHubRateLimitEnabled, cfg.GitHubRateLimitPerMinute, cfg.GitHubRateLimitBurst = true, 120, 30
	}
	metrics := githubmetrics.Noop()
	if len(recorders) > 0 && recorders[0] != nil {
		metrics = recorders[0]
	}
	var limiter *authRateLimiter
	if cfg.GitHubRateLimitEnabled {
		limiter = newWebhookRateLimiter(cfg.GitHubRateLimitPerMinute, cfg.GitHubRateLimitBurst)
	}
	return &githubWebhookHandler{cfg: cfg, secrets: secrets, store: inbox, sem: make(chan struct{}, cfg.GitHubMaxConcurrent), limiter: limiter, metrics: metrics, now: time.Now}
}

func (h *githubWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		fixed(w, http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Content-Encoding") != "" {
		fixed(w, http.StatusUnsupportedMediaType)
		return
	}
	contentType, ok := oneHeader(r.Header, "Content-Type")
	if !ok {
		fixed(w, http.StatusUnsupportedMediaType)
		return
	}
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || media != "application/json" {
		fixed(w, http.StatusUnsupportedMediaType)
		return
	}
	for k := range params {
		if !strings.EqualFold(k, "charset") {
			fixed(w, http.StatusUnsupportedMediaType)
			return
		}
	}
	if cs, ok := params["charset"]; ok && !strings.EqualFold(cs, "utf-8") {
		fixed(w, http.StatusUnsupportedMediaType)
		return
	}
	if allowed, _ := h.limiter.allowKey(webhookClientIP(r.RemoteAddr)); !allowed {
		h.metrics.Ingress("", "", "rate_limited")
		fixed(w, http.StatusTooManyRequests)
		return
	}
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		h.metrics.Ingress("", "", "overload")
		fixed(w, http.StatusTooManyRequests)
		return
	}
	delivery, event, signature, ok := authHeaders(r.Header)
	if !ok {
		h.metrics.Ingress("", "", "auth_invalid")
		fixed(w, http.StatusUnauthorized)
		return
	}
	parentCtx := r.Context()
	controller := http.NewResponseController(w)
	deadlineSet := h.cfg.GitHubBodyReadTimeout > 0 && controller.SetReadDeadline(h.now().Add(h.cfg.GitHubBodyReadTimeout)) == nil
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.GitHubBodyLimit)
	body, err := io.ReadAll(r.Body)
	if deadlineSet {
		_ = controller.SetReadDeadline(time.Time{})
	}
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			fixed(w, http.StatusRequestEntityTooLarge)
		} else if deadlineSet && isBodyReadTimeout(err) {
			fixed(w, http.StatusServiceUnavailable)
		} else {
			fixed(w, http.StatusBadRequest)
		}
		return
	}
	if err := parentCtx.Err(); err != nil {
		fixed(w, http.StatusServiceUnavailable)
		return
	}
	matched, unavailable := h.verify(parentCtx, body, signature)
	if unavailable {
		fixed(w, http.StatusServiceUnavailable)
		return
	}
	if matched == "" {
		h.metrics.Ingress(event, "", "auth_invalid")
		fixed(w, http.StatusUnauthorized)
		return
	}
	canonical, err := canonicalPayload(event, body)
	if err != nil {
		fixed(w, http.StatusBadRequest)
		return
	}
	// Ping is scoped to the GitHub App rather than any tenant. Authentication is
	// sufficient; replay and conflict semantics do not apply because no inbox row
	// is created.
	if event == "ping" {
		h.metrics.Ingress(event, canonical.action, "accepted")
		fixed(w, http.StatusAccepted)
		return
	}
	digest := sha256.Sum256(body)
	payloadDigest := hex.EncodeToString(digest[:])
	lookup, _, err := h.store.LookupGitHubWebhook(parentCtx, store.LookupGitHubWebhookParams{DeliveryID: delivery, Event: event, Action: canonical.action, PayloadDigest: payloadDigest, InstallationID: canonical.installationID, RepositoryID: canonical.repositoryID, RepositoryNodeID: canonical.nodeID})
	if err != nil {
		fixed(w, http.StatusServiceUnavailable)
		return
	}
	switch lookup {
	case store.GitHubWebhookReplay:
		h.metrics.Ingress(event, canonical.action, "replayed")
		fixed(w, http.StatusAccepted)
		return
	case store.GitHubWebhookMismatch:
		h.metrics.Ingress(event, canonical.action, "conflict")
		fixed(w, http.StatusConflict)
		return
	case store.GitHubWebhookNotFound:
	default:
		fixed(w, http.StatusServiceUnavailable)
		return
	}
	identity, err := h.store.ResolveGitHubWebhookIdentity(parentCtx, canonical.installationID, canonical.repositoryID, canonical.nodeID)
	if err != nil {
		fixed(w, http.StatusServiceUnavailable)
		return
	}
	disposition, _, err := h.store.ReceiveGitHubWebhook(parentCtx, store.ReceiveGitHubWebhookParams{DeliveryID: delivery, Event: event, Action: canonical.action, OrgID: identity.OrgID, PayloadDigest: payloadDigest, SignatureKeyVersion: matched, Payload: body, InstallationID: identity.InstallationID, RepositoryID: identity.RepositoryID, RepositoryNodeID: identity.RepositoryNodeID})
	if err != nil {
		fixed(w, http.StatusServiceUnavailable)
		return
	}
	switch disposition {
	case store.GitHubWebhookAccepted:
		h.metrics.Ingress(event, canonical.action, "accepted")
		fixed(w, http.StatusAccepted)
	case store.GitHubWebhookReplayed:
		h.metrics.Ingress(event, canonical.action, "replayed")
		fixed(w, http.StatusAccepted)
	case store.GitHubWebhookConflict:
		h.metrics.Ingress(event, canonical.action, "conflict")
		fixed(w, http.StatusConflict)
	default:
		fixed(w, http.StatusServiceUnavailable)
	}
}

func isBodyReadTimeout(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func repositoryObject(repository map[string]json.RawMessage) json.RawMessage {
	raw, _ := json.Marshal(repository)
	return raw
}

func fixed(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(code)
}
func authHeaders(header http.Header) (string, string, []byte, bool) {
	d, ok1 := oneHeader(header, "X-GitHub-Delivery")
	e, ok2 := oneHeader(header, "X-GitHub-Event")
	s, ok3 := oneHeader(header, "X-GitHub-Signature-256")
	if !ok1 || !ok2 || !ok3 || uuid.Validate(d) != nil || !githubEvent.MatchString(e) || len(s) != 71 || !strings.HasPrefix(s, "sha256=") {
		return "", "", nil, false
	}
	decoded, err := hex.DecodeString(s[7:])
	if err != nil || len(decoded) != 32 || s[7:] != strings.ToLower(s[7:]) {
		return "", "", nil, false
	}
	return d, e, decoded, true
}
func oneHeader(header http.Header, name string) (string, bool) {
	values, exists := header[http.CanonicalHeaderKey(name)]
	if !exists || len(values) != 1 || strings.Contains(values[0], ",") {
		return "", false
	}
	return values[0], true
}
func (h *githubWebhookHandler) verify(ctx context.Context, body, signature []byte) (string, bool) {
	active, err := h.secrets.Resolve(ctx, h.cfg.GitHubActiveSecretRef)
	if err != nil {
		return "", true
	}
	defer clear(active)
	a := hmac.New(sha256.New, active)
	_, _ = a.Write(body)
	activeOK := hmac.Equal(a.Sum(nil), signature)
	if activeOK {
		return h.cfg.GitHubActiveKeyVersion, false
	}
	if h.cfg.GitHubPreviousSecretRef != "" && h.now().Before(h.cfg.GitHubSecretOverlapUntil) {
		previous, e := h.secrets.Resolve(ctx, h.cfg.GitHubPreviousSecretRef)
		if e != nil {
			return "", true
		}
		defer clear(previous)
		p := hmac.New(sha256.New, previous)
		_, _ = p.Write(body)
		if hmac.Equal(p.Sum(nil), signature) {
			return h.cfg.GitHubPreviousKeyVersion, false
		}
	}
	return "", false
}

type canonicalGitHubPayload struct {
	action                       string
	installationID, repositoryID *int64
	nodeID                       *string
}

func canonicalPayload(event string, body []byte) (canonicalGitHubPayload, error) {
	if err := rejectDuplicateJSON(body); err != nil {
		return canonicalGitHubPayload{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root map[string]json.RawMessage
	if err := dec.Decode(&root); err != nil {
		return canonicalGitHubPayload{}, err
	}
	if root == nil {
		return canonicalGitHubPayload{}, errors.New("object required")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return canonicalGitHubPayload{}, errors.New("trailing JSON")
	}
	var out canonicalGitHubPayload
	if raw := root["action"]; raw != nil {
		if json.Unmarshal(raw, &out.action) != nil || !regexp.MustCompile(`^[a-z0-9_]{1,63}$`).MatchString(out.action) {
			return out, errors.New("invalid action")
		}
	}
	extractID := func(raw json.RawMessage) (*int64, error) {
		if raw == nil {
			return nil, nil
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return nil, errors.New("invalid identity")
		}
		var n json.Number
		if json.Unmarshal(obj["id"], &n) != nil {
			return nil, errors.New("invalid identity")
		}
		v, e := n.Int64()
		if e != nil || v <= 0 {
			return nil, errors.New("invalid identity")
		}
		return &v, nil
	}
	var err error
	out.installationID, err = extractID(root["installation"])
	if err != nil {
		return out, err
	}
	out.repositoryID, err = extractID(root["repository"])
	if err != nil {
		return out, err
	}
	if raw := root["repository"]; raw != nil {
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(raw, &obj)
		var node string
		if json.Unmarshal(obj["node_id"], &node) != nil || strings.TrimSpace(node) == "" {
			return out, errors.New("invalid repository node")
		}
		out.nodeID = &node
	}
	switch event {
	case "ping":
		if out.action != "" || out.installationID != nil || out.repositoryID != nil {
			return out, errors.New("ping schema")
		}
	case "installation":
		if out.installationID == nil || out.repositoryID != nil || out.action == "" {
			return out, errors.New("installation schema")
		}
	case "installation_repositories":
		if (out.action != "added" && out.action != "removed") || out.installationID == nil || out.repositoryID != nil || out.nodeID != nil {
			return out, errors.New("installation repositories schema")
		}
		selected, opposite := "repositories_added", "repositories_removed"
		if out.action == "removed" {
			selected, opposite = opposite, selected
		}
		if root[selected] == nil || root[opposite] == nil {
			return out, errors.New("installation repositories arrays")
		}
		var repositories []map[string]json.RawMessage
		if json.Unmarshal(root[selected], &repositories) != nil || repositories == nil {
			return out, errors.New("invalid installation repositories")
		}
		var oppositeRepositories []map[string]json.RawMessage
		if json.Unmarshal(root[opposite], &oppositeRepositories) != nil || oppositeRepositories == nil || len(oppositeRepositories) != 0 {
			return out, errors.New("invalid opposite installation repositories")
		}
		seenIDs, seenNodes := map[int64]struct{}{}, map[string]struct{}{}
		for _, repository := range repositories {
			id, err := extractID(repositoryObject(repository))
			if err != nil || id == nil {
				return out, errors.New("invalid installation repository")
			}
			var node string
			if json.Unmarshal(repository["node_id"], &node) != nil || strings.TrimSpace(node) == "" {
				return out, errors.New("invalid installation repository node")
			}
			if _, duplicate := seenIDs[*id]; duplicate {
				return out, errors.New("duplicate installation repository")
			}
			if _, duplicate := seenNodes[node]; duplicate {
				return out, errors.New("duplicate installation repository")
			}
			seenIDs[*id], seenNodes[node] = struct{}{}, struct{}{}
		}
	case "push":
		if out.action != "" || out.installationID == nil || out.repositoryID == nil || out.nodeID == nil {
			return out, errors.New("repository schema")
		}
	default:
		if out.repositoryID != nil && out.installationID == nil {
			return out, errors.New("ambiguous repository identity")
		}
	}
	return out, nil
}
func rejectDuplicateJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		t, e := d.Token()
		if e != nil {
			return e
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				key := k.(string)
				if _, ok := seen[key]; ok {
					return errors.New("duplicate key")
				}
				seen[key] = struct{}{}
				if e = walk(); e != nil {
					return e
				}
			}
			_, e = d.Token()
			return e
		case '[':
			for d.More() {
				if e := walk(); e != nil {
					return e
				}
			}
			_, e = d.Token()
			return e
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
