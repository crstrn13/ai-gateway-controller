//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/praxis-proxy/dockyard"
	"github.com/praxis-proxy/dockyard/feature"
	"github.com/praxis-proxy/dockyard/gateway"
)

// TestLLMD exercises the self-hosted / llm-d path: istio routes straight to an
// InferencePool and the EPP (endpoint picker) chooses a vLLM pod per request — the
// Gateway API Inference Extension (GIE). This is RHAIGW-468 groundwork:
//
//   - The EPP + vLLM-sim pool (manifests/llmd) are vendored from the praxis
//     grid-llmd demo — known-good components on kind.
//   - The HTTPRoute backendRef -> InferencePool (manifests/llmd/40-route.yaml) is
//     the NEW, unproven bit: istio calling the EPP via GIE. The praxis demo routed
//     via Grid, not istio, so this arrow has not been validated on this stack.
//   - This controller has NO llm-d reconciliation, so nothing here is controller-
//     driven; it's a pure data-plane test of llm-d + istio-GIE.
//
// Prereqs come from baseline (TestMain): cluster, deps, the GIE InferencePool CRD,
// istio with the inference extension. This test adds the EPP + pool + GIE route in
// WithSetup. Note: the vcr members pull a tokenizer from HuggingFace (needs egress),
// and the istio-GIE routing (HTTPRoute→InferencePool) is the unvalidated frontier.
func TestLLMD(t *testing.T) {
	// The model the vLLM-vcr pool members serve (see manifests/llmd/20-members.yaml).
	const model = "Qwen/Qwen3-0.6B"

	environment.Test(t,
		features.NewWithDescription(t.Name(), "istio routes to an InferencePool; the EPP picks a vLLM pod (GIE).").
			WithLabel(LabelArea, LabelAreaLLMD).
			WithLabel(LabelSize, LabelSizeLarge).
			WithLabel(dockyard.LabelTestSuite, dockyard.TestSuiteDefault).
			WithSetup("EPP + InferencePool + members + GIE route applied and ready", feature.AllOf(
				feature.ApplyResources("manifests/llmd", "*.yaml"),
				// istio auto-provisions the ai-gateway-istio Deployment/Service for the
				// Gateway in 05-gateway.yaml; wait for it so the LB IP is assigned before
				// the Assess resolves it via feature.GatewayClient.
				feature.DeploymentBecomesAvailableWithin(3*time.Minute, "istio-system", "ai-gateway-istio"),
				feature.DeploymentBecomesAvailableWithin(3*time.Minute, "llmd", "llmd-epp"),
				feature.DeploymentBecomesAvailableWithin(5*time.Minute, "llmd", "vcr-1"),
				feature.DeploymentBecomesAvailableWithin(5*time.Minute, "llmd", "vcr-2"),
			)).
			Assess("a request routes through the InferencePool to a pool member (200)",
				func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
					gw, err := feature.GatewayClient(ctx, c, "ai-gateway", "istio-system")
					if err != nil {
						t.Fatalf("gateway client: %v", err)
					}
					resp, err := gw.ChatCompletion(ctx, gateway.ChatRequest{Model: model, Messages: gateway.User("hello")})
					if err != nil {
						t.Fatalf("chat completion: %v", err)
					}
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("want %d, got %d: %s", http.StatusOK, resp.StatusCode, resp.String())
					}
					return ctx
				},
			).
			WithTeardown("remove the llm-d stack", feature.DeleteResources("manifests/llmd", "*.yaml")).
			Feature(),
	)
}
