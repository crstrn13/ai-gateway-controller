//go:build e2e

// Package kind holds the lightweight, control-plane-only end-to-end suite: it
// stands up a cluster, installs the controller's CRDs, runs the reconcilers
// in-process, and asserts reconcile behavior — no data plane (no metallb, istio,
// kuadrant or gateway). It is driven by the shared dockyard harness via a
// control-plane-only Vessel (product.NewControlPlaneVessel); the full data-plane
// suite lives in the parent package. Features (one per logical assertion group)
// live in sibling *_test.go files.
//
// Run with (klog verbosity surfaces the framework's own provisioning steps —
// kind create/destroy and the commands they run, logged at V(4)):
//
//	go test -tags=e2e -v -timeout=20m ./test/e2e/kind/... -args -v=4
//
// Flags (from dockyard): -create-cluster=false reuses the current kubeconfig,
// -cluster-name pins the kind cluster, -destroy-cluster=false keeps it for debugging.
package kind

import (
	"os"
	"testing"

	klog "k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/e2e-framework/klient/conf"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/product"
	"github.com/praxis-proxy/dockyard"
)

// testenv is the shared suite environment. Features reference it via
// testenv.Test(t, feature).
var testenv = dockyard.NewEnvironmentFromFlags()

// crdDirs are applied before the controllers start. The two config/ dirs are
// this repo's own CRDs; the envtest testdata dir holds stub CRDs for the
// external kinds the controller watches/creates (Gateway API, Istio, MaaS) —
// definitions only, no operators run. Paths are relative to this package.
var crdDirs = []string{
	"../../../config/crd/bases",
	"../../../config/self/crd/bases",
	"../../envtest/testdata/crds",
}

// controllerManifestPath is the kustomize overlay the tenant reconciler renders
// the ext-proc workload from, relative to this package.
const controllerManifestPath = "../../../config/manifests/external-model/overlays/odh"

func TestMain(m *testing.M) {
	log.SetLogger(klog.NewKlogr())
	// Note: do NOT call klog.InitFlags here — envconf.NewFromFlags() already
	// registers klog's flags (incl. -v); calling it twice panics with
	// "flag redefined". -v=N still works via the framework's own parsing.

	// The control-plane-only vessel: CRDs + in-process reconcilers, no data plane.
	testenv.WithVessel(product.NewControlPlaneVessel(crdDirs, controllerManifestPath))

	cfg, err := envconf.NewFromFlags()
	if err != nil {
		panic(err)
	}

	// This lightweight suite has a single area, so it does not use dockyard's
	// suite/label selection (that is for the multi-area data-plane suite). Every
	// Feature here runs; no LabelTestSuite selector is applied.

	var setup, finish []env.Func

	if testenv.ShouldCreateCluster() {
		setup = append(setup, testenv.CreateClusterFunc())
	} else {
		cfg.WithKubeconfigFile(conf.ResolveKubeConfigFile())
	}

	testenv.SetEnvironment(env.NewWithConfig(cfg))

	// Always register the controller's CRD types on the client scheme.
	setup = append(setup, testenv.Vessel().AddTypesToScheme())

	// Run the reconcilers in-process; their watched CRDs must exist before the
	// manager starts, so the vessel installs those first (in Install).
	if testenv.Vessel().Enabled() {
		cpSetup, cpTeardown := testenv.Vessel().Install()
		setup = append(setup, cpSetup...)
		finish = append([]env.Func{cpTeardown}, finish...) // stop it before the cluster goes away
	}

	if testenv.ShouldDestroyCluster() {
		finish = append(finish, testenv.DestroyClusterFunc())
	}

	testenv.Setup(setup...)
	testenv.Finish(finish...)
	os.Exit(testenv.Run(m))
}
