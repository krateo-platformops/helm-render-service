---
type: ExampleIndex
title: helm-render-service — examples
description: Runnable examples under examples/ — two live-verifiable request bodies (/render, /diff) and the snowplow RESTAction wiring pattern.
resource: oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service
tags: [examples, render, diff, restaction]
timestamp: 2026-08-07T00:00:00Z
---

# Examples

Each example is a runnable artifact + a README with preconditions and the one
command. The two request-body examples need only a reachable service — a local
`go run .` is enough, no cluster required; the RESTAction needs a stock Krateo
installer deploy.

- [render-inline](../examples/render-inline/README.md) — `POST /render` with an
  inline three-file chart tree (the portal-builder draft path); returns the
  rendered ConfigMap.
- [upgrade-impact-diff](../examples/upgrade-impact-diff/README.md) —
  `POST /diff` between two inline chart versions; exercises `added`, `changed`
  **and** the field-level `valuesSchemaDiff` breakdown.
- [snowplow-chart-preview](../examples/snowplow-chart-preview/README.md) — the
  snowplow `RESTAction` that reaches `/render` through the chart's
  `helm-render-endpoint` Secret, with the filter pattern that surfaces render
  failures as widget data.
