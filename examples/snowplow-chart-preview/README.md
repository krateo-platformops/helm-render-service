---
type: Example
title: snowplow RESTAction — chart preview via the Endpoint Secret
description: A RESTAction whose POST api-step reaches /render through the helm-render-endpoint Secret the chart ships — the portal's chart-preview wiring pattern.
resource: restactions.templates.krateo.io
tags: [restaction, snowplow, endpoint, chart-preview]
timestamp: 2026-08-07T00:00:00Z
---

# snowplow RESTAction: chart preview

How the portal actually consumes this service: a snowplow `RESTAction` POSTs to
`/render` through an `endpointRef` that resolves the `helm-render-endpoint`
Secret ([usage](../../docs/usage.md#snowplow-wiring)). The `filter` surfaces
**both** outcomes — `objects` on success, `error` on a failed render — because
`/render` answers `200` either way ([api](../../docs/api.md)).

Illustrative: adapt the chart URL/version, the JQ filter and the namespaces to
your deploy. `payload` is a JSON string (the literal HTTP body).

## Preconditions

- A stock Krateo installer deploy with the portal feature — it installs
  `krateo-helm-render-service` and snowplow, and the chart's default
  `snowplowEndpoint.enabled=true` creates the `helm-render-endpoint` Secret in
  the release namespace (`krateo-system` here).
- Outbound access from the helm-render-service pod to the chart registry the
  payload names (`ghcr.io`).
- To execute it via `GET /call`: a Krateo JWT whose RBAC allows `get` on this
  `restactions.templates.krateo.io` resource.

## Apply

```sh
kubectl apply -f ./manifest.yaml
```
