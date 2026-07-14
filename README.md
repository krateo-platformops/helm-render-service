# helm-render-service

Stateless `helm template` render/dry-run HTTP service for the Krateo
self-service program. It renders a chart against values **without touching a
cluster**: portal builders, preview verbs and upgrade-impact call it over
plain HTTP+JSON (snowplow reaches it through an Endpoint Secret + POST
api-step).

- **No Kubernetes client, no kubeconfig, no cluster access.** Rendering uses
  the helm Go SDK (`action.Install` with `DryRun` + `ClientOnly` + `Replace`),
  exactly like `helm template`. No external binaries are ever executed.
- **Auth: accepted and ignored.** `Authorization: Bearer ...` headers (as
  sent by a snowplow Endpoint Secret) pass through untouched; the service is
  meant to run cluster-internal. Real service-to-service auth is on the
  roadmap.
- **Stateless** — nothing is persisted between requests.

## API

### `POST /render`

Render a chart against values. Two input modes:

```jsonc
// Mode 1: remote chart
{
  "chart": {
    "url": "oci://ghcr.io/org/mychart",   // oci:// | https://...tgz | https:// repo base URL
    "version": "1.2.3",                   // optional (OCI tag / repo entry); empty = latest for repos
    "repo": ""                            // chart name, only for helm-repo base URLs
  },
  "values": {},                           // optional values overlay
  "releaseName": "preview",               // optional, default "render"
  "namespace": "default"                  // optional, default "default"
}

// Mode 2: inline chart tree (builder drafts)
{
  "rawTemplates": {
    "Chart.yaml": "apiVersion: v2\nname: draft\nversion: 0.1.0\n",
    "values.yaml": "who: world\n",
    "templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\n..."
  },
  "values": {"who": "krateo"}
}
```

Chart URL sources (exactly one input mode per request):

| Source | Fields |
| --- | --- |
| OCI registry | `chart.url: "oci://host/path[:tag]"` (+ optional `version` used as the tag) |
| Direct archive | `chart.url: "https://host/path/chart-1.2.3.tgz"` |
| Classic helm repository | `chart.url: "https://charts.example.com"` (repo base URL) + `chart.repo: "<chart name>"` + optional `version` (empty = latest) |
| Inline chart tree | `rawTemplates: {"<path>": "<content>", ...}` — paths relative to the chart root. `chart.files` / top-level `files` (`[{"path", "content"}]` arrays) are accepted as equivalent spellings |

`file://` URLs, local paths and plain `http://` are rejected (`http://` can be
re-enabled with `HRS_ALLOW_HTTP=true` for trusted dev environments only).

Responses — **a chart that fails to fetch, load or render is data, not a
server error**; only a malformed request is a 4xx:

- `200` success:

  ```jsonc
  {
    "objects": [
      {"apiVersion": "apps/v1", "kind": "Deployment", "name": "x",
       "namespace": "default",           // as declared by the manifest; "" when the template omits it
       "yaml": "# Source: ...\napiVersion: apps/v1\n..."}
    ],
    "valuesSchema": { /* the chart's values.schema.json */ },  // omitted when the chart ships none
    "notes": "rendered NOTES.txt"                              // omitted when absent
  }
  ```

  Objects keep helm's rendering order; hook manifests are included (rendered,
  never executed).

- `200` render failure — `{"error": "<helm error text>"}` (bad values against
  the chart schema, template `fail`, unreachable/oversized chart, timeout, ...).
- `400` — `{"error": "..."}` malformed JSON / invalid chart spec / rejected
  URL scheme / more than one input mode / invalid release name.
- `413` — `{"error": "..."}` request body over the size cap.

Examples:

```sh
# Inline chart tree (builder draft)
curl -s localhost:8080/render -X POST -H 'Content-Type: application/json' -d '{
  "rawTemplates": {
    "Chart.yaml": "apiVersion: v2\nname: draft\nversion: 0.1.0\n",
    "values.yaml": "who: world\n",
    "templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-hello\ndata:\n  greeting: hello {{ .Values.who }}\n"
  },
  "values": {"who": "krateo"}
}'

# OCI chart
curl -s localhost:8080/render -X POST -H 'Content-Type: application/json' -d '{
  "chart": {"url": "oci://ghcr.io/braghettos/krateo-frontend-chart", "version": "1.3.5"},
  "values": {}
}'

# Classic helm repository
curl -s localhost:8080/render -X POST -H 'Content-Type: application/json' -d '{
  "chart": {"url": "https://charts.krateo.io", "repo": "snowplow", "version": "1.4.1"},
  "values": {},
  "releaseName": "preview",
  "namespace": "krateo-system"
}'
```

### `POST /diff`

Render two chart versions with the same values and diff the resulting
objects. This feeds "upgrade-impact explain".

```sh
curl -s localhost:8080/diff -X POST -H 'Content-Type: application/json' -d '{
  "base": {"url": "https://charts.krateo.io", "repo": "snowplow", "version": "1.4.1"},
  "head": {"url": "https://charts.krateo.io", "repo": "snowplow", "version": "1.5.1"},
  "values": {}
}'
```

Response `200`:

```json
{
  "added":   [{"apiVersion": "v1", "kind": "Service", "name": "x", "namespace": "default"}],
  "removed": [],
  "changed": [{"ref": {"apiVersion": "apps/v1", "kind": "Deployment", "name": "x", "namespace": ""},
               "summary": "changed top-level fields: spec"}],
  "valuesSchemaChanged": true
}
```

Objects are keyed by `(apiVersion, kind, namespace, name)`; a change summary
names the top-level fields (`spec`, `data`, `metadata`, ...) whose parsed
content differs. YAML comments (e.g. helm's `# Source:` lines) never count as
changes. `releaseName`/`namespace` are accepted and applied to both sides.
Render failures follow the same error model as `/render`: `200` with
`{"error": "base: ..."}` / `{"error": "head: ..."}` naming the failing side.

### `GET /healthz`

Returns `200 ok`.

## Guardrails & semantics

- **Request size cap** — default 2 MiB (`413` beyond it).
- **Chart download cap** — default 20 MiB (exceeding it is a render error:
  `200` + `{error}`).
- **Render timeout** — default 15s per request wall-clock, covering chart
  download and template execution (context timeout; a `/diff` shares one
  budget across both renders). Expiry is a render error: `200` +
  `{"error": "render timed out after 15s"}`.
- **Rendered output cap** — default 5 MiB of manifest output (render error).
- **URL scheme allowlist** — `oci://` and `https://` only; `file://` and
  local paths are always rejected (`400`).
- **No cluster access ever** — no kubeconfig, no `helm install`, no exec of
  external binaries. The template `lookup` function returns empty results,
  like `helm template`.
- **Hooks are rendered, never executed** — hook manifests are included in
  `objects` so callers see every object the chart would create; nothing is
  applied anywhere.
- **Dependencies must be vendored** — subcharts have to be bundled in the
  archive/file tree (as `helm package` does); missing dependencies fail the
  render with a `200` + `{error}`.

## Configuration

| Env var | Default | Meaning |
| --- | --- | --- |
| `HRS_ADDR` | `:8080` | Listen address |
| `HRS_KUBE_VERSION` | `v1.33.0` | `.Capabilities.KubeVersion` presented to templates |
| `HRS_RENDER_TIMEOUT` | `15s` | Per-request render timeout |
| `HRS_MAX_REQUEST_BYTES` | `2097152` (2 MiB) | Request body cap |
| `HRS_MAX_CHART_BYTES` | `20971520` (20 MiB) | Chart download cap |
| `HRS_MAX_OUTPUT_BYTES` | `5242880` (5 MiB) | Rendered output cap |
| `HRS_ALLOW_HTTP` | `false` | Allow plain `http://` chart URLs (trusted dev only) |

## Deploy

The repo ships its own release pipelines — **one bare-semver tag push
publishes both** the container image
(`.github/workflows/release-image.yaml` → `ghcr.io/braghettos/helm-render-service:<tag>`,
linux/amd64) and the helm chart
(`.github/workflows/release-oci.yaml` → `oci://ghcr.io/braghettos/krateo/helm-render-service:<tag>`,
`CHART_VERSION`/`APP_VERSION` placeholders in `chart/Chart.yaml` are resolved
to the tag at package time).

```sh
# 1. Release: tag + push (image AND chart publish from this one tag)
git tag 0.1.0 && git push origin 0.1.0

# 2. Install the chart (creates Deployment + ClusterIP Service :8080;
#    --set snowplowEndpoint.enabled=true also creates the snowplow Endpoint Secret)
helm install helm-render-service oci://ghcr.io/braghettos/krateo/helm-render-service \
  --version 0.1.0 --namespace krateo-system --set snowplowEndpoint.enabled=true

# 3. Verify from inside the cluster
kubectl run curl --rm -it --image=curlimages/curl --restart=Never -n krateo-system -- \
  curl -s http://helm-render-service.krateo-system.svc:8080/healthz
```

### snowplow wiring

`snowplowEndpoint.enabled=true` renders a Secret in the release namespace that
snowplow's `endpointRef` resolves as an external Endpoint (`server-url` is the
only required key; the service ignores the Bearer token snowplow forwards):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: helm-render-endpoint          # values.snowplowEndpoint.name
  namespace: krateo-system
  annotations:
    "krateo.io/verbose": "true"
type: Opaque
stringData:
  server-url: http://helm-render-service.krateo-system.svc:8080
```

A RESTAction api-step can then POST to `/render` (or `/diff`) — **illustrative
example** (adapt chart URL, filter and namespaces; `payload` is a JSON string):

```yaml
apiVersion: templates.krateo.io/v1
kind: RESTAction
metadata:
  name: chart-preview
  namespace: krateo-system
spec:
  api:
    - name: render
      endpointRef:
        name: helm-render-endpoint
        namespace: krateo-system
      path: /render
      verb: POST
      headers:
        - 'Content-Type: application/json'
      payload: |
        {
          "chart": {"url": "oci://ghcr.io/braghettos/krateo-frontend-chart", "version": "1.3.5"},
          "values": {},
          "releaseName": "preview",
          "namespace": "krateo-system"
        }
  # /render answers 200 with either {objects: [...]} or {error: "..."} —
  # surface both so a failed render shows up as widget data, not a step error.
  filter: "{objects: [.render.objects[]? | {kind, name, namespace}], error: (.render.error // empty)}"
```

## Development

```sh
make build    # bin/helm-render-service
make test     # go test ./...
make vet      # go vet ./...
make docker   # ghcr.io/braghettos/helm-render-service:dev
make run      # go run . (listens on :8080)
```

Tests run fully offline: fixture charts live in `testdata/charts/demo-v1 (manifest as Chart.yaml.tpl)` and
`.../demo-v2` (v2 adds a Service, changes the ConfigMap and extends the
values schema, so `/diff` has something to find); the helm-repo/tgz fetch
path is exercised against `httptest` servers and the OCI path through a fake
`render.OCIPuller`.

## v1 roadmap

- **Auth** — service-to-service auth (mTLS or bearer validation) once exposed
  beyond the cluster boundary; today bearer tokens are accepted and ignored.
- **OCI/repo credentials** — private registries and authenticated helm repos
  (per-request secret refs, not baked config).
- **Chart cache** — content-addressed cache of downloaded archives keyed by
  digest/version to cut repeated pulls for hot charts.
- **Structured diff** — per-path (JSONPatch-style) change detail beyond
  top-level field names.
- **Multi-values renders** — accept a list of values overlays in one call for
  matrix previews.
