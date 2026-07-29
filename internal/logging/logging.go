package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

var bearerTokenPattern = regexp.MustCompile(`cyp_v1_[A-Za-z0-9_-]{12}_[A-Za-z0-9_-]{43}`)

type redactingHandler struct {
	next slog.Handler
}

func New(output io.Writer, level string) (*slog.Logger, error) {
	var slogLevel slog.Level
	switch strings.ToLower(level) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		return nil, fmt.Errorf("unsupported log level %q", level)
	}
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slogLevel})
	return slog.New(redactingHandler{next: handler}), nil
}

func (h redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, redactString(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(redactAttr(attr))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		clean[i] = redactAttr(attr)
	}
	return redactingHandler{next: h.next.WithAttrs(clean)}
}

func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{next: h.next.WithGroup(name)}
}

func redactAttr(attr slog.Attr) slog.Attr {
	if sensitiveKey(attr.Key) {
		return slog.String(attr.Key, redacted)
	}
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindAny && composite(value.Any()) {
		clean, err := redactAny(value.Any())
		if err != nil {
			return slog.String(attr.Key, redacted)
		}
		return slog.Any(attr.Key, clean)
	}
	if value.Kind() != slog.KindGroup {
		if value.Kind() == slog.KindString {
			attr.Value = slog.StringValue(redactString(value.String()))
			return attr
		}
		attr.Value = value
		return attr
	}
	group := value.Group()
	clean := make([]slog.Attr, len(group))
	for i, child := range group {
		clean[i] = redactAttr(child)
	}
	return slog.Group(attr.Key, attrsToAny(clean)...)
}

// Composite Any values follow encoding/json field and marshaling rules. Redaction
// is key-based; if a composite value cannot be inspected safely, the whole value
// is redacted rather than passed through.
func redactAny(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return redactJSON(decoded), nil
}

func redactJSON(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if sensitiveKey(key) {
				value[key] = redacted
			} else {
				value[key] = redactJSON(child)
			}
		}
	case []any:
		for i := range value {
			value[i] = redactJSON(value[i])
		}
	case string:
		return redactString(value)
	}
	return value
}

func redactString(value string) string {
	return bearerTokenPattern.ReplaceAllString(value, redacted)
}

func composite(value any) bool {
	if value == nil {
		return false
	}
	reflected := reflect.ValueOf(value)
	kind := reflected.Kind()
	for kind == reflect.Pointer || kind == reflect.Interface {
		if reflected.IsNil() {
			return false
		}
		reflected = reflected.Elem()
		kind = reflected.Kind()
	}
	return kind == reflect.Map || kind == reflect.Struct || kind == reflect.Slice || kind == reflect.Array
}

func attrsToAny(attrs []slog.Attr) []any {
	values := make([]any, len(attrs))
	for i := range attrs {
		values[i] = attrs[i]
	}
	return values
}

func sensitiveKey(key string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(key))
	for _, denied := range []string{"authorization", "cookie", "credential", "password", "privatekey", "secret", "token", "apikey"} {
		if strings.Contains(normalized, denied) {
			return true
		}
	}
	return false
}
