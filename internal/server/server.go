// Package server exposes the HTTP+JSON API of the helm render service:
//
//	POST /render  render a chart against values (client-only dry run)
//	POST /diff    render two chart versions and diff the resulting objects
//	GET  /healthz liveness probe
//
// The service is stateless and cluster-internal: no Kubernetes client, no
// cluster access, no exec of external binaries. Authorization headers
// (e.g. "Bearer ...", as sent by a snowplow Endpoint Secret) are accepted
// and ignored.
//
// Error model of POST /render: a chart that fails to fetch, load or render
// is DATA, not a server error — the response is 200 with {"error": "..."}.
// Only malformed requests get a 400 and oversized bodies a 413.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"helm.sh/helm/v3/pkg/chartutil"

	"github.com/braghettos/helm-render-service/internal/diff"
	"github.com/braghettos/helm-render-service/internal/render"
)

const (
	defaultRenderTimeout   = 15 * time.Second
	defaultMaxRequestBytes = 2 << 20  // 2 MiB
	defaultMaxChartBytes   = 20 << 20 // 20 MiB
	defaultMaxOutputBytes  = 5 << 20  // 5 MiB
)

// Config tunes the service guardrails.
type Config struct {
	// KubeVersion is the .Capabilities.KubeVersion presented to templates.
	// nil keeps the helm SDK default.
	KubeVersion *chartutil.KubeVersion
	// RenderTimeout bounds one /render (or one whole /diff), covering chart
	// download and template execution. Default 15s.
	RenderTimeout time.Duration
	// MaxRequestBytes caps the request body. Default 2 MiB.
	MaxRequestBytes int64
	// MaxChartBytes caps remote chart downloads. Default 20 MiB.
	MaxChartBytes int64
	// MaxOutputBytes caps the total rendered manifest output. Default 5 MiB.
	MaxOutputBytes int64
	// AllowHTTP permits plain http:// chart URLs (tests / trusted dev
	// environments only). file:// and local paths are always rejected.
	AllowHTTP bool
	// OCIPuller overrides the OCI chart pull path (tests). Defaults to the
	// helm SDK registry client with anonymous pulls.
	OCIPuller render.OCIPuller
}

func (c Config) withDefaults() Config {
	if c.RenderTimeout <= 0 {
		c.RenderTimeout = defaultRenderTimeout
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = defaultMaxRequestBytes
	}
	if c.MaxChartBytes <= 0 {
		c.MaxChartBytes = defaultMaxChartBytes
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = defaultMaxOutputBytes
	}
	return c
}

type server struct {
	cfg    Config
	loader *render.Loader
}

// New builds the HTTP handler for the service.
func New(cfg Config) http.Handler {
	s := &server{cfg: cfg.withDefaults()}
	s.loader = &render.Loader{
		MaxChartBytes: s.cfg.MaxChartBytes,
		AllowHTTP:     s.cfg.AllowHTTP,
		OCI:           s.cfg.OCIPuller,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /render", s.handleRender)
	mux.HandleFunc("POST /diff", s.handleDiff)
	return mux
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

type renderRequest struct {
	Chart render.ChartSpec `json:"chart"`
	// RawTemplates is the inline input mode: a map of chart-relative file
	// path ("Chart.yaml", "templates/x.yaml", ...) to file content.
	RawTemplates map[string]string `json:"rawTemplates,omitempty"`
	// Files is accepted at the top level as an alias for chart.files.
	Files       []render.ChartFile     `json:"files,omitempty"`
	Values      map[string]interface{} `json:"values"`
	ReleaseName string                 `json:"releaseName,omitempty"`
	Namespace   string                 `json:"namespace,omitempty"`
}

// renderSuccess is the 200 body of a successful POST /render.
type renderSuccess struct {
	Objects      []render.Manifest `json:"objects"`
	ValuesSchema json.RawMessage   `json:"valuesSchema,omitempty"`
	Notes        *string           `json:"notes,omitempty"`
}

// chartSpec merges the three accepted chart-source spellings (chart{...},
// top-level files, rawTemplates) into one ChartSpec, rejecting combinations.
func (r renderRequest) chartSpec() (render.ChartSpec, error) {
	spec := r.Chart
	sources := 0
	if spec.URL != "" || len(spec.Files) > 0 {
		sources++
	}
	if len(r.Files) > 0 {
		sources++
	}
	if len(r.RawTemplates) > 0 {
		sources++
	}
	if sources > 1 {
		return render.ChartSpec{}, errors.New("provide the chart as exactly one of chart{...}, rawTemplates or top-level files")
	}
	switch {
	case len(r.RawTemplates) > 0:
		paths := make([]string, 0, len(r.RawTemplates))
		for p := range r.RawTemplates {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		files := make([]render.ChartFile, 0, len(paths))
		for _, p := range paths {
			files = append(files, render.ChartFile{Path: p, Content: r.RawTemplates[p]})
		}
		spec = render.ChartSpec{Files: files}
	case len(r.Files) > 0:
		spec = render.ChartSpec{Files: r.Files}
	}
	return spec, nil
}

func (s *server) handleRender(w http.ResponseWriter, r *http.Request) {
	var req renderRequest
	if !s.decode(w, r, &req) {
		return
	}

	spec, err := req.chartSpec()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := spec.Validate(s.cfg.AllowHTTP); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ReleaseName != "" {
		if err := chartutil.ValidateReleaseName(req.ReleaseName); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid releaseName: %v", err))
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RenderTimeout)
	defer cancel()

	result, err := s.renderOne(ctx, spec, req.Values, req.ReleaseName, req.Namespace)
	if err != nil {
		// A chart that fails to fetch, load or render is data, not a server
		// error: 200 with the error string so api-step callers (snowplow)
		// can surface it as widget data.
		writeJSON(w, http.StatusOK, errorResponse{Error: s.renderErrorText(err, "")})
		return
	}
	writeJSON(w, http.StatusOK, renderSuccess{
		Objects:      result.Manifests,
		ValuesSchema: result.ValuesSchema,
		Notes:        result.Notes,
	})
}

type diffRequest struct {
	Base        render.ChartSpec       `json:"base"`
	Head        render.ChartSpec       `json:"head"`
	Values      map[string]interface{} `json:"values"`
	ReleaseName string                 `json:"releaseName,omitempty"`
	Namespace   string                 `json:"namespace,omitempty"`
}

func (s *server) handleDiff(w http.ResponseWriter, r *http.Request) {
	var req diffRequest
	if !s.decode(w, r, &req) {
		return
	}
	if err := req.Base.Validate(s.cfg.AllowHTTP); err != nil {
		writeError(w, http.StatusBadRequest, "base: "+err.Error())
		return
	}
	if err := req.Head.Validate(s.cfg.AllowHTTP); err != nil {
		writeError(w, http.StatusBadRequest, "head: "+err.Error())
		return
	}
	if req.ReleaseName != "" {
		if err := chartutil.ValidateReleaseName(req.ReleaseName); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid releaseName: %v", err))
			return
		}
	}

	// One timeout covers both renders so a /diff costs at most one budget.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RenderTimeout)
	defer cancel()

	baseRes, err := s.renderOne(ctx, req.Base, req.Values, req.ReleaseName, req.Namespace)
	if err != nil {
		writeJSON(w, http.StatusOK, errorResponse{Error: s.renderErrorText(err, "base: ")})
		return
	}
	headRes, err := s.renderOne(ctx, req.Head, req.Values, req.ReleaseName, req.Namespace)
	if err != nil {
		writeJSON(w, http.StatusOK, errorResponse{Error: s.renderErrorText(err, "head: ")})
		return
	}
	writeJSON(w, http.StatusOK, diff.Compute(baseRes, headRes))
}

func (s *server) renderOne(ctx context.Context, spec render.ChartSpec, values map[string]interface{}, releaseName, namespace string) (*render.Result, error) {
	ch, err := s.loader.Load(ctx, spec)
	if err != nil {
		return nil, err
	}
	return render.Render(ctx, ch, values, render.Options{
		ReleaseName:    releaseName,
		Namespace:      namespace,
		KubeVersion:    s.cfg.KubeVersion,
		MaxOutputBytes: s.cfg.MaxOutputBytes,
	})
}

// decode reads the JSON body with the request-size guardrail applied.
func (s *server) decode(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds the %d byte limit", s.cfg.MaxRequestBytes))
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// renderErrorText normalizes a chart-load/render failure into the {error}
// string: timeouts get a stable message, everything else passes the helm
// error text through.
func (s *server) renderErrorText(err error, prefix string) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("%srender timed out after %s", prefix, s.cfg.RenderTimeout)
	}
	return prefix + err.Error()
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
