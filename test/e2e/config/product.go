package config

import "sigs.k8s.io/e2e-framework/pkg/env"

// Product is the per-repo half of the harness: the thing under test (its control
// plane) together with the dependencies and CRD types it needs. The generic
// Environment owns cluster lifecycle, suites and labels, and delegates everything
// product-specific to a Product set via Environment.WithProduct.
//
// This is the seam the convergence rests on: config, funcs and gateway are the
// generic layer every control-plane repo reuses; each repo supplies its own
// Product. Extracting the shared harness into its own module later is then a move
// of those three packages, leaving each repo's Product behind.
type Product interface {
	// InstallDeps returns the ordered dependency stack to install before tests
	// run, composed from the generic installers via funcs.NewDeps(): cluster
	// add-ons (metallb, Gateway API, istio), CRDs, images, and shared infra. It
	// receives the Environment for cluster-derived inputs (name, is-kind, flag
	// values). Runs only when Environment.ShouldInstallDeps() is true.
	InstallDeps(e *Environment) []env.Func

	// AddTypesToScheme registers the product's CRD types on the suite client
	// scheme, so typed Create/Get work for its kinds. Always run.
	AddTypesToScheme() env.Func

	// Enabled reports whether the product's control plane should run (e.g. the
	// -run-controller gate). When false, Install is skipped.
	Enabled() bool

	// Install returns setup funcs that run the product's control plane and a
	// teardown func. Called only when Enabled() is true. The teardown is
	// registered to run before the cluster is destroyed.
	Install() (setup []env.Func, teardown env.Func)
}
