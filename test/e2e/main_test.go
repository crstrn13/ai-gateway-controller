//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/e2e-framework/klient/conf"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/config"
	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/funcs"
)

// environment is the shared suite configuration. Test files reference it via
// environment.Test(t, feature) and register extra suites in init().
var environment = config.NewEnvironmentFromFlags()

func TestMain(m *testing.M) {
	log.SetLogger(klog.NewKlogr())
	// Note: do NOT call klog.InitFlags here — envconf.NewFromFlags() already
	// registers klog's flags (incl. -v); calling it twice panics with
	// "flag redefined". -v=N still works via the framework's own parsing.

	cfg, err := envconf.NewFromFlags()
	if err != nil {
		panic(err)
	}

	// The base suite: the default set every Feature inherits.
	environment.AddDefaultTestSuite(
		config.WithLabelsToSelect(features.Labels{
			config.LabelTestSuite: []string{config.TestSuiteDefault},
		}),
	)

	var setup, finish []env.Func

	if environment.ShouldCreateCluster() {
		setup = append(setup, environment.CreateClusterFunc())
	} else {
		cfg.WithKubeconfigFile(conf.ResolveKubeConfigFile())
	}

	cfg.WithLabels(environment.EnrichLabels(cfg.Labels()))
	environment.SetEnvironment(env.NewWithConfig(cfg))

	if environment.ShouldInstallDeps() {
		setup = append(setup, environment.InstallDeps()...)
	}
	// Suite-specific setup (e.g. a future llm-d suite that also installs a pool).
	setup = append(setup, environment.GetSelectedSuiteAdditionalEnvSetup()...)
	// Always register our CRD types on the client scheme.
	setup = append(setup, funcs.AddTypesToScheme())

	// The AI Gateway reconcilers, run in-process (the "install the product" step,
	// analogous to Crossplane's HelmInstallBaseCrossplane). Their watched CRDs
	// must exist before the manager starts, so install those first. Given the real
	// ext-proc image so the pods it renders actually run.
	if environment.ShouldRunController() {
		cpSetup, cpTeardown := environment.InstallController()
		setup = append(setup, cpSetup...)
		finish = append([]env.Func{cpTeardown}, finish...) // stop it before the cluster goes away
	}

	if environment.ShouldDestroyCluster() {
		finish = append(finish, environment.DestroyClusterFunc())
	}

	// Enforce that every Feature declares the suite it belongs to (Crossplane does
	// the same) — keeps suite filtering honest.
	environment.BeforeEachFeature(func(ctx context.Context, _ *envconf.Config, t *testing.T, feature features.Feature) (context.Context, error) {
		t.Helper()
		if _, ok := feature.Labels()[config.LabelTestSuite]; !ok {
			t.Fatalf("Feature %q is missing the required %q label", feature.Name(), config.LabelTestSuite)
		}
		return ctx, nil
	})

	environment.Setup(setup...)
	environment.Finish(finish...)
	os.Exit(environment.Run(m))
}
