//go:build e2e

package kindenv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/praxis-proxy/dockyard/funcs"
	"github.com/praxis-proxy/dockyard/gateway"
)

// Stage 7 (live): the direct backend credential-enforcement probes from
// test-provider-credentials.sh. Each katan backend enforces its fixture API key
// independently of the gateway: a request with no credential or the wrong bearer is
// rejected (401), the correct bearer is accepted (200), and a stray x-api-key header does
// not override a valid bearer (still 200). The manifest half (args contract) is already
// covered by the cluster-free boundary unit tests. The katan backends serve HTTPS
// (run-CA-signed), so the probe verifies the serving cert against the run CA with SNI
// pinned to the backend's cluster DNS.
func TestProviderCredentialEnforcement(t *testing.T) {
	f := features.New("Provider credentials: katan backends enforce their fixture key directly").
		Assess("provider-a and provider-b reject missing/wrong credentials and accept the fixture key", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			caPEM, err := runOwnedCA(ctx, c)
			if err != nil {
				t.Fatalf("read run-owned CA: %v", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caPEM) {
				t.Fatalf("invalid run CA PEM")
			}
			for _, backend := range []string{"provider-a", "provider-b"} {
				fwd, err := funcs.PortForwardService(ctx, c, apiNamespace, backend, 8000)
				if err != nil {
					t.Fatalf("%s: port-forward: %v", backend, err)
				}
				httpsClient := &http.Client{
					Timeout: 15 * time.Second,
					Transport: &http.Transport{
						TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: backend + ".maas-system.svc"},
					},
				}
				probeBackendCredentials(ctx, t, backend, fwd.URL("https"), httpsClient)
				fwd.Close()
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// probeBackendCredentials runs the four direct credential probes against one katan backend.
func probeBackendCredentials(ctx context.Context, t *testing.T, name, baseURL string, httpsClient *http.Client) {
	t.Helper()
	const fixtureKey = "kind-only-dummy"
	client := gateway.New(baseURL, gateway.WithHTTPClient(httpsClient))
	body := map[string]any{"model": "demo", "messages": []map[string]any{{"role": "user", "content": "credential-fixture"}}}
	path := "/v1/chat/completions"

	post := func(opts ...gateway.RequestOption) int {
		resp, err := client.Post(ctx, path, body, opts...)
		if err != nil {
			t.Fatalf("%s: request: %v", name, err)
		}
		return resp.StatusCode
	}

	if got := post(); got != http.StatusUnauthorized {
		t.Errorf("%s: no credential → %d, want 401", name, got)
	}
	if got := post(gateway.Header("Authorization", "Bearer fixture-wrong")); got != http.StatusUnauthorized {
		t.Errorf("%s: wrong bearer → %d, want 401", name, got)
	}
	if got := post(gateway.Header("Authorization", "Bearer "+fixtureKey)); got != http.StatusOK {
		t.Errorf("%s: fixture bearer → %d, want 200", name, got)
	}
	if got := post(gateway.Header("Authorization", "Bearer "+fixtureKey), gateway.Header("x-api-key", "fixture-wrong")); got != http.StatusOK {
		t.Errorf("%s: fixture bearer + stray x-api-key → %d, want 200", name, got)
	}
}
