//go:build e2e

// Package product is the AI Gateway's repo-specific half of the e2e harness: the
// config.Product implementation (its control plane, dependency stack and CRD
// types) plus the code fixtures its tests parameterize. The generic harness lives
// in test/e2e/{config,funcs,gateway}; this package is what a second repo would
// replace with its own. It is e2e-tagged because it imports controller-runtime
// and the reconcilers.
package product

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/controller"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/tenant"
	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/config"
	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/funcs"
)

// controllerCRDDirs are the CRDs the reconcilers watch, installed before the
// manager starts: the repo's own (inference, aigateway) + the MaaS stubs. NOT the
// Gateway/istio stubs — the dep installers provide the real ones. Paths are
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
// gateway + namespace match the serving stack in manifests/.
const (
	defaultExtProcImage     = "praxis-extproc:dev"
	defaultGatewayName      = "ai-gateway"
	defaultGatewayNamespace = "istio-system"
)

// recorderImage is the harness-built self-identifying sim backend
// (test/e2e/sim/recorder), used by provider-selection and translation tests.
const recorderImage = "ai-gateway-e2e/provider-recorder:dev"

// AIGateway is the config.Product for this repo: it installs the AI Gateway
// dependency stack, registers the inference/aigateway CRD types, and runs the
// reconcilers in-process against the suite cluster.
type AIGateway struct {
	extProcImage  *string
	extProcRepo   *string
	runController *bool
}

// NewAIGateway registers the product's flags and returns the product. Call before
// flags are parsed (i.e. before envconf.NewFromFlags in TestMain).
func NewAIGateway() *AIGateway {
	return &AIGateway{
		extProcImage:  flag.String("extproc-image", cmp.Or(os.Getenv("AIGW_EXTPROC_IMAGE"), defaultExtProcImage), "ext-proc image to load into kind"),
		extProcRepo:   flag.String("extproc-repo", os.Getenv("AIGW_EXTPROC_REPO"), "praxis-extproc checkout (for serving overlays, until vendored)"),
		runController: flag.Bool("run-controller", true, "run the AI Gateway reconcilers in-process (installs the controller's CRDs first)"),
	}
}

// Compile-time assertion that AIGateway satisfies the Product seam.
var _ config.Product = (*AIGateway)(nil)

// ExtProcImage is the ext-proc image to load and render (flag: -extproc-image).
func (p *AIGateway) ExtProcImage() string { return *p.extProcImage }

// ExtProcRepo is the praxis-extproc checkout (flag: -extproc-repo).
func (p *AIGateway) ExtProcRepo() string { return *p.extProcRepo }

// InstallDeps builds the AI Gateway dependency stack in order: cluster add-ons,
// the GIE CRD (before istio, which needs it), istio, the ext-proc image, and the
// shared sim backend the tests route to.
func (p *AIGateway) InstallDeps(e *config.Environment) []env.Func {
	return funcs.NewDeps().
		WithMetalLB(funcs.MetalLBVersion(e.MetalLBVersion())).
		WithGatewayAPI(funcs.GatewayAPIVersion(e.GatewayAPIVersion())).
		WithCRDs("manifests/gie"). // GIE InferencePool CRD, before istio (GIE needs it)
		WithIstio(funcs.IstioInferenceExtension(true)).
		// cert-manager stands in for OpenShift service-ca: it issues the ext-proc
		// serving certs and a CA the gateway trusts (no kind overlay, no shim).
		WithCertManager().
		WithKubectlApply("manifests/cert-manager/issuer.yaml").
		WithCABundle("cert-manager", "e2e-ca", defaultGatewayNamespace, "e2e-service-ca").
		// Kuadrant attaches the gateway WasmPlugin the ext-proc EnvoyFilters anchor to.
		WithKuadrant().
		WithImage(e.GetClusterName(), p.ExtProcImage(), e.ShouldCreateCluster()).
		WithBuiltImage("sim/recorder", recorderImage, e.GetClusterName(), e.ShouldCreateCluster()).
		WithManifests("manifests/backends", "*.yaml").
		WithDeploymentAvailable("llm", "internal-model", 3*time.Minute).
		Build()
}

// AddTypesToScheme registers the inference + aigateway CRD types on the suite
// client scheme, so typed Create/Get work for those kinds.
func (p *AIGateway) AddTypesToScheme() env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		s := cfg.Client().Resources().GetScheme()
		if err := inferencev1alpha1.AddToScheme(s); err != nil {
			return ctx, err
		}
		if err := aigatewayv1alpha1.AddToScheme(s); err != nil {
			return ctx, err
		}
		_ = clientgoscheme.AddToScheme(s)
		return ctx, nil
	}
}

// Enabled reports whether the reconcilers should run (flag: -run-controller).
func (p *AIGateway) Enabled() bool { return *p.runController }

// Install installs the controller's CRDs and runs the AI Gateway reconcilers
// in-process. The reconcilers get the real, loaded ext-proc image so the pods
// they render actually run (not the placeholder the control-plane-only lanes use).
func (p *AIGateway) Install() (setup []env.Func, teardown env.Func) {
	cp := &ControlPlane{
		ManifestPath:     controllerManifestPath,
		Image:            p.ExtProcImage(), // the image given to config, not a placeholder
		GatewayName:      defaultGatewayName,
		GatewayNamespace: defaultGatewayNamespace,
	}
	return []env.Func{funcs.InstallCRDs(controllerCRDDirs...), cp.Setup}, cp.Teardown
}

// ControlPlane runs this repo's controllers in-process against the suite's cluster
// (matching the project's minimal kind dev harness) rather than building/loading
// the controller image. Setup and Teardown are env.Funcs; register Setup after
// InstallCRDs and Teardown before DestroyCluster.
type ControlPlane struct {
	// ManifestPath is the kustomize overlay the tenant reconciler renders the
	// ext-proc workload from. Relative to the test package's working dir.
	ManifestPath string

	// Image overrides the ext-proc image the tenant reconciler renders. Empty uses
	// the placeholder (control-plane lanes that only assert rendered objects); set
	// it to a real, loaded image when the ext-proc pods must actually run.
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
