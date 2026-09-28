package render

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gatedChart is a chart shaped like the Krateo blueprint composer's output: a
// ConfigMap, and a Deployment behind a generated gate that waits for the
// ConfigMap to exist and for a Repository's .status.default_branch — the field
// form (a dig) and the condition form (a range over status.conditions) of
// readiness.
func gatedChart(t *testing.T) []ChartFile {
	t.Helper()
	return []ChartFile{
		{Path: "Chart.yaml", Content: "apiVersion: v2\nname: gated\nversion: 0.1.0\n"},
		{Path: "values.yaml", Content: "{}\n"},
		{Path: "templates/configmap.yaml", Content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-config
data:
  a: b
`},
		{Path: "templates/deployment.yaml", Content: `{{- /* krateo:gate begin — generated from architecture.yaml; edit the descriptor, not this block. */}}
{{- $gate := true -}}
{{- $dep0 := lookup "v1" "ConfigMap" $.Release.Namespace (printf "%s-config" $.Release.Name) -}}
{{- if not $dep0 -}}{{- $gate = false -}}{{- end -}}
{{- $dep1 := lookup "github.krateo.io/v1alpha1" "Repository" $.Release.Namespace (printf "%s-repo" $.Release.Name) -}}
{{- if not (and $dep1 (dig "status" "default_branch" "" $dep1)) -}}{{- $gate = false -}}{{- end -}}
{{- $dep2 := lookup "composition.krateo.io/v0-1-0" "Database" $.Release.Namespace "db" -}}
{{- $ok2Ready := false -}}
{{- range (dig "status" "conditions" (list) $dep2) -}}{{- if and (eq (toString .type) "Ready") (eq (toString .status) "True") -}}{{- $ok2Ready = true -}}{{- end -}}{{- end -}}
{{- if not $ok2Ready -}}{{- $gate = false -}}{{- end -}}
{{- if $gate }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-app
spec:
  template:
    spec:
      containers:
        - name: app
          image: nginx:1.27
{{- end }}
{{- /* krateo:gate end */}}
`},
	}
}

func renderFiles(t *testing.T, files []ChartFile, opts Options) *Result {
	t.Helper()
	ch, err := loadInline(files)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Render(ctx, ch, map[string]interface{}{}, opts)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return res
}

func kinds(res *Result) []string {
	out := make([]string, 0, len(res.Manifests))
	for _, m := range res.Manifests {
		out = append(out, m.Kind+"/"+m.Name)
	}
	return out
}

// openStubs are the stubs that open every guard of gatedChart.
func openStubs() []LookupStub {
	return []LookupStub{
		{APIVersion: "v1", Kind: "ConfigMap"},
		{APIVersion: "github.krateo.io/v1alpha1", Kind: "Repository", Object: map[string]interface{}{
			"status": map[string]interface{}{"default_branch": "main"},
		}},
		{APIVersion: "composition.krateo.io/v0-1-0", Kind: "Database", Object: map[string]interface{}{
			"status": map[string]interface{}{"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True"},
			}},
		}},
	}
}

func TestGateStaysClosedWithoutStubs(t *testing.T) {
	res := renderFiles(t, gatedChart(t), Options{ReleaseName: "r", Namespace: "ns"})
	if got := kinds(res); !reflect.DeepEqual(got, []string{"ConfigMap/r-config"}) {
		t.Fatalf("without stubs the gate must hold the Deployment back, got %v", got)
	}
	if res.Lookups != nil {
		t.Fatalf("a render without stubs reports no lookups, got %v", res.Lookups)
	}
}

func TestStubsOpenTheGate(t *testing.T) {
	res := renderFiles(t, gatedChart(t), Options{ReleaseName: "r", Namespace: "ns", LookupStubs: openStubs()})
	if got := kinds(res); !reflect.DeepEqual(got, []string{"ConfigMap/r-config", "Deployment/r-app"}) {
		t.Fatalf("with every guard stubbed the Deployment renders, got %v", got)
	}
	want := []LookupCall{
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "r-config", Stubbed: true},
		{APIVersion: "github.krateo.io/v1alpha1", Kind: "Repository", Namespace: "ns", Name: "r-repo", Stubbed: true},
		{APIVersion: "composition.krateo.io/v0-1-0", Kind: "Database", Namespace: "ns", Name: "db", Stubbed: true},
	}
	if !reflect.DeepEqual(res.Lookups, want) {
		t.Fatalf("lookups:\n got %+v\nwant %+v", res.Lookups, want)
	}
}

func TestOneMissingStubKeepsTheGateClosedAndSaysWhich(t *testing.T) {
	stubs := openStubs()[:2] // no Database
	res := renderFiles(t, gatedChart(t), Options{ReleaseName: "r", Namespace: "ns", LookupStubs: stubs})
	if got := kinds(res); !reflect.DeepEqual(got, []string{"ConfigMap/r-config"}) {
		t.Fatalf("an unanswered guard keeps the gate closed, got %v", got)
	}
	last := res.Lookups[len(res.Lookups)-1]
	if last.Kind != "Database" || last.Stubbed {
		t.Fatalf("the unanswered lookup is reported as not stubbed, got %+v", last)
	}
}

func TestStubThatDoesNotSatisfyReadinessKeepsTheGateClosed(t *testing.T) {
	stubs := openStubs()
	stubs[1].Object = map[string]interface{}{"status": map[string]interface{}{}} // exists, not ready
	res := renderFiles(t, gatedChart(t), Options{ReleaseName: "r", Namespace: "ns", LookupStubs: stubs})
	if got := kinds(res); len(got) != 1 {
		t.Fatalf("a stub is an object, not a bypass: readiness still decides, got %v", got)
	}
}

func TestExactNameBeatsWildcardAndNamespaceNarrows(t *testing.T) {
	p := newStubProvider([]LookupStub{
		{APIVersion: "v1", Kind: "ConfigMap", Object: map[string]interface{}{"data": map[string]interface{}{"from": "wildcard"}}},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "x", Object: map[string]interface{}{"data": map[string]interface{}{"from": "exact"}}},
		{APIVersion: "v1", Kind: "Secret", Namespace: "other"},
	})
	c, _, _ := p.GetClientFor("v1", "ConfigMap")
	obj, err := c.Namespace("ns").Get(context.Background(), "x", metavOpts)
	if err != nil || obj.Object["data"].(map[string]interface{})["from"] != "exact" {
		t.Fatalf("exact name wins: %v %v", obj, err)
	}
	obj, err = c.Namespace("ns").Get(context.Background(), "y", metavOpts)
	if err != nil || obj.GetName() != "y" || obj.GetNamespace() != "ns" || obj.GetKind() != "ConfigMap" {
		t.Fatalf("a wildcard answers any name, as the object looked up: %v %v", obj, err)
	}
	s, _, _ := p.GetClientFor("v1", "Secret")
	if _, err := s.Namespace("ns").Get(context.Background(), "z", metavOpts); err == nil {
		t.Fatalf("a stub pinned to another namespace does not answer")
	}
}

func TestListLookupReturnsTheMatchingStubs(t *testing.T) {
	files := []ChartFile{
		{Path: "Chart.yaml", Content: "apiVersion: v2\nname: lister\nversion: 0.1.0\n"},
		{Path: "templates/cm.yaml", Content: `{{- $all := lookup "v1" "ConfigMap" $.Release.Namespace "" -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: count
data:
  n: {{ len ($all.items | default list) | quote }}
`},
	}
	res := renderFiles(t, files, Options{Namespace: "ns", LookupStubs: []LookupStub{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "a"},
		{APIVersion: "v1", Kind: "ConfigMap"},
	}})
	if !strings.Contains(res.Manifests[0].YAML, `n: "2"`) {
		t.Fatalf("a list lookup sees every matching stub:\n%s", res.Manifests[0].YAML)
	}
}

func TestValidateLookupStubs(t *testing.T) {
	if err := ValidateLookupStubs([]LookupStub{{Kind: "ConfigMap"}}); err == nil {
		t.Fatalf("apiVersion is required")
	}
	if err := ValidateLookupStubs(make([]LookupStub, MaxLookupStubs+1)); err == nil {
		t.Fatalf("the stub count is capped")
	}
	if err := ValidateLookupStubs(openStubs()); err != nil {
		t.Fatalf("valid stubs: %v", err)
	}
}

// TestStubbedRenderMatchesInstall pins the stubbed path to action.Install's
// output: with stubs that match nothing, every fixture chart renders the same
// manifests, schema and notes either way.
func TestStubbedRenderMatchesInstall(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "charts")
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := map[string][]ChartFile{"gated": gatedChart(t)}
	for _, d := range dirs {
		if d.IsDir() {
			fixtures[d.Name()] = readChartDir(t, filepath.Join(root, d.Name()))
		}
	}
	unmatched := []LookupStub{{APIVersion: "example.com/v1", Kind: "Nothing"}}
	for name, files := range fixtures {
		t.Run(name, func(t *testing.T) {
			plain := renderFiles(t, files, Options{ReleaseName: "r", Namespace: "ns"})
			stubbed := renderFiles(t, files, Options{ReleaseName: "r", Namespace: "ns", LookupStubs: unmatched})
			if !reflect.DeepEqual(plain.Manifests, stubbed.Manifests) {
				t.Fatalf("manifests differ:\n plain %+v\nstubbed %+v", plain.Manifests, stubbed.Manifests)
			}
			if string(plain.ValuesSchema) != string(stubbed.ValuesSchema) || !reflect.DeepEqual(plain.Notes, stubbed.Notes) {
				t.Fatalf("schema or notes differ")
			}
		})
	}
}

func readChartDir(t *testing.T, root string) []ChartFile {
	t.Helper()
	var files []ChartFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		// Fixtures ship Chart.yaml as Chart.yaml.tpl, so the release never publishes them as charts.
		slashed := filepath.ToSlash(rel)
		if slashed == "Chart.yaml.tpl" {
			slashed = "Chart.yaml"
		}
		files = append(files, ChartFile{Path: slashed, Content: string(content)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

var metavOpts = metav1.GetOptions{}
