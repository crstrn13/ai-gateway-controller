//go:build e2e

package product

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/praxis-proxy/dockyard"
	"github.com/praxis-proxy/dockyard/install"
)

// addTypesToScheme registers this repo's CRD types (plus the client-go built-ins)
// on a scheme, so typed Create/Get work for its kinds. Shared by both vessels.
func addTypesToScheme(s *runtime.Scheme) error {
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		return err
	}
	if err := aigatewayv1alpha1.AddToScheme(s); err != nil {
		return err
	}
	_ = clientgoscheme.AddToScheme(s)
	return nil
}

// ControlPlaneVessel is the control-plane-only dockyard.Vessel: it installs the
// controller's CRDs (this repo's own + stub CRDs for the external kinds it watches)
// and runs the reconcilers in-process, with no data-plane dependencies (no metallb,
// istio, kuadrant, gateway, or real ext-proc image). It backs the lightweight kind
// suite that only asserts controller reconcile behavior; the full data-plane suite
// uses AIGateway instead.
type ControlPlaneVessel struct {
	crdDirs      []string
	manifestPath string
}

// NewControlPlaneVessel builds the control-plane-only vessel. crdDirs are the CRD
// directories to apply before the manager starts (relative to the test package);
// manifestPath is the kustomize overlay the tenant reconciler renders from.
func NewControlPlaneVessel(crdDirs []string, manifestPath string) *ControlPlaneVessel {
	return &ControlPlaneVessel{crdDirs: crdDirs, manifestPath: manifestPath}
}

// Compile-time assertion that ControlPlaneVessel satisfies the Vessel seam.
var _ dockyard.Vessel = (*ControlPlaneVessel)(nil)

// InstallDeps installs no data-plane dependencies — this suite is control-plane
// only. The CRDs the controller needs are installed in Install, before the manager
// starts, so its REST mapper resolves every watched kind.
func (v *ControlPlaneVessel) InstallDeps(*dockyard.Environment) []env.Func { return nil }

// AddTypesToScheme registers the inference + aigateway CRD types on the suite
// client scheme.
func (v *ControlPlaneVessel) AddTypesToScheme() env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		return ctx, addTypesToScheme(cfg.Client().Resources().GetScheme())
	}
}

// Enabled always runs the control plane — asserting it reconciles is the point of
// this suite.
func (v *ControlPlaneVessel) Enabled() bool { return true }

// Install applies the controller's CRDs, then runs the reconcilers in-process with
// a placeholder ext-proc image (this suite never runs the rendered pods).
func (v *ControlPlaneVessel) Install() (setup []env.Func, teardown env.Func) {
	cp := &ControlPlane{ManifestPath: v.manifestPath}
	return []env.Func{install.InstallCRDs(v.crdDirs...), cp.Setup}, cp.Teardown
}
