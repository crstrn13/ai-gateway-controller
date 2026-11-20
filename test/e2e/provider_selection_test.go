//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/product"
	"github.com/praxis-proxy/dockyard/config"
	"github.com/praxis-proxy/dockyard/funcs"
	"github.com/praxis-proxy/dockyard/gateway"
)

// TestProviderSelectionWeighted ports the core of kind-env's
// e2e-static-provider-weights.py: one client model bound to two providers with
// unequal weights (4:1 → 80/20), proven by sampling many requests and checking
// the served distribution against the expected split with a chi-square
// goodness-of-fit test (the same statistical model the Python driver uses:
// df=1, alpha=0.001, critical value 10.828).
//
// Attribution: the two backends are recorder sims (test/e2e/sim/recorder) that
// stamp their own identity into every response's "provider" field, so the served
// provider is read straight from the response body — reliable regardless of any
// model rewriting. Each recorder also exposes /__test/observations for a
// server-side cross-check if a test needs one.
func TestProviderSelectionWeighted(t *testing.T) {
	const (
		ns          = "provider-weighted"
		clientModel = "weighted-demo"
		backendA    = "provider-a-sim.llm.svc.cluster.local"
		backendB    = "provider-b-sim.llm.svc.cluster.local"

		samples   = 200
		weightA   = 4 // 4:1 → expected 0.8 / 0.2
		weightB   = 1
		chiSqCrit = 10.828 // df=1, alpha=0.001 (matches e2e-static-provider-weights.py)
	)
	expectedA := float64(weightA) / float64(weightA+weightB)
	expectedB := float64(weightB) / float64(weightA+weightB)

	environment.Test(t,
		features.NewWithDescription(t.Name(), "A 4:1 weighted two-provider model serves traffic in ~80/20 proportion.").
			WithLabel(LabelArea, LabelAreaRouting).
			WithLabel(LabelSize, LabelSizeLarge).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithSetup("two provider backends + a weighted ExternalModel; controller provisions the ext-proc", funcs.AllOf(
				product.ReadyTenant(ns),
				product.ExtProcServingCerts(ns),
				product.MountGatewayCATrust(),
				// Two HTTPS recorder provider backends (providers are SIMPLE-TLS HTTPS),
				// each self-identifying as prov-a / prov-b for attribution.
				product.ProviderBackend("provider-a-sim", "llm", "prov-a"),
				product.ProviderBackend("provider-b-sim", "llm", "prov-b"),
				funcs.DeploymentBecomesAvailableWithin(3*time.Minute, "llm", "provider-a-sim"),
				funcs.DeploymentBecomesAvailableWithin(3*time.Minute, "llm", "provider-b-sim"),
				funcs.Create(
					product.ProviderSecret("prov-a", ns),
					product.ProviderSecret("prov-b", ns),
					product.ExternalProvider("prov-a", ns, backendA),
					product.ExternalProvider("prov-b", ns, backendB),
					product.WeightedExternalModel(clientModel, ns, clientModel,
						product.WeightedRef{Provider: "prov-a", TargetModel: "prov-a", Weight: weightA},
						product.WeightedRef{Provider: "prov-b", TargetModel: "prov-b", Weight: weightB},
					),
				),
				funcs.DeploymentBecomesAvailableWithin(5*time.Minute, ns, "payload-processing-external-model-"+ns),
				funcs.DeploymentBecomesAvailableWithin(3*time.Minute, "istio-system", "ai-gateway-istio"),
			)).
			Assess("sampled traffic matches the 80/20 split (chi-square, alpha=0.001)",
				func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
					gw, err := funcs.GatewayClient(ctx, c, "ai-gateway", "istio-system")
					if err != nil {
						t.Fatalf("gateway client: %v", err)
					}
					req := gateway.ChatRequest{Model: clientModel, Messages: gateway.User("hello")}

					// Warm up: routes/overlay can lag the Deployment becoming Available,
					// so wait for the first 200 before sampling (eventual consistency, inline).
					if err := wait.For(func(context.Context) (bool, error) {
						resp, err := gw.ChatCompletion(ctx, req)
						return err == nil && resp.StatusCode == http.StatusOK, nil
					}, wait.WithTimeout(2*time.Minute), wait.WithInterval(2*time.Second)); err != nil {
						t.Fatalf("model never served a 200: %v", err)
					}

					counts := map[string]int{"a": 0, "b": 0}
					for i := 0; i < samples; i++ {
						resp, err := gw.ChatCompletion(ctx, req)
						if err != nil {
							t.Fatalf("request %d: %v", i, err)
						}
						if resp.StatusCode != http.StatusOK {
							t.Fatalf("request %d: want 200, got %d: %s", i, resp.StatusCode, resp.String())
						}
						p, err := providerFromResponse(resp.Body)
						if err != nil {
							t.Fatalf("request %d: %v (body: %s)", i, err, resp.String())
						}
						counts[p]++
					}

					// Both providers must actually serve, and A must dominate B.
					if counts["a"] == 0 || counts["b"] == 0 {
						t.Fatalf("a provider was never selected: a=%d b=%d", counts["a"], counts["b"])
					}
					if counts["a"] <= counts["b"] {
						t.Fatalf("heavier provider A did not dominate: a=%d b=%d", counts["a"], counts["b"])
					}

					// Chi-square goodness-of-fit against the expected 80/20 split.
					chi := chiSquare(
						[]int{counts["a"], counts["b"]},
						[]float64{expectedA * samples, expectedB * samples},
					)
					if chi >= chiSqCrit {
						t.Errorf("distribution rejects the 80/20 hypothesis: chi-square=%.3f >= %.3f (a=%d b=%d of %d)",
							chi, chiSqCrit, counts["a"], counts["b"], samples)
					}
					t.Logf("weighted selection: a=%d (%.1f%%) b=%d (%.1f%%) chi-square=%.3f",
						counts["a"], 100*float64(counts["a"])/samples,
						counts["b"], 100*float64(counts["b"])/samples, chi)
					return ctx
				},
			).
			WithTeardown("remove the CRs and backends", funcs.AllOf(
				funcs.Delete(
					product.WeightedExternalModel(clientModel, ns, clientModel,
						product.WeightedRef{Provider: "prov-a", TargetModel: "prov-a", Weight: weightA},
						product.WeightedRef{Provider: "prov-b", TargetModel: "prov-b", Weight: weightB},
					),
					product.ExternalProvider("prov-a", ns, backendA),
					product.ExternalProvider("prov-b", ns, backendB),
					product.ProviderSecret("prov-a", ns),
					product.ProviderSecret("prov-b", ns),
				),
				funcs.DeleteResources("manifests/provider-selection", "*.yaml"),
			)).
			Feature(),
	)
}

// providerFromResponse maps a chat-completion response to the provider that
// served it, via the recorder's in-band "provider" field.
func providerFromResponse(body []byte) (string, error) {
	var r struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	switch r.Provider {
	case "prov-a":
		return "a", nil
	case "prov-b":
		return "b", nil
	default:
		return "", &attributionError{provider: r.Provider}
	}
}

type attributionError struct{ provider string }

func (e *attributionError) Error() string {
	return "could not attribute response to a provider (provider=" + e.provider + ")"
}

// chiSquare is Pearson's goodness-of-fit statistic: sum((observed-expected)^2 /
// expected). Pure math — kept in the test file so the assertion stays readable
// where it runs (no helper in funcs).
func chiSquare(observed []int, expected []float64) float64 {
	var sum float64
	for i := range observed {
		d := float64(observed[i]) - expected[i]
		if expected[i] > 0 {
			sum += d * d / expected[i]
		}
	}
	return sum
}
