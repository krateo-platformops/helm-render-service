package render

import (
	"context"
	"net"
	"strings"
	"testing"
)

// TestValidateFetchURL asserts the up-front IP-literal SSRF guard: private,
// loopback, link-local, unspecified and cloud-metadata hosts are refused, while
// ordinary public hosts pass.
func TestValidateFetchURL(t *testing.T) {
	blocked := []string{
		"https://169.254.169.254/latest/meta-data/x.tgz", // cloud metadata
		"https://127.0.0.1/chart.tgz",                    // loopback v4
		"https://[::1]/chart.tgz",                        // loopback v6
		"http://10.0.0.5/chart.tgz",                      // RFC1918
		"https://172.16.0.1/chart.tgz",                   // RFC1918
		"https://192.168.1.1/chart.tgz",                  // RFC1918
		"https://0.0.0.0/chart.tgz",                      // unspecified
		"https://[fe80::1]/chart.tgz",                    // link-local v6
	}
	for _, u := range blocked {
		if err := validateFetchURL(u); err == nil {
			t.Errorf("validateFetchURL(%q) = nil, want SSRF refusal", u)
		}
	}
	allowed := []string{
		"https://charts.example.com/chart.tgz",
		"https://8.8.8.8/chart.tgz", // public IP literal
	}
	for _, u := range allowed {
		if err := validateFetchURL(u); err != nil {
			t.Errorf("validateFetchURL(%q) = %v, want nil", u, err)
		}
	}
}

// TestGuardedDialContext_BlocksLoopback asserts the dial-time guard refuses a
// connection whose address resolves into a blocked range — the layer that stops
// a public hostname pointing at a private IP and DNS rebinding.
func TestGuardedDialContext_BlocksLoopback(t *testing.T) {
	dial := guardedDialContext(&net.Dialer{})
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:80"); err == nil || !strings.Contains(err.Error(), "SSRF guard") {
		t.Fatalf("dial 127.0.0.1 = %v, want SSRF-guard refusal", err)
	}
}

// TestLoad_RejectsPrivateURL asserts the guard fires end-to-end through Load
// (spec validation + fetch) in production mode (AllowHTTP=false), never issuing
// the request.
func TestLoad_RejectsPrivateURL(t *testing.T) {
	l := &Loader{} // AllowHTTP=false → production SSRF guard active
	_, err := l.Load(context.Background(), ChartSpec{URL: "https://127.0.0.1/chart.tgz"})
	if err == nil || !strings.Contains(err.Error(), "SSRF guard") {
		t.Fatalf("Load(private url) = %v, want SSRF-guard error", err)
	}
}
