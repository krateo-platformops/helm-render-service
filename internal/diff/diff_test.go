package diff

import (
	"encoding/json"
	"testing"

	"github.com/braghettos/helm-render-service/internal/render"
)

func manifest(kind, name, yamlDoc string) render.Manifest {
	return render.Manifest{APIVersion: "v1", Kind: kind, Name: name, Namespace: "default", YAML: yamlDoc}
}

func TestComputeIgnoresCommentOnlyDifferences(t *testing.T) {
	base := &render.Result{Manifests: []render.Manifest{
		manifest("ConfigMap", "a", "# Source: x/templates/cm.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  k: v\n"),
	}}
	head := &render.Result{Manifests: []render.Manifest{
		manifest("ConfigMap", "a", "# Source: y/templates/cm.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  k: v\n"),
	}}
	res := Compute(base, head)
	if len(res.Added)+len(res.Removed)+len(res.Changed) != 0 {
		t.Fatalf("comment-only difference reported as a change: %+v", res)
	}
}

func TestComputeNamesChangedTopLevelFields(t *testing.T) {
	base := &render.Result{Manifests: []render.Manifest{
		manifest("ConfigMap", "a", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  k: v\n"),
	}}
	head := &render.Result{Manifests: []render.Manifest{
		manifest("ConfigMap", "a", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  k: v2\nbinaryData: {}\n"),
	}}
	res := Compute(base, head)
	if len(res.Changed) != 1 {
		t.Fatalf("changed = %+v, want one entry", res.Changed)
	}
	want := "changed top-level fields: binaryData, data"
	if res.Changed[0].Summary != want {
		t.Errorf("summary = %q, want %q", res.Changed[0].Summary, want)
	}
}

func TestSchemaChanged(t *testing.T) {
	obj := json.RawMessage(`{"type":"object"}`)
	objReordered := json.RawMessage(`{ "type" : "object" }`)
	other := json.RawMessage(`{"type":"object","required":["x"]}`)

	cases := []struct {
		name       string
		base, head json.RawMessage
		want       bool
	}{
		{"both nil", nil, nil, false},
		{"added", nil, obj, true},
		{"removed", obj, nil, true},
		{"equal modulo whitespace", obj, objReordered, false},
		{"different", obj, other, true},
	}
	for _, tc := range cases {
		if got := schemaChanged(tc.base, tc.head); got != tc.want {
			t.Errorf("%s: schemaChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
}
