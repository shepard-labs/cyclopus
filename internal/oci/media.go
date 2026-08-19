package oci

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

const (
	MediaTypeOCIImageManifest             = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIImageIndex                = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerManifest               = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerList                   = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeOCIConfig                    = "application/vnd.oci.image.config.v1+json"
	MediaTypeDockerConfig                 = "application/vnd.docker.container.image.v1+json"
	MediaTypeOCILayer                     = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeOCILayerGzip                 = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeOCILayerZstd                 = "application/vnd.oci.image.layer.v1.tar+zstd"
	MediaTypeOCINonDistributableLayer     = "application/vnd.oci.image.layer.nondistributable.v1.tar"
	MediaTypeOCINonDistributableLayerGzip = "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip"
	MediaTypeOCINonDistributableLayerZstd = "application/vnd.oci.image.layer.nondistributable.v1.tar+zstd"
	MediaTypeDockerLayer                  = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	MediaTypeDockerForeignLayer           = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"

	MaxManifestBytes        = 8 << 20
	MaxConfigBytes          = 4 << 20
	MaxJSONDepth            = 64
	MaxDescriptorCount      = 1024
	MaxAnnotationCount      = 128
	MaxAnnotationKeyBytes   = 256
	MaxAnnotationValueBytes = 4096
	MaxMediaTypeBytes       = 255
	MaxPlatformStringBytes  = 128
	MaxPlatformFeatureCount = 64
	MaxConfigStringBytes    = 16 << 10
	MaxConfigArrayCount     = 4096
	MaxConfigMapCount       = 1024
	MaxHistoryCount         = 4096
)

var (
	mediaTypeGrammar     = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)
	platformTokenGrammar = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
)

type Platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	Variant      string   `json:"variant,omitempty"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Features     []string `json:"features,omitempty"`
}

type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *Platform         `json:"platform,omitempty"`
}

func (descriptor *Descriptor) UnmarshalJSON(body []byte) error {
	var document struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations,omitempty"`
		Platform    json.RawMessage   `json:"platform,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	var platform *Platform
	if len(document.Platform) != 0 {
		if bytes.Equal(document.Platform, []byte("null")) {
			return NewError(CodeRegistryResponseInvalid)
		}
		var value Platform
		platformDecoder := json.NewDecoder(bytes.NewReader(document.Platform))
		platformDecoder.DisallowUnknownFields()
		if err := platformDecoder.Decode(&value); err != nil {
			return err
		}
		platform = &value
	}
	*descriptor = Descriptor{MediaType: document.MediaType, Digest: document.Digest, Size: document.Size, Annotations: document.Annotations, Platform: platform}
	return nil
}

type ManifestEvidence struct {
	MediaType   string
	Config      *Descriptor
	Layers      []Descriptor
	Manifests   []Descriptor
	Annotations map[string]string
}

type manifestDocument struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        *Descriptor       `json:"config,omitempty"`
	Layers        []Descriptor      `json:"layers,omitempty"`
	Manifests     []Descriptor      `json:"manifests,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type configDocument struct {
	Created         string        `json:"created,omitempty"`
	Author          string        `json:"author,omitempty"`
	Architecture    string        `json:"architecture"`
	Variant         string        `json:"variant,omitempty"`
	OS              string        `json:"os"`
	OSVersion       string        `json:"os.version,omitempty"`
	OSFeatures      []string      `json:"os.features,omitempty"`
	Config          *imageConfig  `json:"config,omitempty"`
	RootFS          *rootFS       `json:"rootfs"`
	History         []historyItem `json:"history,omitempty"`
	Container       string        `json:"container,omitempty"`
	ContainerConfig *imageConfig  `json:"container_config,omitempty"`
	DockerVersion   string        `json:"docker_version,omitempty"`
}

type imageConfig struct {
	Hostname        string                      `json:"Hostname,omitempty"`
	Domainname      string                      `json:"Domainname,omitempty"`
	AttachStdin     bool                        `json:"AttachStdin,omitempty"`
	AttachStdout    bool                        `json:"AttachStdout,omitempty"`
	AttachStderr    bool                        `json:"AttachStderr,omitempty"`
	Tty             bool                        `json:"Tty,omitempty"`
	OpenStdin       bool                        `json:"OpenStdin,omitempty"`
	StdinOnce       bool                        `json:"StdinOnce,omitempty"`
	NetworkDisabled bool                        `json:"NetworkDisabled,omitempty"`
	MacAddress      string                      `json:"MacAddress,omitempty"`
	Env             []string                    `json:"Env,omitempty"`
	Cmd             []string                    `json:"Cmd,omitempty"`
	Entrypoint      []string                    `json:"Entrypoint,omitempty"`
	Labels          map[string]string           `json:"Labels,omitempty"`
	WorkingDir      string                      `json:"WorkingDir,omitempty"`
	User            string                      `json:"User,omitempty"`
	Image           string                      `json:"Image,omitempty"`
	OnBuild         []string                    `json:"OnBuild,omitempty"`
	ExposedPorts    map[string]emptyConfigValue `json:"ExposedPorts,omitempty"`
	Volumes         map[string]emptyConfigValue `json:"Volumes,omitempty"`
	StopSignal      string                      `json:"StopSignal,omitempty"`
	StopTimeout     *int                        `json:"StopTimeout,omitempty"`
	ArgsEscaped     bool                        `json:"ArgsEscaped,omitempty"`
	Shell           []string                    `json:"Shell,omitempty"`
	Healthcheck     *healthConfig               `json:"Healthcheck,omitempty"`
}

type emptyConfigValue struct{}

type healthConfig struct {
	Test          []string `json:"Test,omitempty"`
	Interval      int64    `json:"Interval,omitempty"`
	Timeout       int64    `json:"Timeout,omitempty"`
	StartPeriod   int64    `json:"StartPeriod,omitempty"`
	StartInterval int64    `json:"StartInterval,omitempty"`
	Retries       int      `json:"Retries,omitempty"`
}

type rootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

type historyItem struct {
	Created    string `json:"created,omitempty"`
	CreatedBy  string `json:"created_by,omitempty"`
	Author     string `json:"author,omitempty"`
	Comment    string `json:"comment,omitempty"`
	EmptyLayer bool   `json:"empty_layer,omitempty"`
}

func IsSupportedManifestMediaType(value string) bool {
	switch value {
	case MediaTypeOCIImageManifest, MediaTypeOCIImageIndex, MediaTypeDockerManifest, MediaTypeDockerList:
		return true
	default:
		return false
	}
}

func ConfigMediaType(manifestMediaType string) (string, bool) {
	switch manifestMediaType {
	case MediaTypeOCIImageManifest:
		return MediaTypeOCIConfig, true
	case MediaTypeDockerManifest:
		return MediaTypeDockerConfig, true
	default:
		return "", false
	}
}

func ParseManifest(mediaType string, body []byte) (ManifestEvidence, error) {
	if !IsSupportedManifestMediaType(mediaType) {
		return ManifestEvidence{}, NewError(CodeRegistryMediaTypeUnsupported)
	}
	if len(body) == 0 || len(body) > MaxManifestBytes {
		return ManifestEvidence{}, NewError(CodeRegistryResponseInvalid)
	}
	if err := ValidateJSON(body); err != nil {
		return ManifestEvidence{}, err
	}
	var document manifestDocument
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return ManifestEvidence{}, NewError(CodeRegistryResponseInvalid)
	}
	if document.SchemaVersion != 2 || document.MediaType != mediaType || validateAnnotations(document.Annotations) != nil {
		return ManifestEvidence{}, NewError(CodeRegistryResponseInvalid)
	}
	evidence := ManifestEvidence{MediaType: mediaType, Config: document.Config, Layers: document.Layers,
		Manifests: document.Manifests, Annotations: document.Annotations}
	if expectedConfig, single := ConfigMediaType(mediaType); single {
		if document.Config == nil || document.Config.MediaType != expectedConfig || len(document.Manifests) != 0 || len(document.Layers)+1 > MaxDescriptorCount {
			return ManifestEvidence{}, NewError(CodeRegistryConfigInvalid)
		}
		if err := validateDescriptor(*document.Config, MaxConfigBytes); err != nil {
			return ManifestEvidence{}, NewError(CodeRegistryConfigInvalid)
		}
		for _, descriptor := range document.Layers {
			if !isSupportedLayerMediaType(mediaType, descriptor.MediaType) {
				return ManifestEvidence{}, NewError(CodeRegistryMediaTypeUnsupported)
			}
			if err := validateDescriptor(descriptor, 0); err != nil {
				return ManifestEvidence{}, err
			}
		}
		return evidence, nil
	}
	if document.Config != nil || len(document.Layers) != 0 || len(document.Manifests) == 0 || len(document.Manifests) > MaxDescriptorCount {
		return ManifestEvidence{}, NewError(CodeRegistryResponseInvalid)
	}
	for _, descriptor := range document.Manifests {
		if !IsSupportedManifestMediaType(descriptor.MediaType) {
			return ManifestEvidence{}, NewError(CodeRegistryMediaTypeUnsupported)
		}
		if err := validateDescriptor(descriptor, MaxManifestBytes); err != nil {
			return ManifestEvidence{}, err
		}
	}
	return evidence, nil
}

func ValidateConfigJSON(body []byte) error {
	_, err := ParseConfigLabels(body)
	return err
}

func ParseConfigLabels(body []byte) (map[string]string, error) {
	if len(body) == 0 || len(body) > MaxConfigBytes || ValidateJSON(body) != nil {
		return nil, NewError(CodeRegistryConfigInvalid)
	}
	var document configDocument
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil || validateConfigDocument(document) != nil {
		return nil, NewError(CodeRegistryConfigInvalid)
	}
	if document.Config == nil {
		return nil, nil
	}
	return document.Config.Labels, nil
}

func ValidateJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := validateJSONValue(decoder, 0); err != nil {
		return NewError(CodeRegistryResponseInvalid)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return NewError(CodeRegistryResponseInvalid)
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder, depth int) error {
	if depth > MaxJSONDepth {
		return NewError(CodeRegistryResponseInvalid)
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return NewError(CodeRegistryResponseInvalid)
			}
			if _, duplicate := keys[key]; duplicate {
				return NewError(CodeRegistryResponseInvalid)
			}
			keys[key] = struct{}{}
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return NewError(CodeRegistryResponseInvalid)
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return NewError(CodeRegistryResponseInvalid)
		}
	default:
		return NewError(CodeRegistryResponseInvalid)
	}
	return nil
}

func validateDescriptor(descriptor Descriptor, maxSize int64) error {
	if len(descriptor.MediaType) == 0 || len(descriptor.MediaType) > MaxMediaTypeBytes || !mediaTypeGrammar.MatchString(descriptor.MediaType) {
		return NewError(CodeRegistryMediaTypeUnsupported)
	}
	if _, _, err := ParseDigest(descriptor.Digest); err != nil {
		return err
	}
	if descriptor.Size < 1 || (maxSize > 0 && descriptor.Size > maxSize) || validateAnnotations(descriptor.Annotations) != nil {
		return NewError(CodeRegistryResponseInvalid)
	}
	if descriptor.Platform != nil && validatePlatform(*descriptor.Platform) != nil {
		return NewError(CodeRegistryResponseInvalid)
	}
	return nil
}

func isSupportedLayerMediaType(manifestMediaType, layerMediaType string) bool {
	switch manifestMediaType {
	case MediaTypeOCIImageManifest:
		switch layerMediaType {
		case MediaTypeOCILayer, MediaTypeOCILayerGzip, MediaTypeOCILayerZstd,
			MediaTypeOCINonDistributableLayer, MediaTypeOCINonDistributableLayerGzip, MediaTypeOCINonDistributableLayerZstd:
			return true
		}
	case MediaTypeDockerManifest:
		return layerMediaType == MediaTypeDockerLayer || layerMediaType == MediaTypeDockerForeignLayer
	}
	return false
}

func validatePlatform(platform Platform) error {
	if !validPlatformToken(platform.Architecture, true) || !validPlatformToken(platform.OS, true) ||
		!validPlatformToken(platform.Variant, false) || !validPlatformToken(platform.OSVersion, false) ||
		!validPlatformFeatures(platform.OSFeatures) || !validPlatformFeatures(platform.Features) {
		return NewError(CodeRegistryResponseInvalid)
	}
	return nil
}

func validPlatformToken(value string, required bool) bool {
	if value == "" {
		return !required
	}
	return len(value) <= MaxPlatformStringBytes && platformTokenGrammar.MatchString(value)
}

func validPlatformFeatures(values []string) bool {
	if len(values) > MaxPlatformFeatureCount {
		return false
	}
	for _, value := range values {
		if !validPlatformToken(value, true) {
			return false
		}
	}
	return true
}

func validateConfigDocument(document configDocument) error {
	if !validBoundedString(document.Architecture, true) || !validBoundedString(document.OS, true) ||
		!validBoundedString(document.Created, false) || !validBoundedString(document.Author, false) ||
		!validBoundedString(document.Variant, false) || !validBoundedString(document.OSVersion, false) ||
		!validBoundedString(document.Container, false) || !validBoundedString(document.DockerVersion, false) ||
		!validStringArray(document.OSFeatures) || document.RootFS == nil || document.RootFS.Type != "layers" ||
		len(document.RootFS.DiffIDs) > MaxConfigArrayCount || len(document.History) > MaxHistoryCount {
		return NewError(CodeRegistryConfigInvalid)
	}
	for _, digest := range document.RootFS.DiffIDs {
		if _, _, err := ParseDigest(digest); err != nil {
			return NewError(CodeRegistryConfigInvalid)
		}
	}
	if validateImageConfig(document.Config) != nil || validateImageConfig(document.ContainerConfig) != nil {
		return NewError(CodeRegistryConfigInvalid)
	}
	for _, item := range document.History {
		if !validBoundedString(item.Created, false) || !validBoundedString(item.CreatedBy, false) ||
			!validBoundedString(item.Author, false) || !validBoundedString(item.Comment, false) {
			return NewError(CodeRegistryConfigInvalid)
		}
	}
	return nil
}

func validateImageConfig(config *imageConfig) error {
	if config == nil {
		return nil
	}
	if !validBoundedString(config.Hostname, false) || !validBoundedString(config.Domainname, false) || !validBoundedString(config.MacAddress, false) ||
		!validStringArray(config.Env) || !validStringArray(config.Cmd) || !validStringArray(config.Entrypoint) ||
		!validStringArray(config.OnBuild) || !validStringArray(config.Shell) ||
		!validBoundedString(config.WorkingDir, false) || !validBoundedString(config.User, false) ||
		!validBoundedString(config.Image, false) || !validBoundedString(config.StopSignal, false) || validateAnnotations(config.Labels) != nil ||
		!validConfigMap(config.ExposedPorts) || !validConfigMap(config.Volumes) {
		return NewError(CodeRegistryConfigInvalid)
	}
	if config.StopTimeout != nil && *config.StopTimeout < 0 {
		return NewError(CodeRegistryConfigInvalid)
	}
	if config.Healthcheck != nil && (!validStringArray(config.Healthcheck.Test) || config.Healthcheck.Interval < 0 ||
		config.Healthcheck.Timeout < 0 || config.Healthcheck.StartPeriod < 0 || config.Healthcheck.StartInterval < 0 || config.Healthcheck.Retries < 0) {
		return NewError(CodeRegistryConfigInvalid)
	}
	return nil
}

func validStringArray(values []string) bool {
	if len(values) > MaxConfigArrayCount {
		return false
	}
	for _, value := range values {
		if !validBoundedString(value, false) {
			return false
		}
	}
	return true
}

func validConfigMap(values map[string]emptyConfigValue) bool {
	if len(values) > MaxConfigMapCount {
		return false
	}
	for key := range values {
		if !validBoundedString(key, true) {
			return false
		}
	}
	return true
}

func validBoundedString(value string, required bool) bool {
	if value == "" {
		return !required
	}
	return len(value) <= MaxConfigStringBytes && strings.TrimSpace(value) == value && !strings.ContainsRune(value, '\x00')
}

func validateAnnotations(annotations map[string]string) error {
	if len(annotations) > MaxAnnotationCount {
		return NewError(CodeRegistryResponseInvalid)
	}
	for key, value := range annotations {
		if len(key) == 0 || len(key) > MaxAnnotationKeyBytes || len(value) > MaxAnnotationValueBytes {
			return NewError(CodeRegistryResponseInvalid)
		}
	}
	return nil
}
