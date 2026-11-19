package migration

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestRestoresAnnotationsWhenControllerRecreatedConfigMap(t *testing.T) {
	current := mustJSON(t, `{"data":{"extproc.yaml":"baseline"},"metadata":{"labels":{"owner":"grid"}}}`)
	original := mustJSON(t, `{"data":{"extproc.yaml":"baseline"},"metadata":{"labels":{"owner":"grid"},"annotations":{"opendatahub.io/managed":"false"}}}`)

	got := ConfigMapRestorePatch(current, original)
	want := []JSONPatchOp{{
		Op:    "add",
		Path:  "/metadata/annotations",
		Value: map[string]any{"opendatahub.io/managed": "false"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("patch = %+v, want %+v", got, want)
	}
}

func TestReplacesDataAndRemovesRunAddedAnnotations(t *testing.T) {
	current := mustJSON(t, `{"data":{"extproc.yaml":"temporary","run-only.yaml":"temporary"},"metadata":{"labels":{"owner":"grid","run-id":"current"},"annotations":{"run.example/id":"current"}}}`)
	original := mustJSON(t, `{"data":{"extproc.yaml":"baseline"},"metadata":{"labels":{"owner":"grid"},"annotations":{}}}`)

	got := ConfigMapRestorePatch(current, original)
	want := []JSONPatchOp{
		{Op: "add", Path: "/data", Value: map[string]any{"extproc.yaml": "baseline"}},
		{Op: "add", Path: "/metadata/labels", Value: map[string]any{"owner": "grid"}},
		{Op: "remove", Path: "/metadata/annotations"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("patch = %+v, want %+v", got, want)
	}
}

func TestReturnsEmptyPatchWhenSnapshotMatches(t *testing.T) {
	snapshot := mustJSON(t, `{"data":{"extproc.yaml":"baseline"},"metadata":{"labels":{"owner":"grid"},"annotations":{}}}`)
	// Pass two independent decodes so the function cannot rely on identity.
	other := mustJSON(t, `{"data":{"extproc.yaml":"baseline"},"metadata":{"labels":{"owner":"grid"},"annotations":{}}}`)
	if got := ConfigMapRestorePatch(snapshot, other); len(got) != 0 {
		t.Errorf("patch = %+v, want empty", got)
	}
}
