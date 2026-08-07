---
type: Example
title: /render — inline chart tree (builder draft)
description: POST an inline Chart.yaml + values.yaml + template to /render and get back the rendered ConfigMap — the portal-builder draft path, no registry involved.
resource: http://krateo-helm-render-service.krateo-system.svc:8080/render
tags: [render, inline, builder]
timestamp: 2026-08-07T00:00:00Z
---

# `/render`: inline chart tree

Renders a three-file draft chart (`rawTemplates` input mode) against a values
overlay. Fully self-contained: no registry, no cluster — the service builds the
chart in memory and runs the helm SDK client-only.

## Preconditions

A reachable helm-render-service. Either:

- **local** — from the repo root: `go run .` (listens on `:8080`), or
- **in-cluster** — a stock Krateo installer deploy (portal feature); run the
  `curl` from a pod and target
  `http://krateo-helm-render-service.krateo-system.svc:8080`.

## Run

```sh
curl -s localhost:8080/render -X POST \
  -H 'Content-Type: application/json' --data @request.json
```

Expected response (`200`): one object — the ConfigMap `preview-hello` with
`greeting: hello krateo` (the `values` overlay wins over the chart's
`values.yaml`). No `valuesSchema` key appears because this draft ships no
`values.schema.json`; the object's `namespace` is `""` because the template
declares none.
