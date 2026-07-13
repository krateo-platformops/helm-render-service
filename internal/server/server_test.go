package server_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braghettos/helm-render-service/internal/render"
	"github.com/braghettos/helm-render-service/internal/server"
)

// ---- helpers ---------------------------------------------------------------

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	return server.New(server.Config{RenderTimeout: 30 * time.Second})
}

// chartFiles loads a fixture chart directory into an inline files payload.
func chartFiles(t *testing.T, dir string) []render.ChartFile {
	t.Helper()
	root := filepath.Join("..", "..", "testdata", "charts", dir)
	var files []render.ChartFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, render.ChartFile{Path: filepath.ToSlash(rel), Content: string(content)})
		return nil
	})
	if err != nil {
		t.Fatalf("load fixture chart %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("fixture chart %s is empty", dir)
	}
	return files
}

func withoutFile(files []render.ChartFile, name string) []render.ChartFile {
	out := make([]render.ChartFile, 0, len(files))
	for _, f := range files {
		if f.Path != name {
			out = append(out, f)
		}
	}
	return out
}

func post(t *testing.T, h http.Handler, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// Response mirrors, decoded from JSON to keep tests honest about the wire shape.

type manifestResp struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	YAML       string `json:"yaml"`
}

type renderResp struct {
	Manifests    []manifestResp  `json:"manifests"`
	ValuesSchema json.RawMessage `json:"valuesSchema"`
	Notes        *string         `json:"notes"`
}

type objectRefResp struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
}

type changeResp struct {
	Ref     objectRefResp `json:"ref"`
	Summary string        `json:"summary"`
}

type diffResp struct {
	Added               []objectRefResp `json:"added"`
	Removed             []objectRefResp `json:"removed"`
	Changed             []changeResp    `json:"changed"`
	ValuesSchemaChanged bool            `json:"valuesSchemaChanged"`
}

type errorResp struct {
	Error string `json:"error"`
}

func findManifest(manifests []manifestResp, kind, name string) *manifestResp {
	for i := range manifests {
		if manifests[i].Kind == kind && manifests[i].Name == name {
			return &manifests[i]
		}
	}
	return nil
}

// ---- /healthz ---------------------------------------------------------------

func TestHealthz(t *testing.T) {
	h := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rec.Code)
	}
}

// ---- /render ----------------------------------------------------------------

func TestRenderInlineChartHappyPath(t *testing.T) {
	h := newHandler(t)
	rec := post(t, h, "/render", map[string]interface{}{
		"chart":  map[string]interface{}{"files": chartFiles(t, "demo-v1")},
		"values": map[string]interface{}{"message": "from-test"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /render = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp renderResp
	decodeBody(t, rec, &resp)

	cm := findManifest(resp.Manifests, "ConfigMap", "render-demo")
	if cm == nil {
		t.Fatalf("ConfigMap render-demo missing from manifests: %+v", resp.Manifests)
	}
	if cm.Namespace != "default" {
		t.Errorf("ConfigMap namespace = %q, want default", cm.Namespace)
	}
	if !strings.Contains(cm.YAML, `message: "from-test"`) {
		t.Errorf("ConfigMap yaml does not carry the supplied value:\n%s", cm.YAML)
	}
	dep := findManifest(resp.Manifests, "Deployment", "render-demo")
	if dep == nil {
		t.Fatalf("Deployment render-demo missing from manifests")
	}
	if dep.APIVersion != "apps/v1" {
		t.Errorf("Deployment apiVersion = %q, want apps/v1", dep.APIVersion)
	}

	// values.schema.json extraction
	if string(resp.ValuesSchema) == "null" || len(resp.ValuesSchema) == 0 {
		t.Fatalf("valuesSchema is null, want the chart schema")
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(resp.ValuesSchema, &schema); err != nil {
		t.Fatalf("valuesSchema is not an object: %v", err)
	}
	if _, ok := schema["properties"].(map[string]interface{})["replicaCount"]; !ok {
		t.Errorf("valuesSchema lost the replicaCount property: %s", resp.ValuesSchema)
	}

	if resp.Notes == nil || !strings.Contains(*resp.Notes, "demo 0.1.0") {
		t.Errorf("notes = %v, want the rendered NOTES.txt", resp.Notes)
	}
}

func TestRenderBadValuesReturns422(t *testing.T) {
	h := newHandler(t)
	rec := post(t, h, "/render", map[string]interface{}{
		"chart":  map[string]interface{}{"files": chartFiles(t, "demo-v1")},
		"values": map[string]interface{}{"message": "ok", "replicaCount": "three"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /render with schema-violating values = %d, want 422; body %s", rec.Code, rec.Body.String())
	}
	var resp errorResp
	decodeBody(t, rec, &resp)
	if !strings.Contains(resp.Error, "replicaCount") {
		t.Errorf("error %q does not mention the offending field", resp.Error)
	}
}

func TestRenderBadInputReturns400(t *testing.T) {
	h := newHandler(t)

	// no chart source at all
	rec := post(t, h, "/render", map[string]interface{}{"values": map[string]interface{}{}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /render without a chart = %d, want 400; body %s", rec.Code, rec.Body.String())
	}

	// both url and files
	rec = post(t, h, "/render", map[string]interface{}{
		"chart": map[string]interface{}{
			"url":   "https://example.com/chart-1.0.0.tgz",
			"files": chartFiles(t, "demo-v1"),
		},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /render with url+files = %d, want 400; body %s", rec.Code, rec.Body.String())
	}

	// malformed JSON
	req := httptest.NewRequest(http.MethodPost, "/render", strings.NewReader("{not json"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /render with malformed JSON = %d, want 400", rr.Code)
	}
}

func TestRenderSchemaNullWhenChartHasNone(t *testing.T) {
	h := newHandler(t)
	files := withoutFile(chartFiles(t, "demo-v1"), "values.schema.json")
	rec := post(t, h, "/render", map[string]interface{}{
		"chart": map[string]interface{}{"files": files},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /render = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp renderResp
	decodeBody(t, rec, &resp)
	if string(resp.ValuesSchema) != "null" {
		t.Errorf("valuesSchema = %s, want null for a chart without values.schema.json", resp.ValuesSchema)
	}
}

// ---- /render via a helm repository (offline, httptest-served) ---------------

// packageChart builds a chart .tgz (top-level directory = chart name) from an
// inline file tree, without shelling out to helm.
func packageChart(t *testing.T, name string, files []render.ChartFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		hdr := &tar.Header{
			Name: name + "/" + f.Path,
			Mode: 0o644,
			Size: int64(len(f.Content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(f.Content)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func TestRenderFromHelmRepoAndTgzURL(t *testing.T) {
	tgz := packageChart(t, "demo", chartFiles(t, "demo-v1"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `apiVersion: v1
entries:
  demo:
    - apiVersion: v2
      name: demo
      version: 0.1.0
      urls:
        - charts/demo-0.1.0.tgz
`)
	})
	mux.HandleFunc("GET /charts/demo-0.1.0.tgz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tgz)
	})
	repoSrv := httptest.NewServer(mux)
	defer repoSrv.Close()

	h := newHandler(t)

	// helm-repo mode: url = repo base URL, repo = chart name, version pinned
	rec := post(t, h, "/render", map[string]interface{}{
		"chart":  map[string]interface{}{"url": repoSrv.URL, "repo": "demo", "version": "0.1.0"},
		"values": map[string]interface{}{"message": "via-repo"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /render (helm repo) = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp renderResp
	decodeBody(t, rec, &resp)
	if cm := findManifest(resp.Manifests, "ConfigMap", "render-demo"); cm == nil || !strings.Contains(cm.YAML, "via-repo") {
		t.Fatalf("helm-repo render lost the ConfigMap or the value: %+v", resp.Manifests)
	}

	// direct tgz mode
	rec = post(t, h, "/render", map[string]interface{}{
		"chart":  map[string]interface{}{"url": repoSrv.URL + "/charts/demo-0.1.0.tgz"},
		"values": map[string]interface{}{"message": "via-tgz"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /render (tgz url) = %d, body %s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &resp)
	if cm := findManifest(resp.Manifests, "ConfigMap", "render-demo"); cm == nil || !strings.Contains(cm.YAML, "via-tgz") {
		t.Fatalf("tgz render lost the ConfigMap or the value: %+v", resp.Manifests)
	}
}

// ---- /diff -------------------------------------------------------------------

func TestDiffDetectsAddedAndChangedAndSchemaChange(t *testing.T) {
	h := newHandler(t)
	rec := post(t, h, "/diff", map[string]interface{}{
		"base":   map[string]interface{}{"files": chartFiles(t, "demo-v1")},
		"head":   map[string]interface{}{"files": chartFiles(t, "demo-v2")},
		"values": map[string]interface{}{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /diff = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp diffResp
	decodeBody(t, rec, &resp)

	if len(resp.Added) != 1 || resp.Added[0].Kind != "Service" || resp.Added[0].Name != "render-demo" {
		t.Errorf("added = %+v, want exactly the new Service render-demo", resp.Added)
	}
	if len(resp.Removed) != 0 {
		t.Errorf("removed = %+v, want empty", resp.Removed)
	}
	if len(resp.Changed) != 1 {
		t.Fatalf("changed = %+v, want exactly the ConfigMap", resp.Changed)
	}
	if resp.Changed[0].Ref.Kind != "ConfigMap" || resp.Changed[0].Ref.Name != "render-demo" {
		t.Errorf("changed ref = %+v, want ConfigMap render-demo", resp.Changed[0].Ref)
	}
	if !strings.Contains(resp.Changed[0].Summary, "data") {
		t.Errorf("change summary %q does not name the data field", resp.Changed[0].Summary)
	}
	if !resp.ValuesSchemaChanged {
		t.Errorf("valuesSchemaChanged = false, want true (v2 adds the service block)")
	}
}

func TestDiffReverseDetectsRemoved(t *testing.T) {
	h := newHandler(t)
	rec := post(t, h, "/diff", map[string]interface{}{
		"base": map[string]interface{}{"files": chartFiles(t, "demo-v2")},
		"head": map[string]interface{}{"files": chartFiles(t, "demo-v1")},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /diff = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp diffResp
	decodeBody(t, rec, &resp)
	if len(resp.Removed) != 1 || resp.Removed[0].Kind != "Service" {
		t.Errorf("removed = %+v, want exactly the Service", resp.Removed)
	}
	if len(resp.Added) != 0 {
		t.Errorf("added = %+v, want empty", resp.Added)
	}
}

func TestDiffBadBaseReturns422WithPrefix(t *testing.T) {
	h := newHandler(t)
	// base renders fine, head violates the schema through values shared by both:
	// use a broken head chart instead (missing required template include).
	broken := []render.ChartFile{
		{Path: "Chart.yaml", Content: "apiVersion: v2\nname: broken\nversion: 0.1.0\n"},
		{Path: "values.yaml", Content: ""},
		{Path: "templates/bad.yaml", Content: "{{ fail \"boom\" }}\n"},
	}
	rec := post(t, h, "/diff", map[string]interface{}{
		"base": map[string]interface{}{"files": chartFiles(t, "demo-v1")},
		"head": map[string]interface{}{"files": broken},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /diff with broken head = %d, want 422; body %s", rec.Code, rec.Body.String())
	}
	var resp errorResp
	decodeBody(t, rec, &resp)
	if !strings.HasPrefix(resp.Error, "head: ") {
		t.Errorf("error %q does not identify the failing side", resp.Error)
	}
}
