//go:build e2e

package kindenv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/praxis-proxy/dockyard/funcs"
	"github.com/praxis-proxy/dockyard/gateway"
)

// Stage 4 (increment 1): the staged provider-set transition from
// e2e-equal-provider-selection.py. Moving an ExternalModel from one active provider to
// two (and back) must republish the overlay candidate set to match, with traffic
// following each published set — one provider serves everything, both share once both are
// active, and after withdrawing one the other drains to serving everything again.
func TestProviderSetTransition(t *testing.T) {
	f := features.New("Provider-set transition: one↔two active providers republish and drain cleanly").
		Assess("A-only serves A, A+B shares, then A-only drains B and serves A again", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
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

			// 1. One provider (A): the overlay has only A and every request is served by A.
			if err := patchExternalModelWeights(ctx, c, 1, 0); err != nil {
				t.Fatalf("set A-only: %v", err)
			}
			if err := waitActiveProviders(ctx, c, true, false, 2*time.Minute); err != nil {
				t.Fatalf("A-only did not converge: %v", err)
			}
			if counts := sampleProviderSplit(ctx, authed, demoPath, 60); counts["b"] != 0 || counts["a"] < 54 {
				t.Errorf("A-only split = %v, want all A", counts)
			}

			// 2. Add B: the overlay republishes both and traffic is shared.
			if err := patchExternalModelWeights(ctx, c, 1, 1); err != nil {
				t.Fatalf("add B: %v", err)
			}
			if err := waitActiveProviders(ctx, c, true, true, 2*time.Minute); err != nil {
				t.Fatalf("A+B did not converge: %v", err)
			}
			shared := sampleProviderSplit(ctx, authed, demoPath, 120)
			if shared["a"] == 0 || shared["b"] == 0 {
				t.Errorf("A+B split = %v, want both providers to receive traffic", shared)
			}

			// 3. Withdraw B: the overlay drops back to A-only and B drains to zero.
			if err := patchExternalModelWeights(ctx, c, 1, 0); err != nil {
				t.Fatalf("withdraw B: %v", err)
			}
			if err := waitActiveProviders(ctx, c, true, false, 2*time.Minute); err != nil {
				t.Fatalf("A-only did not reconverge: %v", err)
			}
			if counts := sampleProviderSplit(ctx, authed, demoPath, 60); counts["b"] != 0 || counts["a"] < 54 {
				t.Errorf("post-transition split = %v, want all A (B fully drained)", counts)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// waitActiveProviders polls until the overlay is converged with exactly the wanted
// providers active (two stable samples).
func waitActiveProviders(ctx context.Context, c *envconf.Config, wantA, wantB bool, timeout time.Duration) error {
	stable := 0
	var last overlayState
	return funcs.Retry(ctx, timeout, 2*time.Second, fmt.Sprintf("providers converged a=%v b=%v", wantA, wantB), func(ctx context.Context) error {
		st, err := readOverlayState(ctx, c)
		if err != nil {
			return err
		}
		converged := st.declared != "" && st.declared == st.recomputed &&
			st.declared == st.mounted && st.declared == st.accepted && st.declared == st.serving &&
			st.activeA == wantA && st.activeB == wantB
		if converged && st == last {
			stable++
		} else {
			stable = 0
		}
		last = st
		if stable >= 2 {
			return nil
		}
		return fmt.Errorf("not yet 2 stable samples: %+v", st)
	})
}
