//go:build e2e

// Package internal holds the shared e2e harness: cluster-independent setup
// (CRD install, running the controllers) and fixtures. It is imported by the
// platform suites (kind, and later openshift), which supply only their own
// TestMain and Features. Being under internal/ means nothing outside test/e2e
// can import it.
package internal

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

// InstallCRDs returns an env.Func that applies every given CRD directory into
// the cluster and waits for each CRD to report Established, so the manager's
// REST mapper resolves every watched kind on its first attempt (avoids the
// discovery-race retry storm). Paths are relative to the test package's working
// directory (the package go test runs).
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
// building/loading the FIPS controller image. Setup and Teardown are env.Funcs;
// register Setup after InstallCRDs and Teardown before DestroyCluster.
type ControlPlane struct {
	// ManifestPath is the kustomize overlay the tenant reconciler renders the
	// praxis-extproc workload from. Relative to the test package's working dir.
	ManifestPath string

	cancel context.CancelFunc
	done   chan error
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
		Image:                "quay.io/example/praxis-extproc:test",
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
		GatewayName:      "maas-default-gateway",
		GatewayNamespace: "openshift-ingress",
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
