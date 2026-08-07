---
type: Configuration
title: helm-render-service — configuration
description: The whole config surface — every HRS_* env var with its default, and every chart value (image, service, resources, config, snowplowEndpoint), schema-typed.
resource: oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service
tags: [configuration, values, env]
timestamp: 2026-08-07T00:00:00Z
---

# Configuration

Two layers: the binary reads `HRS_*` environment variables (`main.go`); the
chart sets them from `values.config` and wires the deployment around them. The
chart ships a full-coverage `values.schema.json` (`additionalProperties: false`
— a typo'd value fails `helm install`).

## Environment variables (the binary)

| Env var | Default | Meaning |
| --- | --- | --- |
| `HRS_ADDR` | `:8080` | Listen address |
| `HRS_KUBE_VERSION` | `v1.33.0` | `.Capabilities.KubeVersion` presented to templates |
| `HRS_RENDER_TIMEOUT` | `15s` | Per-request render timeout (Go duration); one `/diff` shares a single budget across both renders |
| `HRS_MAX_REQUEST_BYTES` | `2097152` (2 MiB) | Request body cap (`413` beyond it) |
| `HRS_MAX_CHART_BYTES` | `20971520` (20 MiB) | Chart/repo-index download cap (exceeding is a render error: `200` + `{error}`) |
| `HRS_MAX_OUTPUT_BYTES` | `5242880` (5 MiB) | Rendered output cap, manifests + hooks (render error) |
| `HRS_ALLOW_HTTP` | `false` | Allow plain `http://` chart URLs (trusted dev environments only; `file://` and local paths are always rejected) |

An invalid value (unparseable duration/int/bool, bad kube version) is fatal at
startup, by design.

## Chart values

From `chart/values.yaml` (chart name: `krateo-helm-render-service`):

| Value | Default | Effect |
| --- | --- | --- |
| `global.imageRegistry` | `""` | When set, overrides the registry **host** of the image (repository path preserved) — mirror / air-gapped installs |
| `replicaCount` | `1` | Deployment replicas |
| `image.registry` | `ghcr.io` | Image registry host |
| `image.repository` | `krateo-platformops/helm-render-service` | Image repository |
| `image.pullPolicy` | `IfNotPresent` | Pull policy |
| `image.tag` | `""` | Overrides the tag whose default is the chart `appVersion` |
| `imagePullSecrets` | `[]` | Pull secrets |
| `nameOverride` / `fullnameOverride` | `""` | Standard name overrides |
| `service.type` / `service.port` | `ClusterIP` / `8080` | The Service in front of the pod (container port is fixed at 8080) |
| `resources` | requests `100m`/`128Mi`, limits `1`/`512Mi` | Pod resources |
| `config.kubeVersion` | `""` | Sets `HRS_KUBE_VERSION` |
| `config.renderTimeout` | `""` | Sets `HRS_RENDER_TIMEOUT` |
| `config.maxRequestBytes` | `""` | Sets `HRS_MAX_REQUEST_BYTES` |
| `config.maxChartBytes` | `""` | Sets `HRS_MAX_CHART_BYTES` |
| `config.maxOutputBytes` | `""` | Sets `HRS_MAX_OUTPUT_BYTES` |
| `config.allowHTTP` | `false` | When `true`, sets `HRS_ALLOW_HTTP=true` |
| `snowplowEndpoint.enabled` | `true` | Render the snowplow Endpoint Secret ([usage](./usage.md#snowplow-wiring)); default ON so the installer component needs no extra values |
| `snowplowEndpoint.name` | `helm-render-endpoint` | The Secret's name |

**The `config.*` contract**: an empty string leaves the env var unset, so the
binary's built-in default (table above) applies — the chart never restates
binary defaults. The numeric caps are typed int-or-string in the schema
(`x-kubernetes-int-or-string`) so they survive CRD generation when the chart is
consumed as a Krateo composition.

## Fixed by the chart (not values)

- **Security posture**: `automountServiceAccountToken: false` (the service has
  no Kubernetes client — the pod never holds a token), `runAsNonRoot` with
  numeric UID/GID `65532` (distroless `nonroot` is non-numeric; kubelet cannot
  verify `runAsNonRoot` without it), `seccompProfile: RuntimeDefault`,
  `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, all capabilities
  dropped.
- **Probes**: liveness and readiness both `GET /healthz` on the container port.
