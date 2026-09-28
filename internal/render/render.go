// Package render turns a chart source (inline files, OCI reference, tgz URL
// or classic helm repository) into rendered Kubernetes manifests using the
// helm SDK in pure client-only dry-run mode: no Kubernetes client is ever
// built and no cluster is contacted. The template `lookup` function returns
// empty results, exactly like `helm template`. Hooks are rendered and
// included in the output but are never executed.
package render

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	"sigs.k8s.io/yaml"
)

// Options control a single render.
type Options struct {
	ReleaseName string
	Namespace   string
	// KubeVersion sets the .Capabilities.KubeVersion seen by templates.
	// nil keeps the helm SDK default.
	KubeVersion *chartutil.KubeVersion
	// MaxOutputBytes caps the total rendered manifest size (manifests +
	// hooks). 0 means unlimited.
	MaxOutputBytes int64
	// LookupStubs, when non-empty, answer the template `lookup` function
	// (see LookupStub). Empty keeps `helm template` semantics exactly.
	LookupStubs []LookupStub
}

// Manifest is one rendered Kubernetes object.
type Manifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	YAML       string `json:"yaml"`
}

// Result is the outcome of a successful render. It doubles as the JSON body
// of a 200 response from POST /render.
type Result struct {
	Manifests    []Manifest      `json:"manifests"`
	ValuesSchema json.RawMessage `json:"valuesSchema"` // null when the chart has no values.schema.json
	Notes        *string         `json:"notes"`        // null when the chart has no NOTES.txt
	// Lookups lists every distinct lookup a stubbed render made; nil for a
	// render without stubs.
	Lookups []LookupCall `json:"lookups,omitempty"`
}

// Render renders ch against values with `helm template` semantics
// (action.Install with DryRun+ClientOnly+Replace). The context deadline is
// the render timeout: template execution runs in a goroutine and is abandoned
// if the deadline expires.
func Render(ctx context.Context, ch *chart.Chart, values map[string]interface{}, opts Options) (*Result, error) {
	if opts.ReleaseName == "" {
		opts.ReleaseName = "render"
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if values == nil {
		values = map[string]interface{}{}
	}
	if len(opts.LookupStubs) > 0 {
		return renderStubbed(ctx, ch, values, opts)
	}

	cfg := new(action.Configuration)
	cfg.Log = func(string, ...interface{}) {}

	inst := action.NewInstall(cfg)
	inst.DryRun = true
	inst.DryRunOption = "client" // never interact with a remote cluster; `lookup` returns empty
	inst.ClientOnly = true
	inst.Replace = true
	inst.IncludeCRDs = true
	inst.ReleaseName = opts.ReleaseName
	inst.Namespace = opts.Namespace
	inst.KubeVersion = opts.KubeVersion

	type outcome struct {
		rel *release.Release
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{nil, fmt.Errorf("render panicked: %v", r)}
			}
		}()
		rel, err := inst.RunWithContext(ctx, ch, values)
		done <- outcome{rel, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case o := <-done:
		if o.err != nil {
			return nil, o.err
		}
		if err := checkOutputSize(o.rel, opts.MaxOutputBytes); err != nil {
			return nil, err
		}
		return buildResult(ch, o.rel), nil
	}
}

// renderStubbed renders like Render, with `lookup` answered from the stubs.
// helm's action.Install offers no way to hand the engine a lookup provider
// short of a live cluster, so this runs the steps a client-only install runs —
// dependencies, capabilities, schema-validated values, the engine, NOTES,
// manifest sort, CRDs first — around engine.RenderWithClientProvider.
// TestStubbedRenderMatchesInstall pins the two paths to the same output.
func renderStubbed(ctx context.Context, ch *chart.Chart, values map[string]interface{}, opts Options) (*Result, error) {
	provider := newStubProvider(opts.LookupStubs)

	type outcome struct {
		rel *release.Release
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{nil, fmt.Errorf("render panicked: %v", r)}
			}
		}()
		rel, err := stubbedRelease(ch, values, opts, provider)
		done <- outcome{rel, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case o := <-done:
		if o.err != nil {
			return nil, o.err
		}
		if err := checkOutputSize(o.rel, opts.MaxOutputBytes); err != nil {
			return nil, err
		}
		res := buildResult(ch, o.rel)
		res.Lookups = provider.lookups()
		return res, nil
	}
}

func stubbedRelease(ch *chart.Chart, values map[string]interface{}, opts Options, provider engine.ClientProvider) (*release.Release, error) {
	if err := chartutil.ProcessDependenciesWithMerge(ch, values); err != nil {
		return nil, err
	}
	caps := chartutil.DefaultCapabilities.Copy()
	if opts.KubeVersion != nil {
		caps.KubeVersion = *opts.KubeVersion
	}
	if ch.Metadata.KubeVersion != "" && !chartutil.IsCompatibleRange(ch.Metadata.KubeVersion, caps.KubeVersion.String()) {
		return nil, fmt.Errorf("chart requires kubeVersion: %s which is incompatible with Kubernetes %s", ch.Metadata.KubeVersion, caps.KubeVersion.String())
	}
	renderValues, err := chartutil.ToRenderValuesWithSchemaValidation(ch, values, chartutil.ReleaseOptions{
		Name:      opts.ReleaseName,
		Namespace: opts.Namespace,
		Revision:  1,
		IsInstall: true,
	}, caps, false)
	if err != nil {
		return nil, err
	}
	files, err := engine.RenderWithClientProvider(ch, renderValues, provider)
	if err != nil {
		return nil, err
	}

	// NOTES.txt renders like a template but is no manifest: only the top-level
	// chart's is kept (SubNotes off), as helm does.
	var notes string
	for name, content := range files {
		if strings.HasSuffix(name, notesSuffix) {
			if name == path.Join(ch.Name(), "templates", notesSuffix) {
				notes = content
			}
			delete(files, name)
		}
	}

	hooks, manifests, err := releaseutil.SortManifests(files, nil, releaseutil.InstallOrder)
	if err != nil {
		return nil, err
	}
	var doc bytes.Buffer
	for _, crd := range ch.CRDObjects() {
		fmt.Fprintf(&doc, "---\n# Source: %s\n%s\n", crd.Filename, string(crd.File.Data))
	}
	for _, m := range manifests {
		fmt.Fprintf(&doc, "---\n# Source: %s\n%s\n", m.Name, m.Content)
	}
	return &release.Release{Manifest: doc.String(), Hooks: hooks, Info: &release.Info{Notes: notes}}, nil
}

const notesSuffix = "NOTES.txt"

// checkOutputSize enforces the rendered-output guardrail (manifests plus
// hook manifests).
func checkOutputSize(rel *release.Release, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	total := int64(len(rel.Manifest))
	for _, h := range rel.Hooks {
		if h != nil {
			total += int64(len(h.Manifest))
		}
	}
	if total > maxBytes {
		return fmt.Errorf("rendered output is %d bytes, over the %d byte limit", total, maxBytes)
	}
	return nil
}

func buildResult(ch *chart.Chart, rel *release.Release) *Result {
	res := &Result{Manifests: []Manifest{}}

	for _, doc := range splitDocs(rel.Manifest) {
		if m, ok := parseManifest(doc); ok {
			res.Manifests = append(res.Manifests, m)
		}
	}
	// Hooks are rendered but never executed; include them so callers see the
	// full set of objects the chart would create.
	for _, h := range rel.Hooks {
		if h == nil {
			continue
		}
		if m, ok := parseManifest(h.Manifest); ok {
			res.Manifests = append(res.Manifests, m)
		}
	}

	if len(ch.Schema) > 0 && json.Valid(ch.Schema) {
		res.ValuesSchema = json.RawMessage(ch.Schema)
	}
	if rel.Info != nil {
		if notes := strings.TrimSpace(rel.Info.Notes); notes != "" {
			res.Notes = &notes
		}
	}
	return res
}

// splitDocs splits a multi-document manifest string preserving helm's
// rendering order (SplitManifests returns a map keyed "manifest-N").
func splitDocs(manifest string) []string {
	byKey := releaseutil.SplitManifests(manifest)
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return manifestKeyIndex(keys[i]) < manifestKeyIndex(keys[j]) })
	docs := make([]string, 0, len(keys))
	for _, k := range keys {
		docs = append(docs, byKey[k])
	}
	return docs
}

func manifestKeyIndex(key string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(key, "manifest-"))
	if err != nil {
		return math.MaxInt
	}
	return n
}

// parseManifest extracts the object header from one YAML document. Documents
// that hold no object (empty or comments only) are dropped.
func parseManifest(doc string) (Manifest, bool) {
	trimmed := strings.TrimSpace(doc)
	if trimmed == "" {
		return Manifest{}, false
	}

	var generic map[string]interface{}
	if err := yaml.Unmarshal([]byte(trimmed), &generic); err == nil && len(generic) == 0 {
		return Manifest{}, false // comments-only or null document
	}

	m := Manifest{YAML: trimmed + "\n"}
	var head struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(trimmed), &head); err == nil {
		m.APIVersion = head.APIVersion
		m.Kind = head.Kind
		m.Name = head.Metadata.Name
		m.Namespace = head.Metadata.Namespace
	}
	return m, true
}
