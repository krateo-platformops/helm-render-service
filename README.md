# helm-render-service

Stateless `helm template` render/dry-run HTTP service for the Krateo
self-service program. It renders a chart against values **without touching a
cluster**: portal builders, preview verbs and upgrade-impact call it over
plain HTTP+JSON (snowplow reaches it through its existing external
`endpointRef` mechanism).

- **No Kubernetes client, no cluster access.** Rendering uses the helm SDK
  (`action.Install` with `DryRun` + `ClientOnly` + `Replace`), exactly like
  `helm template`.
- **No auth in v0** — the service is meant to run cluster-internal.
- **Stateless** — nothing is persisted between requests.

## API

### `POST /render`

Render a chart against values.

Request body:

```json
{
  "chart": {
    "url": "oci://ghcr.io/org/mychart",
    "repo": "",
    "version": "1.2.3",
    "files": []
  },
  "values": {},
  "releaseName": "preview",
  "namespace": "default"
}
```

Chart sources (exactly one):

| Source | Fields |
| --- | --- |
| OCI registry | `url: "oci://host/path[:tag]"` (+ optional `version` used as the tag) |
| Direct archive | `url: "https://host/path/chart-1.2.3.tgz"` |
| Classic helm repository | `url: "https://charts.example.com"` (repo base URL) + `repo: "<chart name>"` + optional `version` (empty = latest) |
| Inline chart tree (builder drafts) | `files: [{"path": "Chart.yaml", "content": "..."}, ...]` — paths relative to the chart root; `files` is also accepted at the top level of the request as an alias |

Responses:

- `200` — `{"manifests": [{"apiVersion", "kind", "name", "namespace", "yaml"}], "valuesSchema": <object|null>, "notes": <string|null>}`.
  `valuesSchema` is the chart's `values.schema.json` when present; `notes` is
  the rendered `NOTES.txt`.
- `400` — `{"error": "..."}` malformed JSON / invalid chart spec / invalid release name.
- `413` — `{"error": "..."}` request body over the size cap.
- `422` — `{"error": "..."}` chart fetch/load/render failure; the helm error text is passed through (this is where schema-invalid values land).
- `504` — `{"error": "..."}` render exceeded the timeout.

Examples:

```sh
# Inline chart tree (builder draft)
curl -s localhost:8080/render -X POST -H 'Content-Type: application/json' -d '{
  "chart": {"files": [
    {"path": "Chart.yaml", "content": "apiVersion: v2\nname: draft\nversion: 0.1.0\n"},
    {"path": "values.yaml", "content": "who: world\n"},
    {"path": "templates/cm.yaml", "content": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-hello\ndata:\n  greeting: hello {{ .Values.who }}\n"}
  ]},
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
Errors are prefixed with the failing side (`"base: ..."` / `"head: ..."`).

### `GET /healthz`

Returns `200 ok`.

## Guardrails & semantics

- **Request size cap** — default 10 MiB (`413` beyond it).
- **Render timeout** — default 30s per request, covering chart download and
  template execution; a `/diff` shares one budget across both renders (`504`
  on expiry).
- **Chart download cap** — default 50 MiB.
- **`lookup` returns empty** — like `helm template`, the template `lookup`
  function returns empty results because there is no cluster to query.
  Charts whose output depends on `lookup` render, but with those branches
  empty.
- **Hooks are rendered, never executed** — hook manifests are included in
  `manifests` so callers see every object the chart would create; nothing is
  applied anywhere.
- **Dependencies must be vendored** — subcharts have to be bundled in the
  archive/file tree (as `helm package` does); missing dependencies fail the
  render with a `422`.

## Configuration

| Env var | Default | Meaning |
| --- | --- | --- |
| `HRS_ADDR` | `:8080` | Listen address |
| `HRS_KUBE_VERSION` | `v1.33.0` | `.Capabilities.KubeVersion` presented to templates |
| `HRS_RENDER_TIMEOUT` | `30s` | Per-request render timeout |
| `HRS_MAX_REQUEST_BYTES` | `10485760` | Request body cap |
| `HRS_MAX_CHART_BYTES` | `52428800` | Chart download cap |

## Development

```sh
make build    # bin/helm-render-service
make test     # go test ./...
make docker   # ghcr.io/braghettos/helm-render-service:dev
make run      # go run . (listens on :8080)
```

Fixture charts for the tests live in `testdata/charts/demo-v1` and
`.../demo-v2` (v2 adds a Service, changes the ConfigMap and extends the
values schema, so `/diff` has something to find).

## v1 roadmap

- **Auth** — service-to-service auth (mTLS or bearer) once exposed beyond the
  cluster boundary.
- **OCI/repo credentials** — private registries and authenticated helm repos
  (per-request secret refs, not baked config).
- **Chart cache** — content-addressed cache of downloaded archives keyed by
  digest/version to cut repeated pulls for hot charts.
- **Structured diff** — per-path (JSONPatch-style) change detail beyond
  top-level field names.
- **Multi-values renders** — accept a list of values overlays in one call for
  matrix previews.
