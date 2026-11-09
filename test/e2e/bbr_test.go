//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/config"
	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/funcs"
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
// Gated behind AIGW_EXTPROC_READY=1 because it needs the real ext-proc image
// loaded (so the rendered pods run) and the full provider transport — not part of
// the default green run yet.
func TestBBR(t *testing.T) {
	if os.Getenv("AIGW_EXTPROC_READY") != "1" {
		t.Skip("controller-driven BBR: set AIGW_EXTPROC_READY=1 (needs the real ext-proc image loaded)")
	}

	const ns = "bbr-reconciled"
	const path = "/v1/chat/completions"
	// The in-cluster sim from manifests/bbr (internal-model in the llm namespace).
	const backend = "internal-model.llm.svc.cluster.local"

	environment.Test(t,
		features.NewWithDescription(t.Name(), "The controller provisions the BBR data plane from CRs; the body `model` routes.").
			WithLabel(LabelArea, LabelAreaBBR).
			WithLabel(LabelSize, LabelSizeLarge).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithSetup("CRs applied; controller provisions the ext-proc", funcs.AllOf(
				// The backend the provider routes to.
				funcs.ApplyResources("manifests/bbr", "00-backend.yaml"),
				funcs.DeploymentBecomesAvailableWithin(3*time.Minute, "llm", "internal-model"),
				// A Ready tenant + the provider/model the controller reconciles.
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
				funcs.ChatThroughGatewayWithin(5*time.Minute, "ai-gateway", "istio-system", path,
					funcs.ChatBody("granite-8b", "hello"), nil, 200),
			).
			WithTeardown("remove the CRs", funcs.Delete(
				funcs.ExternalModel("granite-8b", ns, "granite-8b", "sim", "granite-8b"),
				funcs.ExternalProvider("sim", ns, backend),
				funcs.ProviderSecret("sim", ns),
			)).
			Feature(),
	)
}
