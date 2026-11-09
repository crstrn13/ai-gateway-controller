//go:build e2e

// Package e2e implements end-to-end tests for the AI Gateway controller and its
// data plane, built on sigs.k8s.io/e2e-framework and arranged after Crossplane's
// test/e2e: a flag-driven config.Environment (suites + labels), a funcs library
// (env + feature helpers), manifests/ as YAML fixtures, and one Feature per area.
package e2e

// Label keys applied to Features for suite selection (go test ... --labels area=bbr).
// Assessments roll up to Features, Features to areas — the Crossplane model.
const (
	// LabelArea is the functional area a Feature exercises.
	LabelArea = "area"

	LabelAreaBBR     = "bbr"                 // body-based routing
	LabelAreaRouting = "intelligent-routing" // scoring / weighted selection
	LabelAreaLLMD    = "llm-d"               // InferencePool / self-hosted serving
	LabelAreaPPE     = "ppe"                 // endpoint picker protocol

	// LabelSize is the rough cost/footprint of a Feature.
	LabelSize      = "size"
	LabelSizeSmall = "small"
	LabelSizeLarge = "large"
)

// FieldManager is the server-side-apply field manager for resources the suite applies.
const FieldManager = "ai-gateway-e2e-tests"
