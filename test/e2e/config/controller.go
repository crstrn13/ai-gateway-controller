//go:build e2e

package config

import (
	"context"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/controller"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/tenant"
)

// This file is the "install the product" layer — the in-process AI Gateway
// control plane and the CRD install it needs — living on the config Environment
// (the AI Gateway analogue of Crossplane's HelmInstall). It is e2e-tagged
// because it imports controller-runtime and the controller's reconcilers;
// environment.go stays untagged so default builds are unaffected.

// controllerCRDDirs are the CRDs the reconcilers watch, installed before the
// manager starts: the repo's own (inference, aigateway) + the MaaS stubs. NOT
// the Gateway/istio stubs — the dep installers provide the real ones. Paths are
// relative to the test package (test/e2e).
var controllerCRDDirs = []string{
	"../../config/crd/bases",
	"../../config/self/crd/bases",
	"manifests/crds",
}

// controllerManifestPath is the kustomize overlay the tenant reconciler renders
// the ext-proc workload from, relative to the test package.
const controllerManifestPath = "../../config/manifests/external-model/overlays/odh"

// Generic defaults for the kind environment (no OpenShift-specific names). The
// gateway + namespace match the serving stack in manifests/; the image matches
// config's -extproc-image default.
const (
	defaultExtProcImage     = "praxis-extproc:dev"
	defaultGatewayName      = "ai-gateway"
	defaultGatewayNamespace = "istio-system"
)

// InstallController returns the setup env.Funcs that install the controller's
// CRDs and run the AI Gateway reconcilers in-process, plus the teardown func.
// The reconcilers get the real, loaded ext-proc image so the pods they render
// actually run (not the placeholder the control-plane-only lanes use).
func (e *Environment) InstallController() (setup []env.Func, teardown env.Func) {
	cp := &ControlPlane{
		ManifestPath:     controllerManifestPath,
		Image:            e.ExtProcImage(), // the image given to config, not a placeholder
		GatewayName:      defaultGatewayName,
		GatewayNamespace: defaultGatewayNamespace,
	}
	return []env.Func{InstallCRDs(controllerCRDDirs...), cp.Setup}, cp.Teardown
}

// InstallCRDs returns an env.Func that applies every given CRD directory and
// waits for each CRD to report Established, so the manager's REST mapper resolves
// every watched kind on its first attempt (avoids the discovery-race retry
// storm). Paths are relative to the test package's working directory.
func InstallCRDs(dirs ...string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		r, err := resources.New(cfg.Client().RESTConfig())
		if err != nil {
			return ctx, fmt.Errorf("new resources client: %w", err)
		}
		if err := apiextensionsv1.AddToScheme(r.GetScheme()); err != nil {
			return ctx, fmt.Errorf("add apiextensions scheme: %w", err)
		}
		for _, dir := range dirs {
			if err := decoder.ApplyWithManifestDir(ctx, r, dir, "*.yaml", nil); err != nil {
				return ctx, fmt.Errorf("apply CRDs from %s: %w", dir, err)
			}
		}

		var crds apiextensionsv1.CustomResourceDefinitionList
		if err := r.List(ctx, &crds); err != nil {
			return ctx, fmt.Errorf("list CRDs: %w", err)
		}
		for i := range crds.Items {
			crd := &crds.Items[i]
			err := wait.For(
				conditions.New(r).ResourceMatch(crd, func(obj k8s.Object) bool {
					c, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
					if !ok {
						return false
					}
					for _, cond := range c.Status.Conditions {
						if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
							return true
						}
					}
					return false
				}),
				wait.WithTimeout(60*time.Second),
				wait.WithInterval(time.Second),
			)
			if err != nil {
				return ctx, fmt.Errorf("CRD %s not Established: %w", crd.Name, err)
			}
		}
		return ctx, nil
	}
}

// ControlPlane runs this repo's controllers in-process against the suite's
// cluster (matching the project's minimal kind dev harness) rather than
// building/loading the controller image. Setup and Teardown are env.Funcs;
// register Setup after InstallCRDs and Teardown before DestroyCluster.
type ControlPlane struct {
	// ManifestPath is the kustomize overlay the tenant reconciler renders the
	// ext-proc workload from. Relative to the test package's working dir.
	ManifestPath string

	// Image overrides the ext-proc image the tenant reconciler renders. Empty
	// uses the placeholder (control-plane lanes that only assert rendered objects);
	// set it to a real, loaded image when the ext-proc pods must actually run.
	Image string
	// GatewayName/GatewayNamespace override the gateway the model reconciler
	// attaches routes to. Empty uses the production defaults.
	GatewayName      string
	GatewayNamespace string

	cancel context.CancelFunc
	done   chan error
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Setup wires the same reconcilers cmd/manager does and starts the manager in a
// goroutine against the suite cluster.
func (cp *ControlPlane) Setup(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	crlog.SetLogger(zap.New(zap.UseDevMode(true)))

	if err := inferencev1alpha1.AddToScheme(clientgoscheme.Scheme); err != nil {
		return ctx, err
	}
	if err := aigatewayv1alpha1.AddToScheme(clientgoscheme.Scheme); err != nil {
		return ctx, err
	}

	mgr, err := ctrl.NewManager(cfg.Client().RESTConfig(), ctrl.Options{
		Scheme:  clientgoscheme.Scheme,
		Metrics: server.Options{BindAddress: "0"}, // disable the metrics listener in-test
	})
	if err != nil {
		return ctx, fmt.Errorf("new manager: %w", err)
	}

	tenantReconciler := &tenant.Reconciler{
		Client:               mgr.GetClient(),
		APIReader:            mgr.GetAPIReader(),
		ManifestPath:         cp.ManifestPath,
		Image:                or(cp.Image, defaultExtProcImage),
		MaaSAPIRouteNameBase: "maas-api-route",
		ResyncInterval:       time.Minute,
		Log:                  ctrl.Log.WithName("tenant"),
	}
	if err := tenantReconciler.SetupWithManager(mgr); err != nil {
		return ctx, fmt.Errorf("setup tenant reconciler: %w", err)
	}

	modelReconciler := &controller.Reconciler{
		Client:           mgr.GetClient(),
		APIReader:        mgr.GetAPIReader(),
		Scheme:           mgr.GetScheme(),
		GatewayName:      or(cp.GatewayName, defaultGatewayName),
		GatewayNamespace: or(cp.GatewayNamespace, defaultGatewayNamespace),
		Network:          "external-model",
		Log:              ctrl.Log.WithName("external-model"),
	}
	if err := modelReconciler.SetupWithManager(mgr); err != nil {
		return ctx, fmt.Errorf("setup model reconciler: %w", err)
	}

	mgrCtx, cancel := context.WithCancel(context.Background())
	cp.cancel = cancel
	cp.done = make(chan error, 1)
	go func() { cp.done <- mgr.Start(mgrCtx) }()

	return ctx, nil
}

// Teardown cancels the manager and waits for it to exit.
func (cp *ControlPlane) Teardown(ctx context.Context, _ *envconf.Config) (context.Context, error) {
	if cp.cancel != nil {
		cp.cancel()
		<-cp.done
	}
	return ctx, nil
}
