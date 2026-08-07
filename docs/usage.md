---
type: Usage
title: helm-render-service — usage
description: Install via the Krateo installer (portal feature) or direct helm install oci://…; the snowplow Endpoint wiring; verifying in-cluster; running locally.
resource: oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service
tags: [install, helm, installer, snowplow]
timestamp: 2026-08-07T00:00:00Z
---

# Usage

## Via the Krateo installer (the normal path)

The Krateo installer pins this chart as the component `krateo-helm-render-service`
(platform tier, **portal** feature) — a stock deploy with the portal enabled
installs it into the Krateo namespace with the chart defaults, including the
snowplow Endpoint Secret (`snowplowEndpoint.enabled` defaults to `true`, so the
component needs no extra `componentValues`). Nothing to do by hand.

## Standalone `helm install`

```sh
helm install krateo-helm-render-service \
  oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service \
  --version 0.3.0 --namespace krateo-system
```

This creates a Deployment and a ClusterIP Service on port `8080`, plus (by
default) the snowplow Endpoint Secret. Use the release name
`krateo-helm-render-service` as above: it matches the chart name, so the
Service is named exactly `krateo-helm-render-service`. Any other release name
`X` yields `X-krateo-helm-render-service` (the standard fullname helper) — and
the Endpoint Secret's `server-url` tracks whatever the real name is.

Verify from inside the cluster:

```sh
kubectl run curl --rm -it --image=curlimages/curl --restart=Never -n krateo-system -- \
  curl -s http://krateo-helm-render-service.krateo-system.svc:8080/healthz
```

## snowplow wiring

With `snowplowEndpoint.enabled=true` (the default) the chart renders a Secret
in the release namespace that snowplow's `endpointRef` resolves as an external
Endpoint — `server-url` is the only required key; the service ignores the
Bearer token snowplow forwards:

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
  server-url: http://krateo-helm-render-service.krateo-system.svc:8080
```

A `RESTAction` api-step can then POST to `/render` or `/diff` — the full
manifest with the failure-surfacing filter pattern is the
[snowplow-chart-preview example](../examples/snowplow-chart-preview/README.md).

## Calling it directly

Any HTTP client works — the contract is [api](./api.md), and
[examples/render-inline](../examples/render-inline/README.md) /
[examples/upgrade-impact-diff](../examples/upgrade-impact-diff/README.md) are
ready-made request bodies:

```sh
curl -s http://krateo-helm-render-service.krateo-system.svc:8080/render \
  -X POST -H 'Content-Type: application/json' -d '{
  "chart": {"url": "oci://ghcr.io/krateo-platformops/charts/frontend", "version": "1.3.5"},
  "values": {}
}'
```

## Running locally (development)

```sh
make build    # bin/helm-render-service
make test     # go test ./...
make vet      # go vet ./...
make docker   # ghcr.io/krateo-platformops/helm-render-service:dev
make run      # go run . (listens on :8080)
```

No cluster is needed for any of it — the tests are fully offline
([overview](./overview.md#tests)), and a locally-running binary serves the
examples as-is.
