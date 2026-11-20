//go:build e2e

package kindenv

import (
	"cmp"
	"flag"
	"os"
	"path/filepath"
	"testing"

	klog "k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/e2e-framework/klient/conf"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	"github.com/praxis-proxy/dockyard/config"
)

// testenv is the shared suite environment; Features reference it via testenv.Test.
var testenv = config.NewEnvironmentFromFlags()

func TestMain(m *testing.M) {
	log.SetLogger(klog.NewKlogr())

	// run.sh owns the cluster name (LOCAL_ENV_CLUSTER) and its lifecycle in this
	// bridge stage. Keep -create-cluster=false so dockyard does not also create one.
	keepCluster := flag.Bool("keep-cluster", false, "skip run.sh --destroy on finish (keep the provisioned cluster)")
	nativeProvision := flag.Bool("native-provision", false, "use the in-progress native Go provisioner instead of the run.sh bridge (WIP)")

	cluster := cmp.Or(os.Getenv("LOCAL_ENV_CLUSTER"), "external-model-kind")
	evidence := os.Getenv("LOCAL_ENV_EVIDENCE_ROOT")
	runScript, err := filepath.Abs("../kind-env/run.sh")
	if err != nil {
		panic(err)
	}

	// Parse flags (envconf.NewFromFlags calls flag.Parse) before reading our flag
	// values; the vessel is built from the parsed -native-provision.
	cfg, err := envconf.NewFromFlags()
	if err != nil {
		panic(err)
	}

	platform := NewMaaSPlatform(runScript, cluster, evidence, *nativeProvision)

	var setup, finish []env.Func
	switch {
	case *nativeProvision:
		// Native: dockyard owns the cluster lifecycle. Create it (if requested) before
		// the vessel provisions into it; it must exist before the image loads.
		if testenv.ShouldCreateCluster() {
			setup = append(setup, testenv.CreateClusterFunc())
		} else {
			cfg.WithKubeconfigFile(conf.ResolveKubeConfigFile())
		}
	default:
		// Bridge: run.sh owns the cluster; use the kubeconfig kind create leaves
		// (current-context kind-<cluster>), resolved lazily on first client use.
		cfg.WithKubeconfigFile(conf.ResolveKubeConfigFile())
	}
	testenv.SetEnvironment(env.NewWithConfig(cfg))

	if testenv.ShouldInstallDeps() {
		setup = append(setup, platform.InstallDeps(&testenv)...)
	}
	setup = append(setup, platform.AddTypesToScheme())

	if !*keepCluster {
		if *nativeProvision {
			finish = append(finish, testenv.DestroyClusterFunc())
		} else {
			finish = append(finish, platform.Teardown) // run.sh --destroy
		}
	}

	testenv.Setup(setup...)
	testenv.Finish(finish...)
	os.Exit(testenv.Run(m))
}
