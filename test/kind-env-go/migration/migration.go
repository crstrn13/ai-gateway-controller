// Package migration holds the pure, cluster-free helpers behind the IPP
// translation-migration cutover/rollback suite (Stage 6). Ported from
// test/kind-env/e2e-ipp-translation-migration.py; unit tested in migration_test.go.
package migration

import "reflect"

// JSONPatchOp is one RFC 6902 JSON-Patch operation.
type JSONPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// ConfigMapRestorePatch computes the JSON-Patch that restores a ConfigMap's data and
// metadata (labels, annotations) from current back to original — including when a
// controller recreated the ConfigMap during cleanup and dropped original annotations, or
// a run added keys that must be removed. Maps are replaced wholesale (not merged) so
// run-added keys do not linger. Returns an empty patch when current already matches.
func ConfigMapRestorePatch(current, original map[string]any) []JSONPatchOp {
	desiredData := mapField(original, "data")
	currentMeta := mapField(current, "metadata")
	originalMeta := mapField(original, "metadata")

	patch := []JSONPatchOp{}
	if !reflect.DeepEqual(mapField(current, "data"), desiredData) {
		patch = append(patch, JSONPatchOp{Op: "add", Path: "/data", Value: desiredData})
	}
	for _, field := range []string{"labels", "annotations"} {
		currentValues := mapField(currentMeta, field)
		desiredValues := mapField(originalMeta, field)
		if reflect.DeepEqual(currentValues, desiredValues) {
			continue
		}
		path := "/metadata/" + field
		switch {
		case len(desiredValues) > 0:
			// Replacing the complete map also removes run-added metadata keys.
			patch = append(patch, JSONPatchOp{Op: "add", Path: path, Value: desiredValues})
		case len(currentValues) > 0:
			patch = append(patch, JSONPatchOp{Op: "remove", Path: path})
		}
	}
	return patch
}

// mapField returns obj[key] as a map, or an empty (non-nil) map when absent or not a map,
// mirroring Python's `obj.get(key) or {}`.
func mapField(obj map[string]any, key string) map[string]any {
	if obj == nil {
		return map[string]any{}
	}
	if m, ok := obj[key].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
