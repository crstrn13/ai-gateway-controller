//go:build e2e

package kindenv

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/praxis-proxy/dockyard/feature"
	"github.com/praxis-proxy/dockyard/gateway"
	"github.com/praxis-proxy/dockyard/install"
	"github.com/praxis-proxy/dockyard/stats"
)

// Stage 3 (three-recorder scenarios) from e2e-static-provider-weights.py. Three
// credential-enforcing proof recorders (proof-a/b/c, each stamping its PROVIDER_NAME into
// the response) let a three-way weighted split be qualified with a Pearson chi-square
// goodness-of-fit at df=2: both 2:1:1 and 50:25:25 must fit the same 2/3:1/6:1/6
// distribution (ratio-equivalence — small integers normalize like percentages). This
// provisions the three proof backends + providers, proves the split, and tears them down.
var proofProviders = []string{"provider-proof-a", "provider-proof-b", "provider-proof-c"}

func TestThreeProviderWeights(t *testing.T) {
	f := features.New("Three-provider weights: 2:1:1 and 50:25:25 fit a chi-square goodness-of-fit (df=2)").
		Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			if err := provisionProofStack(ctx, t, c); err != nil {
				t.Fatalf("provision proof stack: %v", err)
			}
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			teardownProofStack(ctx, t, c)
			return ctx
		}).
		Assess("2:1:1 and 50:25:25 weighted splits both fit the expected distribution", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiFwd, _, key := translationClientSetup(ctx, t, c)
			defer apiFwd.Close()
			gwFwd, err := feature.PortForwardService(ctx, c, apiNamespace, gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key), gateway.WithTimeout(30*time.Second))
			demoPath := "/" + tenantNamespace + "/demo/v1/chat/completions"

			cases := []struct {
				name    string
				weights [3]int
			}{
				{"ratio-2-1-1", [3]int{2, 1, 1}},
				{"percent-50-25-25", [3]int{50, 25, 25}},
			}
			for _, tc := range cases {
				if err := setProofWeights(ctx, c, tc.weights); err != nil {
					t.Fatalf("%s: set weights: %v", tc.name, err)
				}
				if err := waitOverlayCandidates(ctx, c, []string{
					"provider-provider-proof-a", "provider-provider-proof-b", "provider-provider-proof-c",
				}, 3*time.Minute); err != nil {
					t.Fatalf("%s: overlay did not converge on three candidates: %v", tc.name, err)
				}
				_ = sampleProofSplit(ctx, authed, demoPath, 60) // warm up

				counts := sampleProofSplit(ctx, authed, demoPath, 600)
				weights := map[string]int{"a": tc.weights[0], "b": tc.weights[1], "c": tc.weights[2]}
				fits, ok, r := stats.FitsDistribution(counts, weights, statisticalAlpha)
				t.Logf("%s: counts=%v expected=%v χ²=%.3f df=%d", tc.name, counts, r.Expected, r.Statistic, r.DegreesOfFreedom)
				if !ok {
					t.Errorf("%s: no critical value for df=%d", tc.name, r.DegreesOfFreedom)
				}
				if !fits {
					t.Errorf("%s: split %v does not fit %v (χ²=%.3f)", tc.name, counts, weights, r.Statistic)
				}
				for _, p := range []string{"a", "b", "c"} {
					if counts[p] == 0 {
						t.Errorf("%s: proof-%s received no traffic", tc.name, p)
					}
				}
			}
			return ctx
		}).
		Assess("an unready provider is withdrawn from the overlay while healthy siblings keep serving", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// Equal weights across all three, converged.
			if err := setProofWeights(ctx, c, [3]int{1, 1, 1}); err != nil {
				t.Fatalf("set equal weights: %v", err)
			}
			if err := waitOverlayCandidates(ctx, c, []string{
				"provider-provider-proof-a", "provider-provider-proof-b", "provider-provider-proof-c",
			}, 3*time.Minute); err != nil {
				t.Fatalf("three candidates did not converge: %v", err)
			}

			// Always restore proof-c's credential.
			defer func() {
				if err := ensureSecret(ctx, c, tenantNamespace, "provider-proof-c-credentials", "proof-key-provider-proof-c"); err != nil {
					t.Errorf("restore proof-c credential: %v", err)
				}
			}()

			// Remove proof-c's auth credential (key only; the Secret stays so its ext-proc
			// projection does not wedge) → ExternalProvider goes unready → candidate withdrawn.
			if err := clearSecretKey(ctx, c, tenantNamespace, "provider-proof-c-credentials", "api-key"); err != nil {
				t.Fatalf("clear proof-c credential: %v", err)
			}
			if err := waitExternalProviderNotReady(ctx, c, "provider-proof-c", 3*time.Minute); err != nil {
				t.Fatalf("proof-c did not go unready: %v", err)
			}
			if err := waitOverlayCandidates(ctx, c, []string{
				"provider-provider-proof-a", "provider-provider-proof-b",
			}, 3*time.Minute); err != nil {
				t.Fatalf("overlay did not withdraw proof-c to two healthy candidates: %v", err)
			}

			// Traffic is served only by the healthy siblings; proof-c gets none.
			apiFwd, _, key := translationClientSetup(ctx, t, c)
			defer apiFwd.Close()
			gwFwd, err := feature.PortForwardService(ctx, c, apiNamespace, gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key), gateway.WithTimeout(30*time.Second))
			_ = sampleProofSplit(ctx, authed, "/"+tenantNamespace+"/demo/v1/chat/completions", 40) // warm up
			counts := sampleProofSplit(ctx, authed, "/"+tenantNamespace+"/demo/v1/chat/completions", 120)
			t.Logf("unready-withdrawal counts=%v", counts)
			if counts["c"] != 0 {
				t.Errorf("withdrawn proof-c still served %d requests", counts["c"])
			}
			if counts["a"] == 0 || counts["b"] == 0 {
				t.Errorf("healthy siblings did not both serve: %v", counts)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// sampleProofSplit sends n requests (bounded concurrency) and tallies the proof backend
// that served each (from the recorder's PROVIDER_NAME in the body).
func sampleProofSplit(ctx context.Context, client *gateway.Client, path string, n int) map[string]int {
	payload := map[string]any{"model": "demo", "messages": []map[string]any{{"role": "user", "content": "weights3"}}}
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
				return
			}
			results[idx] = classifyProof(resp.String())
		}(i)
	}
	wg.Wait()
	counts := map[string]int{"a": 0, "b": 0, "c": 0}
	for _, r := range results {
		if r != "" {
			counts[r]++
		}
	}
	return counts
}

func classifyProof(body string) string {
	switch {
	case strings.Contains(body, "proof-a"):
		return "a"
	case strings.Contains(body, "proof-b"):
		return "b"
	case strings.Contains(body, "proof-c"):
		return "c"
	default:
		return ""
	}
}

// --- provisioning ---

func provisionProofStack(ctx context.Context, t *testing.T, c *envconf.Config) error {
	if _, err := install.LoadImage(nativeClusterName, recorderImage, true)(ctx, c); err != nil {
		t.Logf("kind load %s (continuing): %v", recorderImage, err)
	}
	for _, p := range proofProviders {
		token := "proof-key-" + p
		if err := ensureSecret(ctx, c, tenantNamespace, p+"-credentials", token); err != nil {
			return err
		}
		if err := ensureSecret(ctx, c, apiNamespace, p+"-backend-credential", token); err != nil {
			return err
		}
	}
	if _, err := install.Manifest("../kind-env/manifests/provider-selection-proof.yaml")(ctx, c); err != nil {
		return fmt.Errorf("apply proof backends: %w", err)
	}
	for _, suffix := range []string{"a", "b", "c"} {
		if err := waitDeploymentRolledOut(ctx, c, apiNamespace, "provider-selection-proof-"+suffix, 3*time.Minute); err != nil {
			return fmt.Errorf("proof-%s backend not ready (is %s loaded?): %w", suffix, recorderImage, err)
		}
	}
	if _, err := install.Manifest("../kind-env/manifests/provider-proof-external-providers.yaml")(ctx, c); err != nil {
		return fmt.Errorf("apply proof providers: %w", err)
	}
	for _, p := range proofProviders {
		if err := waitExternalProviderReady(ctx, c, p, 3*time.Minute); err != nil {
			return err
		}
	}
	// Rewire demo-model to the three proof providers (equal to start).
	if err := setProofWeights(ctx, c, [3]int{1, 1, 1}); err != nil {
		return err
	}
	return waitOverlayCandidates(ctx, c, []string{
		"provider-provider-proof-a", "provider-provider-proof-b", "provider-provider-proof-c",
	}, 3*time.Minute)
}

func teardownProofStack(ctx context.Context, t *testing.T, c *envconf.Config) {
	if err := restoreDemoModelBaseline(ctx, c); err != nil {
		t.Errorf("restore demo-model: %v", err)
	}
	if err := waitOverlay(ctx, c, "a", 2*time.Minute); err != nil {
		t.Errorf("demo-model did not reconverge on provider-a: %v", err)
	}
	for _, p := range proofProviders {
		ep := &inferencev1alpha1.ExternalProvider{ObjectMeta: metav1.ObjectMeta{Name: p, Namespace: tenantNamespace}}
		_ = c.Client().Resources().Delete(ctx, ep)
		_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var live inferencev1alpha1.ExternalProvider
			if err := c.Client().Resources().Get(ctx, p, tenantNamespace, &live); err != nil {
				return nil //nolint:nilerr // gone is success
			}
			if len(live.Finalizers) == 0 {
				return nil
			}
			live.Finalizers = nil
			return c.Client().Resources().Update(ctx, &live)
		})
	}
	for _, suffix := range []string{"a", "b", "c"} {
		_ = c.Client().Resources().Delete(ctx, deploymentObj(apiNamespace, "provider-selection-proof-"+suffix))
		_ = c.Client().Resources().Delete(ctx, serviceObj(apiNamespace, "provider-proof-"+suffix+"-ext"))
	}
	// Credential secrets intentionally left (the controller does not prune their ext-proc
	// projection — see the translation teardown note).
	_ = waitDeploymentRolledOut(ctx, c, tenantNamespace, extDeployment, 2*time.Minute)
}

// clearSecretKey removes a single data key from a Secret (leaving the Secret in place so a
// projected-volume mount referencing it does not break), via retry-on-conflict.
func clearSecretKey(ctx context.Context, c *envconf.Config, namespace, name, key string) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var s corev1.Secret
		if err := r.Get(ctx, name, namespace, &s); err != nil {
			return err
		}
		delete(s.Data, key)
		delete(s.StringData, key)
		return r.Update(ctx, &s)
	})
}

// waitExternalProviderNotReady polls until an ExternalProvider's phase is not Ready.
func waitExternalProviderNotReady(ctx context.Context, c *envconf.Config, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var ep inferencev1alpha1.ExternalProvider
		if err := c.Client().Resources().Get(ctx, name, tenantNamespace, &ep); err == nil && ep.Status.Phase != "Ready" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ExternalProvider %s stayed Ready", name)
		}
		time.Sleep(2 * time.Second)
	}
}

// setProofWeights rewires demo-model to the three proof providers with the given weights.
func setProofWeights(ctx context.Context, c *envconf.Config, weights [3]int) error {
	refs := make([]inferencev1alpha1.ExternalProviderRef, 3)
	for i, p := range proofProviders {
		refs[i] = translationRef(p, "demo", "openai-chat", "/v1/chat/completions", weights[i])
	}
	return updateDemoModelRefs(ctx, c, refs)
}
