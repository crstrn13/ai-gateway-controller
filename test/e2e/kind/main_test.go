//go:build e2e

// Package kind holds the kind-backed end-to-end suite driven by
// sigs.k8s.io/e2e-framework. TestMain owns the cluster lifecycle and wires the
// shared harness from test/e2e/config; the Features (one per logical
// assertion group) live in sibling *_test.go files.
//
// Run with (klog verbosity surfaces the framework's own provisioning steps —
// kind create/destroy and the commands they run, logged at V(4)):
//
//	go test -tags=e2e -v -timeout=20m ./test/e2e/kind/... -args -v=4
package kind

import (
	"flag"
	"os"
	"testing"

	klog "k8s.io/klog/v2"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/support/kind"

	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/config"
)

// testenv is the shared suite environment. Features reference it via
// testenv.Test(t, feature).
var testenv env.Environment

// crdDirs are applied before the controllers start. The two config/ dirs are
// this repo's own CRDs; the envtest testdata dir holds stub CRDs for the
// external kinds the controller watches/creates (Gateway API, Istio, MaaS) —
// definitions only, no operators run. Paths are relative to this package.
var crdDirs = []string{
	"../../../config/crd/bases",
	"../../../config/self/crd/bases",
	"../../envtest/testdata/crds",
}

func TestMain(m *testing.M) {
	// Register klog's flags (notably -v) on the test binary so `-args -v=N`
	// raises verbosity; the framework logs its provisioning steps at V(4).
	klog.InitFlags(nil)
	flag.Parse()

	testenv = env.New()
	// A random name keeps this suite from colliding with any cluster a human
	// (or the old run.sh) left behind, e.g. external-model-two-plane.
	clusterName := envconf.RandomName("aigc-e2e", 16)

	controlPlane := &config.ControlPlane{
		ManifestPath: "../../../config/manifests/external-model/overlays/odh",
	}

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		config.InstallCRDs(crdDirs...),
		controlPlane.Setup,
	)
	testenv.Finish(
		controlPlane.Teardown,
		envfuncs.DestroyCluster(clusterName),
	)

	os.Exit(testenv.Run(m))
}
