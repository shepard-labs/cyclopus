package secretref

import "testing"

func TestParse(t *testing.T) {
	parsed, err := Parse("k8s-secret://tenant/runtime#DATABASE_URL")
	if err != nil || parsed.Namespace != "tenant" || parsed.Name != "runtime" || parsed.Key != "DATABASE_URL" {
		t.Fatalf("Parse() = %+v, %v", parsed, err)
	}
	for _, value := range []string{
		"", "tenant/runtime#key", "k8s-secret://tenant/runtime", "k8s-secret:///runtime#key",
		"k8s-secret://tenant/#key", "k8s-secret://tenant/runtime#", "k8s-secret://tenant/extra/runtime#key",
		"k8s-secret://tenant/runtime#key#extra", " k8s-secret://tenant/runtime#key",
		"k8s-secret://Tenant/runtime#key", "k8s-secret://tenant/runtime/name#key", "k8s-secret://tenant/runtime#bad/key",
	} {
		if _, err := Parse(value); err == nil {
			t.Fatalf("Parse(%q) succeeded", value)
		}
	}
}
