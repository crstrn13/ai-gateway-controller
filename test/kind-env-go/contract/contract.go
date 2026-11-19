// Package contract holds the pure, cluster-free helpers behind the API-translation
// qualification's runtime contracts — exact-size request builders, the mounted ExtProc
// limit parser, Prometheus counter math, 413 attribution, and the diagnostic-policy
// ownership gate. Ported from test/kind-env/api_translation_contract.py; these are unit
// tested (contract_test.go) and consumed by the Stage 5 translation suite.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ExactSizeOptions tunes the exact-size request builders.
type ExactSizeOptions struct {
	Model                  string
	CompatibilityFields    bool
	CompatibilityUserValue string
}

func (o ExactSizeOptions) withDefaults() ExactSizeOptions {
	if o.Model == "" {
		o.Model = "demo"
	}
	if o.CompatibilityUserValue == "" {
		o.CompatibilityUserValue = "compatibility-test-user"
	}
	return o
}

// compactJSON marshals v as compact JSON without HTML escaping (matching Python's
// json.dumps(separators=(",",":"), ensure_ascii=True) for ASCII payloads).
func compactJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder appends a newline; trim it to match json.dumps.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ChatRequestWithExactJSONSize builds a valid ASCII Chat Completions body whose compact
// JSON encoding is exactly size bytes, padding the single message content.
func ChatRequestWithExactJSONSize(size int, opts ExactSizeOptions) (map[string]any, error) {
	opts = opts.withDefaults()
	body := map[string]any{
		"model":    opts.Model,
		"messages": []any{map[string]any{"role": "user", "content": ""}},
	}
	if opts.CompatibilityFields {
		body["user"] = opts.CompatibilityUserValue
		body["n"] = 2
		body["seed"] = 17
		body["parallel_tool_calls"] = true
	}
	empty, err := compactJSON(body)
	if err != nil {
		return nil, err
	}
	contentSize := size - len(empty)
	if contentSize < 0 {
		return nil, fmt.Errorf("requested JSON size %d is smaller than the fixed request envelope %d", size, len(empty))
	}
	body["messages"].([]any)[0].(map[string]any)["content"] = strings.Repeat("x", contentSize)
	encoded, err := compactJSON(body)
	if err != nil {
		return nil, err
	}
	if len(encoded) != size {
		return nil, fmt.Errorf("constructed JSON size %d does not match requested %d", len(encoded), size)
	}
	return body, nil
}

// ChatRequestJSONWithExactSize builds an exact-size request whose payload ends in
// trailingWhitespaceBytes of ignorable JSON whitespace. Returns the body and the exact
// wire payload string.
func ChatRequestJSONWithExactSize(size, trailingWhitespaceBytes int, opts ExactSizeOptions) (map[string]any, string, error) {
	if trailingWhitespaceBytes < 0 || trailingWhitespaceBytes >= size {
		return nil, "", fmt.Errorf("trailing whitespace must be non-negative and smaller than the requested size")
	}
	body, err := ChatRequestWithExactJSONSize(size-trailingWhitespaceBytes, opts)
	if err != nil {
		return nil, "", err
	}
	compact, err := compactJSON(body)
	if err != nil {
		return nil, "", err
	}
	payload := string(compact) + strings.Repeat(" ", trailingWhitespaceBytes)
	if len(payload) != size {
		return nil, "", fmt.Errorf("constructed request size %d does not match requested %d", len(payload), size)
	}
	return body, payload, nil
}

var diagnosticScenarioUnsafe = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

// CompactTranslationDiagnosticID builds a short diagnostic ID accepted by the provider
// recorder: "translation-<runToken>-<safeScenario>", truncated to 80 chars.
func CompactTranslationDiagnosticID(runPrefix, scenario string) string {
	runToken := strings.TrimRight(runPrefix, "-")
	if i := strings.LastIndex(runToken, "-"); i >= 0 {
		runToken = runToken[i+1:]
	}
	safe := strings.Trim(diagnosticScenarioUnsafe.ReplaceAllString(scenario, "-"), "-")
	if safe == "" {
		safe = "request"
	}
	value := fmt.Sprintf("translation-%s-%s", runToken, safe)
	if len(value) > 80 {
		value = value[:80]
	}
	return value
}

// TranslationLimits are the body-size limits read from the mounted ExtProc config.
type TranslationLimits struct {
	ServerMaxBodyBytes             *int
	TranslatorMaxRequestBodyBytes  *int
	TranslatorMaxResponseBodyBytes *int
	TranslatorFilterPresent        bool
}

var translatorFilterRE = regexp.MustCompile(`^(\s*)- filter: openai_chat_completions_to_anthropic\s*$`)

// ExtProcTranslationLimits reads only the server and translator body-size limits from the
// mounted ExtProc YAML, without confusing the server block with the translator filter.
func ExtProcTranslationLimits(configText string) TranslationLimits {
	lines := strings.Split(configText, "\n")

	var server []string
	for i, line := range lines {
		if line == "server:" {
			server = indentedBlock(lines, i, 0)
			break
		}
	}

	var translator []string
	present := false
	for i, line := range lines {
		if m := translatorFilterRE.FindStringSubmatch(line); m != nil {
			translator = indentedBlock(lines, i, len(m[1]))
			present = true
			break
		}
	}

	return TranslationLimits{
		ServerMaxBodyBytes:             integerField(server, "max_body_bytes"),
		TranslatorMaxRequestBodyBytes:  integerField(translator, "max_request_body_bytes"),
		TranslatorMaxResponseBodyBytes: integerField(translator, "max_response_body_bytes"),
		TranslatorFilterPresent:        present,
	}
}

// indentedBlock returns the lines more-indented than parentIndent after start.
func indentedBlock(lines []string, start, parentIndent int) []string {
	var block []string
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) != "" && (len(line)-len(strings.TrimLeft(line, " "))) <= parentIndent {
			break
		}
		block = append(block, line)
	}
	return block
}

func integerField(lines []string, key string) *int {
	re := regexp.MustCompile(`^\s+` + regexp.QuoteMeta(key) + `:\s*(\d+)\s*(?:#.*)?$`)
	for _, line := range lines {
		if m := re.FindStringSubmatch(line); m != nil {
			var n int
			_, _ = fmt.Sscanf(m[1], "%d", &n)
			return &n
		}
	}
	return nil
}

// PrometheusCounterTotal sums a Prometheus counter across its label sets. The bool is
// false when the metric has no samples at all.
func PrometheusCounterTotal(metricsText, metricName string) (float64, bool) {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(metricName) + `(?:\{[^}]*\})?\s+([0-9.eE+-]+)(?:\s|$)`)
	var sum float64
	found := false
	for _, line := range strings.Split(metricsText, "\n") {
		if m := re.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			var v float64
			if _, err := fmt.Sscanf(m[1], "%g", &v); err == nil {
				sum += v
				found = true
			}
		}
	}
	return sum, found
}

// PrometheusCounterTotalOrZero reads a lazily-exported counter, treating an omitted zero
// sample as 0.
func PrometheusCounterTotalOrZero(metricsText, metricName string) float64 {
	total, found := PrometheusCounterTotal(metricsText, metricName)
	if !found {
		return 0
	}
	return total
}

// ClassifyBodyRejection attributes a 413 only when the ExtProc body-rejection counter
// proves contact. delta is nil when no reliable counter sample pair was available.
func ClassifyBodyRejection(status string, delta *int) string {
	if status != "413" {
		return "not_a_413"
	}
	if delta != nil && *delta == 1 {
		return "extproc_body_size_rejection_observed"
	}
	if delta != nil && *delta == 0 {
		return "not_attributed_to_extproc_body_size_counter"
	}
	return "unattributed_counter_unavailable_or_ambiguous"
}

// CounterSample is one observation of the ExtProc body-rejection counter plus the pod
// identity it came from.
type CounterSample struct {
	Count                 *int
	PodUID                string
	ContainerIDSHA256     string
	ContainerRestartCount *int
	// present marks which identity keys were supplied, so a changed key fails the delta
	// (matching the Python "key in before and before != after" check).
	hasContainerID      bool
	hasContainerRestart bool
}

// NewCounterSample builds a sample; pass nil for an absent count. The has* flags default
// to whether the string/pointer fields are set.
func NewCounterSample(count *int, podUID, containerID string, restart *int) CounterSample {
	return CounterSample{
		Count:                 count,
		PodUID:                podUID,
		ContainerIDSHA256:     containerID,
		ContainerRestartCount: restart,
		hasContainerID:        containerID != "",
		hasContainerRestart:   restart != nil,
	}
}

// ExtProcCounterDelta returns a counter delta only when both samples came from the same
// pod (and container identity, when present, is unchanged); nil otherwise.
func ExtProcCounterDelta(before, after CounterSample) *int {
	if before.Count == nil || after.Count == nil {
		return nil
	}
	if before.PodUID == "" || before.PodUID != after.PodUID {
		return nil
	}
	if before.hasContainerID && before.ContainerIDSHA256 != after.ContainerIDSHA256 {
		return nil
	}
	if before.hasContainerRestart {
		if after.ContainerRestartCount == nil || *before.ContainerRestartCount != *after.ContainerRestartCount {
			return nil
		}
	}
	if *after.Count < *before.Count {
		return nil
	}
	d := *after.Count - *before.Count
	return &d
}

// IsOwnedTranslationDiagnosticAuthPolicy requires exact identity before removing an
// interrupted diagnostic route policy.
func IsOwnedTranslationDiagnosticAuthPolicy(policy map[string]any, namespace, name, routeName string) bool {
	metadata, _ := policy["metadata"].(map[string]any)
	spec, _ := policy["spec"].(map[string]any)
	target, _ := spec["targetRef"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)

	return str(metadata["namespace"]) == namespace &&
		str(metadata["name"]) == name &&
		str(labels["app.kubernetes.io/managed-by"]) == "api-translation-kind-e2e-diagnostic" &&
		str(target["group"]) == "gateway.networking.k8s.io" &&
		str(target["kind"]) == "HTTPRoute" &&
		str(target["name"]) == routeName
}

var scopeRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AuthorinoScopesForPath returns the sorted, unique auth-service scopes used by gateway
// action sets whose predicates mention pathPrefix. pluginConfig may be a JSON string or
// an already-decoded map.
func AuthorinoScopesForPath(pluginConfig any, pathPrefix string) []string {
	cfg := asMap(pluginConfig)
	if cfg == nil {
		return []string{}
	}
	actionSets, _ := cfg["actionSets"].([]any)
	seen := map[string]struct{}{}
	for _, asItem := range actionSets {
		set, _ := asItem.(map[string]any)
		conds, _ := set["routeRuleConditions"].(map[string]any)
		preds, _ := conds["predicates"].([]any)
		matched := false
		for _, p := range preds {
			if s, ok := p.(string); ok && strings.Contains(s, pathPrefix) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		actions, _ := set["actions"].([]any)
		for _, a := range actions {
			action, _ := a.(map[string]any)
			scope := str(action["scope"])
			if str(action["service"]) == "auth-service" && scopeRE.MatchString(scope) {
				seen[scope] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func asMap(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case string:
		var m map[string]any
		if err := json.Unmarshal([]byte(t), &m); err != nil {
			return nil
		}
		return m
	default:
		return nil
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
