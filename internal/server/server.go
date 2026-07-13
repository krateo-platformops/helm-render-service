// Package server exposes the HTTP+JSON API of the helm render service:
//
//	POST /render  render a chart against values (client-only dry run)
//	POST /diff    render two chart versions and diff the resulting objects
//	GET  /healthz liveness probe
//
// The service is stateless and cluster-internal: no auth in v0, no
// Kubernetes client, no cluster access.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"helm.sh/helm/v3/pkg/chartutil"

	"github.com/braghettos/helm-render-service/internal/diff"
	"github.com/braghettos/helm-render-service/internal/render"
)

const (
	defaultRenderTimeout   = 30 * time.Second
	defaultMaxRequestBytes = 10 << 20 // 10 MiB
	defaultMaxChartBytes   = 50 << 20 // 50 MiB
)

// Config tunes the service guardrails.
type Config struct {
	// KubeVersion is the .Capabilities.KubeVersion presented to templates.
	// nil keeps the helm SDK default.
	KubeVersion *chartutil.KubeVersion
	// RenderTimeout bounds one /render (or one whole /diff). Default 30s.
	RenderTimeout time.Duration
	// MaxRequestBytes caps the request body. Default 10 MiB.
	MaxRequestBytes int64
	// MaxChartBytes caps remote chart downloads. Default 50 MiB.
	MaxChartBytes int64
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
	return c
}

type server struct {
	cfg    Config
	loader *render.Loader
}

// New builds the HTTP handler for the service.
func New(cfg Config) http.Handler {
	s := &server{cfg: cfg.withDefaults()}
	s.loader = &render.Loader{MaxChartBytes: s.cfg.MaxChartBytes}

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
	// Files is accepted at the top level as an alias for chart.files.
	Files       []render.ChartFile     `json:"files,omitempty"`
	Values      map[string]interface{} `json:"values"`
	ReleaseName string                 `json:"releaseName,omitempty"`
	Namespace   string                 `json:"namespace,omitempty"`
}

func (s *server) handleRender(w http.ResponseWriter, r *http.Request) {
	var req renderRequest
	if !s.decode(w, r, &req) {
		return
	}

	spec := req.Chart
	if len(req.Files) > 0 {
		if len(spec.Files) > 0 || spec.URL != "" {
			writeError(w, http.StatusBadRequest, "provide the chart either as top-level files or as chart{...}, not both")
			return
		}
		spec.Files = req.Files
	}
	if err := spec.Validate(); err != nil {
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
		s.writeRenderError(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, result)
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
	if err := req.Base.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "base: "+err.Error())
		return
	}
	if err := req.Head.Validate(); err != nil {
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
		s.writeRenderError(w, err, "base: ")
		return
	}
	headRes, err := s.renderOne(ctx, req.Head, req.Values, req.ReleaseName, req.Namespace)
	if err != nil {
		s.writeRenderError(w, err, "head: ")
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
		ReleaseName: releaseName,
		Namespace:   namespace,
		KubeVersion: s.cfg.KubeVersion,
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

// writeRenderError maps chart-load and render failures: timeouts become 504,
// everything else is a 422 with the helm error text passed through.
func (s *server) writeRenderError(w http.ResponseWriter, err error, prefix string) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout,
			fmt.Sprintf("%srender timed out after %s", prefix, s.cfg.RenderTimeout))
		return
	}
	writeError(w, http.StatusUnprocessableEntity, prefix+err.Error())
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
