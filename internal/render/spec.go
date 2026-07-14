package render

import (
	"fmt"
	"path"
	"strings"
)

// ChartFile is a single file of an inline chart tree. Path is relative to the
// chart root (e.g. "Chart.yaml", "templates/deployment.yaml"). A single shared
// top-level directory (tgz-style tree, e.g. "mychart/Chart.yaml") is stripped
// automatically when no root-level Chart.yaml is present.
type ChartFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ChartSpec identifies a chart to render. Exactly one source must be set:
//
//   - Files: an inline chart tree (builder drafts / rawTemplates).
//   - URL "oci://host/path[:tag]": an OCI chart reference. Version is used as
//     the tag when the reference does not carry one.
//   - URL "https://.../chart-1.2.3.tgz": a direct chart archive URL.
//   - URL "https://charts.example.com" + Repo (+ Version): a classic helm
//     repository. URL is the repository base URL and Repo is the chart name
//     inside it; an empty Version selects the latest entry in the index.
//
// file:// URLs and local filesystem paths are always rejected. Plain http://
// is rejected unless the service is explicitly configured to allow it
// (tests / trusted dev environments only).
type ChartSpec struct {
	URL     string      `json:"url,omitempty"`
	Repo    string      `json:"repo,omitempty"`
	Version string      `json:"version,omitempty"`
	Files   []ChartFile `json:"files,omitempty"`
}

// Validate checks that the spec designates exactly one chart source and that
// the source is well formed. allowHTTP permits plain http:// chart URLs
// (never file:// or local paths). Errors from Validate are client errors
// (HTTP 400).
func (s ChartSpec) Validate(allowHTTP bool) error {
	hasFiles := len(s.Files) > 0
	hasURL := s.URL != ""

	switch {
	case hasFiles && hasURL:
		return fmt.Errorf("chart: set either url or files, not both")
	case !hasFiles && !hasURL:
		return fmt.Errorf("chart: one of url, files or rawTemplates is required")
	case hasFiles && s.Repo != "":
		return fmt.Errorf("chart: repo cannot be combined with files")
	}

	if hasFiles {
		for _, f := range s.Files {
			clean := path.Clean(strings.TrimPrefix(f.Path, "/"))
			if f.Path == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
				return fmt.Errorf("chart: invalid file path %q", f.Path)
			}
		}
		return nil
	}

	switch {
	case strings.HasPrefix(s.URL, "oci://"):
		// ok; version optional (may be part of the reference as a tag)
	case strings.HasPrefix(s.URL, "https://"):
		return s.validateHTTPTarget()
	case strings.HasPrefix(s.URL, "http://"):
		if !allowHTTP {
			return fmt.Errorf("chart: plain http:// chart urls are not allowed (use oci:// or https://): %q", s.URL)
		}
		return s.validateHTTPTarget()
	default:
		return fmt.Errorf("chart: unsupported url scheme in %q (want oci:// or https://; file:// and local paths are rejected)", s.URL)
	}
	return nil
}

// validateHTTPTarget checks that an http(s) URL is either a direct .tgz
// archive or a helm repository base URL accompanied by a chart name.
func (s ChartSpec) validateHTTPTarget() error {
	lower := strings.ToLower(s.URL)
	isArchive := strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.gz")
	if s.Repo == "" && !isArchive {
		return fmt.Errorf("chart: http(s) url must point to a .tgz archive, or set repo (the chart name) to treat url as a helm repository base URL")
	}
	return nil
}
