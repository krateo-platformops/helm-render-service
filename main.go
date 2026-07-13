// helm-render-service is a stateless helm-template render/dry-run HTTP
// service for the Krateo self-service program: it renders charts against
// values without touching a cluster (helm SDK, client-only), so portal
// builders, preview verbs and upgrade-impact can call it over plain
// HTTP+JSON (snowplow reaches it via its external endpointRef mechanism).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"helm.sh/helm/v3/pkg/chartutil"

	"github.com/braghettos/helm-render-service/internal/server"
)

const defaultKubeVersion = "v1.33.0"

func main() {
	addr := envString("HRS_ADDR", ":8080")
	kubeVersionRaw := envString("HRS_KUBE_VERSION", defaultKubeVersion)
	renderTimeout := envDuration("HRS_RENDER_TIMEOUT", 30*time.Second)
	maxRequestBytes := envInt64("HRS_MAX_REQUEST_BYTES", 10<<20)
	maxChartBytes := envInt64("HRS_MAX_CHART_BYTES", 50<<20)

	kubeVersion, err := chartutil.ParseKubeVersion(kubeVersionRaw)
	if err != nil {
		log.Fatalf("invalid HRS_KUBE_VERSION %q: %v", kubeVersionRaw, err)
	}

	handler := server.New(server.Config{
		KubeVersion:     kubeVersion,
		RenderTimeout:   renderTimeout,
		MaxRequestBytes: maxRequestBytes,
		MaxChartBytes:   maxChartBytes,
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      renderTimeout + 30*time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		log.Printf("helm-render-service listening on %s (kubeVersion=%s renderTimeout=%s maxRequestBytes=%d maxChartBytes=%d)",
			addr, kubeVersionRaw, renderTimeout, maxRequestBytes, maxChartBytes)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("invalid %s %q: %v", key, v, err)
	}
	return d
}

func envInt64(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Fatalf("invalid %s %q: %v", key, v, err)
	}
	return n
}
