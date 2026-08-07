---
type: Architecture
title: helm-render-service — overview
description: What it does and how it works — the client-only render pipeline, the four chart sources, the guardrails, the zero-cluster-access posture, and its place in the portal.
resource: oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service
tags: [architecture, render, helm-sdk]
timestamp: 2026-08-07T00:00:00Z
---

# Overview

helm-render-service answers one question over plain HTTP+JSON: *what would this
chart produce, with these values?* — and its derivative: *what changes between
these two versions?* It exists so the Krateo portal (builder drafts, chart
previews, "upgrade-impact explain") can show rendered manifests and version
diffs **before** anything is installed, on any chart, without granting the
portal path any cluster credentials.

## Design invariants

- **No cluster access, ever.** There is no Kubernetes client and no kubeconfig
  anywhere in the codebase; rendering uses the helm Go SDK's `action.Install`
  with `DryRun` + `DryRunOption: "client"` + `ClientOnly` + `Replace`
  (`internal/render/render.go`), exactly like `helm template`. The template
  `lookup` function returns empty results. The chart pairs this with
  `automountServiceAccountToken: false` — the pod never even holds a token.
- **No external binaries.** Everything is in-process SDK; the runtime image is
  distroless static (`nonroot`, numeric UID 65532, read-only root filesystem,
  all capabilities dropped) — a single-file image with no shell.
- **Stateless.** Nothing is persisted between requests; there is no cache
  (a content-addressed chart cache is a roadmap item, not shipped).
- **Auth: accepted and ignored.** `Authorization: Bearer …` headers (as sent by
  a snowplow Endpoint Secret) pass through untouched; the service is meant to
  run cluster-internal. Real service-to-service auth is on the roadmap.
- **A failed render is data, not a server error.** Fetch/load/render failures
  answer `200` with `{"error": "…"}` so snowplow api-steps surface them as
  widget content; only a malformed request is a `4xx` ([api](./api.md)).

## Request pipeline

`internal/server` decodes the request (body capped at `HRS_MAX_REQUEST_BYTES`),
validates the chart spec and release name, and runs the whole request under one
`HRS_RENDER_TIMEOUT` context — a `/diff` shares a single budget across both of
its renders.

`internal/render.Loader` resolves one of four chart sources
(`internal/render/spec.go`, `load.go`):

1. **inline file tree** — `rawTemplates` / `files`: chart built in memory
   (`loader.LoadFiles`); a single shared top-level directory (tgz-style tree) is
   stripped automatically when no root `Chart.yaml` is present;
2. **OCI reference** — `oci://host/path[:tag]`, anonymous pull via the helm SDK
   registry client; `version` becomes the tag when the reference carries none;
3. **direct archive** — `https://…/chart-X.Y.Z.tgz`;
4. **classic helm repository** — repo base URL + `repo` (the chart name) +
   optional `version` (empty = latest entry in `index.yaml`).

`file://` and local paths are always rejected; plain `http://` only with
`HRS_ALLOW_HTTP=true` (trusted dev). Downloads (archives and repo indexes) are
capped at `HRS_MAX_CHART_BYTES`.

`internal/render.Render` executes the install dry-run in a goroutine (abandoned
on deadline, panics recovered), enforces the `HRS_MAX_OUTPUT_BYTES` cap over
manifests **plus** hook manifests, then splits the multi-document output
preserving helm's rendering order. Hooks are rendered and included — never
executed. CRDs are included (`IncludeCRDs`). Each object is returned with its
parsed header (`apiVersion`, `kind`, `name`, `namespace` — `""` when the
template omits it) and its YAML. Subchart dependencies must be vendored in the
archive/file tree (as `helm package` does); a missing dependency fails the
render as data.

`internal/diff.Compute` keys objects by `(apiVersion, kind, namespace, name)`
and reports `added` / `removed` / `changed` (change summaries name the
top-level fields whose **parsed** content differs — YAML comments such as
helm's `# Source:` lines never count), plus a values-schema comparison at two
grains: the coarse `valuesSchemaChanged` boolean and the field-level
`valuesSchemaDiff` (fields added / removed / newly required / default changed,
dot-paths for nesting) that tells the portal what actually changed in the
create form ([api](./api.md#post-diff)).

## Place in the platform

Deployed by the Krateo installer as the `krateo-helm-render-service` component
(platform tier, portal feature). The chart ships an optional-but-default-on
snowplow **Endpoint Secret** (`helm-render-endpoint`) whose `server-url` points
at the chart's ClusterIP Service — snowplow `RESTAction` POST api-steps resolve
it via `endpointRef` and call `/render` / `/diff`
([usage](./usage.md#snowplow-wiring),
[example](../examples/snowplow-chart-preview/README.md)).

## Tests

The suite runs fully offline: fixture charts under `testdata/charts/demo-v1`
and `demo-v2` (v2 adds a Service, changes the ConfigMap and extends the values
schema, so `/diff` has something to find). Fixtures store their manifest as
`Chart.yaml.tpl` — the test loader renames it to `Chart.yaml` in memory — so
the release workflow's chart discovery (`find . -name Chart.yaml`) never
publishes them. The helm-repo/tgz path is exercised against `httptest` servers
and the OCI path through a fake `render.OCIPuller`.
