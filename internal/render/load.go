package render

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	"sigs.k8s.io/yaml"
)

const defaultMaxChartBytes = 50 << 20 // 50 MiB

// Loader resolves a ChartSpec into a loaded *chart.Chart without ever
// touching a Kubernetes cluster. Remote fetches are bounded by MaxChartBytes
// and by the deadline of the context passed to Load.
type Loader struct {
	// HTTPClient used for tgz and repo-index downloads. Defaults to a plain
	// http.Client (timeouts come from the request context).
	HTTPClient *http.Client
	// MaxChartBytes caps downloaded chart archives and repo indexes.
	// Defaults to 50 MiB.
	MaxChartBytes int64
}

// Load resolves and loads the chart designated by spec.
func (l *Loader) Load(ctx context.Context, spec ChartSpec) (*chart.Chart, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	switch {
	case len(spec.Files) > 0:
		return loadInline(spec.Files)
	case strings.HasPrefix(spec.URL, "oci://"):
		return l.loadOCI(ctx, spec)
	case spec.Repo != "":
		return l.loadFromRepo(ctx, spec)
	default:
		return l.loadTgzURL(ctx, spec.URL)
	}
}

// loadInline builds a chart from an inline file tree.
func loadInline(files []ChartFile) (*chart.Chart, error) {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = path.Clean(strings.TrimPrefix(f.Path, "/"))
	}

	// If Chart.yaml is not at the root but every file shares a single
	// top-level directory (a tgz-extracted tree), strip that directory.
	rootChart := false
	for _, n := range names {
		if n == "Chart.yaml" {
			rootChart = true
			break
		}
	}
	if !rootChart {
		top, uniform := "", true
		for _, n := range names {
			i := strings.Index(n, "/")
			if i < 0 {
				uniform = false
				break
			}
			switch {
			case top == "":
				top = n[:i+1]
			case n[:i+1] != top:
				uniform = false
			}
			if !uniform {
				break
			}
		}
		if uniform && top != "" {
			for i := range names {
				names[i] = strings.TrimPrefix(names[i], top)
			}
		}
	}

	buffered := make([]*loader.BufferedFile, len(files))
	for i, f := range files {
		buffered[i] = &loader.BufferedFile{Name: names[i], Data: []byte(f.Content)}
	}
	ch, err := loader.LoadFiles(buffered)
	if err != nil {
		return nil, fmt.Errorf("load inline chart: %w", err)
	}
	return ch, nil
}

// loadOCI pulls a chart from an OCI registry (anonymous in v0).
func (l *Loader) loadOCI(_ context.Context, spec ChartSpec) (*chart.Chart, error) {
	ref := strings.TrimPrefix(spec.URL, "oci://")
	if spec.Version != "" && !lastSegmentHasTag(ref) {
		ref += ":" + spec.Version
	}

	client, err := registry.NewClient()
	if err != nil {
		return nil, fmt.Errorf("create registry client: %w", err)
	}
	res, err := client.Pull(ref, registry.PullOptWithChart(true))
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", spec.URL, err)
	}
	data := res.Chart.Data
	if int64(len(data)) > l.maxBytes() {
		return nil, fmt.Errorf("chart %s exceeds the %d byte limit", spec.URL, l.maxBytes())
	}
	ch, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("load chart archive from %s: %w", spec.URL, err)
	}
	return ch, nil
}

// lastSegmentHasTag reports whether the final path segment of an OCI
// reference already carries a ":tag" (host ports live in earlier segments).
func lastSegmentHasTag(ref string) bool {
	last := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		last = ref[i+1:]
	}
	return strings.Contains(last, ":")
}

// loadFromRepo resolves a chart from a classic helm repository index.
func (l *Loader) loadFromRepo(ctx context.Context, spec ChartSpec) (*chart.Chart, error) {
	base := strings.TrimSuffix(spec.URL, "/")
	data, err := l.fetch(ctx, base+"/index.yaml")
	if err != nil {
		return nil, fmt.Errorf("fetch repo index: %w", err)
	}
	var idx repo.IndexFile
	if err := yaml.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse repo index from %s: %w", base, err)
	}
	idx.SortEntries()
	cv, err := idx.Get(spec.Repo, spec.Version)
	if err != nil {
		return nil, fmt.Errorf("chart %q version %q not found in repository %s: %w", spec.Repo, spec.Version, base, err)
	}
	if len(cv.URLs) == 0 {
		return nil, fmt.Errorf("chart %q version %q has no download URL in repository %s", spec.Repo, spec.Version, base)
	}
	chartURL, err := repo.ResolveReferenceURL(base, cv.URLs[0])
	if err != nil {
		return nil, fmt.Errorf("resolve chart URL: %w", err)
	}
	return l.loadTgzURL(ctx, chartURL)
}

// loadTgzURL downloads and loads a packaged chart archive.
func (l *Loader) loadTgzURL(ctx context.Context, url string) (*chart.Chart, error) {
	data, err := l.fetch(ctx, url)
	if err != nil {
		return nil, err
	}
	ch, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("load chart archive from %s: %w", url, err)
	}
	return ch, nil
}

func (l *Loader) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", url, err)
	}
	resp, err := l.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, l.maxBytes()+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if int64(len(data)) > l.maxBytes() {
		return nil, fmt.Errorf("download from %s exceeds the %d byte limit", url, l.maxBytes())
	}
	return data, nil
}

func (l *Loader) httpClient() *http.Client {
	if l.HTTPClient != nil {
		return l.HTTPClient
	}
	return http.DefaultClient
}

func (l *Loader) maxBytes() int64 {
	if l.MaxChartBytes > 0 {
		return l.MaxChartBytes
	}
	return defaultMaxChartBytes
}
