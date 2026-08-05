package observability

import (
	"errors"
	"testing"
)

func TestValidateBackendBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{"https external", "https://oo.example.com", false},
		{"https in-cluster", "https://openobserve.cyclopus.svc:5080", false},

		// The installer's actual values (scripts/install.sh): plaintext is the
		// point — these Services terminate no TLS and carry no public name.
		{"http .svc", "http://openobserve.cyclopus.svc:5080", false},
		{"http .svc beszel", "http://beszel.cyclopus.svc:8090", false},
		{"http fully qualified .svc", "http://openobserve.cyclopus.svc.cluster.local:5080", false},
		{"http custom cluster domain", "http://openobserve.cyclopus.svc.k8s.internal:5080", false},
		{"http .svc trailing dot", "http://openobserve.cyclopus.svc.:5080", false},
		{"http .svc uppercase", "http://OpenObserve.Cyclopus.SVC:5080", false},

		// Anything that could resolve publicly still requires TLS.
		{"http public host", "http://oo.example.com", true},
		{"http bare host", "http://openobserve", true},
		{"http namespace-qualified without .svc", "http://openobserve.cyclopus", true},
		{"http host merely containing svc", "http://svc.example.com", true},
		{"http host with svc prefix label", "http://svcfoo.example.com", true},
		{"http ip literal", "http://10.43.0.12:5080", true},
		{"http localhost", "http://localhost:5080", true},

		{"empty", "", true},
		{"not a url", "not-a-url", true},
		{"scheme only", "https://", true},
		{"unsupported scheme", "ftp://openobserve.cyclopus.svc", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBackendBaseURL(tc.rawURL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateBackendBaseURL(%q) = nil, want error", tc.rawURL)
				}
				if !errors.Is(err, ErrInsecureBackendURL) {
					t.Fatalf("ValidateBackendBaseURL(%q) error = %v, want ErrInsecureBackendURL", tc.rawURL, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateBackendBaseURL(%q) = %v, want nil", tc.rawURL, err)
			}
		})
	}
}
