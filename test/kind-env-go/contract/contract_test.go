package contract

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func intp(i int) *int { return &i }

func TestExactSizeChatJSONBuilderPreservesValidModelIdentity(t *testing.T) {
	body, err := ChatRequestWithExactJSONSize(512, ExactSizeOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	encoded, _ := compactJSON(body)
	if len(encoded) != 512 {
		t.Errorf("size = %d, want 512", len(encoded))
	}
	var round map[string]any
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if round["model"] != "demo" {
		t.Errorf("model = %v, want demo", round["model"])
	}
}

func TestTrailingJSONWhitespaceOffsetsOnlyTargetModelExpansion(t *testing.T) {
	const size = 512
	body, payload, err := ChatRequestJSONWithExactSize(size, 13, ExactSizeOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(payload) != size {
		t.Errorf("payload size = %d, want %d", len(payload), size)
	}
	var round map[string]any
	if err := json.Unmarshal([]byte(payload), &round); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if !reflect.DeepEqual(round, body) {
		t.Errorf("payload did not round-trip to body")
	}
	if body["model"] != "demo" {
		t.Errorf("model = %v, want demo", body["model"])
	}
	// Expanding "demo" (4) to "mock-openai-model" (17) adds 13 bytes, exactly offsetting
	// the 13 whitespace bytes, so the rewritten compact body is still size bytes.
	rewritten := map[string]any{}
	for k, v := range body {
		rewritten[k] = v
	}
	rewritten["model"] = "mock-openai-model"
	rewrittenCompact, _ := compactJSON(rewritten)
	if len(rewrittenCompact) != size {
		t.Errorf("rewritten size = %d, want %d", len(rewrittenCompact), size)
	}
}

func TestExactSizeChatJSONBuilderRejectsInvalidPadding(t *testing.T) {
	if _, _, err := ChatRequestJSONWithExactSize(32, 32, ExactSizeOptions{}); err == nil {
		t.Error("expected error when trailing whitespace >= size")
	}
}

func TestCompactDiagnosticIDsFitRecorderContract(t *testing.T) {
	id := CompactTranslationDiagnosticID(
		"ipp-route-trace-20261029T003533-31c04cac4690-",
		"anthropic-legacy-parallel_tool_calls",
	)
	if !regexp.MustCompile(`^[A-Za-z0-9.-]{1,80}$`).MatchString(id) {
		t.Errorf("id %q does not match recorder contract", id)
	}
	if !strings.Contains(id, "parallel-tool-calls") {
		t.Errorf("id %q missing scenario", id)
	}
	if strings.Contains(id, "_") {
		t.Errorf("id %q contains underscore", id)
	}
}

func TestReadsServerAndFilterLimitsWithoutConfusingThem(t *testing.T) {
	config := `server:
  max_body_bytes: 10485760
filter_chains:
  - name: post-auth
    filters:
      - filter: intelligent_route
      - filter: openai_chat_completions_to_anthropic_stream
        max_partial_event_bytes: 1048576
      - filter: openai_chat_completions_to_anthropic
        max_request_body_bytes: 2097152
        max_response_body_bytes: 1048576
      - filter: credential_inject
`
	got := ExtProcTranslationLimits(config)
	want := TranslationLimits{
		ServerMaxBodyBytes:             intp(10485760),
		TranslatorMaxRequestBodyBytes:  intp(2097152),
		TranslatorMaxResponseBodyBytes: intp(1048576),
		TranslatorFilterPresent:        true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("limits = %+v, want %+v", got, want)
	}
}

func TestMissingTranslatorHasNoTranslationLimits(t *testing.T) {
	got := ExtProcTranslationLimits("server:\n  max_body_bytes: 10485760\nfilter_chains: []\n")
	if got.ServerMaxBodyBytes == nil || *got.ServerMaxBodyBytes != 10485760 {
		t.Errorf("server limit = %v, want 10485760", got.ServerMaxBodyBytes)
	}
	if got.TranslatorMaxRequestBodyBytes != nil || got.TranslatorMaxResponseBodyBytes != nil {
		t.Errorf("translator limits should be nil, got %v/%v", got.TranslatorMaxRequestBodyBytes, got.TranslatorMaxResponseBodyBytes)
	}
	if got.TranslatorFilterPresent {
		t.Error("translator filter should be absent")
	}
}

func TestSumsBodyRejectionCounterAndIgnoresComments(t *testing.T) {
	metrics := `# TYPE praxis_extproc_body_size_rejections_total counter
praxis_extproc_body_size_rejections_total{processor="a"} 2
praxis_extproc_body_size_rejections_total{processor="b"} 3
other_metric 99
`
	total, found := PrometheusCounterTotal(metrics, "praxis_extproc_body_size_rejections_total")
	if !found || total != 5 {
		t.Errorf("total = %v (found=%v), want 5", total, found)
	}
	if _, found := PrometheusCounterTotal("# no samples\n", "missing_metric"); found {
		t.Error("missing metric should report not-found")
	}
}

func TestOmittedZeroCounterSampleIsReadAsZero(t *testing.T) {
	if got := PrometheusCounterTotalOrZero("# TYPE lazy_counter_total counter\n", "lazy_counter_total"); got != 0 {
		t.Errorf("lazy counter = %v, want 0", got)
	}
	if got := PrometheusCounterTotalOrZero("lazy_counter_total 4\n", "lazy_counter_total"); got != 4 {
		t.Errorf("counter = %v, want 4", got)
	}
}

func TestBodyRejectionAttributionRequiresCounterEvidence(t *testing.T) {
	cases := []struct {
		status string
		delta  *int
		want   string
	}{
		{"200", intp(1), "not_a_413"},
		{"413", intp(1), "extproc_body_size_rejection_observed"},
		{"413", intp(0), "not_attributed_to_extproc_body_size_counter"},
		{"413", nil, "unattributed_counter_unavailable_or_ambiguous"},
	}
	for _, c := range cases {
		if got := ClassifyBodyRejection(c.status, c.delta); got != c.want {
			t.Errorf("classify(%s,%v) = %q, want %q", c.status, c.delta, got, c.want)
		}
	}
}

func TestExtProcCounterDeltaRequiresSamePodIdentity(t *testing.T) {
	before := NewCounterSample(intp(2), "pod-a", "container-a", intp(0))

	if d := ExtProcCounterDelta(before, NewCounterSample(intp(3), "pod-a", "container-a", intp(0))); d == nil || *d != 1 {
		t.Errorf("same pod delta = %v, want 1", d)
	}
	if d := ExtProcCounterDelta(before, NewCounterSample(intp(0), "pod-b", "container-a", intp(0))); d != nil {
		t.Errorf("different pod should be nil, got %v", d)
	}
	if d := ExtProcCounterDelta(before, NewCounterSample(intp(2), "pod-a", "container-b", intp(0))); d != nil {
		t.Errorf("different container should be nil, got %v", d)
	}
	if d := ExtProcCounterDelta(before, NewCounterSample(intp(2), "pod-a", "container-a", intp(1))); d != nil {
		t.Errorf("different restart count should be nil, got %v", d)
	}
	if d := ExtProcCounterDelta(before, NewCounterSample(nil, "pod-a", "container-a", intp(0))); d != nil {
		t.Errorf("nil count should be nil, got %v", d)
	}
}

func TestRecognizesOnlyExactOwnedTranslationRoutePolicy(t *testing.T) {
	policyJSON := `{
		"metadata": {
			"namespace": "models-as-a-service",
			"name": "api-translation-messages-auth",
			"labels": {"app.kubernetes.io/managed-by": "api-translation-kind-e2e-diagnostic"}
		},
		"spec": {"targetRef": {
			"group": "gateway.networking.k8s.io",
			"kind": "HTTPRoute",
			"name": "api-translation-messages"
		}}
	}`
	var policy map[string]any
	if err := json.Unmarshal([]byte(policyJSON), &policy); err != nil {
		t.Fatal(err)
	}
	if !IsOwnedTranslationDiagnosticAuthPolicy(policy, "models-as-a-service", "api-translation-messages-auth", "api-translation-messages") {
		t.Error("exact match should be owned")
	}
	if IsOwnedTranslationDiagnosticAuthPolicy(policy, "other-namespace", "api-translation-messages-auth", "api-translation-messages") {
		t.Error("wrong namespace must not match")
	}
	if IsOwnedTranslationDiagnosticAuthPolicy(policy, "models-as-a-service", "api-translation-messages-auth", "another-route") {
		t.Error("wrong route must not match")
	}
	foreign := map[string]any{}
	for k, v := range policy {
		foreign[k] = v
	}
	fm := map[string]any{}
	for k, v := range policy["metadata"].(map[string]any) {
		fm[k] = v
	}
	fm["labels"] = map[string]any{}
	foreign["metadata"] = fm
	if IsOwnedTranslationDiagnosticAuthPolicy(foreign, "models-as-a-service", "api-translation-messages-auth", "api-translation-messages") {
		t.Error("missing managed-by label must not match")
	}
}

func TestExtractsOnlyAuthServiceScopesForMatchingRoutePrefix(t *testing.T) {
	a64 := strings.Repeat("a", 64)
	configJSON := `{
		"actionSets": [
			{"routeRuleConditions": {"predicates": ["request.url_path.startsWith('/models-as-a-service/demo')"]},
			 "actions": [
				{"service": "auth-service", "scope": "` + a64 + `"},
				{"service": "ratelimit-check-service", "scope": "` + strings.Repeat("b", 64) + `"}
			 ]},
			{"routeRuleConditions": {"predicates": ["request.url_path.startsWith('/v1/messages')"]},
			 "actions": [{"service": "auth-service", "scope": "` + strings.Repeat("c", 64) + `"}]},
			{"routeRuleConditions": {"predicates": ["request.url_path.startsWith('/models-as-a-service/demo')"]},
			 "actions": [{"service": "auth-service", "scope": "` + a64 + `"}]}
		]
	}`
	var config map[string]any
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		t.Fatal(err)
	}
	got := AuthorinoScopesForPath(config, "/models-as-a-service/demo")
	if !reflect.DeepEqual(got, []string{a64}) {
		t.Errorf("scopes = %v, want [%s]", got, a64)
	}
}

func TestScopeExtractionAcceptsJSONAndRejectsMalformed(t *testing.T) {
	d64 := strings.Repeat("d", 64)
	validJSON := `{
		"actionSets": [{
			"routeRuleConditions": {"predicates": ["path startsWith('/tenant/model')"]},
			"actions": [
				{"service": "auth-service", "scope": "` + d64 + `"},
				{"service": "metadata-service", "scope": "` + strings.Repeat("e", 64) + `"},
				{"service": "auth-service", "scope": "not-a-scope"}
			]
		}]
	}`
	if got := AuthorinoScopesForPath(validJSON, "/tenant/model"); !reflect.DeepEqual(got, []string{d64}) {
		t.Errorf("scopes from JSON string = %v, want [%s]", got, d64)
	}
	if got := AuthorinoScopesForPath("not-json", "/tenant/model"); len(got) != 0 {
		t.Errorf("malformed config should yield no scopes, got %v", got)
	}
	var cfg map[string]any
	_ = json.Unmarshal([]byte(validJSON), &cfg)
	if got := AuthorinoScopesForPath(cfg, "/another-model"); len(got) != 0 {
		t.Errorf("non-matching prefix should yield no scopes, got %v", got)
	}
}
