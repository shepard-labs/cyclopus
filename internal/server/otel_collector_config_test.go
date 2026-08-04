package server

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// collectorConfigMapDoc is one document of the multi-document collector manifest.
// Only the embedded config.yaml of the ConfigMap is asserted on.
type collectorConfigMapDoc struct {
	Kind string `yaml:"kind"`
	Data struct {
		ConfigYAML string `yaml:"config.yaml"`
	} `yaml:"data"`
}

type collectorConfig struct {
	Processors struct {
		K8sattributes struct {
			PodAssociation []struct {
				Sources []struct {
					From string `yaml:"from"`
					Name string `yaml:"name"`
				} `yaml:"sources"`
			} `yaml:"pod_association"`
			Extract struct {
				Labels      []extractRule `yaml:"labels"`
				Annotations []extractRule `yaml:"annotations"`
			} `yaml:"extract"`
		} `yaml:"k8sattributes"`
	} `yaml:"processors"`
	Service struct {
		Pipelines map[string]struct {
			Processors []string `yaml:"processors"`
		} `yaml:"pipelines"`
	} `yaml:"service"`
}

type extractRule struct {
	TagName string `yaml:"tag_name"`
	Key     string `yaml:"key"`
}

// loadCollectorConfig reads infra/observability/otel-collector.yaml, finds the
// ConfigMap document, and parses its embedded collector config.yaml.
func loadCollectorConfig(t *testing.T) collectorConfig {
	t.Helper()
	path := filepath.Join("..", "..", "infra", "observability", "otel-collector.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var embedded string
	for {
		var doc collectorConfigMapDoc
		if err := decoder.Decode(&doc); err != nil {
			break
		}
		if doc.Kind == "ConfigMap" && doc.Data.ConfigYAML != "" {
			embedded = doc.Data.ConfigYAML
			break
		}
	}
	if embedded == "" {
		t.Fatal("ConfigMap with data.config.yaml not found in manifest")
	}
	var cfg collectorConfig
	if err := yaml.Unmarshal([]byte(embedded), &cfg); err != nil {
		t.Fatalf("parse embedded collector config: %v", err)
	}
	return cfg
}

// TestCollectorStampsTenantIdentityBeforeExport verifies the ingest-side tenant
// stamping contract (§8.1): app-supplied tenant attributes are stripped before
// association, the trusted connection association is tried first, and the pod's
// Cyclopus metadata is mapped to the canonical query columns.
func TestCollectorStampsTenantIdentityBeforeExport(t *testing.T) {
	cfg := loadCollectorConfig(t)

	wantOrder := []string{"memory_limiter", "transform/strip-tenant", "k8sattributes", "transform/scrub", "batch"}
	for _, pipeline := range []string{"logs", "traces"} {
		got := cfg.Service.Pipelines[pipeline].Processors
		if !reflect.DeepEqual(got, wantOrder) {
			t.Fatalf("%s pipeline processors = %v, want %v", pipeline, got, wantOrder)
		}
	}

	assoc := cfg.Processors.K8sattributes.PodAssociation
	if len(assoc) == 0 || len(assoc[0].Sources) == 0 || assoc[0].Sources[0].From != "connection" {
		t.Fatalf("first pod_association source = %+v, want connection first (untrusted attrs must not win)", assoc)
	}

	extract := cfg.Processors.K8sattributes.Extract
	labelKey := map[string]string{}
	for _, rule := range extract.Labels {
		labelKey[rule.TagName] = rule.Key
	}
	annKey := map[string]string{}
	for _, rule := range extract.Annotations {
		annKey[rule.TagName] = rule.Key
	}
	if labelKey["environment_id"] != "cyclopus.dev/environment-id" {
		t.Fatalf("environment_id must map from the cyclopus.dev/environment-id label, got %q", labelKey["environment_id"])
	}
	for tag, key := range map[string]string{
		"org_id":     "cyclopus.dev/org-id",
		"app_id":     "cyclopus.dev/app-id",
		"release_id": "cyclopus.dev/release-id",
	} {
		if annKey[tag] != key {
			t.Fatalf("%s must map from the %s annotation, got %q", tag, key, annKey[tag])
		}
	}
}
