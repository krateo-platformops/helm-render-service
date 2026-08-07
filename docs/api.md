---
type: API
title: helm-render-service — API
description: The HTTP+JSON contract — POST /render (four chart sources), POST /diff (object diff + field-level valuesSchemaDiff), GET /healthz, and the 200-error model.
resource: oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service
tags: [api, http, render, diff]
timestamp: 2026-08-07T00:00:00Z
---

# API

Three routes (`internal/server/server.go`): `POST /render`, `POST /diff`,
`GET /healthz`. All request/response bodies are JSON. The service owns no CRDs.

**The error model** — a chart that fails to *fetch, load or render* is **data,
not a server error**: the response is `200` with `{"error": "…"}` (so snowplow
api-steps surface it as widget content). Only a malformed request is `400` and
an oversized body `413`. Timeouts get the stable text
`render timed out after <timeout>`.

## `POST /render`

Render a chart against values. Exactly **one** chart source per request:

| Source | Fields |
| --- | --- |
| OCI registry | `chart.url: "oci://host/path[:tag]"` (+ optional `version` used as the tag when the reference carries none; anonymous pull) |
| Direct archive | `chart.url: "https://host/path/chart-1.2.3.tgz"` (also `.tar.gz`) |
| Classic helm repository | `chart.url: "https://charts.example.com"` (repo base URL) + `chart.repo: "<chart name>"` + optional `version` (empty = latest) |
| Inline chart tree | `rawTemplates: {"<path>": "<content>", …}` — paths relative to the chart root. `chart.files` / top-level `files` (`[{"path", "content"}]` arrays) are equivalent spellings |

`file://` URLs and local paths are always rejected (`400`); plain `http://`
only with `HRS_ALLOW_HTTP=true`. Optional fields: `values` (overlay object),
`releaseName` (default `"render"`, validated as a release name), `namespace`
(default `"default"`).

```jsonc
// Mode 1: remote chart
{
  "chart": {
    "url": "oci://ghcr.io/org/mychart",   // oci:// | https://...tgz | https:// repo base URL
    "version": "1.2.3",                   // optional (OCI tag / repo entry; empty = latest for repos)
    "repo": ""                            // chart name, only for helm-repo base URLs
  },
  "values": {},
  "releaseName": "preview",
  "namespace": "default"
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

### Responses

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

  Objects keep helm's rendering order; CRDs are included; hook manifests are
  included (rendered, never executed).

- `200` render failure — `{"error": "<helm error text>"}` (bad values against
  the chart schema, template `fail`, unreachable/oversized chart, missing
  vendored dependency, timeout, …).
- `400` — `{"error": "…"}` malformed JSON / invalid chart spec / rejected URL
  scheme / more than one input mode / invalid release name.
- `413` — `{"error": "…"}` request body over `HRS_MAX_REQUEST_BYTES`.

## `POST /diff`

Render two chart versions with the same values and diff the resulting objects
— this feeds "upgrade-impact explain". `base` and `head` are each a full chart
spec (any of the four sources, inline included). `values`, `releaseName` and
`namespace` are applied to both sides; both renders share **one** timeout
budget.

```sh
curl -s localhost:8080/diff -X POST -H 'Content-Type: application/json' -d '{
  "base": {"url": "oci://ghcr.io/krateo-platformops/charts/snowplow", "version": "1.8.0"},
  "head": {"url": "oci://ghcr.io/krateo-platformops/charts/snowplow", "version": "1.9.0"},
  "values": {}
}'
```

Response `200`:

```jsonc
{
  "added":   [{"apiVersion": "v1", "kind": "Service", "name": "x", "namespace": "default"}],
  "removed": [],
  "changed": [{"ref": {"apiVersion": "apps/v1", "kind": "Deployment", "name": "x", "namespace": ""},
               "summary": "changed top-level fields: spec"}],
  "valuesSchemaChanged": true,
  "valuesSchemaDiff": {                  // omitted when NEITHER version ships a values.schema.json
    "added": ["replicas"],               // fields new in head's schema
    "removed": [],                       // fields gone from head's schema
    "nowRequired": ["replicas"],         // fields required in head but not in base
    "changedDefaults": ["greeting"]      // fields whose default changed
  }
}
```

- Objects are keyed by `(apiVersion, kind, namespace, name)`; a change summary
  names the top-level fields (`spec`, `data`, `metadata`, …) whose **parsed**
  content differs — YAML comments (helm's `# Source:` lines) never count.
- `valuesSchemaChanged` is the coarse structural comparison of the two
  `values.schema.json` payloads. `valuesSchemaDiff` refines it to create-form
  fields: dot-paths for nesting (`database.replicas`), container nodes recurse
  to their leaves. Present (possibly all-empty) whenever at least one version
  ships a schema — so consumers can distinguish "no schema" from "no change".
  Best-effort by design: it names fields, not every JSON-Schema keyword
  (enum/description changes still trip the coarse boolean only).
- Render failures follow the `/render` error model: `200` with
  `{"error": "base: …"}` / `{"error": "head: …"}` naming the failing side.
  `400`s from spec validation carry the same `base:` / `head:` prefixes.

A live-verified request/response pair is
[examples/upgrade-impact-diff](../examples/upgrade-impact-diff/README.md).

## `GET /healthz`

Returns `200` with body `ok` (text/plain). Used by both chart probes.
