package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

type logPayload struct {
	Name        string         `json:"name"`
	Credentials map[string]any `json:"credentials"`
}

type secretLogValue struct{}

func (secretLogValue) LogValue() slog.Value {
	return slog.AnyValue(map[string]any{
		"token": "valuer-secret",
		"safe":  "valuer-safe",
	})
}

func TestRedaction(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, "info")
	if err != nil {
		t.Fatal(err)
	}

	logger.With("api_token", "bound-secret").Info(
		"request",
		"password", "top-secret",
		"request", slog.GroupValue(
			slog.String("Authorization", "Bearer secret"),
			slog.String("method", "GET"),
		),
	)

	line := output.String()
	for _, secret := range []string{"bound-secret", "top-secret", "Bearer secret"} {
		if strings.Contains(line, secret) {
			t.Errorf("log contains secret %q: %s", secret, line)
		}
	}
	if count := strings.Count(line, redacted); count != 3 {
		t.Fatalf("redaction count = %d, want 3: %s", count, line)
	}
	if !strings.Contains(line, `"method":"GET"`) {
		t.Fatalf("non-sensitive field missing: %s", line)
	}
}

func TestRedactionInCompositeValues(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, "info")
	if err != nil {
		t.Fatal(err)
	}
	pointerValue := any(map[string]any{"secret": "pointer-secret", "safe": "pointer-safe"})

	logger.Info("request",
		"map", map[string]any{
			"authorization": "map-secret",
			"nested": map[string]any{
				"safe":    "map-safe",
				"api_key": "nested-map-secret",
			},
		},
		"struct", logPayload{
			Name: "struct-safe",
			Credentials: map[string]any{
				"password": "struct-secret",
			},
		},
		"slice", []any{
			map[string]any{"private_key": "slice-secret", "safe": "slice-safe"},
			logPayload{Credentials: map[string]any{"cookie": "nested-slice-secret"}},
		},
		"valuer", secretLogValue{},
		"pointer", &pointerValue,
	)

	line := output.String()
	for _, secret := range []string{
		"map-secret",
		"nested-map-secret",
		"struct-secret",
		"slice-secret",
		"nested-slice-secret",
		"valuer-secret",
		"pointer-secret",
	} {
		if strings.Contains(line, secret) {
			t.Errorf("log contains secret %q: %s", secret, line)
		}
	}
	for _, safe := range []string{"map-safe", "struct-safe", "slice-safe", "valuer-safe", "pointer-safe"} {
		if !strings.Contains(line, safe) {
			t.Errorf("log is missing safe value %q: %s", safe, line)
		}
	}
	if count := strings.Count(line, redacted); count != 7 {
		t.Fatalf("redaction count = %d, want 7: %s", count, line)
	}
}

func TestRedactionWithAttrsAndGroups(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, "info")
	if err != nil {
		t.Fatal(err)
	}

	logger.
		With("context", map[string]any{
			"credentials": []any{map[string]any{"token": "with-secret"}},
			"safe":        "with-safe",
		}).
		WithGroup("request").
		With("payload", logPayload{
			Name:        "group-safe",
			Credentials: map[string]any{"password": "group-secret"},
		}).
		Info("handled", slog.Any("details", secretLogValue{}))

	line := output.String()
	for _, secret := range []string{"with-secret", "group-secret", "valuer-secret"} {
		if strings.Contains(line, secret) {
			t.Errorf("log contains secret %q: %s", secret, line)
		}
	}
	for _, safe := range []string{"with-safe", "group-safe", "valuer-safe"} {
		if !strings.Contains(line, safe) {
			t.Errorf("log is missing safe value %q: %s", safe, line)
		}
	}

	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	request, ok := entry["request"].(map[string]any)
	if !ok {
		t.Fatalf("request group = %#v, want object", entry["request"])
	}
	if _, ok := request["payload"]; !ok {
		t.Fatalf("request group is missing payload: %#v", request)
	}
}

func TestUninspectableCompositeIsFullyRedacted(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, "info")
	if err != nil {
		t.Fatal(err)
	}

	cyclic := map[string]any{"password": "cycle-secret"}
	cyclic["self"] = cyclic
	logger.Info("request", "payload", cyclic)

	line := output.String()
	if strings.Contains(line, "cycle-secret") {
		t.Fatalf("log contains secret from uninspectable value: %s", line)
	}
	if !strings.Contains(line, `"payload":"[REDACTED]"`) {
		t.Fatalf("uninspectable value was not fully redacted: %s", line)
	}
}

func TestTokenEnvelopeIsRedactedRegardlessOfKey(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, "info")
	if err != nil {
		t.Fatal(err)
	}
	secret := "cyp_v1_AAAAAAAAAAAA_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	logger.Info("failed "+secret, "value", secret, "payload", map[string]any{"safe": secret})
	if strings.Contains(output.String(), secret) || strings.Count(output.String(), redacted) != 3 {
		t.Fatalf("token envelope was not fully redacted: %s", output.String())
	}
}
