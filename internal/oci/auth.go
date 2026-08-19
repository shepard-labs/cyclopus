package oci

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxTokenResponseBytes = 32 << 10
	maxBearerTokenBytes   = 16 << 10
	maxTokenCacheEntries  = 128
	maxAuthChallenges     = 16
	maxAuthParameters     = 32
	defaultTokenLifetime  = time.Minute
	maxTokenLifetime      = time.Hour
	tokenExpirySkew       = 30 * time.Second
	maxJSONSafeInteger    = int64(1<<53 - 1)
)

type bearerChallenge struct {
	realm   *url.URL
	service string
	scope   string
}

type tokenCacheKey struct {
	host, repository, orgID, registryID string
	generation                          int64
	username, pat                       [sha256.Size]byte
}

type tokenCacheEntry struct {
	key     tokenCacheKey
	token   []byte
	expires time.Time
}

type tokenFlight struct {
	done    chan struct{}
	token   []byte
	err     error
	waiters int
}

type tokenCache struct {
	mu       sync.Mutex
	entries  map[tokenCacheKey]*list.Element
	lru      list.List
	inflight map[tokenCacheKey]*tokenFlight
	closed   bool
}

func newTokenCache() *tokenCache {
	return &tokenCache{entries: make(map[tokenCacheKey]*list.Element), inflight: make(map[tokenCacheKey]*tokenFlight)}
}

func credentialCacheKey(host, repository string, credential *Credential) tokenCacheKey {
	return tokenCacheKey{host: host, repository: repository, orgID: credential.OrgID, registryID: credential.RegistryID, generation: credential.Generation, username: sha256.Sum256(credential.Username), pat: sha256.Sum256(credential.PAT)}
}

func (c *tokenCache) get(key tokenCacheKey, now time.Time) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	entry := element.Value.(*tokenCacheEntry)
	if !entry.expires.After(now.Add(tokenExpirySkew)) {
		c.remove(element)
		return nil, false
	}
	c.lru.MoveToFront(element)
	return bytes.Clone(entry.token), true
}

func (c *tokenCache) evictToken(key tokenCacheKey, token []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[key]; element != nil && bytes.Equal(element.Value.(*tokenCacheEntry).token, token) {
		c.remove(element)
	}
}

func (c *tokenCache) remove(element *list.Element) {
	entry := element.Value.(*tokenCacheEntry)
	clear(entry.token)
	delete(c.entries, entry.key)
	c.lru.Remove(element)
}

func (c *tokenCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for element := c.lru.Front(); element != nil; {
		next := element.Next()
		c.remove(element)
		element = next
	}
}

func (c *tokenCache) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *tokenCache) load(ctx context.Context, key tokenCacheKey, now time.Time, fetch func() ([]byte, time.Time, error)) ([]byte, error) {
	return c.loadOwned(ctx, key, now, fetch, nil)
}

func (c *tokenCache) loadOwned(ctx context.Context, key tokenCacheKey, now time.Time, fetch func() ([]byte, time.Time, error), cleanup func()) ([]byte, error) {
	if token, ok := c.get(key, now); ok {
		if cleanup != nil {
			cleanup()
		}
		return token, nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		return nil, NewError(CodeClientClosed)
	}
	if flight := c.inflight[key]; flight != nil {
		flight.waiters++
		c.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		return c.waitForFlight(ctx, key, flight)
	}
	flight := &tokenFlight{done: make(chan struct{}), waiters: 1}
	c.inflight[key] = flight
	c.mu.Unlock()

	go func() {
		if cleanup != nil {
			defer cleanup()
		}
		c.runFlight(key, now, flight, fetch)
	}()
	return c.waitForFlight(ctx, key, flight)
}

func (c *tokenCache) runFlight(key tokenCacheKey, now time.Time, flight *tokenFlight, fetch func() ([]byte, time.Time, error)) {
	token, expires, err := fetch()
	c.mu.Lock()
	if c.closed {
		clear(token)
		err = NewError(CodeClientClosed)
	}
	flight.err = err
	if err == nil {
		flight.token = bytes.Clone(token)
		if expires.After(now) {
			entry := &tokenCacheEntry{key: key, token: bytes.Clone(flight.token), expires: expires}
			element := c.lru.PushFront(entry)
			c.entries[key] = element
			if c.lru.Len() > maxTokenCacheEntries {
				c.remove(c.lru.Back())
			}
		}
	}
	close(flight.done)
	if flight.waiters == 0 {
		delete(c.inflight, key)
		clear(flight.token)
	}
	c.mu.Unlock()
}

func (c *tokenCache) waitForFlight(ctx context.Context, key tokenCacheKey, flight *tokenFlight) ([]byte, error) {
	select {
	case <-ctx.Done():
		c.releaseFlight(key, flight)
		return nil, ctx.Err()
	case <-flight.done:
		c.mu.Lock()
		var token []byte
		if flight.err == nil {
			token = bytes.Clone(flight.token)
		}
		err := flight.err
		c.releaseFlightLocked(key, flight)
		c.mu.Unlock()
		return token, err
	}
}

func (c *tokenCache) releaseFlight(key tokenCacheKey, flight *tokenFlight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseFlightLocked(key, flight)
}

func (c *tokenCache) releaseFlightLocked(key tokenCacheKey, flight *tokenFlight) {
	flight.waiters--
	if flight.waiters == 0 {
		select {
		case <-flight.done:
			delete(c.inflight, key)
			clear(flight.token)
		default:
		}
	}
}

func parseBearerChallenge(header http.Header, expectedRealm, repository string) (bearerChallenge, error) {
	values := header.Values("WWW-Authenticate")
	if len(values) == 0 || len(values) > maxAuthChallenges {
		return bearerChallenge{}, NewError(CodeRegistryAuthChallengeInvalid)
	}
	var selected map[string]string
	challengeCount := 0
	headerBytes := 0
	for _, raw := range values {
		headerBytes += len(raw)
		if headerBytes > maxResponseHeaders {
			return bearerChallenge{}, NewError(CodeRegistryAuthChallengeInvalid)
		}
		challenges, err := parseAuthChallenges(raw)
		if err != nil {
			return bearerChallenge{}, NewError(CodeRegistryAuthChallengeInvalid)
		}
		challengeCount += len(challenges)
		if challengeCount > maxAuthChallenges {
			return bearerChallenge{}, NewError(CodeRegistryAuthChallengeInvalid)
		}
		for _, challenge := range challenges {
			if !strings.EqualFold(challenge.scheme, "Bearer") {
				continue
			}
			if selected != nil {
				return bearerChallenge{}, NewError(CodeRegistryAuthChallengeInvalid)
			}
			selected = challenge.parameters
		}
	}
	realm, realmOK := selected["realm"]
	service, serviceOK := selected["service"]
	scope, scopeOK := selected["scope"]
	if !realmOK || !serviceOK || !scopeOK || realm != expectedRealm || service != GHCRHost || scope != "repository:"+repository+":pull" {
		return bearerChallenge{}, NewError(CodeRegistryAuthChallengeInvalid)
	}
	realmURL, parseErr := url.Parse(realm)
	if parseErr != nil || realmURL.Scheme != "https" || realmURL.User != nil || realmURL.RawQuery != "" || realmURL.Fragment != "" || realmURL.Opaque != "" {
		return bearerChallenge{}, NewError(CodeRegistryAuthChallengeInvalid)
	}
	return bearerChallenge{realm: realmURL, service: service, scope: scope}, nil
}

type authChallenge struct {
	scheme     string
	parameters map[string]string
}

func parseAuthChallenges(raw string) ([]authChallenge, error) {
	index := 0
	result := make([]authChallenge, 0, 2)
	for {
		skipAuthWhitespace(raw, &index)
		start := index
		for index < len(raw) && authTokenByte(raw[index]) {
			index++
		}
		if start == index {
			return nil, NewError(CodeRegistryAuthChallengeInvalid)
		}
		challenge := authChallenge{scheme: raw[start:index], parameters: make(map[string]string)}
		nextChallenge := false
		if index < len(raw) && raw[index] != ' ' && raw[index] != '\t' && raw[index] != ',' {
			return nil, NewError(CodeRegistryAuthChallengeInvalid)
		}
		hadWhitespace := skipAuthWhitespace(raw, &index)
		if index < len(raw) && raw[index] != ',' {
			if !hadWhitespace {
				return nil, NewError(CodeRegistryAuthChallengeInvalid)
			}
			credentialEnd := index
			for credentialEnd < len(raw) && token68Byte(raw[credentialEnd]) {
				credentialEnd++
			}
			if credentialEnd > index && (credentialEnd == len(raw) || raw[credentialEnd] == ',') {
				index = credentialEnd
				result = append(result, challenge)
				if index == len(raw) {
					return result, nil
				}
				index++
				continue
			}
			for {
				nameStart := index
				for index < len(raw) && authTokenByte(raw[index]) {
					index++
				}
				if nameStart == index || index >= len(raw) || raw[index] != '=' || len(challenge.parameters) >= maxAuthParameters {
					return nil, NewError(CodeRegistryAuthChallengeInvalid)
				}
				name := strings.ToLower(raw[nameStart:index])
				index++
				value, err := parseAuthValue(raw, &index)
				if err != nil {
					return nil, err
				}
				if _, duplicate := challenge.parameters[name]; duplicate {
					return nil, NewError(CodeRegistryAuthChallengeInvalid)
				}
				challenge.parameters[name] = value
				skipAuthWhitespace(raw, &index)
				if index == len(raw) || raw[index] != ',' {
					break
				}
				index++
				skipAuthWhitespace(raw, &index)
				lookahead := index
				for lookahead < len(raw) && authTokenByte(raw[lookahead]) {
					lookahead++
				}
				if lookahead == index {
					return nil, NewError(CodeRegistryAuthChallengeInvalid)
				}
				if lookahead >= len(raw) || raw[lookahead] != '=' {
					nextChallenge = true
					break
				}
			}
		}
		result = append(result, challenge)
		if len(result) > maxAuthChallenges {
			return nil, NewError(CodeRegistryAuthChallengeInvalid)
		}
		if index == len(raw) {
			return result, nil
		}
		if nextChallenge {
			continue
		}
		if raw[index] != ',' {
			return nil, NewError(CodeRegistryAuthChallengeInvalid)
		}
		index++
	}
}

func parseAuthValue(raw string, index *int) (string, error) {
	if *index >= len(raw) {
		return "", NewError(CodeRegistryAuthChallengeInvalid)
	}
	if raw[*index] != '"' {
		start := *index
		for *index < len(raw) && authTokenByte(raw[*index]) {
			*index++
		}
		if start == *index {
			return "", NewError(CodeRegistryAuthChallengeInvalid)
		}
		return raw[start:*index], nil
	}
	*index++
	var value strings.Builder
	for *index < len(raw) {
		character := raw[*index]
		*index++
		if character == '"' {
			return value.String(), nil
		}
		if character == '\\' {
			if *index >= len(raw) {
				return "", NewError(CodeRegistryAuthChallengeInvalid)
			}
			character = raw[*index]
			*index++
		}
		if character < 0x20 || character == 0x7f {
			return "", NewError(CodeRegistryAuthChallengeInvalid)
		}
		value.WriteByte(character)
	}
	return "", NewError(CodeRegistryAuthChallengeInvalid)
}

func skipAuthWhitespace(raw string, index *int) bool {
	start := *index
	for *index < len(raw) && (raw[*index] == ' ' || raw[*index] == '\t') {
		*index++
	}
	return *index != start
}

func authToken(value string) bool {
	for index := range value {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			continue
		}
		return false
	}
	return value != ""
}

func authTokenByte(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character))
}

func token68Byte(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-._~+/=", rune(character))
}

func (c *Client) fetchBearerToken(ctx context.Context, challenge bearerChallenge, credential *Credential) ([]byte, time.Time, error) {
	target := *challenge.realm
	query := target.Query()
	query.Set("service", challenge.service)
	query.Set("scope", challenge.scope)
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, time.Time{}, NewError(CodeRegistryAuthChallengeInvalid)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", clientUserAgent)
	raw := make([]byte, 0, len(credential.Username)+len(credential.PAT)+1)
	raw = append(raw, credential.Username...)
	raw = append(raw, ':')
	raw = append(raw, credential.PAT...)
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
	base64.StdEncoding.Encode(encoded, raw)
	authorization := append([]byte("Basic "), encoded...)
	request.Header.Set("Authorization", string(authorization))
	clear(raw)
	clear(encoded)
	clear(authorization)
	response, doErr := c.httpClient.Do(request)
	request.Header.Del("Authorization")
	if doErr != nil {
		return nil, time.Time{}, doErr
	}
	if response == nil || response.Body == nil {
		return nil, time.Time{}, NewError(CodeRegistryResponseInvalid)
	}
	defer closeResponse(response)
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, time.Time{}, NewError(CodeRegistryResponseInvalid)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, time.Time{}, NewError(CodeRegistryAuthFailed)
	}
	if response.StatusCode != http.StatusOK {
		return nil, time.Time{}, c.statusError(response, ResponseValidation)
	}
	if err := validateIdentityResponse(response); err != nil || response.ContentLength > maxTokenResponseBytes {
		return nil, time.Time{}, NewError(CodeRegistryResponseInvalid)
	}
	rawContentType, contentTypeErr := exactHeader(response.Header, "Content-Type")
	mediaType, parameters, mediaErr := mime.ParseMediaType(rawContentType)
	if contentTypeErr != nil || mediaErr != nil || mediaType != "application/json" || len(parameters) > 1 || len(parameters) == 1 && !strings.EqualFold(parameters["charset"], "utf-8") {
		return nil, time.Time{}, NewError(CodeRegistryResponseInvalid)
	}
	body, readErr := readBounded(response.Body, maxTokenResponseBytes)
	if readErr != nil {
		return nil, time.Time{}, readErr
	}
	defer clear(body)
	return parseTokenResponse(body, c.now())
}

func parseTokenResponse(body []byte, now time.Time) ([]byte, time.Time, error) {
	parser := tokenJSONParser{body: body}
	primary, alternate, expiresIn, issuedAt, err := parser.parse()
	defer clear(primary)
	defer clear(alternate)
	defer clear(issuedAt)
	if err != nil || len(primary) == 0 && len(alternate) == 0 || len(primary) != 0 && len(alternate) != 0 && !bytes.Equal(primary, alternate) {
		return nil, time.Time{}, NewError(CodeRegistryResponseInvalid)
	}
	if len(primary) == 0 {
		primary = alternate
		alternate = nil
	}
	lifetime := defaultTokenLifetime
	if expiresIn != nil {
		seconds, parseErr := strconv.ParseInt(string(expiresIn), 10, 64)
		if parseErr != nil || seconds <= 0 || seconds > maxJSONSafeInteger {
			return nil, time.Time{}, NewError(CodeRegistryResponseInvalid)
		}
		if seconds >= int64(maxTokenLifetime/time.Second) {
			lifetime = maxTokenLifetime
		} else {
			lifetime = time.Duration(seconds) * time.Second
		}
	}
	issued := now
	if issuedAt != nil {
		parsed, parseErr := time.Parse(time.RFC3339, string(issuedAt))
		if parseErr != nil || parsed.After(now.Add(tokenExpirySkew)) {
			return nil, time.Time{}, NewError(CodeRegistryResponseInvalid)
		}
		issued = parsed
	}
	return bytes.Clone(primary), issued.Add(lifetime), nil
}

type tokenJSONParser struct {
	body  []byte
	index int
}

func (p *tokenJSONParser) parse() (primary, alternate, expiresIn, issuedAt []byte, err error) {
	defer func() {
		if err != nil {
			clear(primary)
			clear(alternate)
			clear(issuedAt)
		}
	}()
	p.space()
	if !p.take('{') {
		return nil, nil, nil, nil, NewError(CodeRegistryResponseInvalid)
	}
	seen := make(map[string]bool, 4)
	for {
		p.space()
		if p.take('}') {
			break
		}
		nameBytes, parseErr := p.string(false)
		if parseErr != nil {
			return nil, nil, nil, nil, parseErr
		}
		name := string(nameBytes)
		clear(nameBytes)
		if seen[name] || name != "token" && name != "access_token" && name != "expires_in" && name != "issued_at" {
			return nil, nil, nil, nil, NewError(CodeRegistryResponseInvalid)
		}
		seen[name] = true
		p.space()
		if !p.take(':') {
			return nil, nil, nil, nil, NewError(CodeRegistryResponseInvalid)
		}
		p.space()
		switch name {
		case "token", "access_token":
			value, valueErr := p.string(true)
			if valueErr != nil || !validBearerToken(value) {
				clear(value)
				return nil, nil, nil, nil, NewError(CodeRegistryResponseInvalid)
			}
			if name == "token" {
				primary = value
			} else {
				alternate = value
			}
		case "issued_at":
			issuedAt, err = p.string(false)
			if err != nil {
				return nil, nil, nil, nil, err
			}
		case "expires_in":
			start := p.index
			for p.index < len(p.body) && p.body[p.index] >= '0' && p.body[p.index] <= '9' {
				p.index++
			}
			if start == p.index {
				return nil, nil, nil, nil, NewError(CodeRegistryResponseInvalid)
			}
			expiresIn = p.body[start:p.index]
		}
		p.space()
		if p.take('}') {
			break
		}
		if !p.take(',') {
			return nil, nil, nil, nil, NewError(CodeRegistryResponseInvalid)
		}
	}
	p.space()
	if p.index != len(p.body) {
		return nil, nil, nil, nil, NewError(CodeRegistryResponseInvalid)
	}
	return primary, alternate, expiresIn, issuedAt, nil
}

func (p *tokenJSONParser) string(disallowEscapes bool) ([]byte, error) {
	if !p.take('"') {
		return nil, NewError(CodeRegistryResponseInvalid)
	}
	result := make([]byte, 0, 64)
	for p.index < len(p.body) {
		character := p.body[p.index]
		p.index++
		switch {
		case character == '"':
			if !utf8.Valid(result) {
				clear(result)
				return nil, NewError(CodeRegistryResponseInvalid)
			}
			return result, nil
		case character == '\\':
			if disallowEscapes {
				clear(result)
				return nil, NewError(CodeRegistryResponseInvalid)
			}
			if err := p.escape(&result); err != nil {
				clear(result)
				return nil, err
			}
		case character < 0x20:
			clear(result)
			return nil, NewError(CodeRegistryResponseInvalid)
		default:
			result = append(result, character)
		}
	}
	clear(result)
	return nil, NewError(CodeRegistryResponseInvalid)
}

func (p *tokenJSONParser) escape(result *[]byte) error {
	if p.index >= len(p.body) {
		return NewError(CodeRegistryResponseInvalid)
	}
	character := p.body[p.index]
	p.index++
	switch character {
	case '"', '\\', '/':
		*result = append(*result, character)
	case 'b':
		*result = append(*result, '\b')
	case 'f':
		*result = append(*result, '\f')
	case 'n':
		*result = append(*result, '\n')
	case 'r':
		*result = append(*result, '\r')
	case 't':
		*result = append(*result, '\t')
	case 'u':
		first, err := p.hexRune()
		if err != nil {
			return err
		}
		runeValue := rune(first)
		if utf16.IsSurrogate(runeValue) {
			if p.index+2 > len(p.body) || p.body[p.index] != '\\' || p.body[p.index+1] != 'u' {
				return NewError(CodeRegistryResponseInvalid)
			}
			p.index += 2
			second, secondErr := p.hexRune()
			if secondErr != nil {
				return secondErr
			}
			runeValue = utf16.DecodeRune(runeValue, rune(second))
			if runeValue == utf8.RuneError {
				return NewError(CodeRegistryResponseInvalid)
			}
		}
		*result = utf8.AppendRune(*result, runeValue)
	default:
		return NewError(CodeRegistryResponseInvalid)
	}
	return nil
}

func (p *tokenJSONParser) hexRune() (uint16, error) {
	if p.index+4 > len(p.body) {
		return 0, NewError(CodeRegistryResponseInvalid)
	}
	var value uint16
	for range 4 {
		character := p.body[p.index]
		p.index++
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value |= uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value |= uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value |= uint16(character-'A') + 10
		default:
			return 0, NewError(CodeRegistryResponseInvalid)
		}
	}
	return value, nil
}

func (p *tokenJSONParser) space() {
	for p.index < len(p.body) && (p.body[p.index] == ' ' || p.body[p.index] == '\t' || p.body[p.index] == '\r' || p.body[p.index] == '\n') {
		p.index++
	}
}

func (p *tokenJSONParser) take(character byte) bool {
	if p.index >= len(p.body) || p.body[p.index] != character {
		return false
	}
	p.index++
	return true
}

func validBearerToken(token []byte) bool {
	if len(token) == 0 || len(token) > maxBearerTokenBytes {
		return false
	}
	padding := false
	for _, character := range token {
		if character == '=' {
			padding = true
			continue
		}
		if padding || !bearerTokenByte(character) {
			return false
		}
	}
	return true
}

func bearerTokenByte(character byte) bool {
	return character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || strings.ContainsRune("-._~+/", rune(character))
}
