// Package config contains the e2e test configuration. It wraps the
// e2e-framework environment with flag-driven cluster lifecycle, dependency
// install, and a test-suite/label system — modeled on Crossplane's
// test/e2e/config. The product-install step (Crossplane helm-installs its chart)
// is adapted here to the AI Gateway's dependency stack, since the controller
// ships no helm chart.
package config

import (
	"cmp"
	"os"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"

	"flag"

	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/funcs"
)

// LabelTestSuite marks which suite a Feature belongs to. Every Feature must set
// it; TestMain's BeforeEachFeature enforces that (as in Crossplane).
const LabelTestSuite = "test-suite"

// TestSuiteDefault is the base suite key/value for LabelTestSuite.
const TestSuiteDefault = "base"

const testSuiteFlag = "test-suite"

// Environment is the e2e configuration; it embeds the e2e-framework environment
// so it IS an env.Environment with extra, domain-aware methods.
type Environment struct {
	env.Environment

	createCluster  *bool
	destroyCluster *bool
	installDeps    *bool
	runController  *bool
	clusterName    *string
	extProcImage   *string
	extProcRepo    *string
	gatewayAPIVer  *string
	metalLBVer     *string

	selected *selectedTestSuite
	suites   map[string]testSuite
}

// selectedTestSuite implements flag.Value to distinguish unset from empty.
type selectedTestSuite struct {
	name string
	set  bool
}

func (s *selectedTestSuite) String() string {
	if !s.set {
		return TestSuiteDefault
	}
	return s.name
}

func (s *selectedTestSuite) Set(v string) error {
	s.name, s.set = v, true
	return nil
}

// testSuite is a named set of options applied on top of the base suite.
type testSuite struct {
	excludeBase    bool
	additionalFns  []conditionalSetupFunc
	labelsToSelect features.Labels
}

// conditionalSetupFunc gates env.Funcs behind a runtime condition.
type conditionalSetupFunc struct {
	condition func() bool
	fns       []env.Func
}

// NewEnvironmentFromFlags registers the flags but does not parse them (the
// caller does, in TestMain).
func NewEnvironmentFromFlags() Environment {
	e := Environment{suites: map[string]testSuite{}}
	e.createCluster = flag.Bool("create-cluster", true, "create a kind cluster before running (false: use the current kubeconfig)")
	e.destroyCluster = flag.Bool("destroy-cluster", true, "destroy the kind cluster when tests complete")
	e.installDeps = flag.Bool("install-deps", true, "install metallb + Gateway API + istio + load the ext-proc image")
	e.runController = flag.Bool("run-controller", true, "run the AI Gateway reconcilers in-process (installs the controller's CRDs first)")
	e.clusterName = flag.String("cluster-name", "", "kind cluster name (random if empty)")
	e.extProcImage = flag.String("extproc-image", cmp.Or(os.Getenv("AIGW_EXTPROC_IMAGE"), "praxis-extproc:dev"), "ext-proc image to load into kind")
	e.extProcRepo = flag.String("extproc-repo", os.Getenv("AIGW_EXTPROC_REPO"), "praxis-extproc checkout (for serving overlays, until vendored)")
	e.gatewayAPIVer = flag.String("gateway-api-version", cmp.Or(os.Getenv("AIGW_GATEWAY_API_VERSION"), "v1.2.1"), "Gateway API release for the CRD bundle")
	e.metalLBVer = flag.String("metallb-version", cmp.Or(os.Getenv("AIGW_METALLB_VERSION"), "v0.14.9"), "metallb release for the LoadBalancer IP pool")
	e.selected = &selectedTestSuite{}
	flag.Var(e.selected, testSuiteFlag, "test suite defining environment setup and tests to run")
	return e
}

// --- suite registration (mirrors Crossplane's Add*TestSuite + Option) --------

// Option configures a testSuite.
type Option func(*testSuite)

// WithLabelsToSelect sets the labels a suite selects when chosen.
func WithLabelsToSelect(l features.Labels) Option {
	return func(s *testSuite) { s.labelsToSelect = l }
}

// WithAdditionalSetup adds env.Funcs run (when condition holds) if the suite is selected.
func WithAdditionalSetup(condition func() bool, fns ...env.Func) Option {
	return func(s *testSuite) {
		s.additionalFns = append(s.additionalFns, conditionalSetupFunc{condition: condition, fns: fns})
	}
}

// WithoutBaseDefaultTestSuite marks the default suite as not inheriting the base.
func WithoutBaseDefaultTestSuite() Option {
	return func(s *testSuite) { s.excludeBase = true }
}

// AddDefaultTestSuite registers the base suite.
func (e *Environment) AddDefaultTestSuite(opts ...Option) {
	e.AddTestSuite(TestSuiteDefault, opts...)
}

// AddTestSuite registers a named suite (call from a test file's init()).
func (e *Environment) AddTestSuite(name string, opts ...Option) {
	s := testSuite{}
	for _, o := range opts {
		o(&s)
	}
	e.suites[name] = s
}

// EnrichLabels merges the selected suite's labelsToSelect into the caller's
// labels (user-provided values win).
func (e *Environment) EnrichLabels(labels features.Labels) features.Labels {
	if labels == nil {
		labels = features.Labels{}
	}
	for k, v := range e.suites[e.selected.String()].labelsToSelect {
		if _, exists := labels[k]; !exists {
			labels[k] = v
		}
	}
	return labels
}

// GetSelectedSuiteAdditionalEnvSetup returns the selected suite's conditional setup funcs.
func (e *Environment) GetSelectedSuiteAdditionalEnvSetup() []env.Func {
	var fns []env.Func
	for _, c := range e.suites[e.selected.String()].additionalFns {
		if c.condition == nil || c.condition() {
			fns = append(fns, c.fns...)
		}
	}
	return fns
}

// SetEnvironment sets the wrapped e2e-framework environment.
func (e *Environment) SetEnvironment(inner env.Environment) { e.Environment = inner }

// --- accessors ---------------------------------------------------------------

func (e *Environment) IsKindCluster() bool { return *e.createCluster || *e.clusterName != "" }

func (e *Environment) GetClusterName() string {
	if !e.IsKindCluster() {
		return ""
	}
	if *e.clusterName == "" {
		name := envconf.RandomName("aigc-e2e", 24)
		e.clusterName = &name
	}
	return *e.clusterName
}

func (e *Environment) ShouldCreateCluster() bool  { return *e.createCluster }
func (e *Environment) ShouldDestroyCluster() bool { return *e.createCluster && *e.destroyCluster }
func (e *Environment) ShouldInstallDeps() bool    { return *e.installDeps }
func (e *Environment) ShouldRunController() bool  { return *e.runController }
func (e *Environment) ExtProcImage() string       { return *e.extProcImage }
func (e *Environment) ExtProcRepo() string        { return *e.extProcRepo }

// --- setup composition -------------------------------------------------------

// CreateClusterFunc returns the cluster-create env.Func (kind).
func (e *Environment) CreateClusterFunc() env.Func {
	return envfuncs.CreateCluster(kind.NewProvider(), e.GetClusterName())
}

// DestroyClusterFunc returns the cluster-destroy env.Func.
func (e *Environment) DestroyClusterFunc() env.Func {
	return envfuncs.DestroyCluster(e.GetClusterName())
}

// InstallDeps returns the dependency-stack env.Funcs: metallb → Gateway API →
// istio → load the ext-proc image. This is the AI Gateway analogue of
// Crossplane's HelmInstallBaseCrossplane.
func (e *Environment) InstallDeps() []env.Func {
	return []env.Func{
		funcs.InstallMetalLB(*e.metalLBVer),
		funcs.InstallGatewayAPI(*e.gatewayAPIVer),
		funcs.InstallIstio(),
		funcs.LoadImage(e.GetClusterName(), *e.extProcImage, e.ShouldCreateCluster()),
	}
}
