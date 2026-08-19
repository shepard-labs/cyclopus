package oci

import (
	"fmt"
	"strings"
	"testing"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestManifestMediaPairingAndDescriptors(t *testing.T) {
	manifestCases := []struct{ manifest, config, layer string }{
		{MediaTypeOCIImageManifest, MediaTypeOCIConfig, MediaTypeOCILayerGzip},
		{MediaTypeDockerManifest, MediaTypeDockerConfig, MediaTypeDockerLayer},
	}
	for _, tc := range manifestCases {
		body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":2},"layers":[{"mediaType":%q,"digest":%q,"size":1}]}`, tc.manifest, tc.config, testDigest, tc.layer, testDigest))
		parsed, err := ParseManifest(tc.manifest, body)
		if err != nil || parsed.Config == nil || len(parsed.Layers) != 1 {
			t.Fatalf("ParseManifest(%s) = %+v, %v", tc.manifest, parsed, err)
		}
	}
	for _, tc := range []struct{ mediaType, childMediaType string }{
		{MediaTypeOCIImageIndex, MediaTypeOCIImageManifest},
		{MediaTypeDockerList, MediaTypeDockerManifest},
	} {
		body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":1,"platform":{"architecture":"amd64","os":"linux"}},{"mediaType":%q,"digest":%q,"size":1,"platform":{"architecture":"arm64","os":"linux","variant":"v8","os.version":"10.0.20348","os.features":["win32k"],"features":["sse4"]}}]}`, tc.mediaType, tc.childMediaType, testDigest, tc.childMediaType, testDigest))
		parsed, err := ParseManifest(tc.mediaType, body)
		if err != nil || len(parsed.Manifests) != 2 || parsed.Config != nil || parsed.Manifests[1].Platform == nil || parsed.Manifests[1].Platform.Architecture != "arm64" {
			t.Fatalf("ParseManifest(%s) = %+v, %v", tc.mediaType, parsed, err)
		}
	}

	invalid := []struct{ mediaType, body string }{
		{"application/json", `{}`},
		{MediaTypeOCIImageManifest, `{"schemaVersion":1,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`},
		{MediaTypeOCIImageManifest, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","digest":"` + testDigest + `","size":1},"layers":[]}`},
		{MediaTypeOCIImageIndex, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`},
		{MediaTypeOCIImageIndex, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[],"unknown":true}`},
	}
	for _, tc := range invalid {
		if _, err := ParseManifest(tc.mediaType, []byte(tc.body)); err == nil {
			t.Fatalf("ParseManifest(%s, %s) succeeded", tc.mediaType, tc.body)
		}
	}
}

func TestManifestRejectsInvalidPlatformsAndLayerMediaTypes(t *testing.T) {
	for _, platform := range []string{
		`{"os":"linux"}`,
		`{"architecture":"amd64"}`,
		`{"architecture":"AMD64","os":"linux"}`,
		`{"architecture":"amd64","os":"linux","variant":"v 8"}`,
		`{"architecture":"amd64","os":"linux","unknown":true}`,
		`null`,
	} {
		body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":1,"platform":%s}]}`, MediaTypeOCIImageIndex, MediaTypeOCIImageManifest, testDigest, platform))
		if _, err := ParseManifest(MediaTypeOCIImageIndex, body); err == nil {
			t.Fatalf("platform %s accepted", platform)
		}
	}

	allowed := map[string][]string{
		MediaTypeOCIImageManifest: {MediaTypeOCILayer, MediaTypeOCILayerGzip, MediaTypeOCILayerZstd, MediaTypeOCINonDistributableLayer, MediaTypeOCINonDistributableLayerGzip, MediaTypeOCINonDistributableLayerZstd},
		MediaTypeDockerManifest:   {MediaTypeDockerLayer, MediaTypeDockerForeignLayer},
	}
	configs := map[string]string{MediaTypeOCIImageManifest: MediaTypeOCIConfig, MediaTypeDockerManifest: MediaTypeDockerConfig}
	for manifestMediaType, layerMediaTypes := range allowed {
		for _, layerMediaType := range layerMediaTypes {
			body := manifestBody(manifestMediaType, configs[manifestMediaType], layerMediaType)
			if _, err := ParseManifest(manifestMediaType, body); err != nil {
				t.Fatalf("allowed layer %q rejected: %v", layerMediaType, err)
			}
		}
	}
	for _, tc := range []struct{ manifest, layer string }{
		{MediaTypeOCIImageManifest, MediaTypeDockerLayer},
		{MediaTypeDockerManifest, MediaTypeOCILayerGzip},
		{MediaTypeOCIImageManifest, "application/vnd.example.layer.v1.tar+gzip"},
	} {
		_, err := ParseManifest(tc.manifest, manifestBody(tc.manifest, configs[tc.manifest], tc.layer))
		if !IsCode(err, CodeRegistryMediaTypeUnsupported) {
			t.Fatalf("unsupported layer %q error = %v", tc.layer, err)
		}
	}
}

func manifestBody(manifestMediaType, configMediaType, layerMediaType string) []byte {
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":2},"layers":[{"mediaType":%q,"digest":%q,"size":1}]}`, manifestMediaType, configMediaType, testDigest, layerMediaType, testDigest))
}

func TestManifestLimitsAndStrictJSON(t *testing.T) {
	for _, body := range []string{
		`{"a":1,"a":2}`,
		`{"outer":{"a":1,"a":2}}`,
		`{} {}`,
		`{"a":1} trailing`,
	} {
		if err := ValidateJSON([]byte(body)); err == nil {
			t.Fatalf("ValidateJSON(%q) succeeded", body)
		}
	}
	validDepth := strings.Repeat(`[`, MaxJSONDepth) + `0` + strings.Repeat(`]`, MaxJSONDepth)
	if err := ValidateJSON([]byte(validDepth)); err != nil {
		t.Fatalf("depth boundary rejected: %v", err)
	}
	tooDeep := `[` + validDepth + `]`
	if err := ValidateJSON([]byte(tooDeep)); err == nil {
		t.Fatal("depth +1 accepted")
	}
	if err := ValidateConfigJSON(make([]byte, MaxConfigBytes+1)); !IsCode(err, CodeRegistryConfigInvalid) {
		t.Fatalf("config overflow error = %v", err)
	}
	validConfig := `{"created":"2026-08-19T12:00:00Z","author":"Acme","architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/bin"],"Cmd":["/app"],"Entrypoint":["/bin/sh"],"Labels":{"org.opencontainers.image.source":"https://github.com/acme/api"},"WorkingDir":"/srv","User":"1000","ExposedPorts":{"8080/tcp":{}},"Volumes":{"/data":{}},"StopSignal":"SIGTERM"},"rootfs":{"type":"layers","diff_ids":["` + testDigest + `"]},"history":[{"created":"2026-08-19T12:00:00Z","created_by":"COPY . /srv","author":"Acme","comment":"build"}]}`
	labels, err := ParseConfigLabels([]byte(validConfig))
	if err != nil || labels[AnnotationSource] != "https://github.com/acme/api" {
		t.Fatalf("ParseConfigLabels() = %#v, %v", labels, err)
	}
	if _, err := ParseConfigLabels([]byte(strings.Replace(validConfig, `"org.opencontainers.image.source":"https://github.com/acme/api"`, `"a":"1","a":"2"`, 1))); !IsCode(err, CodeRegistryConfigInvalid) {
		t.Fatalf("duplicate config labels error = %v", err)
	}
	for _, body := range []string{
		`{}`,
		`{"architecture":null,"os":"linux","rootfs":{"type":"layers","diff_ids":["` + testDigest + `"]}}`,
		`{"architecture":"amd64","os":null,"rootfs":{"type":"layers","diff_ids":["` + testDigest + `"]}}`,
		`{"architecture":"amd64","os":"linux","rootfs":null}`,
		`{"architecture":"amd64","os":"linux","rootfs":{"type":"other","diff_ids":["` + testDigest + `"]}}`,
		`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha512:bad"]}}`,
		`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + testDigest + `"]},"unknown":true}`,
		`{"architecture":"amd64","os":"linux","config":{"Labels":null},"rootfs":{"type":"layers","diff_ids":["` + testDigest + `"]}} trailing`,
	} {
		if err := ValidateConfigJSON([]byte(body)); !IsCode(err, CodeRegistryConfigInvalid) {
			t.Fatalf("invalid config %s error = %v", body, err)
		}
	}
	if err := ValidateConfigJSON([]byte(`{"architecture":"arm64","variant":"v8","os":"linux","rootfs":{"type":"layers","diff_ids":["` + testDigest + `"]}}`)); err != nil {
		t.Fatalf("minimal config rejected: %v", err)
	}
	if err := ValidateConfigJSON([]byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)); err != nil {
		t.Fatalf("scratch config rejected: %v", err)
	}

	descriptors := make([]string, MaxDescriptorCount+1)
	for i := range descriptors {
		descriptors[i] = fmt.Sprintf(`{"mediaType":%q,"digest":%q,"size":1}`, MediaTypeOCIImageManifest, testDigest)
	}
	for count := MaxDescriptorCount; count <= MaxDescriptorCount+1; count++ {
		body := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[%s]}`, MediaTypeOCIImageIndex, strings.Join(descriptors[:count], ","))
		_, err := ParseManifest(MediaTypeOCIImageIndex, []byte(body))
		if (count == MaxDescriptorCount) != (err == nil) {
			t.Fatalf("descriptor count %d error = %v", count, err)
		}
	}
}

func FuzzValidateJSON(f *testing.F) {
	f.Add([]byte(`{"a":[1,true,null]}`))
	f.Add([]byte(`{"a":1,"a":2}`))
	f.Fuzz(func(t *testing.T, body []byte) { _ = ValidateJSON(body) })
}
