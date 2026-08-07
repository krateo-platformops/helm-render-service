# helm-render-service

Stateless `helm template` render/dry-run HTTP service for the Krateo
self-service program — chart previews and upgrade impact **without touching a
cluster**.

## What is this

It renders a chart (OCI, helm repo, `.tgz` URL or inline file tree) against
values using the helm Go SDK in pure client-only dry-run mode — no Kubernetes
client, no kubeconfig, no external binaries — and diffs two versions object by
object. Portal builders, preview verbs and upgrade-impact call `POST /render`
and `POST /diff` over plain HTTP+JSON; snowplow reaches it through the Endpoint
Secret the chart ships.
Full picture: [docs/index.md](docs/index.md).

## Install

Normally installed by the **Krateo installer** (component
`krateo-helm-render-service`, portal feature). Standalone:

```sh
helm install krateo-helm-render-service \
  oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service \
  --version 0.3.0 --namespace krateo-system
```

Details, snowplow wiring and the in-cluster verify: [docs/usage.md](docs/usage.md).

## Configure

See [docs/configuration.md](docs/configuration.md). Most used:

| Setting | Default | Effect |
|---|---|---|
| `snowplowEndpoint.enabled` | `true` | Ship the `helm-render-endpoint` Secret snowplow RESTActions resolve via `endpointRef`. |
| `config.renderTimeout` | `""` (binary default `15s`) | Per-request render timeout; one `/diff` shares a single budget across both renders. |
| `config.allowHTTP` | `false` | Allow plain `http://` chart URLs (trusted dev only; `file://` always rejected). |

## Examples

- [examples/render-inline](examples/render-inline) — `POST /render` with an
  inline chart tree (builder draft path).
- [examples/upgrade-impact-diff](examples/upgrade-impact-diff) — `POST /diff`
  between two chart versions, incl. the field-level `valuesSchemaDiff`.
- [examples/snowplow-chart-preview](examples/snowplow-chart-preview) — the
  snowplow `RESTAction` wiring pattern.

## Docs

- [docs/index.md](docs/index.md) — the map
- [docs/overview.md](docs/overview.md) — what it does and how it works
- [docs/usage.md](docs/usage.md) — how to install / consume it
- [docs/configuration.md](docs/configuration.md) — the whole config surface
- [docs/api.md](docs/api.md) — the HTTP contract it exposes
- [docs/examples.md](docs/examples.md) — examples index
- [docs/release.md](docs/release.md) — how a release ships
- [docs/log.md](docs/log.md) — curated history

## Develop & release

`make test` (fully offline) / `make run` (listens on `:8080`). Tag `X.Y.Z`
(no `v`) ships image + chart — release runbook: [docs/release.md](docs/release.md).
