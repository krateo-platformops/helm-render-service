---
type: Component
title: helm-render-service — index
description: The map of the helm-render-service doc bundle — the stateless helm-template render/dry-run HTTP service the Krateo portal calls for chart previews and upgrade impact.
resource: oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service
tags: [portal, render, helm, dry-run]
timestamp: 2026-08-07T00:00:00Z
---

# helm-render-service

helm-render-service is the **chart render engine of the Krateo portal**: a
stateless HTTP+JSON service that runs `helm template` semantics in-process (helm
Go SDK, client-only dry run) against a chart from an OCI registry, a classic
helm repository, a `.tgz` URL or an inline file tree — **without ever touching a
cluster**. Portal builders, preview verbs and upgrade-impact call `POST /render`
and `POST /diff`; snowplow reaches it through the Endpoint Secret the chart
ships. It is a flat Go repo: app at the root, chart under `chart/`, one version
line (image and chart publish from the same plain-semver tag).

## The bundle (start here)

- [overview](./overview.md) — what it does and how it works: the render
  pipeline, chart sources, guardrails, the zero-cluster-access posture, where it
  sits between snowplow and the portal.
- [usage](./usage.md) — install via the Krateo installer (portal feature) or
  direct `helm install oci://…`; the snowplow Endpoint wiring; running locally.
- [configuration](./configuration.md) — the whole config surface: `HRS_*` env
  vars and every chart value.
- [api](./api.md) — the HTTP contract: `POST /render`, `POST /diff` (including
  the field-level `valuesSchemaDiff`), `GET /healthz`, the 200-error model.
- [examples](./examples.md) — the runnable examples under `examples/`.
- [release](./release.md) — how a release ships (one tag → image + chart on
  GHCR).
- [log](./log.md) — curated history.
- [llms.txt](./llms.txt) — the version-pinned agent index of this bundle.

## Code map

No separate deep corpus — the repo is small and the code is the reference:
`main.go` (env wiring, HTTP server), `internal/server/` (routes, request
decoding, the error model), `internal/render/` (chart spec validation, loading
from the four sources, the client-only render), `internal/diff/` (object diff +
values-schema field diff), `testdata/charts/` (offline fixture charts for the
test suite — `Chart.yaml` stored as `Chart.yaml.tpl` so the release workflow
never publishes them).
