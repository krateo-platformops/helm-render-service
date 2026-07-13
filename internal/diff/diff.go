// Package diff compares two render results object-by-object. Objects are
// keyed by (apiVersion, kind, namespace, name); a change summary lists the
// top-level fields (spec, data, metadata, ...) whose parsed content differs.
// This feeds "upgrade-impact explain" in the Krateo portal.
package diff

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"github.com/braghettos/helm-render-service/internal/render"
	"sigs.k8s.io/yaml"
)

// ObjectRef identifies a rendered Kubernetes object.
type ObjectRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
}

// Change describes an object present in both renders whose content differs.
type Change struct {
	Ref     ObjectRef `json:"ref"`
	Summary string    `json:"summary"`
}

// Result is the JSON body of a 200 response from POST /diff.
type Result struct {
	Added               []ObjectRef `json:"added"`
	Removed             []ObjectRef `json:"removed"`
	Changed             []Change    `json:"changed"`
	ValuesSchemaChanged bool        `json:"valuesSchemaChanged"`
}

// Compute diffs two renders of the same release (base -> head).
func Compute(base, head *render.Result) Result {
	baseByKey := indexManifests(base.Manifests)
	headByKey := indexManifests(head.Manifests)

	res := Result{
		Added:   []ObjectRef{},
		Removed: []ObjectRef{},
		Changed: []Change{},
	}

	for key, hm := range headByKey {
		bm, inBase := baseByKey[key]
		if !inBase {
			res.Added = append(res.Added, refOf(hm))
			continue
		}
		if fields := changedTopLevelFields(bm.YAML, hm.YAML); len(fields) > 0 {
			res.Changed = append(res.Changed, Change{
				Ref:     refOf(hm),
				Summary: "changed top-level fields: " + strings.Join(fields, ", "),
			})
		}
	}
	for key, bm := range baseByKey {
		if _, inHead := headByKey[key]; !inHead {
			res.Removed = append(res.Removed, refOf(bm))
		}
	}

	sortRefs(res.Added)
	sortRefs(res.Removed)
	sort.Slice(res.Changed, func(i, j int) bool { return refLess(res.Changed[i].Ref, res.Changed[j].Ref) })

	res.ValuesSchemaChanged = schemaChanged(base.ValuesSchema, head.ValuesSchema)
	return res
}

func indexManifests(manifests []render.Manifest) map[ObjectRef]render.Manifest {
	byKey := make(map[ObjectRef]render.Manifest, len(manifests))
	for _, m := range manifests {
		byKey[refOf(m)] = m
	}
	return byKey
}

func refOf(m render.Manifest) ObjectRef {
	return ObjectRef{APIVersion: m.APIVersion, Kind: m.Kind, Name: m.Name, Namespace: m.Namespace}
}

func sortRefs(refs []ObjectRef) {
	sort.Slice(refs, func(i, j int) bool { return refLess(refs[i], refs[j]) })
}

func refLess(a, b ObjectRef) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.APIVersion < b.APIVersion
}

// changedTopLevelFields parses both documents and returns the sorted list of
// top-level fields whose content differs; nil when the objects are equal
// (YAML comments are ignored by construction).
func changedTopLevelFields(baseYAML, headYAML string) []string {
	var baseObj, headObj map[string]interface{}
	baseErr := yaml.Unmarshal([]byte(baseYAML), &baseObj)
	headErr := yaml.Unmarshal([]byte(headYAML), &headObj)
	if baseErr != nil || headErr != nil {
		// Unparseable content: fall back to a raw comparison.
		if strings.TrimSpace(baseYAML) == strings.TrimSpace(headYAML) {
			return nil
		}
		return []string{"(unparsed document)"}
	}
	if reflect.DeepEqual(baseObj, headObj) {
		return nil
	}

	seen := map[string]bool{}
	var fields []string
	for key := range baseObj {
		seen[key] = true
		if !reflect.DeepEqual(baseObj[key], headObj[key]) {
			fields = append(fields, key)
		}
	}
	for key := range headObj {
		if !seen[key] && !reflect.DeepEqual(baseObj[key], headObj[key]) {
			fields = append(fields, key)
		}
	}
	sort.Strings(fields)
	return fields
}

// schemaChanged compares two values.schema.json payloads structurally.
func schemaChanged(base, head json.RawMessage) bool {
	if len(base) == 0 && len(head) == 0 {
		return false
	}
	if len(base) == 0 || len(head) == 0 {
		return true
	}
	var baseVal, headVal interface{}
	if json.Unmarshal(base, &baseVal) != nil || json.Unmarshal(head, &headVal) != nil {
		return !bytes.Equal(base, head)
	}
	return !reflect.DeepEqual(baseVal, headVal)
}
