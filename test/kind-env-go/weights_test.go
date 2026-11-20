//go:build e2e

package kindenv

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/praxis-proxy/dockyard/feature"
	"github.com/praxis-proxy/dockyard/gateway"
	"github.com/praxis-proxy/dockyard/stats"
)

// Stage 3 (increment 1): the statistical weighted-routing qualification, the core of
// e2e-static-provider-weights.py. A configured provider-weight ratio must produce a
// traffic split that fits the distribution under a Pearson chi-square goodness-of-fit
// test at α=0.001. This slice runs against the two katan backends the platform already
// provisions (provider-a → katan-a, provider-b → katan-b); the full three-recorder
// fixture + withdrawal scenarios are a follow-on increment.
const (
	sampleConcurrency = 8
	statisticalAlpha  = 0.001
)

func TestStaticProviderWeights(t *testing.T) {
	f := features.New("Static provider weights: traffic split fits configured ratio (chi-square)").
		Assess("50:50 and 95:5 weight ratios each fit under a chi-square goodness-of-fit test", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiFwd, err := feature.PortForwardService(ctx, c, apiNamespace, maasAPIService, maasAPIHTTPSPort)
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
			gwFwd, err := feature.PortForwardService(ctx, c, apiNamespace, gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key), gateway.WithTimeout(30*time.Second))
			demoPath := "/" + tenantNamespace + "/demo/v1/chat/completions"

			// Restore the default (provider-a only) afterward.
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

			cases := []struct {
				name    string
				weightA int
				weightB int
				samples int
			}{
				{"equal-50-50", 50, 50, 500},
				// Small-integer weights normalize like percentages: 2:1 must fit the same
				// 2/3 : 1/3 distribution as 66:34 would (ratio-equivalence).
				{"unequal-2-1", 2, 1, 500},
				{"canary-95-5", 95, 5, 500},
			}
			for _, tc := range cases {
				if err := patchExternalModelWeights(ctx, c, tc.weightA, tc.weightB); err != nil {
					t.Fatalf("%s: set weights: %v", tc.name, err)
				}
				if err := waitOverlayStable(ctx, c, 3*time.Minute); err != nil {
					t.Fatalf("%s: overlay did not stabilize: %v", tc.name, err)
				}

				// Warm up: discard an initial batch so the measured distribution reflects
				// steady-state routing, not cold-start skew (e.g. right after a prior test
				// rolled the ext-proc pod and envoy is still warming connections).
				_ = sampleProviderSplit(ctx, authed, demoPath, 40)

				counts := sampleProviderSplit(ctx, authed, demoPath, tc.samples)
				weights := map[string]int{"a": tc.weightA, "b": tc.weightB}
				fits, ok, r := stats.FitsDistribution(counts, weights, statisticalAlpha)
				t.Logf("%s: counts=%v expected=%v χ²=%.3f df=%d", tc.name, counts, r.Expected, r.Statistic, r.DegreesOfFreedom)
				if !ok {
					t.Errorf("%s: no critical value for df=%d", tc.name, r.DegreesOfFreedom)
				}
				if !fits {
					t.Errorf("%s: traffic split %v does not fit %v (χ²=%.3f > critical)", tc.name, counts, weights, r.Statistic)
				}
				// Every active provider must receive some traffic.
				if tc.weightA > 0 && counts["a"] == 0 {
					t.Errorf("%s: provider-a received no traffic", tc.name)
				}
				if tc.weightB > 0 && counts["b"] == 0 {
					t.Errorf("%s: provider-b received no traffic", tc.name)
				}
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// sampleProviderSplit sends n chat requests with bounded concurrency and tallies which
// katan backend served each (from the response body attribution marker).
func sampleProviderSplit(ctx context.Context, client *gateway.Client, path string, n int) map[string]int {
	payload := map[string]any{"model": "demo", "messages": []map[string]any{{"role": "user", "content": "weights"}}}

	results := make([]string, n)
	sem := make(chan struct{}, sampleConcurrency)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			resp, err := client.Post(ctx, path, payload)
			if err != nil || resp.StatusCode != 200 {
				results[idx] = ""
				return
			}
			results[idx] = classifyKatan(resp.String())
		}(i)
	}
	wg.Wait()

	counts := map[string]int{"a": 0, "b": 0}
	for _, r := range results {
		if r == "a" || r == "b" {
			counts[r]++
		}
	}
	return counts
}

// classifyKatan maps a response body to the katan backend that stamped it.
func classifyKatan(body string) string {
	switch {
	case strings.Contains(body, "katan-a-"):
		return "a"
	case strings.Contains(body, "katan-b-"):
		return "b"
	default:
		return ""
	}
}

// waitOverlayStable polls until the overlay digests all agree (published, recomputed,
// mounted, accepted, serving) for two consecutive samples, regardless of which providers
// are active — the convergence gate for a multi-active weight split.
func waitOverlayStable(ctx context.Context, c *envconf.Config, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	stable := 0
	var last overlayState
	for {
		st, err := readOverlayState(ctx, c)
		converged := err == nil && st.declared != "" && st.declared == st.recomputed &&
			st.declared == st.mounted && st.declared == st.accepted && st.declared == st.serving
		if converged && st == last {
			stable++
			if stable >= 2 {
				return nil
			}
		} else {
			stable = 0
		}
		last = st
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		time.Sleep(2 * time.Second)
	}
}
