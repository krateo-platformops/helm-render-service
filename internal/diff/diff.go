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

	"github.com/krateo-platformops/helm-render-service/internal/render"
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

// SchemaFieldDiff is the FIELD-LEVEL breakdown of a values.schema.json change —
// which configurable fields a version Update adds, removes, newly requires, or
// changes the default of. Dot-paths name nested fields (e.g. "database.replicas").
// It refines the coarse ValuesSchemaChanged boolean into "what actually changed in
// the create form", so the portal can tell the user BEFORE the gated Update.
type SchemaFieldDiff struct {
	Added           []string `json:"added"`
	Removed         []string `json:"removed"`
	NowRequired     []string `json:"nowRequired"`
	ChangedDefaults []string `json:"changedDefaults"`
}

// Result is the JSON body of a 200 response from POST /diff.
type Result struct {
	Added               []ObjectRef `json:"added"`
	Removed             []ObjectRef `json:"removed"`
	Changed             []Change    `json:"changed"`
	ValuesSchemaChanged bool        `json:"valuesSchemaChanged"`
	// ValuesSchemaDiff is the field-level schema breakdown; nil when neither version
	// ships a values.schema.json (a chart with no configurable form). Present (possibly
	// with all-empty lists) whenever at least one version has a schema.
	ValuesSchemaDiff *SchemaFieldDiff `json:"valuesSchemaDiff,omitempty"`
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
	res.ValuesSchemaDiff = schemaFieldDiff(base.ValuesSchema, head.ValuesSchema)
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

// schemaField is the per-field info flattenSchema tracks for the diff.
type schemaField struct {
	required   bool
	hasDefault bool
	defaultVal interface{}
}

// flattenSchema walks a JSON-Schema's `properties` tree into a map of dot-path ->
// field info. A parent's `required` array marks which of its immediate properties
// are required. Nested objects recurse (parent.child). A missing/unparseable schema
// flattens to an empty map (so a schema appearing/disappearing shows as all-added or
// all-removed). Best-effort by design — it names create-form fields, not every JSON
// Schema keyword (enum/description changes still trip the coarse ValuesSchemaChanged).
func flattenSchema(raw json.RawMessage) map[string]schemaField {
	out := map[string]schemaField{}
	var root map[string]interface{}
	if len(raw) == 0 || json.Unmarshal(raw, &root) != nil || root == nil {
		return out
	}
	var walk func(node map[string]interface{}, prefix string)
	walk = func(node map[string]interface{}, prefix string) {
		props, ok := node["properties"].(map[string]interface{})
		if !ok {
			return
		}
		requiredSet := map[string]bool{}
		if reqs, ok := node["required"].([]interface{}); ok {
			for _, r := range reqs {
				if s, ok := r.(string); ok {
					requiredSet[s] = true
				}
			}
		}
		for name, v := range props {
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			child, _ := v.(map[string]interface{})
			if _, hasNested := child["properties"]; hasNested {
				// A container node — recurse into its leaf fields; don't record the
				// container itself (the form inputs are its leaves, e.g. database.host).
				walk(child, path)
				continue
			}
			def, hasDef := child["default"]
			out[path] = schemaField{required: requiredSet[name], hasDefault: hasDef, defaultVal: def}
		}
	}
	walk(root, "")
	return out
}

// schemaFieldDiff computes the field-level breakdown of a values.schema.json change:
// fields added / removed, fields newly required, and fields whose default changed.
// Returns nil ONLY when neither version ships a schema; otherwise a diff (possibly
// with all-empty lists) so the consumer can distinguish "no schema" from "no change".
func schemaFieldDiff(base, head json.RawMessage) *SchemaFieldDiff {
	if len(base) == 0 && len(head) == 0 {
		return nil
	}
	baseFields := flattenSchema(base)
	headFields := flattenSchema(head)
	diff := &SchemaFieldDiff{Added: []string{}, Removed: []string{}, NowRequired: []string{}, ChangedDefaults: []string{}}
	for path, hf := range headFields {
		bf, inBase := baseFields[path]
		if !inBase {
			diff.Added = append(diff.Added, path)
		} else if hf.hasDefault != bf.hasDefault || !reflect.DeepEqual(hf.defaultVal, bf.defaultVal) {
			diff.ChangedDefaults = append(diff.ChangedDefaults, path)
		}
		if hf.required && !(inBase && bf.required) {
			diff.NowRequired = append(diff.NowRequired, path)
		}
	}
	for path := range baseFields {
		if _, inHead := headFields[path]; !inHead {
			diff.Removed = append(diff.Removed, path)
		}
	}
	sort.Strings(diff.Added)
	sort.Strings(diff.Removed)
	sort.Strings(diff.NowRequired)
	sort.Strings(diff.ChangedDefaults)
	return diff
}
