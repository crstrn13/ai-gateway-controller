//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/config"
	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/funcs"
	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/gateway"
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
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithSetup("EPP + InferencePool + members + GIE route applied and ready", funcs.AllOf(
				funcs.ApplyResources("manifests/llmd", "*.yaml"),
				funcs.DeploymentBecomesAvailableWithin(3*time.Minute, "llmd", "llmd-epp"),
				funcs.DeploymentBecomesAvailableWithin(5*time.Minute, "llmd", "vcr-1"),
				funcs.DeploymentBecomesAvailableWithin(5*time.Minute, "llmd", "vcr-2"),
			)).
			Assess("a request routes through the InferencePool to a pool member (200)",
				func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
					gw, err := funcs.GatewayClient(ctx, c, "ai-gateway", "istio-system")
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
			WithTeardown("remove the llm-d stack", funcs.DeleteResources("manifests/llmd", "*.yaml")).
			Feature(),
	)
}
