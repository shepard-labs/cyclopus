package auth

import "testing"

func TestScopeGrammar(t *testing.T) {
	const appID = "00000000-0000-0000-0000-000000000003"
	scope, err := DeployAppScope(appID)
	if err != nil || scope != "deploy:app:"+appID {
		t.Fatalf("DeployAppScope() = %q, %v", scope, err)
	}
	parsed, err := ParseScope(scope)
	if err != nil || parsed != appID {
		t.Fatalf("ParseScope() = %q, %v", parsed, err)
	}
	for _, invalid := range []string{
		"deploy:app", "deploy:app:3", "deploy:app:00000000-0000-0000-0000-000000000003:extra",
		"reconcile:org:00000000-0000-0000-0000-000000000001",
		"deploy:app:00000000-0000-0000-0000-000000000003 ",
	} {
		if _, err := ParseScope(invalid); err == nil {
			t.Fatalf("ParseScope(%q) succeeded", invalid)
		}
	}
	if err := ValidateScopes([]string{scope, scope}); err == nil {
		t.Fatal("duplicate scopes were accepted")
	}
}
