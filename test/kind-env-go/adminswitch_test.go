//go:build e2e

package kindenv

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/praxis-proxy/dockyard/funcs"
	"github.com/praxis-proxy/dockyard/gateway"
)

// Stage 3 (increment 2): the active-passive / administrative-switch scenarios from
// e2e-static-provider-weights.py's SAMPLE_PLAN (100:0 and 0:100). A single-active weight
// configuration must route every request to that one provider and none to the disabled
// one — the exact (non-statistical) complement to the chi-square split tests.
func TestAdministrativeSwitch(t *testing.T) {
	f := features.New("Administrative switch: 100:0 and 0:100 route exclusively to the active provider").
		Assess("100:0 serves only provider-a and 0:100 serves only provider-b", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiFwd, err := funcs.PortForwardService(ctx, c, apiNamespace, maasAPIService, maasAPIHTTPSPort)
			if err != nil {
				t.Fatalf("port-forward maas-api: %v", err)
			}
			defer apiFwd.Close()
			caPEM, err := runOwnedCA(ctx, c)
			if err != nil {
				t.Fatalf("read run-owned CA: %v", err)
			}
			key, err := mintMaaSKey(ctx, apiFwd.Addr(), caPEM)
			if err != nil {
				t.Fatalf("mint MaaS key: %v", err)
			}
			gwFwd, err := funcs.PortForwardService(ctx, c, apiNamespace, gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key), gateway.WithTimeout(30*time.Second))
			demoPath := "/" + tenantNamespace + "/demo/v1/chat/completions"

			defer func() {
				if err := patchExternalModelWeights(ctx, c, 1, 0); err != nil {
					t.Errorf("restore weights: %v", err)
					return
				}
				// Leave the overlay converged on provider-a so a subsequent
				// attribution-sensitive test does not race this restore.
				if err := waitOverlay(ctx, c, "a", 2*time.Minute); err != nil {
					t.Errorf("restore did not converge on provider-a: %v", err)
				}
			}()

			const samples = 50
			cases := []struct {
				name         string
				weightA      int
				weightB      int
				wantActive   string
				wantDisabled string
			}{
				{"active-passive-100-0", 100, 0, "a", "b"},
				{"administrative-switch-0-100", 0, 100, "b", "a"},
			}
			for _, tc := range cases {
				if err := patchExternalModelWeights(ctx, c, tc.weightA, tc.weightB); err != nil {
					t.Fatalf("%s: set weights: %v", tc.name, err)
				}
				if err := waitOverlayStable(ctx, c, 3*time.Minute); err != nil {
					t.Fatalf("%s: overlay did not stabilize: %v", tc.name, err)
				}
				counts := sampleProviderSplit(ctx, authed, demoPath, samples)
				t.Logf("%s: counts=%v", tc.name, counts)
				if counts[tc.wantActive] < samples*9/10 {
					t.Errorf("%s: active provider %q served %d/%d requests", tc.name, tc.wantActive, counts[tc.wantActive], samples)
				}
				if counts[tc.wantDisabled] != 0 {
					t.Errorf("%s: disabled provider %q served %d requests (want 0)", tc.name, tc.wantDisabled, counts[tc.wantDisabled])
				}
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}
