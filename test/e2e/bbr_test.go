//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/config"
	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/funcs"
	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/gateway"
)

// TestBBR is true, controller-driven body-based routing: create the CRs and let the
// reconcilers provision the data plane, then send a request whose ONLY routing
// signal is the body `model` field. The flow:
//
//	ExternalProvider + ExternalModel (+ Ready tenant)
//	   → controller renders the ext-proc Deployment + EnvoyFilter + HTTPRoute
//	   → the pre-processing ext-proc promotes the body `model` to X-Gateway-Model-Name
//	   → the request routes to the model's backend (200)
//
// Baseline (cluster, deps, loaded ext-proc image, running controller) comes from
// TestMain; this test only adds its CRs in WithSetup.
func TestBBR(t *testing.T) {
	const ns = "bbr-reconciled"
	// The suite-level sim backend (manifests/backends; internal-model in the llm namespace).
	const backend = "internal-model.llm.svc.cluster.local"

	environment.Test(t,
		features.NewWithDescription(t.Name(), "The controller provisions the BBR data plane from CRs; the body `model` routes.").
			WithLabel(LabelArea, LabelAreaBBR).
			WithLabel(LabelSize, LabelSizeLarge).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithSetup("CRs applied; controller provisions the ext-proc", funcs.AllOf(
				// The sim backend is suite-level infra (TestMain). Here: just the CRs.
				funcs.ReadyTenant(ns),
				funcs.Create(
					funcs.ProviderSecret("sim", ns),
					funcs.ExternalProvider("sim", ns, backend),
					funcs.ExternalModel("granite-8b", ns, "granite-8b", "sim", "granite-8b"),
				),
				// The controller must render + run the ext-proc workload.
				funcs.DeploymentBecomesAvailableWithin(5*time.Minute, ns, "payload-processing-external-model-"+ns),
			)).
			Assess("body `model` alone routes to the backend (200)",
				func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
					client, err := funcs.GatewayClient(ctx, c, "ai-gateway", "istio-system")
					if err != nil {
						t.Fatalf("failed to provision gateway client: %q", err)
					}

					resp, err := client.ChatCompletion(ctx, gateway.ChatRequest{
						Model:    "granite-8b",
						Messages: gateway.User("hello"),
					},
					)

					if err != nil {
						t.Fatalf("failed to send chat completion request: %q", err)
					}

					if resp.StatusCode != 200 {
						t.Fatalf("chat completion response status code is %d, instead of 200", resp.StatusCode)
					}

					fmt.Println(string(resp.Body))

					return ctx
				},
			).
			WithTeardown("remove the CRs", funcs.Delete(
				funcs.ExternalModel("granite-8b", ns, "granite-8b", "sim", "granite-8b"),
				funcs.ExternalProvider("sim", ns, backend),
				funcs.ProviderSecret("sim", ns),
			)).
			Feature(),
	)
}
