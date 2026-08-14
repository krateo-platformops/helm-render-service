package render

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"path"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	"sigs.k8s.io/yaml"
)

const defaultMaxChartBytes = 20 << 20 // 20 MiB

// OCIPuller fetches a packaged chart archive from an OCI registry. ref is a
// helm OCI reference without the oci:// prefix ("host/path:tag"). It exists
// as an interface so the network path can be faked in tests.
type OCIPuller interface {
	Pull(ctx context.Context, ref string) ([]byte, error)
}

// registryPuller is the production OCIPuller (anonymous pulls, helm SDK
// registry client). The helm client does not take a context, so the pull runs
// in a goroutine and is abandoned when ctx expires.
type registryPuller struct{}

func (registryPuller) Pull(ctx context.Context, ref string) ([]byte, error) {
	type outcome struct {
		data []byte
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		client, err := registry.NewClient()
		if err != nil {
			done <- outcome{nil, fmt.Errorf("create registry client: %w", err)}
			return
		}
		res, err := client.Pull(ref, registry.PullOptWithChart(true))
		if err != nil {
			done <- outcome{nil, err}
			return
		}
		done <- outcome{res.Chart.Data, nil}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case o := <-done:
		return o.data, o.err
	}
}

// Loader resolves a ChartSpec into a loaded *chart.Chart without ever
// touching a Kubernetes cluster. Remote fetches are bounded by MaxChartBytes
// and by the deadline of the context passed to Load.
type Loader struct {
	// HTTPClient used for tgz and repo-index downloads. Defaults to a plain
	// http.Client (timeouts come from the request context).
	HTTPClient *http.Client
	// OCI overrides the OCI pull path (tests). Defaults to the helm SDK
	// registry client with anonymous pulls.
	OCI OCIPuller
	// MaxChartBytes caps downloaded chart archives and repo indexes.
	// Defaults to 20 MiB.
	MaxChartBytes int64
	// AllowHTTP permits plain http:// chart URLs (tests / trusted dev
	// environments only). file:// and local paths are always rejected. It also
	// opts OUT of the SSRF guard (private/loopback/link-local targets become
	// reachable) — same "trusted" boundary — so it must stay false in production.
	AllowHTTP bool
}

// Load resolves and loads the chart designated by spec.
func (l *Loader) Load(ctx context.Context, spec ChartSpec) (*chart.Chart, error) {
	if err := spec.Validate(l.AllowHTTP); err != nil {
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
func (l *Loader) loadOCI(ctx context.Context, spec ChartSpec) (*chart.Chart, error) {
	ref := strings.TrimPrefix(spec.URL, "oci://")
	if spec.Version != "" && !lastSegmentHasTag(ref) {
		ref += ":" + spec.Version
	}

	data, err := l.oci().Pull(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", spec.URL, err)
	}
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
	// SSRF guard (CWE-918 / CodeQL go/request-forgery): the chart URL is
	// caller-supplied. Reject an IP-literal host in a private/loopback/link-local
	// range up front; hostnames that RESOLVE to such an address are caught at
	// dial time by ssrfGuardedClient (which also defeats DNS rebinding). The
	// guard is production-only: AllowHTTP (tests / trusted dev) targets loopback
	// httptest servers, so it opts out — same trust boundary as permitting http://.
	if !l.AllowHTTP {
		if err := validateFetchURL(url); err != nil {
			return nil, err
		}
	}
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
	if l.AllowHTTP {
		// Trusted dev/test mode: plain client, no SSRF dial guard (loopback
		// httptest targets are expected here).
		return http.DefaultClient
	}
	return ssrfGuardedClient
}

// --- SSRF guard -----------------------------------------------------------
//
// helm-render-service fetches charts from a CALLER-SUPPLIED URL, which makes the
// download a Server-Side Request Forgery sink: a caller could aim it at the
// cluster's internal services or the cloud metadata endpoint (169.254.169.254).
// Two layers defend it:
//   1. validateFetchURL rejects an IP-literal host in a blocked range up front.
//   2. ssrfGuardedClient blocks the CONNECTION at dial time to any address that
//      resolves into a blocked range, and dials the exact IP it checked — so a
//      hostname pointing at a private IP, and DNS rebinding (which a URL-string
//      allowlist cannot stop), are both refused.

// isBlockedIP reports whether ip is one an SSRF would target: loopback (127/8,
// ::1), RFC1918 / ULA private (10/8, 172.16/12, 192.168/16, fc00::/7),
// link-local (169.254/16 incl. the metadata IP, fe80::/10), unspecified, or
// multicast.
func isBlockedIP(ip net.IP) bool {
	return ip == nil ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// validateFetchURL rejects a URL whose host is an IP literal in a blocked range.
func validateFetchURL(rawURL string) error {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url %q: %w", rawURL, err)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url %q has no host", rawURL)
	}
	if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
		return fmt.Errorf("refusing to fetch %q: host %s is a private/loopback/link-local address (SSRF guard)", rawURL, host)
	}
	return nil
}

// ssrfGuardedClient is the production HTTP client for chart/index downloads:
// standard behaviour plus the dial-time private-address block. Request-level
// timeouts come from the caller's context, as before.
var ssrfGuardedClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           guardedDialContext(&net.Dialer{KeepAlive: 30 * time.Second}),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// guardedDialContext resolves the host, refuses the dial if ANY resolved IP is
// blocked, then dials the exact IP it checked (no re-resolution → no rebinding).
func guardedDialContext(d *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no addresses for %q", host)
		}
		for _, ipa := range ips {
			if isBlockedIP(ipa.IP) {
				return nil, fmt.Errorf("refusing to connect to %s: %s is a private/loopback/link-local address (SSRF guard)", host, ipa.IP)
			}
		}
		var lastErr error
		for _, ipa := range ips {
			conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		return nil, lastErr
	}
}

func (l *Loader) oci() OCIPuller {
	if l.OCI != nil {
		return l.OCI
	}
	return registryPuller{}
}

func (l *Loader) maxBytes() int64 {
	if l.MaxChartBytes > 0 {
		return l.MaxChartBytes
	}
	return defaultMaxChartBytes
}
