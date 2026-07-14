package server_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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

// newHandler builds a handler with the production scheme policy (no plain
// http://). Tests that fetch from httptest servers use newHandlerWith and
// AllowHTTP.
func newHandler(t *testing.T) http.Handler {
	t.Helper()
	return newHandlerWith(t, server.Config{})
}

func newHandlerWith(t *testing.T, cfg server.Config) http.Handler {
	t.Helper()
	if cfg.RenderTimeout == 0 {
		cfg.RenderTimeout = 30 * time.Second
	}
	return server.New(cfg)
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

// tinyChart is a minimal rawTemplates chart: one ConfigMap driven by
// .Values.who.
func tinyChart() map[string]string {
	return map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: tiny\nversion: 0.1.0\n",
		"values.yaml": "who: world\n",
		"templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n" +
			"  name: {{ .Release.Name }}-hello\n  namespace: {{ .Release.Namespace }}\ndata:\n" +
			"  greeting: hello {{ .Values.who }}\n",
	}
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

type objectResp struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	YAML       string `json:"yaml"`
}

type renderResp struct {
	Objects      []objectResp    `json:"objects"`
	ValuesSchema json.RawMessage `json:"valuesSchema"`
	Notes        *string         `json:"notes"`
	Error        string          `json:"error"`
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
	Error               string          `json:"error"`
}

type errorResp struct {
	Error string `json:"error"`
}

func findObject(objects []objectResp, kind, name string) *objectResp {
	for i := range objects {
		if objects[i].Kind == kind && objects[i].Name == name {
			return &objects[i]
		}
	}
	return nil
}

// renderOK posts to /render and fails the test unless the response is a 200
// success (no error field).
func renderOK(t *testing.T, h http.Handler, body interface{}) renderResp {
	t.Helper()
	rec := post(t, h, "/render", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /render = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp renderResp
	decodeBody(t, rec, &resp)
	if resp.Error != "" {
		t.Fatalf("POST /render returned a render error: %q", resp.Error)
	}
	return resp
}

// renderErr posts to /render and fails the test unless the response is a 200
// with a render error (bad chart is data, not a server error).
func renderErr(t *testing.T, h http.Handler, body interface{}) string {
	t.Helper()
	rec := post(t, h, "/render", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /render = %d, want 200 with {error}; body %s", rec.Code, rec.Body.String())
	}
	var resp errorResp
	decodeBody(t, rec, &resp)
	if resp.Error == "" {
		t.Fatalf("POST /render succeeded, want a render error; body %s", rec.Body.String())
	}
	return resp.Error
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

// ---- /render: input modes ----------------------------------------------------

func TestRenderRawTemplatesHappyPath(t *testing.T) {
	h := newHandler(t)
	resp := renderOK(t, h, map[string]interface{}{
		"rawTemplates": tinyChart(),
		"values":       map[string]interface{}{"who": "krateo"},
	})
	if len(resp.Objects) != 1 {
		t.Fatalf("objects = %+v, want exactly the ConfigMap", resp.Objects)
	}
	cm := resp.Objects[0]
	if cm.Kind != "ConfigMap" || cm.APIVersion != "v1" || cm.Name != "render-hello" {
		t.Errorf("object header = %+v, want v1 ConfigMap render-hello", cm)
	}
	if cm.Namespace != "default" {
		t.Errorf("namespace = %q, want default", cm.Namespace)
	}
	if !strings.Contains(cm.YAML, "greeting: hello krateo") {
		t.Errorf("rendered yaml does not carry the supplied value:\n%s", cm.YAML)
	}
}

func TestRenderInlineChartHappyPath(t *testing.T) {
	h := newHandler(t)
	resp := renderOK(t, h, map[string]interface{}{
		"chart":  map[string]interface{}{"files": chartFiles(t, "demo-v1")},
		"values": map[string]interface{}{"message": "from-test"},
	})

	cm := findObject(resp.Objects, "ConfigMap", "render-demo")
	if cm == nil {
		t.Fatalf("ConfigMap render-demo missing from objects: %+v", resp.Objects)
	}
	if cm.Namespace != "default" {
		t.Errorf("ConfigMap namespace = %q, want default", cm.Namespace)
	}
	if !strings.Contains(cm.YAML, `message: "from-test"`) {
		t.Errorf("ConfigMap yaml does not carry the supplied value:\n%s", cm.YAML)
	}
	dep := findObject(resp.Objects, "Deployment", "render-demo")
	if dep == nil {
		t.Fatalf("Deployment render-demo missing from objects")
	}
	if dep.APIVersion != "apps/v1" {
		t.Errorf("Deployment apiVersion = %q, want apps/v1", dep.APIVersion)
	}

	// values.schema.json extraction
	if len(resp.ValuesSchema) == 0 || string(resp.ValuesSchema) == "null" {
		t.Fatalf("valuesSchema missing, want the chart schema")
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

func TestRenderValuesChangeOutput(t *testing.T) {
	h := newHandler(t)
	render := func(who string) string {
		resp := renderOK(t, h, map[string]interface{}{
			"rawTemplates": tinyChart(),
			"values":       map[string]interface{}{"who": who},
		})
		if len(resp.Objects) != 1 {
			t.Fatalf("objects = %+v, want one ConfigMap", resp.Objects)
		}
		return resp.Objects[0].YAML
	}
	alice, bob := render("alice"), render("bob")
	if alice == bob {
		t.Fatalf("values did not change the output:\n%s", alice)
	}
	if !strings.Contains(alice, "hello alice") || !strings.Contains(bob, "hello bob") {
		t.Errorf("outputs do not carry their values:\n--- alice:\n%s\n--- bob:\n%s", alice, bob)
	}
	// default from values.yaml when the key is not supplied
	resp := renderOK(t, h, map[string]interface{}{"rawTemplates": tinyChart()})
	if !strings.Contains(resp.Objects[0].YAML, "hello world") {
		t.Errorf("default values.yaml not applied:\n%s", resp.Objects[0].YAML)
	}
}

func TestRenderBearerAuthAcceptedAndIgnored(t *testing.T) {
	h := newHandler(t)
	raw, _ := json.Marshal(map[string]interface{}{"rawTemplates": tinyChart()})
	req := httptest.NewRequest(http.MethodPost, "/render", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer whatever-snowplow-sends")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /render with a bearer token = %d, want 200 (auth accepted and ignored)", rec.Code)
	}
}

// ---- /render: render errors are data (200 + {error}) -------------------------

func TestRenderTemplateFailureSurfacesInError(t *testing.T) {
	h := newHandler(t)
	broken := map[string]string{
		"Chart.yaml":         "apiVersion: v2\nname: broken\nversion: 0.1.0\n",
		"values.yaml":        "",
		"templates/bad.yaml": "{{ fail \"boom\" }}\n",
	}
	errText := renderErr(t, h, map[string]interface{}{"rawTemplates": broken})
	if !strings.Contains(errText, "boom") {
		t.Errorf("error %q does not carry the template failure", errText)
	}
}

func TestRenderSchemaViolatingValuesSurfaceInError(t *testing.T) {
	h := newHandler(t)
	errText := renderErr(t, h, map[string]interface{}{
		"chart":  map[string]interface{}{"files": chartFiles(t, "demo-v1")},
		"values": map[string]interface{}{"message": "ok", "replicaCount": "three"},
	})
	if !strings.Contains(errText, "replicaCount") {
		t.Errorf("error %q does not mention the offending field", errText)
	}
}

// ---- /render: malformed requests are 400 --------------------------------------

func TestRenderBadInputReturns400(t *testing.T) {
	h := newHandler(t)

	// no chart source at all
	rec := post(t, h, "/render", map[string]interface{}{"values": map[string]interface{}{}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /render without a chart = %d, want 400; body %s", rec.Code, rec.Body.String())
	}

	// two chart sources at once
	rec = post(t, h, "/render", map[string]interface{}{
		"chart":        map[string]interface{}{"url": "https://example.com/chart-1.0.0.tgz"},
		"rawTemplates": tinyChart(),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /render with url+rawTemplates = %d, want 400; body %s", rec.Code, rec.Body.String())
	}

	// malformed JSON
	req := httptest.NewRequest(http.MethodPost, "/render", strings.NewReader("{not json"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /render with malformed JSON = %d, want 400", rr.Code)
	}
}

func TestRenderRejectsNonHTTPSSchemes(t *testing.T) {
	h := newHandler(t) // production policy: no AllowHTTP
	for _, url := range []string{
		"file:///etc/passwd",
		"file://./chart",
		"/var/charts/local-chart.tgz",
		"./relative/chart.tgz",
		"ftp://example.com/chart.tgz",
		"http://example.com/chart-1.0.0.tgz", // plain http rejected by default
	} {
		rec := post(t, h, "/render", map[string]interface{}{
			"chart": map[string]interface{}{"url": url},
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST /render with url %q = %d, want 400; body %s", url, rec.Code, rec.Body.String())
		}
	}
}

// ---- /render: guardrails ------------------------------------------------------

func TestRenderOversizedBodyRejected(t *testing.T) {
	h := newHandlerWith(t, server.Config{MaxRequestBytes: 1024})
	big := map[string]interface{}{
		"rawTemplates": tinyChart(),
		"values":       map[string]interface{}{"pad": strings.Repeat("x", 4096)},
	}
	rec := post(t, h, "/render", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST /render with an oversized body = %d, want 413; body %s", rec.Code, rec.Body.String())
	}
	var resp errorResp
	decodeBody(t, rec, &resp)
	if !strings.Contains(resp.Error, "1024") {
		t.Errorf("error %q does not state the limit", resp.Error)
	}
}

func TestRenderChartDownloadCapEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64<<10))
	}))
	defer srv.Close()

	h := newHandlerWith(t, server.Config{AllowHTTP: true, MaxChartBytes: 1024})
	errText := renderErr(t, h, map[string]interface{}{
		"chart": map[string]interface{}{"url": srv.URL + "/big-chart-1.0.0.tgz"},
	})
	if !strings.Contains(errText, "1024 byte limit") {
		t.Errorf("error %q does not state the download cap", errText)
	}
}

func TestRenderTimeoutSurfacesInError(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // hold the download until the render deadline fires
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	h := newHandlerWith(t, server.Config{AllowHTTP: true, RenderTimeout: 100 * time.Millisecond})
	errText := renderErr(t, h, map[string]interface{}{
		"chart": map[string]interface{}{"url": srv.URL + "/slow-chart-1.0.0.tgz"},
	})
	if !strings.Contains(errText, "timed out") {
		t.Errorf("error %q does not report the timeout", errText)
	}
}

func TestRenderOutputCapEnforced(t *testing.T) {
	files := tinyChart()
	files["templates/big.yaml"] = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n" +
		"  name: big\ndata:\n  blob: " + strings.Repeat("a", 2048) + "\n"
	h := newHandlerWith(t, server.Config{MaxOutputBytes: 512})
	errText := renderErr(t, h, map[string]interface{}{"rawTemplates": files})
	if !strings.Contains(errText, "512 byte limit") {
		t.Errorf("error %q does not state the output cap", errText)
	}
}

// ---- /render: valuesSchema presence -------------------------------------------

func TestRenderSchemaOmittedWhenChartHasNone(t *testing.T) {
	h := newHandler(t)
	files := withoutFile(chartFiles(t, "demo-v1"), "values.schema.json")
	rec := post(t, h, "/render", map[string]interface{}{
		"chart": map[string]interface{}{"files": files},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /render = %d, body %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	decodeBody(t, rec, &raw)
	if _, present := raw["valuesSchema"]; present {
		t.Errorf("valuesSchema = %s, want the key omitted for a chart without values.schema.json", raw["valuesSchema"])
	}
	if _, present := raw["objects"]; !present {
		t.Errorf("objects missing from a successful render: %s", rec.Body.String())
	}
}

// ---- /render via a helm repository (offline, httptest-served) ------------------

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

	h := newHandlerWith(t, server.Config{AllowHTTP: true})

	// helm-repo mode: url = repo base URL, repo = chart name, version pinned
	resp := renderOK(t, h, map[string]interface{}{
		"chart":  map[string]interface{}{"url": repoSrv.URL, "repo": "demo", "version": "0.1.0"},
		"values": map[string]interface{}{"message": "via-repo"},
	})
	if cm := findObject(resp.Objects, "ConfigMap", "render-demo"); cm == nil || !strings.Contains(cm.YAML, "via-repo") {
		t.Fatalf("helm-repo render lost the ConfigMap or the value: %+v", resp.Objects)
	}

	// direct tgz mode
	resp = renderOK(t, h, map[string]interface{}{
		"chart":  map[string]interface{}{"url": repoSrv.URL + "/charts/demo-0.1.0.tgz"},
		"values": map[string]interface{}{"message": "via-tgz"},
	})
	if cm := findObject(resp.Objects, "ConfigMap", "render-demo"); cm == nil || !strings.Contains(cm.YAML, "via-tgz") {
		t.Fatalf("tgz render lost the ConfigMap or the value: %+v", resp.Objects)
	}
}

// ---- /render via OCI (offline, fake puller) ------------------------------------

// fakeOCIPuller implements render.OCIPuller from a fixed ref -> archive map.
type fakeOCIPuller struct {
	charts map[string][]byte
	pulled []string
}

func (f *fakeOCIPuller) Pull(_ context.Context, ref string) ([]byte, error) {
	f.pulled = append(f.pulled, ref)
	data, ok := f.charts[ref]
	if !ok {
		return nil, errors.New("manifest unknown: " + ref)
	}
	return data, nil
}

func TestRenderFromOCIViaFakePuller(t *testing.T) {
	tgz := packageChart(t, "demo", chartFiles(t, "demo-v1"))
	fake := &fakeOCIPuller{charts: map[string][]byte{
		"ghcr.io/acme/demo:0.1.0": tgz,
	}}
	h := newHandlerWith(t, server.Config{OCIPuller: fake})

	// version becomes the tag when the reference has none
	resp := renderOK(t, h, map[string]interface{}{
		"chart":  map[string]interface{}{"url": "oci://ghcr.io/acme/demo", "version": "0.1.0"},
		"values": map[string]interface{}{"message": "via-oci"},
	})
	if cm := findObject(resp.Objects, "ConfigMap", "render-demo"); cm == nil || !strings.Contains(cm.YAML, "via-oci") {
		t.Fatalf("oci render lost the ConfigMap or the value: %+v", resp.Objects)
	}
	if len(fake.pulled) != 1 || fake.pulled[0] != "ghcr.io/acme/demo:0.1.0" {
		t.Errorf("pulled refs = %v, want [ghcr.io/acme/demo:0.1.0]", fake.pulled)
	}

	// a failed pull is a render error, not a server error
	errText := renderErr(t, h, map[string]interface{}{
		"chart": map[string]interface{}{"url": "oci://ghcr.io/acme/missing", "version": "9.9.9"},
	})
	if !strings.Contains(errText, "oci://ghcr.io/acme/missing") {
		t.Errorf("error %q does not identify the failing chart", errText)
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

	if resp.Error != "" {
		t.Fatalf("POST /diff returned an error: %q", resp.Error)
	}
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

func TestDiffBrokenHeadSurfacesInErrorWithPrefix(t *testing.T) {
	h := newHandler(t)
	broken := []render.ChartFile{
		{Path: "Chart.yaml", Content: "apiVersion: v2\nname: broken\nversion: 0.1.0\n"},
		{Path: "values.yaml", Content: ""},
		{Path: "templates/bad.yaml", Content: "{{ fail \"boom\" }}\n"},
	}
	rec := post(t, h, "/diff", map[string]interface{}{
		"base": map[string]interface{}{"files": chartFiles(t, "demo-v1")},
		"head": map[string]interface{}{"files": broken},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /diff with a broken head = %d, want 200 with {error}; body %s", rec.Code, rec.Body.String())
	}
	var resp errorResp
	decodeBody(t, rec, &resp)
	if !strings.HasPrefix(resp.Error, "head: ") {
		t.Errorf("error %q does not identify the failing side", resp.Error)
	}
}
