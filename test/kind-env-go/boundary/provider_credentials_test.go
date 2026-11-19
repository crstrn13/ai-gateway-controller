package boundary

import (
	"path/filepath"
	"testing"
)

// manifestPath resolves a kind-env manifest relative to this package.
func manifestPath(name string) string {
	return filepath.Join("..", "..", "kind-env", "manifests", name)
}

// TestProviderCredentialFixturesEnforceKeys asserts the backend manifest contract: every
// Katan backend enforces API-key validation with its expected fixture key, the transition
// Secret matches its backend, the IPP-compatibility Service is isolated, and images are
// digest-pinned. Mirrors the manifest half of test-provider-credentials.sh (no cluster).
func TestProviderCredentialFixturesEnforceKeys(t *testing.T) {
	backends, err := ParseManifests(manifestPath("00-backends.yaml"))
	if err != nil {
		t.Fatalf("parse 00-backends.yaml: %v", err)
	}

	for _, deployment := range []string{"katan-a", "katan-b", "katan-a-tenant-b", "katan-b-tenant-b"} {
		expected := "kind-only-dummy"
		if len(deployment) > len("-tenant-b") && deployment[len(deployment)-len("-tenant-b"):] == "-tenant-b" {
			expected = "tenant-b-controller-only-reference"
		}
		args, ok := DeploymentContainerArgs(backends, deployment)
		if !ok {
			t.Errorf("%s: deployment or args not found", deployment)
			continue
		}
		if !EnforcesAPIKeys(args, expected) {
			t.Errorf("%s: does not enforce API keys with key %q (args: %v)", deployment, expected, args)
		}
	}

	transitionArgs, ok := DeploymentContainerArgs(backends, "katan-transition")
	if !ok {
		t.Fatal("katan-transition deployment not found")
	}
	if !EnforcesAPIKeys(transitionArgs, "transition-provider-key") {
		t.Errorf("katan-transition does not enforce its fixture key")
	}

	if app, ok := ServiceSelectorApp(backends, "provider-a-legacy"); !ok || app != "katan-transition" {
		t.Errorf("provider-a-legacy selector.app = %q (found=%v), want katan-transition", app, ok)
	}

	pinned, err := AllKatanImagesDigestPinned(manifestPath("00-backends.yaml"))
	if err != nil {
		t.Fatalf("digest check: %v", err)
	}
	if !pinned {
		t.Error("a backend image is not pinned by digest")
	}

	transition, err := ParseManifests(manifestPath("42-transition-fixtures.yaml"))
	if err != nil {
		t.Fatalf("parse 42-transition-fixtures.yaml: %v", err)
	}
	if key, ok := SecretStringData(transition, "transition-provider-credentials", "api-key"); !ok || key != "transition-provider-key" {
		t.Errorf("transition Secret api-key = %q (found=%v), want transition-provider-key", key, ok)
	}
}
