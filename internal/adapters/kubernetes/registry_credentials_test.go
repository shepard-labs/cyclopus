package kubernetes

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/shepard-labs/cyclopus/internal/oci"
)

func TestRegistryCredentialSecretRoundTripAndIdempotence(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("create", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
		secret := action.(ktesting.CreateAction).GetObject().(*corev1.Secret)
		secret.UID, secret.ResourceVersion = types.UID("uid-one"), "1"
		return false, nil, nil
	})
	adapter, err := NewRegistryCredentialSecretAdapter(client, "cyclopus")
	if err != nil {
		t.Fatal(err)
	}
	credential := &oci.Credential{Username: []byte("octocat"), PAT: []byte("ghp_secret"), OrgID: "org-one", RegistryID: "registry-one", Generation: 1}
	ref, uid, version, err := adapter.WriteCredential(context.Background(), "org-one", "registry-one", "operation-one", 1, credential)
	if err != nil || uid != "uid-one" || version != "1" || !strings.HasSuffix(ref, "#.dockerconfigjson") {
		t.Fatalf("write = %q %q %q %v", ref, uid, version, err)
	}
	if replay, _, _, err := adapter.WriteCredential(context.Background(), "org-one", "registry-one", "operation-one", 1, credential); err != nil || replay != ref {
		t.Fatalf("replay = %q %v", replay, err)
	}
	read, err := adapter.ReadCredential(context.Background(), "org-one", "registry-one", 1, ref)
	if err != nil || string(read.Username) != "octocat" || string(read.PAT) != "ghp_secret" {
		t.Fatalf("read = %+v %v", read, err)
	}
	read.Cleanup()
	if read.Username != nil || read.PAT != nil {
		t.Fatal("credential cleanup did not clear slices")
	}
	secret, _ := client.CoreV1().Secrets("cyclopus").Get(context.Background(), strings.Split(strings.Split(ref, "/")[3], "#")[0], metav1.GetOptions{})
	if secret.Type != corev1.SecretTypeDockerConfigJson || secret.Immutable == nil || !*secret.Immutable || len(secret.Data) != 1 {
		t.Fatalf("secret = %+v", secret)
	}
}

func TestRegistryCredentialSecretCollisionAndCrossOrg(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("create", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
		secret := action.(ktesting.CreateAction).GetObject().(*corev1.Secret)
		secret.UID = types.UID("uid-two")
		return false, nil, nil
	})
	adapter, _ := NewRegistryCredentialSecretAdapter(client, "cyclopus")
	credential := &oci.Credential{Username: []byte("user"), PAT: []byte("pat"), OrgID: "org-one", RegistryID: "registry", Generation: 2}
	ref, _, _, err := adapter.WriteCredential(t.Context(), "org-one", "registry", "operation", 2, credential)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.Split(strings.Split(ref, "/")[3], "#")[0]
	secret, _ := client.CoreV1().Secrets("cyclopus").Get(t.Context(), name, metav1.GetOptions{})
	secret.Labels[ManagedByLabel] = "other"
	_, _ = client.CoreV1().Secrets("cyclopus").Update(t.Context(), secret, metav1.UpdateOptions{})
	if _, _, _, err = adapter.WriteCredential(t.Context(), "org-one", "registry", "operation", 2, credential); !errors.Is(err, ErrRegistryCredentialCollision) {
		t.Fatalf("collision error=%v", err)
	}
	if _, err = adapter.ReadCredential(t.Context(), "org-two", "registry", 2, ref); !errors.Is(err, ErrRegistryCredentialInvalid) {
		t.Fatalf("cross-org error=%v", err)
	}
}

func TestMarshalDockerConfigClearsInputsOnlyByCallerContract(t *testing.T) {
	username, password := []byte("u"), []byte("p")
	payload, err := marshalDockerConfig(username, password)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(payload)
	gotUser, gotPassword, err := parseDockerConfig(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotUser) != "u" || string(gotPassword) != "p" {
		t.Fatalf("parsed=%q/%q", gotUser, gotPassword)
	}
	clear(gotUser)
	clear(gotPassword)
}

func TestRegistryCredentialWriteRejectsInvalidMaterialBeforeSideEffects(t *testing.T) {
	for name, credential := range map[string]*oci.Credential{
		"non-UTF-8 username":    {Username: []byte{'o', 0xff, 'c'}, PAT: []byte("pat"), OrgID: "org-one", RegistryID: "registry", Generation: 1},
		"non-UTF-8 PAT":         {Username: []byte("user"), PAT: []byte{'p', 0xc3, 0x28}, OrgID: "org-one", RegistryID: "registry", Generation: 1},
		"control byte username": {Username: []byte("us\ner"), PAT: []byte("pat"), OrgID: "org-one", RegistryID: "registry", Generation: 1},
		"control byte PAT":      {Username: []byte("user"), PAT: []byte("pa\rt"), OrgID: "org-one", RegistryID: "registry", Generation: 1},
		"NUL byte PAT":          {Username: []byte("user"), PAT: []byte{'p', 0x00, 't'}, OrgID: "org-one", RegistryID: "registry", Generation: 1},
		"DEL byte username":     {Username: []byte{'u', 0x7f}, PAT: []byte("pat"), OrgID: "org-one", RegistryID: "registry", Generation: 1},
		"colon username":        {Username: []byte("us:er"), PAT: []byte("pat"), OrgID: "org-one", RegistryID: "registry", Generation: 1},
		"oversized PAT":         {Username: []byte("user"), PAT: bytes.Repeat([]byte("p"), maxRegistryCredentialMaterialBytes+1), OrgID: "org-one", RegistryID: "registry", Generation: 1},
		"identity mismatch":     {Username: []byte("user"), PAT: []byte("pat"), OrgID: "org-two", RegistryID: "registry", Generation: 1},
		"generation mismatch":   {Username: []byte("user"), PAT: []byte("pat"), OrgID: "org-one", RegistryID: "registry", Generation: 9},
		"empty username":        {Username: nil, PAT: []byte("pat"), OrgID: "org-one", RegistryID: "registry", Generation: 1},
		"valid UTF-8 escapes":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			if credential == nil {
				return
			}
			client := fake.NewClientset()
			adapter, err := NewRegistryCredentialSecretAdapter(client, "cyclopus")
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = adapter.WriteCredential(t.Context(), "org-one", "registry", "operation", 1, credential)
			if !errors.Is(err, ErrRegistryCredentialInvalid) {
				t.Fatalf("write error = %v, want invalid", err)
			}
			secrets, listErr := client.CoreV1().Secrets("cyclopus").List(t.Context(), metav1.ListOptions{})
			if listErr != nil || len(secrets.Items) != 0 {
				t.Fatalf("invalid material created Secrets: %v %+v", listErr, secrets.Items)
			}
		})
	}
}

func TestRegistryCredentialWriteAcceptsUnicode(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("create", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
		secret := action.(ktesting.CreateAction).GetObject().(*corev1.Secret)
		secret.UID, secret.ResourceVersion = types.UID("uid-unicode"), "1"
		return false, nil, nil
	})
	adapter, _ := NewRegistryCredentialSecretAdapter(client, "cyclopus")
	credential := &oci.Credential{Username: []byte("usér-名前"), PAT: []byte("töken:✓"), OrgID: "org-one", RegistryID: "registry", Generation: 1}
	ref, _, _, err := adapter.WriteCredential(t.Context(), "org-one", "registry", "operation", 1, credential)
	if err != nil {
		t.Fatal(err)
	}
	read, err := adapter.ReadCredential(t.Context(), "org-one", "registry", 1, ref)
	if err != nil || !bytes.Equal(read.Username, credential.Username) || !bytes.Equal(read.PAT, credential.PAT) {
		t.Fatalf("unicode round trip = %q/%q %v", read.Username, read.PAT, err)
	}
}

func TestRegistryCredentialReadMissingSecretIsNotFound(t *testing.T) {
	client := fake.NewClientset()
	adapter, _ := NewRegistryCredentialSecretAdapter(client, "cyclopus")
	name, err := RegistryCredentialSecretName("org-one", 3)
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.ReadCredential(t.Context(), "org-one", "registry", 3, "k8s-secret://cyclopus/"+name+"#"+RegistryCredentialKey)
	if !errors.Is(err, ErrRegistryCredentialNotFound) {
		t.Fatalf("read error = %v, want not found", err)
	}
}

func TestParseDockerConfigRejectsConflictingAuth(t *testing.T) {
	valid := dockerConfigFixture(t, "octocat", "ghp_secret", base64.StdEncoding.EncodeToString([]byte("octocat:ghp_secret")))
	username, password, err := parseDockerConfig(valid)
	if err != nil || string(username) != "octocat" || string(password) != "ghp_secret" {
		t.Fatalf("valid fixture = %q/%q %v", username, password, err)
	}
	defer func() { clear(username); clear(password) }()
	for name, mutate := range map[string]func([]byte) []byte{
		"conflicting auth": func(payload []byte) []byte {
			return bytes.Replace(payload, []byte(base64.StdEncoding.EncodeToString([]byte("octocat:ghp_secret"))),
				[]byte(base64.StdEncoding.EncodeToString([]byte("octocat:other"))), 1)
		},
		"invalid base64 auth": func(payload []byte) []byte {
			return bytes.Replace(payload, []byte(base64.StdEncoding.EncodeToString([]byte("octocat:ghp_secret"))), []byte("!!!"), 1)
		},
		"duplicate username": func(payload []byte) []byte {
			return bytes.Replace(payload, []byte(`"password":`), []byte(`"username":"second","password":`), 1)
		},
		"duplicate auths": func(payload []byte) []byte {
			return append(bytes.TrimSuffix(payload, []byte("}")), []byte(`,"auths":{}}`)...)
		},
		"unknown top-level key": func(payload []byte) []byte {
			return append(bytes.TrimSuffix(payload, []byte("}")), []byte(`,"extra":{}}`)...)
		},
		"unknown entry key": func(payload []byte) []byte {
			return bytes.Replace(payload, []byte(`"auth":`), []byte(`"email":"a@b.c","auth":`), 1)
		},
		"second host": func(payload []byte) []byte {
			return bytes.Replace(payload, []byte(`}}}`), []byte(`,"docker.io":{"username":"octocat","password":"ghp_secret","auth":"`+
				base64.StdEncoding.EncodeToString([]byte("octocat:ghp_secret"))+`"}}}`), 1)
		},
		"trailing data": func(payload []byte) []byte {
			return append(append([]byte(nil), payload...), ' ', 'x')
		},
		"trailing comma": func(payload []byte) []byte {
			return bytes.Replace(payload, []byte(`}}}`), []byte(`},}}`), 1)
		},
		"empty auth": func(payload []byte) []byte {
			return bytes.Replace(payload, []byte(base64.StdEncoding.EncodeToString([]byte("octocat:ghp_secret"))), []byte(""), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			payload := mutate(append([]byte(nil), valid...))
			defer clear(payload)
			if _, _, err := parseDockerConfig(payload); !errors.Is(err, ErrRegistryCredentialInvalid) {
				t.Fatalf("parse error = %v, want invalid", err)
			}
		})
	}
}

func TestParseDockerConfigEscapesAndUTF8(t *testing.T) {
	// JSON escapes round-trip to the exact credential bytes: é (2-byte),
	// 名 (3-byte), a surrogate pair (4-byte), and escaped controls rejected by
	// the credential-material validator after decode.
	payload := []byte(`{"auths":{"ghcr.io":{"username":"usér-\u540d-\ud83d\ude00","password":"p\u00e4t","auth":"` +
		base64.StdEncoding.EncodeToString([]byte("usér-名-😀:pät")) + `"}}}`)
	defer clear(payload)
	username, password, err := parseDockerConfig(payload)
	if err != nil || string(username) != "usér-名-😀" || string(password) != "pät" {
		t.Fatalf("escape round trip = %q/%q %v", username, password, err)
	}
	clear(username)
	clear(password)
	for name, fixture := range map[string]string{
		"lone high surrogate": `{"auths":{"ghcr.io":{"username":"\ud83d","password":"p","auth":"cA=="}}}`,
		"raw control byte":    "{\"auths\":{\"ghcr.io\":{\"username\":\"u\x01\",\"password\":\"p\",\"auth\":\"cA==\"}}}",
		"bad escape":          `{"auths":{"ghcr.io":{"username":"\x","password":"p","auth":"cA=="}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			payload := []byte(fixture)
			defer clear(payload)
			if _, _, err := parseDockerConfig(payload); !errors.Is(err, ErrRegistryCredentialInvalid) {
				t.Fatalf("parse error = %v, want invalid", err)
			}
		})
	}
}

func TestParseDockerConfigZeroesObservedBuffers(t *testing.T) {
	// Buffer-observer hook: capture every credential-bearing scratch region the
	// parser allocates, then verify each was zeroed after the call returned.
	payload := dockerConfigFixture(t, "observer-user", "observer-pass", base64.StdEncoding.EncodeToString([]byte("observer-user:observer-pass")))
	var observed [][]byte
	original := dockerConfigObserve
	dockerConfigObserve = func(buffer []byte) { observed = append(observed, buffer) }
	defer func() { dockerConfigObserve = original }()
	username, password, err := parseDockerConfig(payload)
	if err != nil {
		t.Fatal(err)
	}
	clear(username)
	clear(password)
	if len(observed) < 4 {
		t.Fatalf("observer captured %d buffers, want scratch/username/password/auth", len(observed))
	}
	for index, buffer := range observed {
		for _, character := range buffer {
			if character != 0 {
				t.Fatalf("observed buffer %d not zeroed: %q", index, buffer)
			}
		}
	}
	if payload[0] != '{' {
		t.Fatal("caller payload was unexpectedly mutated")
	}
}

func dockerConfigFixture(t *testing.T, username, password, auth string) []byte {
	t.Helper()
	payload, err := marshalDockerConfig([]byte(username), []byte(password))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"auth":"`+auth+`"`) {
		t.Fatalf("fixture auth mismatch: %q", payload)
	}
	t.Cleanup(func() { clear(payload) })
	return payload
}
